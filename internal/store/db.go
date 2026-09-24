// Package store owns the single embedded DuckDB file — one table per wiretap, plus ingestion metadata.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "github.com/marcboeker/go-duckdb"

	"ducktective/internal/appdir"
)

type DB struct {
	sql  *sql.DB
	path string
}

// Path returns the on-disk file this DB was opened from — used to report file size before/after CompactWiretap.
func (db *DB) Path() string {
	return db.path
}

// Open resolves <UserConfigDir>/ducktective/ducktective.duckdb, creating the directory and running migrations.
func Open() (*DB, error) {
	dir, err := appdir.Dir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return OpenAt(filepath.Join(dir, "ducktective.duckdb"))
}

// OpenAt opens (or creates) the DuckDB file at an explicit path, running migrations. Exists separately from
// Open so tests can point it at a temp file instead of the real per-user config directory.
func OpenAt(path string) (*DB, error) {
	sqlDB, err := sql.Open("duckdb", path)
	if err != nil {
		return nil, err
	}

	db := &DB{sql: sqlDB, path: path}
	if err := db.migrate(); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return db, nil
}

func (db *DB) Close() error {
	return db.sql.Close()
}

func (db *DB) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS _meta_wiretaps (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			source_type TEXT NOT NULL,
			gcs_config_json TEXT,
			prefix TEXT NOT NULL,
			table_name TEXT NOT NULL,
			fields_json TEXT NOT NULL,
			retention_days INTEGER NOT NULL DEFAULT 0,
			auto_load_enabled BOOLEAN NOT NULL DEFAULT false,
			poll_interval_minutes INTEGER NOT NULL DEFAULT 15,
			created_at TIMESTAMP NOT NULL,
			last_polled_at TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS _meta_ingested_files (
			wiretap_id TEXT NOT NULL,
			file_name TEXT NOT NULL,
			ingested_at TIMESTAMP NOT NULL,
			rows_ingested INTEGER NOT NULL,
			PRIMARY KEY (wiretap_id, file_name)
		)`,
		`ALTER TABLE _meta_wiretaps ADD COLUMN IF NOT EXISTS load_days_back INTEGER DEFAULT 0`,
	}
	for _, s := range stmts {
		if _, err := db.sql.Exec(s); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}
