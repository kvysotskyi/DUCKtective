# internal/gcs

Thin wrapper over `cloud.google.com/go/storage`, authenticated purely via
ADC (see [internal/gcp](../gcp/CLAUDE.md) — this package doesn't touch
auth itself, `storage.NewClient(ctx)` picks up ADC automatically).

- `Client.ListBuckets(ctx, projectID)` — buckets visible to the ADC identity in one project, sorted by name (for the Wiretap-creation bucket picker).
- `Client.ListObjects(ctx, bucket, prefix)` — objects under a key prefix, in whatever order GCS returns (lexicographic by name; not re-sorted).
- `Client.OpenObject(ctx, bucket, name)` — streaming reader; **callers must `Close()` it** (see `internal/app`'s `downloadAll`, which does `io.ReadAll` then closes immediately).

This package is one concrete backend behind `internal/source`'s `Source`
interface (see [source/gcs.go](../source/gcs.go)) — it has no knowledge of
Wiretaps or the `Source` abstraction itself, just raw GCS operations.
