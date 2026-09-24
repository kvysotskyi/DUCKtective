package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
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
	// `_` and `%` are literal characters in a needle, not LIKE wildcards.
	if wild, err := db.Search(ctx, w, Filters{Text: "study%uid"}); err != nil || len(wild.Rows) != 0 {
		t.Fatalf("needle with %% matched as a wildcard: rows=%d err=%v", len(wild.Rows), err)
	}
	if lit, err := db.Search(ctx, w, Filters{Text: "study_uid"}); err != nil || len(lit.Rows) != 1 {
		t.Fatalf("literal needle with _ : rows=%d err=%v", len(lit.Rows), err)
	}

	levels, err := db.DistinctLevels(ctx, w)
	if err != nil {
		t.Fatalf("DistinctLevels: %v", err)
	}
	if len(levels) != 3 {
		t.Errorf("DistinctLevels = %v, want 3 distinct non-null levels", levels)
	}

	raw, err := db.RawLine(ctx, w, res.Rows[0].SourceFile, res.Rows[0].SourceLine)
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
	if db.NeedsReencode(w) {
		t.Errorf("migrated wiretap file is not in the ZSTD-capable storage format")
	}
	if _, err := os.Stat(db.legacyBackupPath()); err != nil {
		t.Errorf("legacy backup missing: %v", err)
	}
	if legacyLeft, err := db.hasLegacyTables(ctx); err != nil || legacyLeft {
		t.Errorf("catalog still has wiretap tables after migration (err=%v)", err)
	}
	h, err := db.handle(w)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	var fileHashCols int
	if err := h.sql.QueryRow(`SELECT COUNT(*) FROM duckdb_columns() WHERE table_name = ? AND column_name = 'file_hash'`, w.TableName).Scan(&fileHashCols); err != nil {
		t.Fatalf("duckdb_columns: %v", err)
	}
	if fileHashCols != 0 {
		t.Error("legacy file_hash column should have been dropped when the wiretap file was first opened")
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

// randomLines builds n NDJSON lines with incompressible payloads and strictly increasing timestamps.
func randomLines(n int, payloadBytes int) string {
	rng := rand.New(rand.NewSource(1))
	var sb strings.Builder
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	buf := make([]byte, payloadBytes)
	for i := 0; i < n; i++ {
		rng.Read(buf)
		fmt.Fprintf(&sb, `{"time":"%s","level":"INFO","msg":"%x"}`+"\n",
			base.Add(time.Duration(i)*time.Second).Format(time.RFC3339), buf)
	}
	return sb.String()
}

// A whole-file transaction of ~200K rows used to be checkpointed in one go and blew past the memory
// cap with "could not allocate block ... Failed to create checkpoint", which invalidates the whole
// instance. Chunked commits and rowid-batched copies must keep every transaction under even a tiny cap.
func TestLoadAndCompactStayCheckpointSafeUnderTinyMemoryLimit(t *testing.T) {
	prev := wiretapMemoryLimitBytes
	wiretapMemoryLimitBytes = 20 << 20
	t.Cleanup(func() { wiretapMemoryLimitBytes = prev })

	db := openTestDB(t)
	ctx := context.Background()
	w := createTestWiretap(t, db, "w-tiny-cap", DefaultFields())

	const n = 200_000
	result, err := db.LoadFile(ctx, w, "big.jsonl", strings.NewReader(randomLines(n, 100)))
	if err != nil {
		t.Fatalf("LoadFile under 20MB cap: %v", err)
	}
	if result.RowsInserted != n {
		t.Fatalf("RowsInserted = %d, want %d", result.RowsInserted, n)
	}
	if _, err := db.CompactWiretap(ctx, w); err != nil {
		t.Fatalf("CompactWiretap under 20MB cap: %v", err)
	}
	res, err := db.Search(ctx, w, Filters{})
	if err != nil {
		t.Fatalf("Search after compact: %v", err)
	}
	if len(res.Rows) != PageSize {
		t.Errorf("Search returned %d rows, want a full page of %d", len(res.Rows), PageSize)
	}
}

// LoadFile commits per chunk, so a crash can leave a file's rows behind without its dedup marker.
// Re-loading such a file must replace those rows, not duplicate them.
func TestLoadFileReloadAfterPartialAttemptIsIdempotent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	w := createTestWiretap(t, db, "w-reload", DefaultFields())

	if _, err := db.LoadFile(ctx, w, "f.jsonl", strings.NewReader(sampleLines)); err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	h, err := db.handle(w)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if _, err := h.sql.ExecContext(ctx, `DELETE FROM _meta_ingested_files`); err != nil {
		t.Fatalf("simulate missing marker: %v", err)
	}
	if loaded, _ := db.IsFileLoaded(ctx, w, "f.jsonl"); loaded {
		t.Fatal("marker should be gone")
	}

	if _, err := db.LoadFile(ctx, w, "f.jsonl", strings.NewReader(sampleLines)); err != nil {
		t.Fatalf("re-LoadFile: %v", err)
	}
	res, err := db.Search(ctx, w, Filters{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Rows) != 4 {
		t.Errorf("rows after reload = %d, want 4 (the earlier attempt's rows replaced, not duplicated)", len(res.Rows))
	}
}

// A read error mid-file must surface from CommitFile, leave the file unmarked so it is retried, and
// the retry must replace the partial rows rather than add to them.
func TestCommitFileReadErrorLeavesFileUnmarked(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	w := createTestWiretap(t, db, "w-readerr", DefaultFields())

	broken := io.MultiReader(strings.NewReader(sampleLines), iotest.ErrReader(errors.New("connection reset")))
	if _, err := db.LoadFile(ctx, w, "f.jsonl", broken); err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("LoadFile with a failing reader: err = %v, want the read error", err)
	}
	if loaded, _ := db.IsFileLoaded(ctx, w, "f.jsonl"); loaded {
		t.Fatal("a file whose read failed must not be marked loaded")
	}

	if _, err := db.LoadFile(ctx, w, "f.jsonl", strings.NewReader(sampleLines)); err != nil {
		t.Fatalf("retry LoadFile: %v", err)
	}
	res, err := db.Search(ctx, w, Filters{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Rows) != 4 {
		t.Errorf("rows after retry = %d, want 4 (partial rows replaced, not duplicated)", len(res.Rows))
	}
}

// Several files can be prepared (streaming and parsing concurrently) before any is committed; commits
// stay in order and each file's rows and marker land intact.
func TestPrepareManyThenCommitInOrder(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	w := createTestWiretap(t, db, "w-pipeline", DefaultFields())

	names := []string{"a.jsonl", "b.jsonl", "c.jsonl"}
	pending := make([]*PendingFile, len(names))
	for i, n := range names {
		pending[i] = db.PrepareFile(ctx, w, n, strings.NewReader(sampleLines))
	}
	for i, p := range pending {
		result, err := db.CommitFile(ctx, p)
		if err != nil {
			t.Fatalf("CommitFile %s: %v", names[i], err)
		}
		if result.RowsInserted != 4 {
			t.Errorf("%s: RowsInserted = %d, want 4", names[i], result.RowsInserted)
		}
		if loaded, _ := db.IsFileLoaded(ctx, w, names[i]); !loaded {
			t.Errorf("%s not marked loaded", names[i])
		}
	}
	res, err := db.Search(ctx, w, Filters{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Rows) != 12 {
		t.Errorf("total rows = %d, want 12", len(res.Rows))
	}
}

// lineStream streams n realistic NDJSON lines without materialising them, so multi-million-row tests
// don't need gigabytes of test memory.
type lineStream struct {
	n, i int
	buf  []byte
	rng  *rand.Rand
	base time.Time
}

func newLineStream(n int) *lineStream {
	return &lineStream{n: n, rng: rand.New(rand.NewSource(7)), base: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
}

func (s *lineStream) Read(p []byte) (int, error) {
	for len(s.buf) < len(p) && s.i < s.n {
		var trace [16]byte
		s.rng.Read(trace[:])
		ts := s.base.Add(time.Duration(s.i) * 5 * time.Millisecond)
		acc := fmt.Sprintf("ACC%07d", s.rng.Intn(400000))
		s.buf = fmt.Appendf(s.buf, `{"time":"%s","level":"INFO","msg":"processed study for %s in %dms (%x)",":topic":"gateway","accession":"%s","study_uid":"1.2.840.%d","trace":"%x"}`+"\n",
			ts.Format(time.RFC3339Nano), acc, s.rng.Intn(5000), trace[:8], acc, s.rng.Intn(9999999), trace)
		s.i++
	}
	if len(s.buf) == 0 {
		return 0, io.EOF
	}
	n := copy(p, s.buf)
	s.buf = s.buf[n:]
	return n, nil
}

// Opt-in (DUCKTECTIVE_BIG_TESTS=1): loads a few million realistic rows under the production cap and
// compacts them, printing timings. Compaction cost must stay flat per batch — rowid batching visited
// the whole table per batch and blew the cap on a 9.6GB wiretap.
func TestCompactLargeTableUnderCap(t *testing.T) {
	if os.Getenv("DUCKTECTIVE_BIG_TESTS") == "" {
		t.Skip("set DUCKTECTIVE_BIG_TESTS=1 to run")
	}
	db := openTestDB(t)
	ctx := context.Background()
	w := createTestWiretap(t, db, "w-big", DefaultFields())

	const n = 3_000_000
	start := time.Now()
	result, err := db.LoadFile(ctx, w, "big.jsonl", newLineStream(n))
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	loadDur := time.Since(start)
	before := db.wiretapSize(w.ID)
	t.Logf("loaded %d rows (%d MB on disk) in %s", result.RowsInserted, before>>20, loadDur.Round(time.Millisecond))

	start = time.Now()
	if _, err := db.CompactWiretap(ctx, w); err != nil {
		t.Fatalf("CompactWiretap under %d MB cap: %v", wiretapMemoryLimitBytes>>20, err)
	}
	t.Logf("compacted %d MB in %s (cap %d MB)", before>>20, time.Since(start).Round(time.Millisecond), wiretapMemoryLimitBytes>>20)

	res, err := db.Search(ctx, w, Filters{})
	if err != nil || len(res.Rows) != PageSize {
		t.Fatalf("Search after compact: rows=%d err=%v", len(res.Rows), err)
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

func rawCompressions(t *testing.T, h *wiretapHandle, table string) []string {
	t.Helper()
	if _, err := h.sql.Exec(`CHECKPOINT`); err != nil {
		t.Fatalf("CHECKPOINT: %v", err)
	}
	rows, err := h.sql.Query(`SELECT DISTINCT compression FROM pragma_storage_info('` + table + `') WHERE column_name = 'raw' AND segment_type = 'VARCHAR' AND persistent ORDER BY 1`)
	if err != nil {
		t.Fatalf("pragma_storage_info: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

func TestNewWiretapStoresTextAsZstd(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	w := createTestWiretap(t, db, "z1", DefaultFields())
	if _, err := db.LoadFile(ctx, w, "f.jsonl", newLineStream(5000)); err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if v, err := storageVersion(db.WiretapPath(w)); err != nil || v < zstdStorageVersion {
		t.Fatalf("storage version = %d (err %v), want >= %d", v, err, zstdStorageVersion)
	}
	h, err := db.handle(w)
	if err != nil {
		t.Fatal(err)
	}
	if got := rawCompressions(t, h, w.TableName); len(got) != 1 || got[0] != "ZSTD" {
		t.Fatalf("raw column compression = %v, want [ZSTD]", got)
	}
}

func TestLegacyFormatFileIsReencodedByCompaction(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	w := createTestWiretap(t, db, "z2", DefaultFields())
	path := db.WiretapPath(w)
	if err := db.removeWiretapFiles(w.ID); err != nil {
		t.Fatal(err)
	}

	// A plain open uses DuckDB's default (pre-ZSTD) storage format, exactly what DuckDB 1.1 wrote.
	old, err := sql.Open("duckdb", path)
	if err != nil {
		t.Fatal(err)
	}
	ddl := `CREATE TABLE "` + w.TableName + `" (raw TEXT, source_file TEXT, source_line INTEGER, ingested_at TIMESTAMP`
	for _, f := range w.Fields {
		ddl += `, "` + f.Column + `" ` + strings.TrimSuffix(columnType(f.Column), " USING COMPRESSION zstd")
	}
	for _, s := range []string{
		ddl + `)`,
		`INSERT INTO "` + w.TableName + `" SELECT '{"time":"2024-01-01T00:00:00Z","level":"INFO","msg":"' || repeat('m' || i || ' ', 40) || '"}', 'f.jsonl', i, '2024-01-01 00:00:00', '2024-01-01 00:00:00'::TIMESTAMP + INTERVAL (i) SECOND, 'INFO', repeat('m' || i || ' ', 40), 'gateway', 'ACC' || i, '1.2.840.' || i FROM range(1, 30001) r(i)`,
	} {
		if _, err := old.Exec(s); err != nil {
			t.Fatalf("legacy setup: %v", err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	if !db.NeedsReencode(w) {
		t.Fatalf("NeedsReencode = false for a default-format file")
	}
	h, err := db.handle(w)
	if err != nil {
		t.Fatal(err)
	}
	if got := rawCompressions(t, h, w.TableName); len(got) == 1 && got[0] == "ZSTD" {
		t.Fatalf("legacy-format file unexpectedly already ZSTD: %v", got)
	}
	before := db.wiretapSize(w.ID)

	if _, err := db.CompactWiretap(ctx, w); err != nil {
		t.Fatalf("CompactWiretap: %v", err)
	}
	if db.NeedsReencode(w) {
		t.Errorf("NeedsReencode still true after compaction")
	}
	h, err = db.handle(w)
	if err != nil {
		t.Fatal(err)
	}
	if got := rawCompressions(t, h, w.TableName); len(got) != 1 || got[0] != "ZSTD" {
		t.Errorf("raw compression after re-encode = %v, want [ZSTD]", got)
	}
	if after := db.wiretapSize(w.ID); after >= before {
		t.Errorf("re-encoded file is not smaller: %d -> %d bytes", before, after)
	}
	var n int
	if err := h.sql.QueryRow(`SELECT count(*) FROM "` + w.TableName + `"`).Scan(&n); err != nil || n != 30000 {
		t.Fatalf("rows after re-encode = %d (err %v), want 30000", n, err)
	}
	// Column order must survive the rebuild: the Appender binds positionally.
	if _, err := db.LoadFile(ctx, w, "g.jsonl", strings.NewReader(sampleLines)); err != nil {
		t.Fatalf("LoadFile after re-encode: %v", err)
	}
	res, err := db.Search(ctx, w, Filters{Level: "ERROR"})
	if err != nil || len(res.Rows) != 1 || res.Rows[0].Fields["msg"] != "second" || res.Rows[0].Fields["topic"] != "gateway" {
		t.Fatalf("Search after re-encode: rows=%+v err=%v", res.Rows, err)
	}
}

func TestInstanceSettingsApplied(t *testing.T) {
	db := openTestDB(t)
	w := createTestWiretap(t, db, "settings", DefaultFields())
	h, err := db.handle(w)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"late_materialization_max_rows": "256",
		"storage_compatibility_version": storageCompatVersion,
		"preserve_insertion_order":      "false",
	} {
		var got string
		if err := h.sql.QueryRow(`SELECT current_setting(?)`, name).Scan(&got); err != nil {
			t.Fatalf("current_setting(%s): %v", name, err)
		}
		if got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}
