# internal/source

The pluggable-source extension point. One small interface, deliberately
kept minimal so a new source type never has to touch `internal/store`'s
ingest/search logic (which only ever deals in `io.Reader` /
`source.ObjectInfo`, never in GCS/S3/filesystem specifics):

```go
type Source interface {
    ListObjects(ctx context.Context, prefix string) ([]ObjectInfo, error)
    OpenObject(ctx context.Context, name string) (io.ReadCloser, error)
}
```

## gcs.go — the only implementation today

`gcsSource` wraps an already-authenticated `*gcs.Client` with a bucket
bound at construction (`NewGCS(client, bucket)`), so callers never pass a
bucket per-call — that's an artifact of Wiretaps being GCS-specific
config (`w.GCS.Bucket`) resolved once in `app.sourceFor`.

## Adding a new source type

See [internal/CLAUDE.md](../CLAUDE.md#adding-a-new-log-source-type-eg-local-filesystem-s3)
for the full checklist (config struct in `store`, factory case in
`app.sourceFor`, UI sub-form). This package's own part is just: implement
`Source` in a new file here (e.g. `local.go`, `s3.go`) — nothing else in
this package needs to change.
