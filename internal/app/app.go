// Package app is the Wails-bound surface — thin glue over gcp/gcs/store, no logic of its own.
package app

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"time"

	"golang.org/x/sync/errgroup"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"ducktective/internal/gcp"
	"ducktective/internal/gcs"
	"ducktective/internal/parse"
	"ducktective/internal/source"
	"ducktective/internal/store"
)

type App struct {
	ctx       context.Context
	db        *store.DB
	gcs       *gcs.Client
	dbErr     error
	scheduler *scheduler
}

func New() *App {
	return &App{}
}

func (a *App) Startup(ctx context.Context) {
	a.ctx = ctx
	a.db, a.dbErr = store.Open()
	if a.dbErr == nil {
		a.scheduler = newScheduler(a)
		a.scheduler.start(ctx)
	}
}

func (a *App) Shutdown(context.Context) {
	if a.scheduler != nil {
		a.scheduler.stop()
	}
	if a.db != nil {
		a.db.Close()
	}
	if a.gcs != nil {
		a.gcs.Close()
	}
}

func (a *App) CheckAuth() gcp.AuthStatus {
	return gcp.CheckADC(a.ctx)
}

func (a *App) ensureGCS() (*gcs.Client, error) {
	if a.gcs != nil {
		return a.gcs, nil
	}
	client, err := gcs.NewClient(a.ctx)
	if err != nil {
		return nil, err
	}
	a.gcs = client
	return client, nil
}

// sourceFor resolves a wiretap's configured source type into a concrete source.Source. Adding a new
// source type later means one more case here (and a client/adapter for it) — nothing else changes.
func (a *App) sourceFor(w store.Wiretap) (source.Source, error) {
	switch w.SourceType {
	case store.SourceTypeGCS:
		if w.GCS == nil {
			return nil, fmt.Errorf("wiretap %q has no GCS config", w.Name)
		}
		client, err := a.ensureGCS()
		if err != nil {
			return nil, err
		}
		return source.NewGCS(client, w.GCS.Bucket), nil
	default:
		return nil, fmt.Errorf("unsupported source type %q", w.SourceType)
	}
}

// filterByAge drops objects older than loadDaysBack days (0 = no limit).
func filterByAge(objs []source.ObjectInfo, loadDaysBack int) []source.ObjectInfo {
	if loadDaysBack <= 0 {
		return objs
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -loadDaysBack)
	kept := objs[:0]
	for _, o := range objs {
		if !o.LastModified.Before(cutoff) {
			kept = append(kept, o)
		}
	}
	return kept
}

func (a *App) ListProjects() ([]gcp.Project, error) {
	return gcp.ListProjects(a.ctx)
}

func (a *App) ListBuckets(projectID string) ([]gcs.BucketInfo, error) {
	if projectID == "" {
		return nil, fmt.Errorf("select a project first")
	}
	client, err := a.ensureGCS()
	if err != nil {
		return nil, err
	}
	return client.ListBuckets(a.ctx, projectID)
}

// DefaultFields pre-fills the wiretap-creation form.
func (a *App) DefaultFields() []parse.Field {
	return store.DefaultFields()
}

func (a *App) CreateWiretap(in store.WiretapInput) (store.Wiretap, error) {
	if a.dbErr != nil {
		return store.Wiretap{}, a.dbErr
	}
	return a.db.CreateWiretap(a.ctx, in)
}

func (a *App) ListWiretaps() ([]store.Wiretap, error) {
	if a.dbErr != nil {
		return nil, a.dbErr
	}
	return a.db.ListWiretaps(a.ctx)
}

func (a *App) UpdateWiretap(id string, in store.WiretapInput) (store.Wiretap, error) {
	if a.dbErr != nil {
		return store.Wiretap{}, a.dbErr
	}
	return a.db.UpdateWiretap(a.ctx, id, in)
}

func (a *App) DeleteWiretap(id string) error {
	if a.dbErr != nil {
		return a.dbErr
	}
	return a.db.DeleteWiretap(a.ctx, id)
}

// FileEntry is an object listing row plus whether it's already loaded into the local database.
type FileEntry struct {
	source.ObjectInfo
	Loaded bool `json:"loaded"`
}

