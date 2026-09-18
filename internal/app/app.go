// Package app is the Wails-bound surface — thin glue over gcp/gcs/store, no logic of its own.
package app

import (
	"context"
	"fmt"
	"time"

	"logviewer/internal/gcp"
	"logviewer/internal/gcs"
	"logviewer/internal/store"
)

type App struct {
	ctx     context.Context
	db      *store.DB
	gcs     *gcs.Client
	dbErr   error
	startAt time.Time
}

func New() *App {
	return &App{}
}

func (a *App) Startup(ctx context.Context) {
	a.ctx = ctx
	a.startAt = time.Now()
	a.db, a.dbErr = store.Open()
}

func (a *App) Shutdown(context.Context) {
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

func (a *App) ListBuckets() ([]gcs.BucketInfo, error) {
	client, err := a.ensureGCS()
	if err != nil {
		return nil, err
	}
	status := gcp.CheckADC(a.ctx)
	if status.ProjectID == "" {
		return nil, fmt.Errorf("no active GCP project — run 'gcloud config set project <id>'")
	}
	return client.ListBuckets(a.ctx, status.ProjectID)
}

// FileEntry is an object listing row plus whether it's already loaded into the local database.
type FileEntry struct {
	gcs.ObjectInfo
	Loaded bool `json:"loaded"`
}

func (a *App) ListObjects(bucket, prefix string) ([]FileEntry, error) {
	if a.dbErr != nil {
		return nil, a.dbErr
	}
	client, err := a.ensureGCS()
	if err != nil {
		return nil, err
	}
	objs, err := client.ListObjects(a.ctx, bucket, prefix)
	if err != nil {
		return nil, err
	}

	entries := make([]FileEntry, len(objs))
	for i, o := range objs {
		loaded, _ := a.db.IsFileLoaded(bucket, o.Name)
		entries[i] = FileEntry{ObjectInfo: o, Loaded: loaded}
	}
	return entries, nil
}

// LoadSummary reports the outcome of loading one file — Error is set instead of failing the whole batch.
type LoadSummary struct {
	Name         string `json:"name"`
	RowsInserted int    `json:"rowsInserted"`
	LinesSkipped int    `json:"linesSkipped"`
	Error        string `json:"error,omitempty"`
}

func (a *App) LoadFiles(bucket string, names []string) ([]LoadSummary, error) {
	if a.dbErr != nil {
		return nil, a.dbErr
	}
	client, err := a.ensureGCS()
	if err != nil {
		return nil, err
	}

	summaries := make([]LoadSummary, 0, len(names))
	for _, name := range names {
		summaries = append(summaries, a.loadOne(client, bucket, name))
	}
	return summaries, nil
}

func (a *App) loadOne(client *gcs.Client, bucket, name string) LoadSummary {
	r, err := client.OpenObject(a.ctx, bucket, name)
	if err != nil {
		return LoadSummary{Name: name, Error: err.Error()}
	}
	defer r.Close()

	result, err := a.db.LoadFile(a.ctx, bucket, name, r)
	if err != nil {
		return LoadSummary{Name: name, Error: err.Error()}
	}
	return LoadSummary{Name: name, RowsInserted: result.RowsInserted, LinesSkipped: result.LinesSkipped}
}

func (a *App) Search(bucket string, filters store.Filters) (store.SearchResult, error) {
	if a.dbErr != nil {
		return store.SearchResult{}, a.dbErr
	}
	return a.db.Search(a.ctx, bucket, filters)
}

func (a *App) DistinctLevels(bucket string) ([]string, error) {
	if a.dbErr != nil {
		return nil, a.dbErr
	}
	return a.db.DistinctLevels(a.ctx, bucket)
}

func (a *App) GetRawLine(bucket, fileHash string) (string, error) {
	if a.dbErr != nil {
		return "", a.dbErr
	}
	return a.db.RawLine(a.ctx, bucket, fileHash)
}

func (a *App) ListLoadedBuckets() ([]string, error) {
	if a.dbErr != nil {
		return nil, a.dbErr
	}
	return a.db.LoadedBuckets()
}

func (a *App) DeleteOlderThan(bucket string, cutoff time.Time) (int64, error) {
	if a.dbErr != nil {
		return 0, a.dbErr
	}
	return a.db.DeleteOlderThan(a.ctx, bucket, cutoff)
}
