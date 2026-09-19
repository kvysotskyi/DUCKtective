# Ducktective

Cross-platform desktop log search app. Wails v2 (Go backend, plain
JS/HTML/CSS frontend), embedded DuckDB, log sources browsed live (GCS via
Application Default Credentials today) — no SQL ever reaches the UI.

## Core concept: Wiretap

A **Wiretap** is a configured log source: which bucket/prefix to read,
which JSON fields to extract into columns (`time`/`level`/`msg` required,
rest arbitrary), and its own retention + auto-load schedule. Each Wiretap
gets one DuckDB table (`w_<id>`). See [internal/store/CLAUDE.md](internal/store/CLAUDE.md).

## Layout

- [main.go](main.go) — Wails bootstrap, binds `*app.App`.
- [internal/app/](internal/app/CLAUDE.md) — the entire Wails-bound surface.
- [internal/store/](internal/store/CLAUDE.md) — embedded DuckDB, Wiretap CRUD, ingest, search.
- [internal/source/](internal/source/CLAUDE.md) — pluggable `Source` interface (GCS is the only impl).
- [internal/gcp/](internal/gcp/CLAUDE.md), [internal/gcs/](internal/gcs/CLAUDE.md) — ADC auth, GCS client.
- [internal/parse/](internal/parse/CLAUDE.md) — NDJSON line → column values.
- [internal/appdir/](internal/appdir/CLAUDE.md) — per-OS data directory.
- [frontend/](frontend/CLAUDE.md) — plain JS/HTML/CSS UI, Vite build only.

## Build / dev

- `wails dev` — hot-reload dev server (the user typically already has this running; avoid launching a second one or running `wails generate module` unless asked).
- `wails build` — packaged app. Requires `build:tags: no_duckdb_arrow` (set in [wails.json](wails.json)) on darwin/arm64, or the go-duckdb Arrow C-Data-Interface symbols fail to link.
- `go build ./... && go vet ./... && go test ./...` — verify backend changes.
- `go test -tags integration ./internal/app/... -run TestIntegrationRealBucket -v` — real GCS round-trip against `gs://bgsa-log-bucket/logs/` (needs real ADC creds).

## Hard-won rules — do not relitigate these

1. **Never use native `<input type="date">`/`type="time">`/`type="datetime-local">`.** They render using OS locale (US month/day order, AM/PM) with no HTML/CSS override — rejected explicitly and repeatedly by the user. The search view's date/time picker is a fully custom calendar popup + numeric HH/MM/SS spinners (see [frontend/src/CLAUDE.md](frontend/src/CLAUDE.md)).
2. **Never convert timezones on log content.** A log line's `time` field is stored and displayed using its literal wall-clock digits, offset discarded — DuckDB's `TIMESTAMP` type has no timezone concept and silently normalizes to UTC on write, which would otherwise shift the displayed hour. This applies only to *log* timestamps; the app's own operational timestamps (last-polled, GCS last-modified) correctly use local time. See [internal/parse/CLAUDE.md](internal/parse/CLAUDE.md).
3. **`wailsruntime.EventsEmit(ctx, ...)` calls `log.Fatalf` (kills the whole process, not just the caller) if `ctx` has no Wails-injected `"events"` value.** Never call an `*App` method that emits progress events (`LoadFilesNow`, `autoLoadNewFiles`) with a bare `context.Background()` — this bit an integration test once; call the lower-level `downloadAll`/`db.LoadFile` directly instead when testing outside a real Wails runtime.
4. **Bulk log ingest uses DuckDB's native Appender API, not SQL `INSERT`.** A batched multi-row `INSERT ... VALUES (...), (...) ON CONFLICT DO NOTHING` was measured at a flat ~600µs/row regardless of batch size against a real 90K-line file; switching to the Appender brought that to ~110K rows/sec. Dedup is file-level (`IsFileLoaded`), not a per-row constraint. See [internal/store/CLAUDE.md](internal/store/CLAUDE.md).
5. **Don't run destructive git ops or `wails generate module` unprompted** — the user often has their own `wails dev`/git session running in parallel.
