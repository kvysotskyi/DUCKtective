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

## Diagnosing memory/goroutine issues — `DUCKTECTIVE_PPROF=1`

`main.go`'s `maybeStartPprof` starts a `net/http/pprof` server on
`localhost:6061` when `DUCKTECTIVE_PPROF` is set in the environment —
unset by default, so a normal launch opens no port. To capture a heap
profile while reproducing a suspected leak (e.g. `DUCKTECTIVE_PPROF=1
open /Applications/ducktective.app`, then trigger the load), use
`go tool pprof http://localhost:6061/debug/pprof/heap` (add
`-seconds=N` to `debug/pprof/profile` for CPU instead). Take two heap
snapshots — one at baseline, one after the suspected leak — and diff them
(`go tool pprof -base baseline.pb.gz after.pb.gz`) to see what's actually
still growing, rather than guessing from `downloadAll` alone.

## sourceFor — the pluggable-source factory

`sourceFor(w store.Wiretap) (source.Source, error)` switches on
`w.SourceType` and returns a concrete `source.Source`. This is the single
place that knows how to turn a Wiretap's saved config into something
`ListObjects`/`OpenObject`-able — see [internal/CLAUDE.md](../CLAUDE.md) for
how to add a new source type here.

## Load pipeline concurrency

`LoadFilesNow` and `autoLoadNewFiles` share `loadPipeline`, which
overlaps the network with the database without the memory blow-up the
first design had. `downloadAll` opens up to `maxConcurrentDownloads`
objects and hands each over as a live reader; `loadPipeline` immediately
calls `db.PrepareFile` on it — which starts streaming and parsing that
file into bounded chunks in the background — and keeps up to
`maxInFlightFiles` (4) such files pending. Commits (`db.CommitFile`, the
serialized Appender work) happen strictly in order as the queue fills, so
while file A is being written, files B–E are already downloading and
parsing. Memory is `maxInFlightFiles × (pendingChunkDepth+1)` parsed
chunks (~50–100MB worst case), independent of file count or size, because
a pending file's reader blocks once its channel is full. Measured
baseline before this: ~2–3 s per 160K-line file, ~60% of it waiting on
GCS with the DB idle, files strictly sequential. `LoadFilesNow` still
skips already-loaded files via `IsFileLoaded` *before* downloading them —
the cheap skip; `CommitFile` itself deletes any rows an earlier attempt
left for the file, so a re-load is idempotent (see
[internal/store/CLAUDE.md](../store/CLAUDE.md)).

### ⚠️ downloadAll streams; it must never buffer whole files again

A prior version called `io.ReadAll` per object inside the download
goroutine and sent the resulting `[]byte` through a channel sized to
`len(names)`. Since downloads are network-bound and run up to
`maxConcurrentDownloads` at a time while `db.LoadFile` consumes them one
at a time (sequential by design — see `internal/store/CLAUDE.md`),
downloads could finish arbitrarily far ahead of the consumer, and nothing
bounded how many *finished-but-unprocessed* files piled up in that
channel — each holding its full contents in memory. Confirmed as the
cause of a multi-GB memory spike loading a large backlog (e.g. auto-load
catching up after a gap, or a large "Load now" selection).

