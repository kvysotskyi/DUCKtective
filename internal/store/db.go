// Package store owns the single embedded DuckDB file — one table per bucket, plus ingestion metadata.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "github.com/marcboeker/go-duckdb"

	"logviewer/internal/appdir"
)

type DB struct {
	sql *sql.DB
}

// Open resolves <UserConfigDir>/logviewer/logviewer.duckdb, creating the directory and running migrations.
func Open() (*DB, error) {
	dir, err := appdir.Dir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return OpenAt(filepath.Join(dir, "logviewer.duckdb"))
}

// OpenAt opens (or creates) the DuckDB file at an explicit path, running migrations. Exists separately from
// Open so tests can point it at a temp file instead of the real per-user config directory.
func OpenAt(path string) (*DB, error) {
	sqlDB, err := sql.Open("duckdb", path)
	if err != nil {
		return nil, err
	}

	db := &DB{sql: sqlDB}
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
		`CREATE TABLE IF NOT EXISTS _meta_bucket_tables (
			bucket_name TEXT PRIMARY KEY,
			table_name TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS _meta_ingested_files (
			bucket_name TEXT NOT NULL,
			file_name TEXT NOT NULL,
			ingested_at TIMESTAMP NOT NULL,
			rows_ingested INTEGER NOT NULL,
			PRIMARY KEY (bucket_name, file_name)
		)`,
	}
	for _, s := range stmts {
		if _, err := db.sql.Exec(s); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}
