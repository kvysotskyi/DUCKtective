package store

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"os"
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
	loaded, err := db.IsFileLoaded(ctx, w, "f.jsonl")
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

func TestLoadFileMissingFieldIsNullNotEmptyString(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	w := createTestWiretap(t, db, "w-missing-field", DefaultFields())

	if _, err := db.LoadFile(ctx, w, "f.jsonl", strings.NewReader(sampleLines)); err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	res, err := db.Search(ctx, w, Filters{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	// sampleLines' last valid line, {"level":"INFO"}, has no "msg" or "time" key at all — its msg
	// column must come back as an absent map key (SQL NULL), not present-but-"" (which parse.Line's
	// pooled []*string values must distinguish via nil, same as the map it replaced).
	found := false
	for _, row := range res.Rows {
		if row.Time == nil {
			found = true
			if _, hasMsg := row.Fields["msg"]; hasMsg {
				t.Errorf("Fields[\"msg\"] present for the row with no msg key, want it absent entirely: %+v", row.Fields)
			}
		}
	}
	if !found {
		t.Fatal("no row with a nil time found — expected the {\"level\":\"INFO\"} line to survive with time unset")
	}
}

func TestLoadFileChunkBoundariesKeepAbsoluteLineNumbers(t *testing.T) {
	// Shrink the chunk so sampleLines' 5 lines span three chunks ([1,2] [3,4] [5]); source_line and
	// therefore file_hash must still reflect the absolute position in the file, not the chunk offset.
	prev := ingestChunkLines
	ingestChunkLines = 2
	t.Cleanup(func() { ingestChunkLines = prev })

	db := openTestDB(t)
	ctx := context.Background()
	w := createTestWiretap(t, db, "w-chunks", DefaultFields())

	result, err := db.LoadFile(ctx, w, "f.jsonl", strings.NewReader(sampleLines))
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if result.RowsInserted != 4 || result.LinesSkipped != 1 {
		t.Fatalf("RowsInserted=%d LinesSkipped=%d, want 4 and 1", result.RowsInserted, result.LinesSkipped)
	}

	res, err := db.Search(ctx, w, Filters{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := map[int]bool{}
	for _, row := range res.Rows {
		got[row.SourceLine] = true
	}
	for _, want := range []int{1, 3, 4, 5} {
		if !got[want] {
			t.Errorf("source_line %d missing; got %v (line 2 is the invalid-JSON line)", want, got)
		}
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

	if _, err := db.CompactWiretap(ctx, w); err != nil {
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
	if _, err := os.Stat(db.WiretapPath(w)); !os.IsNotExist(err) {
		t.Errorf("searching a deleted wiretap must not recreate its file (stat err=%v)", err)
	}
}

func TestDeleteWiretapRemovesFile(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	w := createTestWiretap(t, db, "w-file", DefaultFields())
	if _, err := db.LoadFile(ctx, w, "f.jsonl", strings.NewReader(sampleLines)); err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	path := db.WiretapPath(w)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("wiretap file %s should exist after load: %v", path, err)
	}
	got, err := db.GetWiretap(ctx, w.ID)
	if err != nil {
		t.Fatalf("GetWiretap: %v", err)
	}
	if got.SizeBytes <= 0 {
		t.Errorf("SizeBytes = %d, want the wiretap file's size", got.SizeBytes)
	}

	if err := db.DeleteWiretap(ctx, w.ID); err != nil {
		t.Fatalf("DeleteWiretap: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("wiretap file still present after delete (stat err=%v)", err)
	}
}

func TestCloseIdleReleasesAndReopens(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	w := createTestWiretap(t, db, "w-idle", DefaultFields())
	if _, err := db.LoadFile(ctx, w, "f.jsonl", strings.NewReader(sampleLines)); err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if n := db.CloseIdle(0); n != 1 {
		t.Fatalf("CloseIdle(0) closed %d handle(s), want 1", n)
	}
	res, err := db.Search(ctx, w, Filters{})
	if err != nil {
		t.Fatalf("Search after CloseIdle should transparently reopen: %v", err)
	}
	if len(res.Rows) != 4 {
		t.Errorf("rows after reopen = %d, want 4", len(res.Rows))
	}
}

// Builds the pre-split layout by hand (wiretap table and dedup rows inside the catalog file), then
// opens it and checks everything moved into wiretaps/<id>.duckdb with the data and dedup state intact.
func TestLegacyLayoutMigrates(t *testing.T) {
	dir := t.TempDir()
	catalogPath := filepath.Join(dir, "ducktective.duckdb")

	legacy, err := sql.Open("duckdb", catalogPath)
	if err != nil {
		t.Fatalf("open legacy: %v", err)
	}
	for _, s := range []string{
		`CREATE TABLE _meta_wiretaps (
			id TEXT PRIMARY KEY, name TEXT NOT NULL, source_type TEXT NOT NULL, gcs_config_json TEXT,
			prefix TEXT NOT NULL, table_name TEXT NOT NULL, fields_json TEXT NOT NULL,
			retention_days INTEGER NOT NULL DEFAULT 0, auto_load_enabled BOOLEAN NOT NULL DEFAULT false,
			poll_interval_minutes INTEGER NOT NULL DEFAULT 15, created_at TIMESTAMP NOT NULL, last_polled_at TIMESTAMP)`,
		`CREATE TABLE _meta_ingested_files (wiretap_id TEXT NOT NULL, file_name TEXT NOT NULL,
			ingested_at TIMESTAMP NOT NULL, rows_ingested INTEGER NOT NULL, PRIMARY KEY (wiretap_id, file_name))`,
		`CREATE TABLE w_legacy (file_hash TEXT, raw TEXT, source_file TEXT, source_line INTEGER, ingested_at TIMESTAMP,
			"time" TIMESTAMP, "level" TEXT, "msg" TEXT)`,
		`INSERT INTO _meta_wiretaps VALUES ('legacy', 'legacy', 'gcs', '{"projectId":"p","bucket":"b"}', 'logs/', 'w_legacy',
			'[{"column":"time","jsonKeys":["time"],"required":true},{"column":"level","jsonKeys":["level"],"required":true},{"column":"msg","jsonKeys":["msg"],"required":true}]',
			0, false, 15, '2024-01-01 00:00:00', NULL)`,
		`INSERT INTO w_legacy VALUES
			('h1', '{"time":"2024-01-01T00:00:00Z","level":"INFO","msg":"one"}', 'f.jsonl', 1, '2024-01-01 00:00:00', '2024-01-01 00:00:00', 'INFO', 'one'),
			('h2', '{"time":"2024-01-02T00:00:00Z","level":"WARN","msg":"two"}', 'f.jsonl', 2, '2024-01-01 00:00:00', '2024-01-02 00:00:00', 'WARN', 'two')`,
		`INSERT INTO _meta_ingested_files VALUES ('legacy', 'f.jsonl', '2024-01-01 00:00:00', 2)`,
	} {
		if _, err := legacy.Exec(s); err != nil {
			t.Fatalf("legacy setup %q: %v", s[:40], err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy: %v", err)
	}

	db, err := OpenAt(catalogPath)
	if err != nil {
		t.Fatalf("OpenAt (migration): %v", err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()

	wiretaps, err := db.ListWiretaps(ctx)
	if err != nil {
		t.Fatalf("ListWiretaps: %v", err)
	}
	if len(wiretaps) != 1 || wiretaps[0].ID != "legacy" || wiretaps[0].TableName != "w_legacy" {
		t.Fatalf("ListWiretaps = %+v, want the single migrated wiretap", wiretaps)
	}
	w := wiretaps[0]

	if _, err := os.Stat(db.WiretapPath(w)); err != nil {
		t.Fatalf("per-wiretap file missing after migration: %v", err)
	}
	if _, err := os.Stat(db.legacyBackupPath()); err != nil {
		t.Errorf("legacy backup missing: %v", err)
	}
	if legacyLeft, err := db.hasLegacyTables(ctx); err != nil || legacyLeft {
		t.Errorf("catalog still has wiretap tables after migration (err=%v)", err)
	}

	res, err := db.Search(ctx, w, Filters{})
	if err != nil {
		t.Fatalf("Search after migration: %v", err)
	}
	if len(res.Rows) != 2 {
		t.Fatalf("rows after migration = %d, want 2", len(res.Rows))
	}
	loaded, err := db.IsFileLoaded(ctx, w, "f.jsonl")
	if err != nil || !loaded {
		t.Errorf("IsFileLoaded after migration = %v, %v; want true", loaded, err)
	}

	// The copied table must keep the physical column order the Appender binds to.
	line := `{"time":"2024-01-03T00:00:00Z","level":"INFO","msg":"three"}` + "\n"
	if _, err := db.LoadFile(ctx, w, "g.jsonl", strings.NewReader(line)); err != nil {
		t.Fatalf("LoadFile into migrated table: %v", err)
	}
	res, err = db.Search(ctx, w, Filters{Fields: map[string]string{"msg": "three"}})
	if err != nil || len(res.Rows) != 1 {
		t.Fatalf("Search for the post-migration row: rows=%d err=%v", len(res.Rows), err)
	}
}

func TestApplyRetentionSizeCapTrimsAndCompacts(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	w, err := db.CreateWiretap(ctx, WiretapInput{
		Name: "w-cap", SourceType: SourceTypeGCS,
		GCS:    &GCSSourceConfig{ProjectID: "p", Bucket: "b"},
		Fields: DefaultFields(), MaxSizeMB: 1,
	})
	if err != nil {
		t.Fatalf("CreateWiretap: %v", err)
	}

	// Incompressible payload so the file genuinely exceeds the 1MB cap.
	rng := rand.New(rand.NewSource(1))
	var sb strings.Builder
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 20000; i++ {
		buf := make([]byte, 80)
		rng.Read(buf)
		fmt.Fprintf(&sb, `{"time":"%s","level":"INFO","msg":"%x"}`+"\n",
			base.Add(time.Duration(i)*time.Second).Format(time.RFC3339), buf)
	}
	if _, err := db.LoadFile(ctx, w, "big.jsonl", strings.NewReader(sb.String())); err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	before := db.wiretapSize(w.ID)
	if before <= 1<<20 {
		t.Fatalf("test needs a file above the 1MB cap, got %d bytes", before)
	}

	res, err := db.ApplyRetention(ctx, w, time.Now().UTC())
	if err != nil {
		t.Fatalf("ApplyRetention: %v", err)
	}
	if res.RowsDeleted == 0 || !res.Compacted {
		t.Errorf("size cap should trim and compact: %+v", res)
	}
	if res.SizeBytes >= before {
		t.Errorf("file did not shrink: before=%d after=%d", before, res.SizeBytes)
	}
	remaining, err := db.Search(ctx, w, Filters{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(remaining.Rows) == 0 {
		t.Error("size cap deleted everything; it should trim the oldest fraction per pass")
	}
	// Oldest rows go first: the newest line must survive.
	if remaining.Rows[0].Time == nil || !remaining.Rows[0].Time.Equal(base.Add(19999*time.Second)) {
		t.Errorf("newest row missing or wrong after trim: %+v", remaining.Rows[0].Time)
	}
}