// ListObjectsForWiretap browses a wiretap's own source. prefixOverride lets the create/edit form
// preview a different prefix than the one saved so far; pass "" to use the wiretap's saved prefix.
func (a *App) ListObjectsForWiretap(wiretapID, prefixOverride string) ([]FileEntry, error) {
	if a.dbErr != nil {
		return nil, a.dbErr
	}
	w, err := a.db.GetWiretap(a.ctx, wiretapID)
	if err != nil {
		return nil, err
	}
	src, err := a.sourceFor(w)
	if err != nil {
		return nil, err
	}

	prefix := w.Prefix
	if prefixOverride != "" {
		prefix = prefixOverride
	}

	objs, err := src.ListObjects(a.ctx, prefix)
	if err != nil {
		return nil, err
	}
	objs = filterByAge(objs, w.LoadDaysBack)

	entries := make([]FileEntry, len(objs))
	for i, o := range objs {
		loaded, _ := a.db.IsFileLoaded(a.ctx, w.ID, o.Name)
		entries[i] = FileEntry{ObjectInfo: o, Loaded: loaded}
	}
	return entries, nil
}

// LoadSummary reports the outcome of loading one file — Error is set instead of failing the whole batch.
type LoadSummary struct {
	Name         string `json:"name"`
	RowsInserted int    `json:"rowsInserted"`
	LinesSkipped int    `json:"linesSkipped"`
	// AlreadyLoaded is true when the file was skipped without downloading it because it was already
	// fully loaded — LoadFile itself no longer dedupes (see its doc comment), so this check has to
	// happen before calling it.
	AlreadyLoaded bool   `json:"alreadyLoaded,omitempty"`
	Error         string `json:"error,omitempty"`
}

// maxConcurrentDownloads bounds how many GCS objects are fetched at once. Downloading one file at a
// time made "Load now" on a large backlog dominated by per-file network round-trip latency rather
// than actual transfer or parse time; fetching several concurrently amortizes that latency instead.
// Network fetches are I/O-bound, not CPU-bound, but cores+2 is a reasonable default pool size that
// scales with the machine without needing its own tuning knob.
var maxConcurrentDownloads = runtime.NumCPU() + 2

type downloadResult struct {
	name string
	body io.ReadCloser
	err  error
	dur  time.Duration
}

// downloadAll opens every named object with bounded concurrency and streams each straight to the consumer as a live io.ReadCloser — callers must Close each result's body (see internal/app/CLAUDE.md).
func downloadAll(ctx context.Context, src source.Source, names []string) <-chan downloadResult {
	batchStart := time.Now()
	results := make(chan downloadResult, maxConcurrentDownloads)
	go func() {
		defer close(results)
		defer func() {
			log.Printf("[download] %d file(s), pool=%d, total=%s", len(names), maxConcurrentDownloads, time.Since(batchStart))
		}()
		var g errgroup.Group
		g.SetLimit(maxConcurrentDownloads)
		for _, name := range names {
			g.Go(func() error {
				start := time.Now()
				r, err := src.OpenObject(ctx, name)
				results <- downloadResult{name: name, body: r, err: err, dur: time.Since(start)}
				return nil
			})
		}
		g.Wait()
	}()
	return results
}

func (a *App) emitSyncProgress(wiretapID string, done, total int, current string) {
	wailsruntime.EventsEmit(a.ctx, "wiretap:sync-progress", map[string]any{
		"wiretapId": wiretapID,
		"done":      done,
		"total":     total,
		"current":   current,
	})
}

// LoadFilesNow loads an explicit, user-selected set of object names, in the caller's order.
func (a *App) LoadFilesNow(wiretapID string, names []string) ([]LoadSummary, error) {
	if a.dbErr != nil {
		return nil, a.dbErr
	}
	w, err := a.db.GetWiretap(a.ctx, wiretapID)
	if err != nil {
		return nil, err
	}
	src, err := a.sourceFor(w)
	if err != nil {
		return nil, err
	}

	batchStart := time.Now()
	summaries := make(map[string]LoadSummary, len(names))
	var toDownload []string
	for _, name := range names {
		loaded, err := a.db.IsFileLoaded(a.ctx, w.ID, name)
		if err != nil {
			summaries[name] = LoadSummary{Name: name, Error: err.Error()}
			continue
		}
		if loaded {
			summaries[name] = LoadSummary{Name: name, AlreadyLoaded: true}
			continue
		}
		toDownload = append(toDownload, name)
	}

	done := 0
	for res := range downloadAll(a.ctx, src, toDownload) {
		done++
		a.emitSyncProgress(w.ID, done, len(toDownload), res.name)
		log.Printf("[download] %s: opened in %s", res.name, res.dur)
		if res.err != nil {
			summaries[res.name] = LoadSummary{Name: res.name, Error: res.err.Error()}
			continue
		}
		result, err := a.db.LoadFile(a.ctx, w, res.name, res.body)
		res.body.Close()
		if err != nil {
			summaries[res.name] = LoadSummary{Name: res.name, Error: err.Error()}
			continue
		}
		summaries[res.name] = LoadSummary{Name: res.name, RowsInserted: result.RowsInserted, LinesSkipped: result.LinesSkipped}
	}
	log.Printf("[LoadFilesNow] %d file(s) (%d already loaded) in %s", len(names), len(names)-len(toDownload), time.Since(batchStart))

	ordered := make([]LoadSummary, len(names))
	for i, name := range names {
		ordered[i] = summaries[name]
	}
	return ordered, nil
}

