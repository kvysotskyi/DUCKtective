# internal/

Backend packages. Dependency direction is one-way — nothing here imports
`internal/app`, keeping the Wails-bound layer a thin shell over plain Go
packages that are independently testable.

```
app     — Wails-bound surface (*App methods). Depends on everything below.
store   — embedded DuckDB: Wiretap CRUD, ingest, search. Depends on parse.
source  — pluggable Source interface. Depends on gcs.
gcp     — ADC auth + GCP project listing. No internal deps.
gcs     — GCS client wrapper. No internal deps.
parse   — NDJSON line → column values. No internal deps.
appdir  — per-OS data directory path. No internal deps.
```

Each package has its own `CLAUDE.md` with specifics:

- [app/CLAUDE.md](app/CLAUDE.md)
- [store/CLAUDE.md](store/CLAUDE.md)
- [source/CLAUDE.md](source/CLAUDE.md)
- [gcp/CLAUDE.md](gcp/CLAUDE.md)
- [gcs/CLAUDE.md](gcs/CLAUDE.md)
- [parse/CLAUDE.md](parse/CLAUDE.md)
- [appdir/CLAUDE.md](appdir/CLAUDE.md)

## Adding a new log source type (e.g. local filesystem, S3)

This is the one cross-cutting extension point in the whole backend:

1. Implement `source.Source` (`ListObjects`, `OpenObject`) — new file in `source/`.
2. Add a `SourceType*` const + config struct in `store/wiretap.go` (see `GCSSourceConfig`), and a case in its `validateSource`.
3. Add a case in `app.sourceFor` wiring the config to your new `source.Source` impl.
4. Add a matching UI sub-form in `frontend/src/main.js` (see `frontend/src/CLAUDE.md`).

Nothing in `store`'s ingest/search path needs to change — it only ever
deals in `io.Reader`/`source.ObjectInfo`.
