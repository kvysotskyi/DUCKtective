//go:build integration

package app

import (
	"context"
	"path/filepath"
	"testing"

	"logviewer/internal/gcp"
	"logviewer/internal/gcs"
	"logviewer/internal/store"
)

const (
	testBucket = "bgsa-log-bucket"
	testPrefix = "logs/"
)

// Exercises real ADC auth, GCS listing/download, and DuckDB ingestion/search end to end.
// Run with: go test -tags integration ./internal/app/... -run TestIntegrationRealBucket -v
func TestIntegrationRealBucket(t *testing.T) {
	ctx := context.Background()

	status := gcp.CheckADC(ctx)
	if !status.Available {
		t.Fatalf("ADC not available: %s", status.Message)
	}
	t.Logf("authenticated as %s, project %s", status.Account, status.ProjectID)

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

	r, err := client.OpenObject(ctx, testBucket, target.Name)
	if err != nil {
		t.Fatalf("OpenObject(%s): %v", target.Name, err)
	}
	result, err := db.LoadFile(ctx, testBucket, target.Name, r)
	r.Close()
	if err != nil {
		t.Fatalf("LoadFile(%s): %v", target.Name, err)
	}
	t.Logf("loaded %s: %d row(s) inserted, %d line(s) skipped", target.Name, result.RowsInserted, result.LinesSkipped)

	res, err := db.Search(ctx, testBucket, store.Filters{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	t.Logf("search returned %d row(s), truncated=%v", len(res.Rows), res.Truncated)
	if len(res.Rows) == 0 {
		return
	}

	raw, err := db.RawLine(ctx, testBucket, res.Rows[0].FileHash)
	if err != nil {
		t.Fatalf("RawLine: %v", err)
	}
	t.Logf("first row raw line: %s", raw)

	if res.Rows[0].EffectiveTS == nil {
		t.Log("first row has no effective_ts (neither time nor :time present) — skipping retention check")
		return
	}
	deleted, err := db.DeleteOlderThan(ctx, testBucket, res.Rows[0].EffectiveTS.AddDate(100, 0, 0))
	if err != nil {
		t.Fatalf("DeleteOlderThan: %v", err)
	}
	t.Logf("retention delete (100y-future cutoff, everything) removed %d row(s)", deleted)
}
