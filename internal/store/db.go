// Package store owns the embedded DuckDB files — a small catalog plus one self-contained file per wiretap.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/marcboeker/go-duckdb"

	"ducktective/internal/appdir"
)

// wiretapMemoryLimit caps each wiretap's own DuckDB instance; block caching is left to the OS page cache instead (see CLAUDE.md).
const wiretapMemoryLimit = "100MB"

// catalogMemoryLimit is higher only because a one-time legacy migration copies tables through the catalog instance.
const catalogMemoryLimit = "512MB"

const (
	catalogFileName = "ducktective.duckdb"
	wiretapsDirName = "wiretaps"
)

// wiretapHandle is one wiretap's open DuckDB instance. writeMu serializes writers (load, retention, compact, ALTER) among themselves; mu only guards sql's validity — every user RLocks it, and compaction takes it exclusively just for the file swap, so reads never wait on a load.
type wiretapHandle struct {
	sql      *sql.DB
	writeMu  sync.Mutex
	mu       sync.RWMutex
	lastUsed atomic.Int64
}

func (h *wiretapHandle) touch() { h.lastUsed.Store(time.Now().UnixNano()) }

// lockWrite serializes with other writers and pins sql; unlock with unlockWrite.
func (h *wiretapHandle) lockWrite() {
	h.writeMu.Lock()
	h.mu.RLock()
	h.touch()
}

func (h *wiretapHandle) unlockWrite() {
	h.mu.RUnlock()
	h.writeMu.Unlock()
}

func (h *wiretapHandle) lockRead() {
	h.mu.RLock()
	h.touch()
}

func (h *wiretapHandle) unlockRead() { h.mu.RUnlock() }

// DB is the catalog plus lazily opened per-wiretap handles; every data method takes the Wiretap it acts on.
type DB struct {
	catalog     *sql.DB
	catalogPath string
	wiretapDir  string

	mu      sync.Mutex
	handles map[string]*wiretapHandle
}

// Open resolves <UserConfigDir>/ducktective/, creating it, and opens the catalog there.
func Open() (*DB, error) {
	dir, err := appdir.Dir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return OpenAt(filepath.Join(dir, catalogFileName))
}

// OpenAt opens the catalog at catalogPath (wiretap files live in a "wiretaps" dir beside it), first migrating a legacy single-file layout if that is what's there.
func OpenAt(catalogPath string) (*DB, error) {
	db := &DB{
		catalogPath: catalogPath,
		wiretapDir:  filepath.Join(filepath.Dir(catalogPath), wiretapsDirName),
		handles:     map[string]*wiretapHandle{},
	}
	if err := os.MkdirAll(db.wiretapDir, 0o755); err != nil {
		return nil, err
	}
	if err := db.recoverInterruptedCatalogSwap(); err != nil {
		return nil, err
	}
	if err := db.openCatalog(); err != nil {
		return nil, err
	}
	if err := db.migrateLegacyLayout(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("legacy layout migration: %w", err)
	}
	return db, nil
}

func (db *DB) openCatalog() error {
	c, err := openDuckDB(db.catalogPath, catalogMemoryLimit)
	if err != nil {
		return err
	}
	if err := migrateCatalog(c); err != nil {
		c.Close()
		return err
	}
	db.catalog = c
	return nil
}

func openDuckDB(path, memoryLimit string) (*sql.DB, error) {
	d, err := sql.Open("duckdb", path+"?memory_limit="+memoryLimit)
	if err != nil {
		return nil, err
	}
	if err := d.Ping(); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

func migrateCatalog(c *sql.DB) error {
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
		`ALTER TABLE _meta_wiretaps ADD COLUMN IF NOT EXISTS load_days_back INTEGER DEFAULT 0`,
		`ALTER TABLE _meta_wiretaps ADD COLUMN IF NOT EXISTS max_size_mb INTEGER DEFAULT 0`,
	}
	for _, s := range stmts {
		if _, err := c.Exec(s); err != nil {
			return fmt.Errorf("migrate catalog: %w", err)
		}
	}
	return nil
}

