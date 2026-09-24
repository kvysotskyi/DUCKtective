//go:build integration

package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"ducktective/internal/gcp"
	"ducktective/internal/gcs"
	"ducktective/internal/source"
	"ducktective/internal/store"
)

const (
	testBucket = "test"
	testPrefix = "logs/"
)

// Exercises real ADC auth, GCS listing/download, and DuckDB ingestion/search end to end through the
// Wiretap API. Run with: go test -tags integration ./internal/app/... -run TestIntegrationRealBucket -v
func TestIntegrationRealBucket(t *testing.T) {
	ctx := context.Background()

	status := gcp.CheckADC(ctx)
	if !status.Available {
		t.Fatalf("ADC not available: %s", status.Message)
	}
	t.Logf("authenticated as %s, project %s", status.Account, status.ProjectID)

	projects, err := gcp.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	found := false
	for _, p := range projects {
		if p.ID == "test" {
			found = true
		}
	}
	if !found {
		t.Errorf("ListProjects() = %v, want test among them", projects)
	}

	client, err := gcs.NewClient(ctx)
	if err != nil {
		t.Fatalf("gcs.NewClient: %v", err)
	}
	defer client.Close()

	objs, err := client.ListObjects(ctx, testBucket, testPrefix)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(objs) == 0 {
		t.Fatalf("no objects found under gs://%s/%s — adjust testPrefix", testBucket, testPrefix)
	}
	t.Logf("found %d object(s) under prefix", len(objs))

	// Pick the smallest non-empty file to keep the test fast.
	target := objs[0]
	for _, o := range objs {
		if o.Size > 0 && (target.Size == 0 || o.Size < target.Size) {
			target = o
		}
	}
	t.Logf("loading %s (%d bytes)", target.Name, target.Size)

	db, err := store.OpenAt(filepath.Join(t.TempDir(), "integration.duckdb"))
	if err != nil {
		t.Fatalf("store.OpenAt: %v", err)
	}
	defer db.Close()

	w, err := db.CreateWiretap(ctx, store.WiretapInput{
		Name:       "integration-test",
		SourceType: store.SourceTypeGCS,
		GCS:        &store.GCSSourceConfig{ProjectID: "test", Bucket: testBucket},
		Prefix:     testPrefix,
		Fields:     store.DefaultFields(), // time -> ["time", ":time"] fallback, per the real lines below
	})
	if err != nil {
		t.Fatalf("CreateWiretap: %v", err)
	}

	r, err := client.OpenObject(ctx, testBucket, target.Name)
	if err != nil {
		t.Fatalf("OpenObject(%s): %v", target.Name, err)
	}
	result, err := db.LoadFile(ctx, w, target.Name, r)
	r.Close()
	if err != nil {
		t.Fatalf("LoadFile(%s): %v", target.Name, err)
	}
	t.Logf("loaded %s: %d row(s) inserted, %d line(s) skipped", target.Name, result.RowsInserted, result.LinesSkipped)

	res, err := db.Search(ctx, w, store.Filters{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	t.Logf("search returned %d row(s), hasMore=%v", len(res.Rows), res.HasMore)
	if len(res.Rows) == 0 {
		return
	}

	raw, err := db.RawLine(ctx, w, res.Rows[0].SourceFile, res.Rows[0].SourceLine)
	if err != nil {
		t.Fatalf("RawLine: %v", err)
	}
	t.Logf("first row raw line: %s", raw)

	if res.Rows[0].Time == nil {
		t.Log("first row has no resolved time (neither \"time\" nor \":time\" present) — skipping retention check")
		return
	}
	deleted, err := db.DeleteOlderThan(ctx, w, res.Rows[0].Time.AddDate(100, 0, 0))
	if err != nil {
		t.Fatalf("DeleteOlderThan: %v", err)
	}
	t.Logf("retention delete (100y-future cutoff, everything) removed %d row(s)", deleted)

	if err := db.DeleteWiretap(ctx, w.ID); err != nil {
		t.Fatalf("DeleteWiretap: %v", err)
	}
}

// Loads every file under a real, multi-file prefix to exercise the concurrent-download +
// concurrent-parse + batched-insert pipeline together, with the [download]/[ingest] timing logs
// (see downloadAll and DB.LoadFile) visible in -v output.
//
// Deliberately calls downloadAll + db.LoadFile directly rather than going through App.LoadFilesNow:
// LoadFilesNow calls a.emitSyncProgress -> wailsruntime.EventsEmit, which does log.Fatalf (killing the
// whole test binary, not just failing the test) when ctx has no Wails "events" value — which a bare
// context.Background() never does.
//
// Run with: go test -tags integration ./internal/app/... -run TestIntegrationLoadPerformance -v
func TestIntegrationLoadPerformance(t *testing.T) {
	const perfPrefix = "logs/gateway-2026-09"
	ctx := context.Background()

	status := gcp.CheckADC(ctx)
	if !status.Available {
		t.Fatalf("ADC not available: %s", status.Message)
	}

	client, err := gcs.NewClient(ctx)
	if err != nil {
		t.Fatalf("gcs.NewClient: %v", err)
	}
	defer client.Close()

	objs, err := client.ListObjects(ctx, testBucket, perfPrefix)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if len(objs) == 0 {
		t.Fatalf("no objects found under gs://%s/%s", testBucket, perfPrefix)
	}
	names := make([]string, len(objs))
	var totalBytes int64
	for i, o := range objs {
		names[i] = o.Name
		totalBytes += o.Size
	}
	t.Logf("found %d object(s), %d bytes total, under gs://%s/%s", len(names), totalBytes, testBucket, perfPrefix)

	db, err := store.OpenAt(filepath.Join(t.TempDir(), "perf.duckdb"))
	if err != nil {
		t.Fatalf("store.OpenAt: %v", err)
	}
	defer db.Close()

	w, err := db.CreateWiretap(ctx, store.WiretapInput{
		Name:       "perf-test",
		SourceType: store.SourceTypeGCS,
		GCS:        &store.GCSSourceConfig{ProjectID: "test", Bucket: testBucket},
		Prefix:     perfPrefix,
		Fields:     store.DefaultFields(),
	})
	if err != nil {
		t.Fatalf("CreateWiretap: %v", err)
	}
	defer db.DeleteWiretap(ctx, w.ID)

	src := source.NewGCS(client, testBucket)

	start := time.Now()
	var rows, skipped, failed int
	for res := range downloadAll(ctx, src, names) {
		if res.err != nil {
			t.Errorf("download %s: %v", res.name, res.err)
			failed++
			continue
		}
		result, err := db.LoadFile(ctx, w, res.name, res.body)
		res.body.Close()
		if err != nil {
			t.Errorf("LoadFile %s: %v", res.name, err)
			failed++
			continue
		}
		rows += result.RowsInserted
		skipped += result.LinesSkipped
	}
	elapsed := time.Since(start)

	t.Logf("loaded %d file(s) (%d failed), %d row(s) inserted, %d line(s) skipped, in %s (%.0f rows/sec)",
		len(names), failed, rows, skipped, elapsed, float64(rows)/elapsed.Seconds())
}
