package store

import (
	"context"
	"log"
	"os"
	"time"
)

// CompactWiretap copies the wiretap's live rows into a fresh file and swaps it in, so its footprint becomes exactly the live data — in-place rewrites left dead blocks behind (see CLAUDE.md); returns bytes reclaimed.
func (db *DB) CompactWiretap(ctx context.Context, w Wiretap) (int64, error) {
	h, err := db.handle(w)
	if err != nil {
		return 0, err
	}
	h.lockWrite()
	defer h.unlockWrite()
	return compactLocked(ctx, db, h, w)
}

// copyWiretapTo copies w's table and dedup rows from the executing connection's main schema into a brand-new DuckDB file at destPath via ATTACH; CREATE TABLE AS SELECT keeps the load-bearing column order.
func copyWiretapTo(exec func(string, ...any) error, w Wiretap, destPath string) error {
	for _, p := range []string{destPath, destPath + ".wal"} {
		os.Remove(p)
	}
	os.RemoveAll(destPath + ".tmp")

	if err := exec(`ATTACH '` + sqlQuote(destPath) + `' AS dest`); err != nil {
		return err
	}
	defer exec(`DETACH dest`)

	stmts := []string{
		ingestedFilesDDL("dest."),
		`CREATE TABLE dest."` + w.TableName + `" AS SELECT * FROM main."` + w.TableName + `"`,
	}
	for _, s := range stmts {
		if err := exec(s); err != nil {
			return err
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

	exec := func(q string, args ...any) error {
		_, err := h.sql.ExecContext(ctx, q, args...)
		return err
	}
	if err := copyWiretapTo(exec, w, fresh); err != nil {
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
