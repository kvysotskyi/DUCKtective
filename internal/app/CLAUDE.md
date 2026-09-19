# internal/app

The entire Wails-bound surface. Every exported method on `*App` in
[app.go](app.go) is auto-bound to JS (generated into
`frontend/wailsjs/go/app/App.js` by `wails generate module`, which the
user's own `wails dev` runs automatically — don't invoke it yourself
unless asked). This package has no logic of its own beyond wiring:
validation and querying live in `internal/store`, auth in `internal/gcp`.

## Lifecycle

`Startup(ctx)` opens the DuckDB file (`store.Open()`) and starts the
`scheduler`; `Shutdown` stops it and closes DB/GCS handles. `a.dbErr` is
checked at the top of every method instead of panicking if the DB failed
to open — the UI surfaces that error instead of the app crashing.

## sourceFor — the pluggable-source factory

`sourceFor(w store.Wiretap) (source.Source, error)` switches on
`w.SourceType` and returns a concrete `source.Source`. This is the single
place that knows how to turn a Wiretap's saved config into something
`ListObjects`/`OpenObject`-able — see [internal/CLAUDE.md](../CLAUDE.md) for
how to add a new source type here.

## Load pipeline concurrency

`LoadFilesNow` and `autoLoadNewFiles` share `downloadAll`: bounded
concurrent GCS downloads (`maxConcurrentDownloads = runtime.NumCPU()+2`,
via `errgroup`), streamed back over a channel so `db.LoadFile` (parse +
DuckDB Appender insert, itself internally concurrent for parsing — see
[internal/store/CLAUDE.md](../store/CLAUDE.md)) can start on an earlier file
while later downloads are still in flight. `LoadFilesNow` additionally
filters out already-loaded files via `IsFileLoaded` *before* downloading
them at all — dedup is file-level, not something `LoadFile` does itself.

Both paths log `[download]`/`[LoadFilesNow]`/`[autoLoadNewFiles]` timing
lines via the standard `log` package — check these first if load
performance regresses.

## ⚠️ EventsEmit + context.Background()

`emitSyncProgress` calls `wailsruntime.EventsEmit(a.ctx, ...)`. If `ctx`
has no Wails-injected `"events"` value (e.g. a bare `context.Background()`
in a test), that call does `log.Fatalf` — **which kills the entire test
binary, not just that test.** Never exercise `LoadFilesNow` /
`autoLoadNewFiles` / `SyncWiretapNow` outside a real Wails runtime; call
`downloadAll` + `db.LoadFile` directly instead (see
[integration_test.go](integration_test.go)'s `TestIntegrationLoadPerformance`
for the pattern).

## scheduler.go

One `time.Ticker` (`pollTick = 1 minute`) per open app instance, started
in `Startup`. Each tick, every `AutoLoadEnabled` Wiretap whose
`PollIntervalMinutes` has elapsed since `LastPolledAt` gets
`autoLoadNewFiles` + retention cleanup (if `RetentionDays > 0`) +
`MarkPolled`. There is no background service when the app is closed —
this goroutine simply stops on `Shutdown`.