// SyncWiretapNow discovers and loads every not-yet-loaded object under the wiretap's own prefix —
// the same logic the scheduler runs automatically, exposed as a manual "Load now" action.
func (a *App) SyncWiretapNow(wiretapID string) (int, error) {
	if a.dbErr != nil {
		return 0, a.dbErr
	}
	w, err := a.db.GetWiretap(a.ctx, wiretapID)
	if err != nil {
		return 0, err
	}
	return a.autoLoadNewFiles(a.ctx, w)
}

func (a *App) autoLoadNewFiles(ctx context.Context, w store.Wiretap) (int, error) {
	src, err := a.sourceFor(w)
	if err != nil {
		return 0, err
	}
	objs, err := src.ListObjects(ctx, w.Prefix)
	if err != nil {
		return 0, err
	}
	objs = filterByAge(objs, w.LoadDaysBack)

	var toLoad []string
	for _, o := range objs {
		alreadyLoaded, err := a.db.IsFileLoaded(ctx, w.ID, o.Name)
		if err != nil {
			return 0, err
		}
		if !alreadyLoaded {
			toLoad = append(toLoad, o.Name)
		}
	}
	if len(toLoad) == 0 {
		return 0, nil
	}

	batchStart := time.Now()
	loaded := 0
	done := 0
	for res := range downloadAll(ctx, src, toLoad) {
		done++
		a.emitSyncProgress(w.ID, done, len(toLoad), res.name)
		log.Printf("[download] %s: opened in %s", res.name, res.dur)
		if res.err != nil {
			continue // best-effort — the scheduler/manual sync will retry it next tick
		}
		_, err := a.db.LoadFile(ctx, w, res.name, res.body)
		res.body.Close()
		if err == nil {
			loaded++
		}
	}
	log.Printf("[autoLoadNewFiles] %s: %d file(s) in %s", w.Name, len(toLoad), time.Since(batchStart))
	return loaded, nil
}

// RunRetentionNow applies the wiretap's configured retention period immediately.
func (a *App) RunRetentionNow(wiretapID string) (int64, error) {
	if a.dbErr != nil {
		return 0, a.dbErr
	}
	w, err := a.db.GetWiretap(a.ctx, wiretapID)
	if err != nil {
		return 0, err
	}
	if w.RetentionDays <= 0 {
		return 0, fmt.Errorf("wiretap has no retention period configured")
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -w.RetentionDays)
	return a.db.DeleteOlderThan(a.ctx, w, cutoff)
}

// CompactWiretapNow rewrites the wiretap's table to reclaim disk space DELETE never returns to the OS (see internal/store/CLAUDE.md), reporting how many bytes the database file shrank by.
func (a *App) CompactWiretapNow(wiretapID string) (int64, error) {
	if a.dbErr != nil {
		return 0, a.dbErr
	}
	w, err := a.db.GetWiretap(a.ctx, wiretapID)
	if err != nil {
		return 0, err
	}
	before, err := fileSize(a.db.Path())
	if err != nil {
		return 0, err
	}
	if err := a.db.CompactWiretap(a.ctx, w); err != nil {
		return 0, err
	}
	after, err := fileSize(a.db.Path())
	if err != nil {
		return 0, err
	}
	return before - after, nil
}

func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func (a *App) Search(wiretapID string, filters store.Filters) (store.SearchResult, error) {
	if a.dbErr != nil {
		return store.SearchResult{}, a.dbErr
	}
	w, err := a.db.GetWiretap(a.ctx, wiretapID)
	if err != nil {
		return store.SearchResult{}, err
	}
	return a.db.Search(a.ctx, w, filters)
}

func (a *App) DistinctLevels(wiretapID string) ([]string, error) {
	if a.dbErr != nil {
		return nil, a.dbErr
	}
	w, err := a.db.GetWiretap(a.ctx, wiretapID)
	if err != nil {
		return nil, err
	}
	return a.db.DistinctLevels(a.ctx, w)
}

func (a *App) GetRawLine(wiretapID, fileHash string) (string, error) {
	if a.dbErr != nil {
		return "", a.dbErr
	}
	w, err := a.db.GetWiretap(a.ctx, wiretapID)
	if err != nil {
		return "", err
	}
	return a.db.RawLine(a.ctx, w, fileHash)
}
