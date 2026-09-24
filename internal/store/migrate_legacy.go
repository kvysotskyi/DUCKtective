package store

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

// legacyBackupPath is where the pre-split single-file database is kept after migration — safe to delete once satisfied.
func (db *DB) legacyBackupPath() string {
	return strings.TrimSuffix(db.catalogPath, ".duckdb") + ".legacy.duckdb"
}

func (db *DB) newCatalogPath() string {
	return db.catalogPath + ".new"
}

// recoverInterruptedCatalogSwap finishes a rebuildCatalog that crashed between its two renames (legacy already moved aside, new catalog not yet in place).
func (db *DB) recoverInterruptedCatalogSwap() error {
	if _, err := os.Stat(db.catalogPath); err == nil {
		return nil
	}
	if _, err := os.Stat(db.newCatalogPath()); err != nil {
		return nil
	}
	return os.Rename(db.newCatalogPath(), db.catalogPath)
}

func (db *DB) hasLegacyTables(ctx context.Context) (bool, error) {
	var n int
	err := db.catalog.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM duckdb_tables() WHERE database_name = current_database() AND table_name LIKE 'w\_%' ESCAPE '\'`,
	).Scan(&n)
	return n > 0, err
}

// migrateLegacyLayout splits a pre-per-wiretap catalog (wiretap tables inside ducktective.duckdb) into one file per wiretap, then rebuilds a small catalog.
func (db *DB) migrateLegacyLayout(ctx context.Context) error {
	legacy, err := db.hasLegacyTables(ctx)
	if err != nil || !legacy {
		return err
	}
	wiretaps, err := db.ListWiretaps(ctx)
	if err != nil {
		return err
	}
	log.Printf("[migrate] legacy single-file layout: splitting %d wiretap(s) into %s", len(wiretaps), db.wiretapDir)
	for _, w := range wiretaps {
		if err := db.exportLegacyWiretap(ctx, w); err != nil {
			return fmt.Errorf("wiretap %s: %w", w.ID, err)
		}
	}
	return db.rebuildCatalog(ctx)
}

func sqlQuote(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// exportLegacyWiretap copies one wiretap's table and dedup rows out of the legacy catalog into its own file.
func (db *DB) exportLegacyWiretap(ctx context.Context, w Wiretap) error {
	start := time.Now()
	var rows int64
	if err := db.catalog.QueryRowContext(ctx, `SELECT COUNT(*) FROM main."`+w.TableName+`"`).Scan(&rows); err != nil {
		return err
	}
	exec := func(q string, args ...any) error {
		_, err := db.catalog.ExecContext(ctx, q, args...)
		return err
	}
	path := db.wiretapPath(w.ID)
	if err := copyWiretapTo(exec, w, path); err != nil {
		return err
	}
	log.Printf("[migrate] %s: %d row(s) -> %s in %s", w.Name, rows, path, time.Since(start))
	return nil
}

// rebuildCatalog copies the catalog rows into a fresh small file and swaps it in, keeping the legacy file as a backup.
func (db *DB) rebuildCatalog(ctx context.Context) error {
	if err := db.catalog.Close(); err != nil {
		return err
	}
	db.catalog = nil

	newPath := db.newCatalogPath()
	os.Remove(newPath)
	os.Remove(newPath + ".wal")
	fresh, err := openDuckDB(newPath, catalogMemoryLimit)
	if err != nil {
		return err
	}
	if err := migrateCatalog(fresh); err != nil {
		fresh.Close()
		return err
	}
	stmts := []string{
		`ATTACH '` + sqlQuote(db.catalogPath) + `' AS old (READ_ONLY)`,
		`INSERT INTO _meta_wiretaps (` + wiretapColumns + `) SELECT ` + wiretapColumns + ` FROM old._meta_wiretaps`,
		`DETACH old`,
	}
	for _, s := range stmts {
		if _, err := fresh.ExecContext(ctx, s); err != nil {
			fresh.Close()
			return err
		}
	}
	if err := fresh.Close(); err != nil {
		return err
	}

	backup := db.legacyBackupPath()
	if err := os.Rename(db.catalogPath, backup); err != nil {
		return err
	}
	os.Rename(db.catalogPath+".wal", backup+".wal")
	if err := os.Rename(newPath, db.catalogPath); err != nil {
		return err
	}
	log.Printf("[migrate] done; legacy database kept at %s (delete it once satisfied)", backup)
	return db.openCatalog()
}
