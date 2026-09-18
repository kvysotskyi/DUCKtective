# Log Viewer

Desktop app (macOS + Windows) for browsing GCS buckets, loading NDJSON log
files into an embedded database, and searching them with point-and-click
filters — no SQL exposed anywhere in the UI.

## Stack

- Go 1.23+, [Wails v2](https://wails.io) for the desktop shell
- [DuckDB](https://duckdb.org) embedded in-process via `go-duckdb` (CGO)
- GCS access via `cloud.google.com/go/storage`, authenticated purely
  through local Application Default Credentials — no service account keys
- Plain HTML/CSS/JS frontend (Vite for the dev/build step only, no framework)

## Prerequisites

- Go 1.23+ and the [Wails CLI](https://wails.io/docs/gettingstarted/installation)
- Node.js (for the frontend's Vite build step)
- A C toolchain for the DuckDB CGO build (Xcode command line tools on
  macOS; MinGW on Windows — see `.github/workflows/build.yml` for how CI
  installs it)
- `gcloud` CLI, with Application Default Credentials set up:
  ```bash
  gcloud auth application-default login
  gcloud config set project <your-project>
  ```

## Development

```bash
wails dev
```

Runs a Vite dev server with hot reload for the frontend, backed by the
real Go app. Browsing `http://localhost:34115` also lets you call the
bound Go methods directly from devtools.

## Building

```bash
wails build
```

Note: `wails.json` sets `build:tags` to `no_duckdb_arrow` — go-duckdb's
optional Arrow C-Data-Interface bridge references symbols the bundled
static lib doesn't export on darwin/arm64, and this app only uses
`database/sql`, so it's excluded. Don't drop that tag without confirming
the link still succeeds on every target platform.

## Storage

One DuckDB file per user, at the OS's per-user config directory
(`~/Library/Application Support/logviewer` on macOS, `%AppData%\logviewer`
on Windows) — one table per bucket, sanitized from the bucket name.

## Per-bucket field mapping overrides

Column-to-JSON-key mapping defaults to the table in the build spec (`time`,
`:time`, `level`, `msg`, `:topic`, `accession`, `study_uid`). A bucket using
different key spellings can override any subset by dropping a JSON file at:

```
<config dir>/logviewer/rules/<bucket-name>.json
```

e.g.:

```json
{
  "msg": "message",
  "colon_topic": "topic"
}
```

Any field not set falls back to the default spelling.

## Testing

```bash
go test ./...                                  # unit tests, no network
go test -tags integration ./internal/app/...   # hits a real GCS bucket via ADC
```

The integration test targets a specific bucket/prefix hardcoded in
`internal/app/integration_test.go` — point it at a bucket you have access
to before running it.
