package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := OpenAt(filepath.Join(t.TempDir(), "test.duckdb"))
	if err != nil {
		t.Fatalf("OpenAt: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

const sampleLines = `{"time":"2024-01-01T00:00:00Z","level":"INFO","msg":"first","accession":"A1"}
not valid json at all
{":time":"2024-01-02 00:00:00","level":"ERROR","msg":"second",":topic":"gateway","study_uid":"S1"}
{"time":1704240000,"level":"WARN","msg":"epoch seconds"}
{"level":"INFO"}
`

func TestLoadFileDedupeAndSkip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	result, err := db.LoadFile(ctx, "my-bucket", "f.jsonl", strings.NewReader(sampleLines))
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if result.RowsInserted != 4 {
		t.Errorf("RowsInserted = %d, want 4", result.RowsInserted)
	}
	if result.LinesSkipped != 1 {
		t.Errorf("LinesSkipped = %d, want 1", result.LinesSkipped)
	}

	// Loading the same file again must not duplicate rows.
	result2, err := db.LoadFile(ctx, "my-bucket", "f.jsonl", strings.NewReader(sampleLines))
	if err != nil {
		t.Fatalf("LoadFile (reload): %v", err)
	}
	if result2.RowsInserted != 4 {
		t.Errorf("reload RowsInserted = %d, want 4 (same file_hash, ON CONFLICT DO NOTHING)", result2.RowsInserted)
	}

	res, err := db.Search(ctx, "my-bucket", Filters{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Rows) != 4 {
		t.Fatalf("Search returned %d rows, want 4 (no duplicates)", len(res.Rows))
	}
}

func TestSearchFiltersAndEffectiveTS(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if _, err := db.LoadFile(ctx, "b1", "f.jsonl", strings.NewReader(sampleLines)); err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	res, err := db.Search(ctx, "b1", Filters{Level: "ERROR"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("level=ERROR returned %d rows, want 1", len(res.Rows))
	}
	row := res.Rows[0]
	if row.EffectiveTS == nil {
		t.Fatal("effective_ts is nil, want COALESCE(time, colon_time) from :time")
	}
	want := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	if !row.EffectiveTS.Equal(want) {
		t.Errorf("effective_ts = %v, want %v (derived from :time)", row.EffectiveTS, want)
	}
	if row.Msg == nil || *row.Msg != "second" {
		t.Errorf("msg = %v, want \"second\"", row.Msg)
	}
	if row.StudyUID == nil || *row.StudyUID != "S1" {
		t.Errorf("study_uid = %v, want \"S1\"", row.StudyUID)
	}

	res, err = db.Search(ctx, "b1", Filters{Accession: "A1"})
	if err != nil {
		t.Fatalf("Search accession: %v", err)
	}
	if len(res.Rows) != 1 || res.Rows[0].Msg == nil || *res.Rows[0].Msg != "first" {
		t.Fatalf("accession filter mismatch: %+v", res.Rows)
	}

	textRes, err := db.Search(ctx, "b1", Filters{Text: "gateway"})
	if err != nil {
		t.Fatalf("Search text: %v", err)
	}
	if len(textRes.Rows) != 1 || textRes.Rows[0].Topic == nil || *textRes.Rows[0].Topic != "gateway" {
		t.Fatalf("free-text filter over raw mismatch: %+v", textRes.Rows)
	}

	levels, err := db.DistinctLevels(ctx, "b1")
	if err != nil {
		t.Fatalf("DistinctLevels: %v", err)
	}
	if len(levels) != 3 {
		t.Errorf("DistinctLevels = %v, want 3 distinct non-null levels", levels)
	}

	raw, err := db.RawLine(ctx, "b1", res.Rows[0].FileHash)
	if err != nil {
		t.Fatalf("RawLine: %v", err)
	}
	if !strings.Contains(raw, "\"first\"") {
		t.Errorf("RawLine = %q, want the original line back verbatim", raw)
	}
}

func TestDeleteOlderThan(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if _, err := db.LoadFile(ctx, "b1", "f.jsonl", strings.NewReader(sampleLines)); err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	cutoff := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	deleted, err := db.DeleteOlderThan(ctx, "b1", cutoff)
	if err != nil {
		t.Fatalf("DeleteOlderThan: %v", err)
	}
	// Only the 2024-01-01T00:00:00Z row is strictly before the cutoff.
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1", deleted)
	}

	res, err := db.Search(ctx, "b1", Filters{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Rows) != 3 {
		t.Errorf("remaining rows = %d, want 3", len(res.Rows))
	}
}

func TestTableForBucketSanitizesAndTracks(t *testing.T) {
	db := openTestDB(t)

	table, err := db.TableForBucket("My.Bucket-123")
	if err != nil {
		t.Fatalf("TableForBucket: %v", err)
	}
	if table == "" {
		t.Fatal("sanitized table name is empty")
	}

	buckets, err := db.LoadedBuckets()
	if err != nil {
		t.Fatalf("LoadedBuckets: %v", err)
	}
	if len(buckets) != 1 || buckets[0] != "My.Bucket-123" {
		t.Errorf("LoadedBuckets = %v, want the original bucket name preserved", buckets)
	}
}
