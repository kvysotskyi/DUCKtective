package app

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"

	"ducktective/internal/store"
)

func TestReencodeIfLegacyRewritesOnce(t *testing.T) {
	ctx := context.Background()
	catalog := filepath.Join(t.TempDir(), "ducktective.duckdb")
	db, err := store.OpenAt(catalog)
	if err != nil {
		t.Fatal(err)
	}
	w, err := db.CreateWiretap(ctx, store.WiretapInput{
		Name: "legacy", SourceType: store.SourceTypeGCS,
		GCS: &store.GCSSourceConfig{ProjectID: "p", Bucket: "b"}, Fields: store.DefaultFields(),
	})
	if err != nil {
		t.Fatal(err)
	}
	path := db.WiretapPath(w)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	os.Remove(path)
	os.Remove(path + ".wal")

	// A plain open writes DuckDB's default (pre-ZSTD) storage format, exactly what DuckDB 1.1 produced.
	old, err := sql.Open("duckdb", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE "` + w.TableName + `" (raw TEXT, source_file TEXT, source_line INTEGER, ingested_at TIMESTAMP, "time" TIMESTAMP, "level" TEXT, msg TEXT, topic TEXT, accession TEXT, study_uid TEXT)`,
		`INSERT INTO "` + w.TableName + `" SELECT '{"msg":"m' || i || '"}', 'f.jsonl', i, now(), now(), 'INFO', 'm' || i, NULL, NULL, NULL FROM range(1, 4) r(i)`,
	} {
		if _, err := old.Exec(q); err != nil {
			t.Fatalf("legacy setup: %v", err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = store.OpenAt(catalog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := newScheduler(&App{db: db})
	if !db.NeedsReencode(w) {
		t.Fatal("NeedsReencode = false for a default-format file")
	}
	s.reencodeIfLegacy(ctx, w, time.Now())
	if db.NeedsReencode(w) {
		t.Fatal("NeedsReencode still true after reencodeIfLegacy")
	}
	res, err := db.Search(ctx, w, store.Filters{})
	if err != nil || len(res.Rows) != 3 {
		t.Fatalf("Search after re-encode: rows=%d err=%v", len(res.Rows), err)
	}

	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	s.reencodeIfLegacy(ctx, w, time.Now())
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) || len(s.reencodeFailed) != 0 {
		t.Fatalf("second reencodeIfLegacy touched the file (mtime %v -> %v) or recorded a failure %v", before.ModTime(), after.ModTime(), s.reencodeFailed)
	}
}