// ingestedFilesDDL builds the per-wiretap dedup table; qualifier is "" or an attached-database prefix like "wt.".
func ingestedFilesDDL(qualifier string) string {
	return `CREATE TABLE IF NOT EXISTS ` + qualifier + `_meta_ingested_files (
		wiretap_id TEXT NOT NULL,
		file_name TEXT NOT NULL,
		ingested_at TIMESTAMP NOT NULL,
		rows_ingested INTEGER NOT NULL,
		PRIMARY KEY (wiretap_id, file_name)
	)`
}

func (db *DB) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	var first error
	for id, h := range db.handles {
		if err := h.sql.Close(); err != nil && first == nil {
			first = err
		}
		delete(db.handles, id)
	}
	if db.catalog != nil {
		if err := db.catalog.Close(); err != nil && first == nil {
			first = err
		}
		db.catalog = nil
	}
	return first
}

func (db *DB) wiretapPath(id string) string {
	return filepath.Join(db.wiretapDir, id+".duckdb")
}

// WiretapPath is the wiretap's own DuckDB file; its size is that wiretap's real on-disk footprint.
func (db *DB) WiretapPath(w Wiretap) string {
	return db.wiretapPath(w.ID)
}

// handle returns the wiretap's open instance, opening its existing file on first use; only CreateWiretap and the legacy migration may create the file (see createHandle).
func (db *DB) handle(w Wiretap) (*wiretapHandle, error) {
	return db.openHandle(w, false)
}

// createHandle is handle for a wiretap whose file is being created right now.
func (db *DB) createHandle(w Wiretap) (*wiretapHandle, error) {
	return db.openHandle(w, true)
}

func (db *DB) openHandle(w Wiretap, create bool) (*wiretapHandle, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if h, ok := db.handles[w.ID]; ok {
		h.touch()
		return h, nil
	}
	path := db.wiretapPath(w.ID)
	if !create {
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("wiretap %q has no database file at %s: %w", w.ID, path, err)
		}
	}
	d, err := openDuckDB(path, wiretapMemoryLimit)
	if err != nil {
		return nil, err
	}
	if _, err := d.Exec(ingestedFilesDDL("")); err != nil {
		d.Close()
		return nil, err
	}
	h := &wiretapHandle{sql: d}
	h.touch()
	db.handles[w.ID] = h
	return h, nil
}

func (db *DB) closeHandle(id string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	h, ok := db.handles[id]
	if !ok {
		return nil
	}
	delete(db.handles, id)
	return h.sql.Close()
}

// CloseIdle releases wiretap instances unused for longer than maxIdle (each holds up to wiretapMemoryLimit), skipping any currently in use.
func (db *DB) CloseIdle(maxIdle time.Duration) int {
	db.mu.Lock()
	defer db.mu.Unlock()
	closed := 0
	for id, h := range db.handles {
		if time.Since(time.Unix(0, h.lastUsed.Load())) < maxIdle || !h.writeMu.TryLock() {
			continue
		}
		if !h.mu.TryLock() {
			h.writeMu.Unlock()
			continue
		}
		h.sql.Close()
		h.mu.Unlock()
		h.writeMu.Unlock()
		delete(db.handles, id)
		closed++
	}
	return closed
}

// removeWiretapFiles closes the wiretap's instance and deletes its file, WAL, and spill directory.
func (db *DB) removeWiretapFiles(id string) error {
	if err := db.closeHandle(id); err != nil {
		return err
	}
	path := db.wiretapPath(id)
	for _, p := range []string{path, path + ".wal"} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return os.RemoveAll(path + ".tmp")
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// wiretapSize is the wiretap's on-disk footprint: its file plus any not-yet-checkpointed WAL, since fresh rows sit in the WAL until DuckDB folds them in and the file alone under-reports.
func (db *DB) wiretapSize(id string) int64 {
	p := db.wiretapPath(id)
	return fileSize(p) + fileSize(p+".wal")
}
