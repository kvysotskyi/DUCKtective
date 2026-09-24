package store

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

// copyBatchRows sizes one INSERT … SELECT to the instance's memory cap — ~2KB of headroom per row, so ~51K rows at 100MB and ~10K at 20MB; each batch is checkpointed before the next so dirty blocks never accumulate.
func copyBatchRows(capBytes int64) int64 {
	return max(capBytes/2048, 1000)
}

// CompactWiretap copies the wiretap's live rows into a fresh file and swaps it in, so its footprint becomes exactly the live data — in-place rewrites left dead blocks behind (see CLAUDE.md); returns bytes reclaimed.
func (db *DB) CompactWiretap(ctx context.Context, w Wiretap) (reclaimed int64, retErr error) {
	h, err := db.handle(w)
	if err != nil {
		return 0, err
	}
	defer func() { db.heal(w.ID, retErr) }()
	h.lockWrite()
	defer h.unlockWrite()
	return compactLocked(ctx, db, h, w)
}

// cloneTableDDL builds CREATE TABLE for dest with the source table's exact column order (load-bearing, see wiretap.go) and ZSTD on every TEXT column — CREATE TABLE AS … LIMIT 0 would keep the order but drop the compression.
func cloneTableDDL(ctx context.Context, src *sql.DB, table, destTable string) (string, error) {
	rows, err := src.QueryContext(ctx,
		`SELECT column_name, data_type FROM duckdb_columns() WHERE database_name = current_database() AND table_name = ? ORDER BY column_index`, table)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			return "", err
		}
		if typ == "VARCHAR" {
			typ = textColumnType
		}
		cols = append(cols, `"`+name+`" `+typ)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(cols) == 0 {
		return "", fmt.Errorf("table %q not found", table)
	}
	return `CREATE TABLE ` + destTable + ` (` + strings.Join(cols, ", ") + `)`, nil
}

// copyWiretapTo copies w's table and dedup rows from src's main schema into a brand-new DuckDB file at destPath via ATTACH, in time-range batches sized to capBytes (see copyRange); the new file is in storageCompatVersion format with ZSTD text, so this is also how legacy files get re-encoded.
func copyWiretapTo(ctx context.Context, src *sql.DB, w Wiretap, destPath string, capBytes int64) error {
	for _, p := range []string{destPath, destPath + ".wal"} {
		os.Remove(p)
	}
	os.RemoveAll(destPath + ".tmp")

	exec := func(q string, args ...any) error {
		_, err := src.ExecContext(ctx, q, args...)
		return err
	}
	if err := exec(`ATTACH '` + sqlQuote(destPath) + `' AS dest`); err != nil {
		return err
	}
	defer exec(`DETACH dest`)

	table := `"` + w.TableName + `"`
	if err := exec(ingestedFilesDDL("dest.")); err != nil {
		return err
	}
	ddl, err := cloneTableDDL(ctx, src, w.TableName, `dest.`+table)
	if err != nil {
		return err
	}
	if err := exec(ddl); err != nil {
		return err
	}
	maxRows := copyBatchRows(capBytes)
	if err := copyRange(ctx, src, table, `"time"`, `"time" IS NOT NULL`, maxRows); err != nil {
		return err
	}
	if err := copyRange(ctx, src, table, `ingested_at`, `"time" IS NULL`, maxRows); err != nil {
		return err
	}
	return exec(`INSERT INTO dest._meta_ingested_files
		SELECT wiretap_id, file_name, ingested_at, rows_ingested FROM main._meta_ingested_files WHERE wiretap_id = ?`, w.ID)
}

// copyRange copies the rows matching where by bisecting [min(col), max(col)] until each span holds at most maxRows — col must be zone-mapped (a timestamp column) so a span only touches its own row groups; batching on rowid visited every row group per batch and both crawled and blew the cap on a 9.6GB table.
func copyRange(ctx context.Context, src *sql.DB, table, col, where string, maxRows int64) error {
	var lo, hi sql.NullTime
	if err := src.QueryRowContext(ctx, `SELECT MIN(`+col+`), MAX(`+col+`) FROM main.`+table+` WHERE `+where).Scan(&lo, &hi); err != nil {
		return err
	}
	if !lo.Valid {
		return nil
	}
	return copySpan(ctx, src, table, col, where, lo.Time, hi.Time.Add(time.Microsecond), maxRows)
}

// copySpan copies one half-open span, splitting it when it holds too many rows or when DuckDB's (non-fatal) Out of Memory says the batch was still too big for the cap.
func copySpan(ctx context.Context, src *sql.DB, table, col, where string, lo, hi time.Time, maxRows int64) error {
	span := ` WHERE ` + where + ` AND ` + col + ` >= ? AND ` + col + ` < ?`
	var n int64
	if err := src.QueryRowContext(ctx, `SELECT COUNT(*) FROM main.`+table+span, lo, hi).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	splittable := n > 1 && hi.Sub(lo) > time.Microsecond
	bisect := func() error {
		mid := lo.Add(hi.Sub(lo) / 2)
		if err := copySpan(ctx, src, table, col, where, lo, mid, maxRows); err != nil {
			return err
		}
		return copySpan(ctx, src, table, col, where, mid, hi, maxRows)
	}
	if n > maxRows && splittable {
		return bisect()
	}
	if _, err := src.ExecContext(ctx, `INSERT INTO dest.`+table+` SELECT * FROM main.`+table+span, lo, hi); err != nil {
		if splittable && strings.Contains(err.Error(), "Out of Memory") {
			return bisect()
		}
		return fmt.Errorf("copy %s in [%s, %s) (%d rows): %w", col, lo.Format(time.RFC3339Nano), hi.Format(time.RFC3339Nano), n, err)
	}
	if _, err := src.ExecContext(ctx, `CHECKPOINT dest`); err != nil {
		return fmt.Errorf("checkpoint after %s < %s: %w", col, hi.Format(time.RFC3339Nano), err)
	}
	return nil
}

// compactLocked runs with the caller holding lockWrite; the copy proceeds alongside readers, and only the file swap takes mu exclusively.
func compactLocked(ctx context.Context, db *DB, h *wiretapHandle, w Wiretap) (int64, error) {
	start := time.Now()
	before := db.wiretapSize(w.ID)
	path := db.wiretapPath(w.ID)
	fresh := path + ".compact"

	if err := copyWiretapTo(ctx, h.sql, w, fresh, wiretapMemoryLimitBytes); err != nil {
		return 0, err
	}

	h.mu.RUnlock()
	h.mu.Lock()
	swapErr := swapWiretapFile(h, path, fresh)
	h.mu.Unlock()
	h.mu.RLock()
	if swapErr != nil {
		db.closeHandle(w.ID)
		return 0, swapErr
	}

	after := db.wiretapSize(w.ID)
	log.Printf("[compact] %s: %d -> %d bytes (reclaimed %d) in %s", w.Name, before, after, before-after, time.Since(start))
	return before - after, nil
}

func swapWiretapFile(h *wiretapHandle, path, fresh string) error {
	if err := h.sql.Close(); err != nil {
		return err
	}
	os.Remove(path + ".wal")
	os.Remove(fresh + ".wal")
	if err := os.Rename(fresh, path); err != nil {
		return err
	}
	os.RemoveAll(path + ".tmp")
	reopened, err := openDuckDB(path, wiretapMemoryLimitBytes)
	if err != nil {
		return err
	}
	h.sql = reopened
	return nil
}
