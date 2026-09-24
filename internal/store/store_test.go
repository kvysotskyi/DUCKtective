package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ducktective/internal/parse"
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

func createTestWiretap(t *testing.T, db *DB, name string, fields []parse.Field) Wiretap {
	t.Helper()
	w, err := db.CreateWiretap(context.Background(), WiretapInput{
		Name:       name,
		SourceType: SourceTypeGCS,
		GCS:        &GCSSourceConfig{ProjectID: "test-project", Bucket: "test-bucket"},
		Fields:     fields,
	})
	if err != nil {
		t.Fatalf("CreateWiretap(%q): %v", name, err)
	}
	return w
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
	w := createTestWiretap(t, db, "w1", DefaultFields())

	result, err := db.LoadFile(ctx, w, "f.jsonl", strings.NewReader(sampleLines))
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if result.RowsInserted != 4 {
		t.Errorf("RowsInserted = %d, want 4", result.RowsInserted)
	}
	if result.LinesSkipped != 1 {
		t.Errorf("LinesSkipped = %d, want 1", result.LinesSkipped)
	}

	// LoadFile itself doesn't dedupe (no per-row uniqueness constraint) — that's the caller's job via
	// IsFileLoaded, checked here directly rather than through LoadFile.
	loaded, err := db.IsFileLoaded(ctx, w.ID, "f.jsonl")
	if err != nil {
		t.Fatalf("IsFileLoaded: %v", err)
	}
	if !loaded {
		t.Error("IsFileLoaded = false after LoadFile, want true")
	}

	res, err := db.Search(ctx, w, Filters{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Rows) != 4 {
		t.Fatalf("Search returned %d rows, want 4", len(res.Rows))
	}
}

func TestSearchFiltersAndTimeFallback(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	w := createTestWiretap(t, db, "w2", DefaultFields())

	if _, err := db.LoadFile(ctx, w, "f.jsonl", strings.NewReader(sampleLines)); err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	res, err := db.Search(ctx, w, Filters{Level: "ERROR"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("level=ERROR returned %d rows, want 1", len(res.Rows))
	}
	row := res.Rows[0]
	if row.Time == nil {
		t.Fatal("time is nil, want the \"time\" field's fallback to \":time\" to have resolved it")
	}
	want := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	if !row.Time.Equal(want) {
		t.Errorf("time = %v, want %v (resolved from the :time fallback key)", row.Time, want)
	}
	if row.Fields["msg"] != "second" {
		t.Errorf("msg = %q, want \"second\"", row.Fields["msg"])
	}
	if row.Fields["study_uid"] != "S1" {
		t.Errorf("study_uid = %q, want \"S1\"", row.Fields["study_uid"])
	}

	res, err = db.Search(ctx, w, Filters{Fields: map[string]string{"accession": "A1"}})
	if err != nil {
		t.Fatalf("Search accession: %v", err)
	}
	if len(res.Rows) != 1 || res.Rows[0].Fields["msg"] != "first" {
		t.Fatalf("accession filter mismatch: %+v", res.Rows)
	}

	textRes, err := db.Search(ctx, w, Filters{Text: "gateway"})
	if err != nil {
		t.Fatalf("Search text: %v", err)
	}
	if len(textRes.Rows) != 1 || textRes.Rows[0].Fields["topic"] != "gateway" {
		t.Fatalf("free-text filter over raw mismatch: %+v", textRes.Rows)
	}

	levels, err := db.DistinctLevels(ctx, w)
	if err != nil {
		t.Fatalf("DistinctLevels: %v", err)
	}
	if len(levels) != 3 {
		t.Errorf("DistinctLevels = %v, want 3 distinct non-null levels", levels)
	}

	raw, err := db.RawLine(ctx, w, res.Rows[0].FileHash)
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
	w := createTestWiretap(t, db, "w3", DefaultFields())

	if _, err := db.LoadFile(ctx, w, "f.jsonl", strings.NewReader(sampleLines)); err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	cutoff := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	deleted, err := db.DeleteOlderThan(ctx, w, cutoff)
	if err != nil {
		t.Fatalf("DeleteOlderThan: %v", err)
	}
	// Only the 2024-01-01T00:00:00Z row is strictly before the cutoff.
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1", deleted)
	}

	res, err := db.Search(ctx, w, Filters{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Rows) != 3 {
		t.Errorf("remaining rows = %d, want 3", len(res.Rows))
	}
}

func TestCreateWiretapValidation(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	gcsCfg := &GCSSourceConfig{ProjectID: "p", Bucket: "b"}

	_, err := db.CreateWiretap(ctx, WiretapInput{
		Name: "missing-msg", SourceType: SourceTypeGCS, GCS: gcsCfg,
		Fields: []parse.Field{
			{Column: "time", JSONKeys: []string{"time"}, Required: true},
			{Column: "level", JSONKeys: []string{"level"}, Required: true},
		},
	})
	if err == nil {
		t.Error("expected error for missing required \"msg\" field")
	}

	badColumnFields := append(append([]parse.Field(nil), DefaultFields()...), parse.Field{
		Column: "Bad Col!", JSONKeys: []string{"x"},
	})
	if _, err := db.CreateWiretap(ctx, WiretapInput{Name: "bad-col", SourceType: SourceTypeGCS, GCS: gcsCfg, Fields: badColumnFields}); err == nil {
		t.Error("expected error for invalid column name")
	}

	if _, err := db.CreateWiretap(ctx, WiretapInput{Name: "dup", SourceType: SourceTypeGCS, GCS: gcsCfg, Fields: DefaultFields()}); err != nil {
		t.Fatalf("CreateWiretap: %v", err)
	}
	if _, err := db.CreateWiretap(ctx, WiretapInput{Name: "dup", SourceType: SourceTypeGCS, GCS: gcsCfg, Fields: DefaultFields()}); err == nil {
		t.Error("expected error creating a second wiretap with the same name")
	}
}

func TestUpdateWiretapAddsColumn(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	w := createTestWiretap(t, db, "w4", DefaultFields())

	newFields := append(append([]parse.Field(nil), DefaultFields()...), parse.Field{
		Column: "custom_field", JSONKeys: []string{"custom_field"},
	})
	updated, err := db.UpdateWiretap(ctx, w.ID, WiretapInput{
		Prefix: "logs/", Fields: newFields,
		RetentionDays: 30, AutoLoadEnabled: true, PollIntervalMinutes: 5,
	})
	if err != nil {
		t.Fatalf("UpdateWiretap: %v", err)
	}

	found := false
	for _, f := range updated.Fields {
		if f.Column == "custom_field" {
			found = true
		}
	}
	if !found {
		t.Fatal("custom_field missing from updated wiretap's Fields")
	}
	if updated.Prefix != "logs/" || updated.RetentionDays != 30 || !updated.AutoLoadEnabled || updated.PollIntervalMinutes != 5 {
		t.Errorf("wiretap settings not applied: %+v", updated)
	}

	line := `{"time":"2024-03-01T00:00:00Z","level":"INFO","msg":"hi","custom_field":"xyz"}` + "\n"
	if _, err := db.LoadFile(ctx, updated, "g.jsonl", strings.NewReader(line)); err != nil {
		t.Fatalf("LoadFile after ALTER TABLE: %v", err)
	}
	res, err := db.Search(ctx, updated, Filters{Fields: map[string]string{"custom_field": "xyz"}})
	if err != nil {
		t.Fatalf("Search on new column: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("expected 1 row matching the new field filter, got %d", len(res.Rows))
	}
}

func TestCompactWiretapPreservesRowsAndStaysWritable(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	w := createTestWiretap(t, db, "w6", DefaultFields())

	if _, err := db.LoadFile(ctx, w, "f.jsonl", strings.NewReader(sampleLines)); err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	cutoff := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	if _, err := db.DeleteOlderThan(ctx, w, cutoff); err != nil {
		t.Fatalf("DeleteOlderThan: %v", err)
	}

	if err := db.CompactWiretap(ctx, w); err != nil {
		t.Fatalf("CompactWiretap: %v", err)
	}

	res, err := db.Search(ctx, w, Filters{})
	if err != nil {
		t.Fatalf("Search after compact: %v", err)
	}
	if len(res.Rows) != 3 {
		t.Fatalf("rows after compact = %d, want 3 (the survivors of DeleteOlderThan)", len(res.Rows))
	}

	// The renamed table must still accept new Appender inserts, not just SELECTs.
	line := `{"time":"2024-03-01T00:00:00Z","level":"INFO","msg":"after compact"}` + "\n"
	if _, err := db.LoadFile(ctx, w, "g.jsonl", strings.NewReader(line)); err != nil {
		t.Fatalf("LoadFile after compact: %v", err)
	}
	res, err = db.Search(ctx, w, Filters{})
	if err != nil {
		t.Fatalf("Search after post-compact LoadFile: %v", err)
	}
	if len(res.Rows) != 4 {
		t.Fatalf("rows after post-compact LoadFile = %d, want 4", len(res.Rows))
	}
}

func TestDeleteWiretapDropsTable(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	w := createTestWiretap(t, db, "w5", DefaultFields())

	if err := db.DeleteWiretap(ctx, w.ID); err != nil {
		t.Fatalf("DeleteWiretap: %v", err)
	}

	wiretaps, err := db.ListWiretaps(ctx)
	if err != nil {
		t.Fatalf("ListWiretaps: %v", err)
	}
	if len(wiretaps) != 0 {
		t.Errorf("ListWiretaps = %v, want none after delete", wiretaps)
	}

	if _, err := db.Search(ctx, w, Filters{}); err == nil {
		t.Error("expected an error searching a dropped wiretap's table")
	}
}
