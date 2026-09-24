package store

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"time"
)

// copyBatchRows bounds one INSERT … SELECT during a copy; each batch is checkpointed before the next so dirty blocks never accumulate past the memory cap.
const copyBatchRows = 20_000

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

// copyWiretapTo copies w's table and dedup rows from src's main schema into a brand-new DuckDB file at destPath via ATTACH, in rowid batches; the schema is cloned with CREATE TABLE AS SELECT … LIMIT 0 so the load-bearing column order survives.
func copyWiretapTo(ctx context.Context, src *sql.DB, w Wiretap, destPath string) error {
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
	if err := exec(`CREATE TABLE dest.` + table + ` AS SELECT * FROM main.` + table + ` LIMIT 0`); err != nil {
		return err
	}

	var maxRow int64
	if err := src.QueryRowContext(ctx, `SELECT COALESCE(MAX(rowid), -1) FROM main.`+table).Scan(&maxRow); err != nil {
		return err
	}
	for lo := int64(0); lo <= maxRow; lo += copyBatchRows {
		if err := exec(`INSERT INTO dest.`+table+` SELECT * FROM main.`+table+` WHERE rowid >= ? AND rowid < ?`, lo, lo+copyBatchRows); err != nil {
			return fmt.Errorf("copy rows %d..%d: %w", lo, lo+copyBatchRows, err)
		}
		if err := exec(`CHECKPOINT dest`); err != nil {
			return fmt.Errorf("checkpoint after rows %d..%d: %w", lo, lo+copyBatchRows, err)
		}
	}
	return exec(`INSERT INTO dest._meta_ingested_files
		SELECT wiretap_id, file_name, ingested_at, rows_ingested FROM main._meta_ingested_files WHERE wiretap_id = ?`, w.ID)
}

// compactLocked runs with the caller holding lockWrite; the copy proceeds alongside readers, and only the file swap takes mu exclusively.
func compactLocked(ctx context.Context, db *DB, h *wiretapHandle, w Wiretap) (int64, error) {
	start := time.Now()
	before := db.wiretapSize(w.ID)
	path := db.wiretapPath(w.ID)
	fresh := path + ".compact"

	if err := copyWiretapTo(ctx, h.sql, w, fresh); err != nil {
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
	reopened, err := openDuckDB(path, wiretapMemoryLimit)
	if err != nil {
		return err
	}
	h.sql = reopened
	return nil
}