The fix: `downloadAll` now hands each result to the consumer as a live,
unread `io.ReadCloser` (`src.OpenObject`'s return value, untouched), and
the channel is bounded to `maxConcurrentDownloads` instead of
`len(names)`. `db.LoadFile` already reads its input via `bufio.Scanner`,
so it streams directly off the network reader — no intermediate `[]byte`
ever holds a whole file. Once the channel is full, a download goroutine
blocks on its send (still occupying its `errgroup` concurrency slot), so
at most `maxConcurrentDownloads` objects are ever open at once, and any
bytes downloaded-but-not-yet-read sit in the OS socket buffer, not the Go
heap. **Callers of `downloadAll` must `Close` each result's `body`** —
`LoadFilesNow`/`autoLoadNewFiles` do this right after their `LoadFile`
call, whether it errors or not. Do not reintroduce `io.ReadAll` here.

### `freeOSMemory` — why RSS stays high even with no real leak

Diagnosed with `DUCKTECTIVE_PPROF=1` (see below): after the fix above,
diffing two heap snapshots taken a minute apart mid-sync showed the live
heap stable at ~150-220MB — a healthy GC, not a leak — while `ps`/Activity
Monitor reported 1-1.9GB RSS, and a forced GC didn't move that number.
This is a known Go-on-Darwin behavior: the runtime returns freed pages to
the OS lazily (`MADV_FREE`), so a burst of large transient allocations
(parsing a 150K-line file) pushes the process's reported RSS to a
high-water mark that doesn't come back down on its own, even though the
memory is logically free and reclaimable under real pressure.
`LoadFilesNow`/`autoLoadNewFiles` call `freeOSMemory` (`debug.FreeOSMemory`)
every `freeMemoryEveryNFiles` files and once more at the end of their
batch, forcing Go to hand those pages back immediately instead of waiting
on its own scavenger. Confirmed live: without the periodic call, RSS
climbed unchecked for the whole duration of a large backlog sync (only
relieved once the entire batch — potentially thousands of files — had
finished); the batch-end-only call is too late to matter during a sync
that's still running. This doesn't change peak usage, just how long the
OS-visible number stays inflated.

**But the Go heap was never the main problem.** While Go sat at
~120–220MB live, `vmmap -summary <pid>` showed ~6GB in `MALLOC_SMALL` —
DuckDB's native buffer pool, invisible to pprof — with the machine at
10.5GB of swap. That fix lives in `internal/store` (`memory_limit` DSN
cap, chunked `LoadFile`, periodic `Appender.Flush`); see
[internal/store/CLAUDE.md](../store/CLAUDE.md#memory-the-big-consumer-is-duckdbs-heap-not-gos).
Lesson for next time: when RSS and pprof disagree by more than ~2x, run
`vmmap -summary` before touching Go code — `VM_ALLOCATE` is Go,
`MALLOC_*` is DuckDB (or any other cgo library).

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
in `Startup`. Each tick first runs `reencodeIfLegacy` on every wiretap:
a file still in the pre-ZSTD storage format (written by DuckDB ≤ 1.1,
`db.NeedsReencode`) is rewritten once via `CompactWiretap` (~2 min for
10GB, searches keep working; a failure is retried after
`reencodeRetryAfter`), logged as `[reencode]` — see
[internal/store/CLAUDE.md](../store/CLAUDE.md#storage-format-v14-files-zstd-text--measured-9-smaller).
Then every Wiretap that has *either* `AutoLoadEnabled`
*or* a retention policy (`RetentionDays > 0` or `MaxSizeMB > 0`) and whose
`PollIntervalMinutes` has elapsed since `LastPolledAt` gets, in order:
`autoLoadNewFiles` (if auto-load is on), `db.ApplyRetention` (if it has a
policy — age expiry, size cap, and compaction in one pass; see
[internal/store/CLAUDE.md](../store/CLAUDE.md#retention-policy-applyretention)),
then `MarkPolled`. Retention deliberately no longer depends on auto-load
being enabled — a wiretap you fill manually still expires. After the loop
the tick calls `db.CloseIdle(idleHandleTTL)` so wiretaps not touched for
10 minutes release their DuckDB instance (and its memory cap's worth of
buffer pool). There is no background service when the app is closed —
this goroutine simply stops on `Shutdown`.

The tick is one goroutine and processes wiretaps sequentially, and every
store write path (`LoadFile`, `ApplyRetention`, `CompactWiretap`) takes
the wiretap's own write mutex — so a UI "Load now"/"Compact" click can no
longer collide with the scheduler on the same table; it just waits.
`RunRetentionNow` (the UI button) returns the same `store.RetentionResult`
the scheduler gets, so the status line can show rows deleted, bytes
reclaimed, and the resulting size.
