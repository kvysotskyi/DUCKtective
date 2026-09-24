# internal/store

Owns the embedded DuckDB files: a small **catalog** (`ducktective.duckdb`,
holding only `_meta_wiretaps`) plus **one self-contained file per Wiretap**
(`wiretaps/<id>.duckdb`, holding that wiretap's `w_<id>` table and its own
`_meta_ingested_files` dedup table). No SQL ever reaches the UI — every
query is built here from typed Go structs.

## Layout: one file per wiretap

Every data method takes the `Wiretap` it acts on and resolves it through
`db.handle(w)` (db.go), which lazily opens `wiretaps/<id>.duckdb` at
`wiretapMemoryLimit` and caches the handle. `handle` refuses to create a
missing file (a stale `Wiretap` for a deleted id must not conjure a ghost
database) — only `CreateWiretap` (`createHandle`) and the legacy migration
create files. Each handle has two locks: `writeMu` serializes writers
(`LoadFile`, `ApplyRetention`/`DeleteOlderThan`, `CompactWiretap`, the
`ALTER` in `UpdateWiretap`) so a manual button can't race the scheduler;
`mu` (RW) only guards the `sql` pointer's validity — every user RLocks it
(`lockRead`/`lockWrite`) and compaction takes it exclusively for the
instant of the file swap. Reads therefore never wait on a load (DuckDB's
MVCC handles read/write overlap), only on a swap. `CloseIdle(maxIdle)`
closes handles unused for a while (both locks via `TryLock`, so nothing
in use is closed; `lastUsed` is refreshed on lock acquisition, not just on
lookup), which keeps total memory ≈ cap × *active* wiretaps.

Why per-file: deleting a wiretap is `os.Remove` (instant, disk fully
returned — `DROP TABLE` in a shared file left dead blocks behind);
retention on one wiretap can't fragment another's data; DuckDB's
single-writer lock is per file, so a long RASLO load no longer blocks a
write to another wiretap; and `SizeBytes` on a `Wiretap` is finally that
wiretap's real footprint. **On-disk size is always file + `.wal`**
(`wiretapSize`) — freshly inserted rows live in the WAL until DuckDB
checkpoints, so the file alone under-reports (a 20K-row load showed as
12KB until checkpoint).

## Storage format: v1.4 files, ZSTD text — measured 9× smaller

Every instance opens with `storage_compatibility_version=v1.4.0`
(`storageCompatVersion`) and every TEXT column is declared
`TEXT USING COMPRESSION zstd` (`textColumnType`, via `columnType`,
`createWiretapTable`, the `ALTER` in `UpdateWiretap`, and
`cloneTableDDL` for compaction copies). Measured on a real 12-hour slice
of RASLO1 (1.58M rows, DuckDB 1.4.1, 100MB cap): old format 1073MB →
116MB; full-table `raw LIKE` 65 → 77ms; top-101 search 51 → 60ms;
Appender ingest 112K → 109K rows/s. `raw` and `msg` had been stored
*Uncompressed* before (DuckDB's default codecs give up on long unique
strings), which is why files were ~2× the raw text.

Two facts that shape the code: (1) **a file's format is fixed at
creation** — the setting is not an in-place upgrade, and (2) in an
older-format file `USING COMPRESSION zstd` is *silently ignored* (stored
Uncompressed). So the only way to shrink a file written by DuckDB ≤ 1.1
is a compaction into a fresh file, and `NeedsReencode` (file-header
format number < `zstdStorageVersion`, read by `storageVersion`) tells the
scheduler which wiretaps to rewrite once (`[reencode]`,
`scheduler.reencodeIfLegacy`). Files in this format need DuckDB ≥ 1.4 to
open — fine, we ship 1.4.1 (root CLAUDE.md rule 6).
`TestNewWiretapStoresTextAsZstd` and
`TestLegacyFormatFileIsReencodedByCompaction` pin both behaviours.

`migrate_legacy.go` handles the pre-split layout (wiretap tables inside
the catalog): on open, each table is copied into its own file via
`copyWiretapTo` (which preserves the load-bearing column order below and
lands in the current storage format), dedup rows are copied alongside, then a fresh small
catalog is built and swapped in with a crash-safe two-rename sequence
(`recoverInterruptedCatalogSwap` finishes it if interrupted). The
original file is kept as `ducktective.legacy.duckdb` — a backup, safe to
delete. Progress is logged as `[migrate]`. `TestLegacyLayoutMigrates`
builds the old layout by hand and checks data, dedup state, and a
post-migration `LoadFile` all survive.

## Files

- `db.go` — `Open`/`OpenAt`, catalog migration, per-wiretap `handle`s, `CloseIdle`, `wiretapSize`.
- `migrate_legacy.go` — one-time split of the old single-file layout.
- `wiretap.go` — Wiretap CRUD, field/schema validation, DDL.
- `ingest.go` — `LoadFile`: chunked parse + Appender insert; `IsFileLoaded`.
- `search.go` — `Search`, `DistinctLevels`, `RawLine`.
- `retention.go` — `DeleteOlderThan`, `ApplyRetention` (the whole policy).
- `compact.go` — `CompactWiretap`: rewrite-and-swap to reclaim disk space DELETE leaves behind.
- `schema.go` — `sanitizeIdent` (Wiretap name → safe table/id suffix).

## ⚠️ Table column order is load-bearing

`createWiretapTable`'s DDL puts bookkeeping columns **first** — `raw,
source_file, source_line, ingested_at` — then the Wiretap's `Fields`
after. (Older tables also had a leading `file_hash` column — a 64-char
SHA-256 per row, 7% of a big wiretap and incompressible; `handle` drops
it on first open via `dropLegacyFileHash`, and a row is now identified by
`(source_file, source_line)`, which `RawLine` keys on.) This is not cosmetic: `LoadFile` inserts via DuckDB's Appender,
which binds row values **positionally** by physical column order, not by
name. `UpdateWiretap`'s `ALTER TABLE ADD COLUMN` (for a newly added
field) always appends to the *end* of the physical table, and
`UpdateWiretap` also appends new fields to the end of `Wiretap.Fields` in
memory — so field columns must be the table's last section for those two
"append at the end" behaviors to stay in sync. Getting this wrong throws
a cast error at insert time for the newest field (caught once by
`TestUpdateWiretapAddsColumn` — keep that test around).

Field/column names come from **user input** (a Wiretap's configured
fields), not a fixed schema — `validColumn` (`^[a-z][a-z0-9_]{0,62}$`) in
`wiretap.go` guards every column name before it's concatenated into DDL/
DML as a quoted identifier. Don't relax this regex without re-checking
every call site that builds SQL from `f.Column`.

## LoadFile — why it's an Appender, not INSERT

Ingest is a two-stage pipeline (ingest.go). `PrepareFile` starts a
goroutine that streams the file and parses it into `ingestChunkLines`
(10K) line chunks (`parseLinesConcurrently`, worker pool sized
`runtime.NumCPU()+2`), handing them over a channel of depth
`pendingChunkDepth` (2) — so the reader blocks, and memory stays bounded,
when the writer falls behind. `CommitFile` takes the wiretap's write lock
and appends the chunks as they arrive, **committing each one**.
`LoadFile` is simply both back-to-back. The split exists so the
app layer can keep several files' network and JSON work in flight while
the single database writer works through them in order (see
[internal/app/CLAUDE.md](../app/CLAUDE.md#load-pipeline-concurrency)).
Each chunk is its own transaction on purpose: DuckDB's checkpoint cannot
spill, it must hold the dirty rows it writes in memory, and a whole
~200K-line file committed at once (~150MB dirty) failed its checkpoint
under the 100MB cap with `could not allocate block of size 256.0 KiB` —
which is a *fatal* error that invalidates the instance. Rows go through
`github.com/duckdb/duckdb-go/v2`'s native **Appender** API — not a
batched multi-row SQL `INSERT`. That was tried
first and measured, against a real ~90K-line file, at a flat ~600µs/row
*regardless of batch size* (1 row or 1000 rows per statement made no
difference) — the bottleneck was the SQL insert path itself (parse/plan/
bind through the driver), not row count. The Appender writes columnar
data chunks directly, bypassing SQL entirely: ~110,000 rows/sec on the
same data, wrapped in a manual `BEGIN`/`COMMIT` on a single `*sql.Conn`
(`conn.Raw()` hands the same underlying `driver.Conn` to
`duckdb.NewAppenderFromConn`, so appended rows participate in that
transaction).

**Parsed field values use a pooled `[]*string`, not a map.** A prior
version had `parse.Line` allocate a fresh `map[string]string` per line —
confirmed via `DUCKTECTIVE_PPROF=1` heap profiling as a major allocation
source (a 150K-line file meant 150K map allocations). `parse.Line` now
fills a caller-owned `[]*string` positionally aligned to `w.Fields` (`nil`
= field absent, same as a missing map key; a non-nil pointer can still
point at `""`, so the absent/empty distinction survives exactly).
`parseLinesConcurrently`/`LoadFile` get these slices from `valuesPool` (a
`sync.Pool`, `ingest.go`) and return them once a row's values are copied
into the Appender's `row []driver.Value` — reusing backing arrays across
lines within a file and across files, instead of allocating fresh ones
each time. See `TestLoadFileMissingFieldIsNullNotEmptyString` for the
nil-vs-empty-string regression this must not reintroduce.

**Dedup is file-level, and `CommitFile` is idempotent.** There is no
per-row key or uniqueness constraint at all (there used to be a hashed
`PRIMARY KEY` + `ON CONFLICT DO NOTHING`, measured and found to make no
difference — DuckDB's conflict-check path is the slow one regardless — and
the hash column itself was later dropped as pure overhead). Because chunks
commit individually, a crash or read error mid-file can leave that file's
earlier rows behind *without* its `_meta_ingested_files` marker (written
last) — so `CommitFile` starts by deleting every row with that
`source_file`, making a re-load replace rather than duplicate
(`TestLoadFileReloadAfterPartialAttemptIsIdempotent`,
`TestCommitFileReadErrorLeavesFileUnmarked`). Callers still check
`IsFileLoaded` first (see `App.LoadFilesNow` / `App.autoLoadNewFiles` in
[internal/app](../app/CLAUDE.md)) — that's the cheap skip that avoids
downloading a finished file at all, not the correctness guarantee.

**A fatal DuckDB error self-heals.** Any operation whose error contains
`database has been invalidated` (DuckDB's state after a failed checkpoint
or similar fatal) goes through `db.heal`, which drops the wiretap's handle
so the next call reopens the file cleanly instead of every call failing
until the app restarts.

## Memory: the big consumer is DuckDB's heap, not Go's

Diagnosed on a real 16GB-database install that was pushing the machine to
10.5GB of swap during loads. Go pprof showed a healthy ~120–220MB live
heap the whole time — because the memory wasn't Go's. `vmmap -summary
<pid>` attributed ~6GB (mostly swapped out) to `MALLOC_SMALL`, i.e. C
`malloc` — DuckDB's own buffer pool, which pprof cannot see at all. Its
default `memory_limit` is 80% of RAM (12.7GiB here); every insert,
retention `DELETE` scan, `CHECKPOINT`, and `raw`-column search pulls
blocks into that pool and it keeps them until it hits the limit, leaving
macOS to swap them out. Three things bound this, in order of impact:

1. Every DuckDB instance is opened with `?memory_limit=` in the DSN —
   go-duckdb forwards DSN query params to `duckdb_set_config`. Wiretap
   files get `wiretapMemoryLimitBytes` (**100MB**); the catalog gets
   `catalogMemoryLimitBytes` (512MB, only so the one-time legacy migration that
   copies tables through it isn't starved — the catalog itself holds one
   tiny table and never approaches it). DuckDB spills to `<file>.tmp` when
   an operator needs more.

   **The cap's hard floor is the largest single transaction**, because a
   checkpoint can't spill: it holds every dirty row it's writing. That is
   why `LoadFile` commits per 10K-line chunk, `copyWiretapTo` copies in
   **time-range** batches sized to the cap (`copyBatchRows`: ~2KB of
   headroom per row, so ~51K rows at 100MB, ~10K at 20MB) with a
   `CHECKPOINT dest` after each (a 100K batch without the checkpoint OOM'd
   on the *second* batch at a 20MB cap — batch-one's dirty blocks were
   still pinned; a fixed 50K batch OOM'd at 20MB too), and every wiretap
   instance opens
   with `preserve_insertion_order=false` (bulk copies run parallel and
   lean; safe because every query orders explicitly).
   `TestLoadAndCompactStayCheckpointSafeUnderTinyMemoryLimit` loads 200K
   rows and compacts under a deliberate 20MB cap to keep it that way.

   **The cap also needs DuckDB ≥ 1.4 (`go-duckdb/v2`).** Log lines over
   4KB (stack traces, payload dumps — ~1% of a real gateway wiretap) are
   stored as compressed "overflow strings" outside the 256KB block. DuckDB
   1.1 (go-duckdb v1.8.x) decompressed each one into its own
   `max(256KB, length)` buffer and pinned all of them to the scanned
   vector, so a 2048-row vector holding 185 long lines cost ~95MB even
   when the filter kept two 119-byte rows. On a real 9.6GB wiretap that
   made *every* read touching that vector fail with `Out of Memory` at
   100MB — the compaction copy (deterministically at the same 2-row time
   span), `raw LIKE` searches, even `WHERE rowid = ?` — while passing at
   300MB. DuckDB 1.4.1 runs the same queries under 30MB, and replays a
   1.1-written WAL correctly (verified: identical row count after the
   WAL's 1.57M-row DELETE). If a read OOMs at the cap again, check
   `duckdb_memory()`'s `OVERFLOW_STRINGS` tag before blaming batch sizes.

   **What 100MB costs.** `memory_limit` bounds DuckDB's *own* block cache
   and operator memory; it does not bound how much data the OS keeps in its
   page cache on the app's behalf — and macOS caches file pages aggressively,
   outside the process's RSS and reclaimable under pressure. So the cap
   mostly trades DuckDB's cache for the OS's, not for disk:
   - *Ingest* (`LoadFile`): essentially unaffected. It streams 10K-line
     chunks through the Appender and flushes each; nothing needs to be
     resident beyond the chunk in flight.
   - *Search*: `ORDER BY time DESC LIMIT 101` is a small top-N; the cost is
     the column scan (`raw LIKE` reads the whole `raw` column). With a 1GB
     cap a repeat search hit DuckDB's cache; at 100MB it re-reads from the
     OS page cache (fast) or, if the OS evicted it, from SSD (a few GB/s —
     roughly a second per couple of GB of `raw`). Per-wiretap files bound
     that scan to one wiretap's data, and `LoadDaysBack`/retention keep it
     to a day or two of logs.
   - *Retention DELETE*: scans only the `time` column — cheap either way.
   - *Compact* (`CREATE TABLE … AS SELECT *`): streams; with a small cap
     it spills more to `<file>.tmp`, so it's more disk-bound and slower on
     multi-GB wiretaps, but it runs in the background tick.
   - *Aggregations* (`DistinctLevels` — `GROUP BY level`): tiny.
   If searches on a large wiretap feel sluggish, raise `wiretapMemoryLimit`
   (256MB is a reasonable next step); it's one constant. Total memory is
   `cap × open handles`, and `CloseIdle` keeps open handles to the ones
   actually in use.
2. `LoadFile` calls `appender.Flush()` after every chunk, so the Appender's
   native data chunks never hold a whole file.
3. `LoadFile` chunks the read/parse itself (`ingestChunkLines`), so the Go
   working set is one chunk's worth of `lines`/`parsed`/values regardless
   of file size — which is also what makes `valuesPool` actually recycle
   within a file instead of only across files. See
   `TestLoadFileChunkBoundariesKeepAbsoluteLineNumbers`: `source_line`
   must be the absolute line number in the file, not the chunk offset.

To re-check attribution later: in `vmmap -summary`, `VM_ALLOCATE` ≈ Go's
heap, `MALLOC_*` ≈ DuckDB. If `MALLOC_SMALL` grows past ~1GB the cap isn't
being applied.

## compact.go — why DELETE/VACUUM don't shrink the file

Confirmed empirically against a real 13.7GB installation: `pragma_database_size()`
showed ~41% of the file was blocks DuckDB had already freed from past
`DeleteOlderThan` calls but never returned to the OS. DuckDB only frees a
storage block once *every* row in it is dead; `DeleteOlderThan`'s deletes are
scattered across blocks (rows land wherever `LoadFile`'s Appender happened to
write them, not clustered by `time`), so in practice almost no block is ever
100% dead, and neither an automatic checkpoint (runs on `db.Close`) nor an
explicit `VACUUM` reclaims the space. `CompactWiretap` sidesteps this by
copying the live rows into a **brand-new file** (`ATTACH`, rebuild the
schema with `cloneTableDDL` — same column order from `duckdb_columns()`,
ZSTD on every TEXT column, which `CREATE TABLE … AS SELECT … LIMIT 0`
would have dropped — then `INSERT … SELECT`
in batches of ≤ `copyBatchRows(cap)` rows, checkpointing `dest` after each,
so no transaction outgrows the memory cap — the same `copyWiretapTo` the
legacy migration uses; DuckDB fills the tail row group before opening a
new one, so small batches don't fragment the result), then swapping the
file in. **Batches are time ranges, never rowid ranges.** `copyRange`
bisects `[min(time), max(time)]` until a span holds ≤ the cap-derived
batch size — and bisects again if DuckDB's non-fatal `Out of Memory` says
a batch was still too big — and rows with no parsable `time` get the same
treatment keyed on `ingested_at`. When a span is already one microsecond
wide and still over the batch (second-resolution logs can put 100K rows
on one `time` value), `copySpanByRowid` bisects that span by **rowid
range** instead — the time predicate still prunes the scan, and unlike
`ORDER BY rowid LIMIT/OFFSET` (tried first: it materialises the whole
span to sort it and OOM'd at a 20MB cap) nothing has to be sorted.
`TestCompactCopesWithManyRowsOnOneTimestamp` pins it with 4× the batch
on one timestamp.
Rowid batching was tried first and measured: DuckDB does *not* prune row
groups on `rowid`, so every 20K-row batch visited the whole table — cost
grew with table size (114ms on 0.5GB, 489ms on 1.25GB), and on the real
9.6GB wiretap the very first batch OOM'd at the 100MB cap. `time` and
`ingested_at` are zone-mapped, so a span only touches its own row groups
and each batch costs the same regardless of table size
(`TestCompactLargeTableUnderCap`, opt-in via `DUCKTECTIVE_BIG_TESTS=1`,
measures this on a multi-million-row table), then
swapping it in under the wiretap's write lock: close the instance, move
the fresh file over the old one, reopen. A new file is exactly the live
data by construction. **Don't go back to an in-place rewrite** (`CREATE
TABLE tmp AS SELECT`, `DROP`, `RENAME`, `CHECKPOINT` in the same file):
it only shrank the file when the data was still in the WAL; once the table
lived in file blocks, every subsequent compaction *grew* the file
(5.0 → 8.7 → 9.7MB in `TestApplyRetentionSizeCapTrimsAndCompacts`)
because the new copy landed in fresh blocks and the old copy's blocks were
never truncated — and a second `CHECKPOINT` did not help. Retention only
marks rows dead; compacting is what actually reclaims the space.
Re-checked on DuckDB 1.5: `DELETE` + `CHECKPOINT` still never shrinks the
file, but the freed blocks *are* reused by later inserts (1.5M rows →
delete ¾ → re-insert ¼: file stayed at 362MB, `free_blocks` fell as the
inserts landed), so steady-state retention plateaus at the high-water
mark rather than growing; compaction is for giving disk back.

Two things learned from watching it run on the real installation: (1) on
this data DuckDB's own checkpoints *do* reclaim whole dead row groups
(files are loaded whole and are time-clustered, so an age cutoff kills
entire row groups) — a 16.1GB file dropped to 5.5GB before Compact was
ever clicked; Compact's job is the remainder (partially-dead row groups)
and actually truncating the file. (2) Rewriting a table that's already
compact costs the full copy and reclaims nothing — one click rewrote 5GB
(file briefly 10.5GB) for a 0-byte gain. That's why compaction is no
longer a manual-only action: `ApplyRetention` runs it only when a pass
has deleted ≥ `compactAfterDeletedFraction` (25%) of what remains, or as
part of a size-cap trim. `CompactWiretap` returns bytes reclaimed
(file + WAL, before − after) and logs `[compact]`; the manual button
remains as an escape hatch.

## Retention policy (`ApplyRetention`)

One locked pass per wiretap per scheduler tick (and behind the UI's "Run
retention"), in this order:

1. **Age** (`RetentionDays > 0`): `DeleteOlderThan(now − days)`. The
   predicate is `"time" < cutoff OR ("time" IS NULL AND ingested_at < cutoff)`
   — rows whose line had no parsable timestamp expire by when they were
   ingested instead of living forever.
2. **Size cap** (`MaxSizeMB > 0`): while `wiretapSize` (file + WAL) exceeds
   the cap, up to `sizeCapMaxPasses` (3) times: trim the oldest fraction of
   rows sized to the overshoot — `(1 − cap/size) × 1.1`, clamped to
   `sizeCapMinTrimFraction`…`sizeCapMaxTrimFraction` (10–50%) — by finding
   the timestamp at that offset (`COALESCE(time, ingested_at)`, +1µs so the
   boundary row is included), deleting before it, compacting, re-measuring.
   Proportional trimming means one rewrite usually lands under the cap
   (each compaction is a full copy, so passes are expensive); the 50%
   ceiling means a misconfigured cap can't empty a wiretap in one tick,
   and bounded passes mean a wildly oversized one converges over a few
   ticks rather than blocking one for minutes. On tables below one row
   group (~123K rows) the file size wobbles ±15% between rewrites from
   DuckDB's block padding/compression choices — expected, negligible at
   real sizes.
3. **Compaction**: if step 2 didn't already compact and step 1 deleted
   ≥ 25% of the remaining rows, compact once.

Returns `RetentionResult{RowsDeleted, Compacted, BytesReclaimed, SizeBytes}`.
Errors if the wiretap has neither knob set. `TestApplyRetentionSizeCapTrimsAndCompacts`
loads 20K incompressible rows under a 1MB cap and checks the oldest rows
go first and the newest survives.

## search.go conventions

- Always named-column `SELECT`s, never `SELECT *` — must stay correct regardless of physical column order (see above).
- `Filters` combine with AND; `PageSize = 100`, paged via `Offset` (frontend does infinite scroll, not page buttons — see `LIMIT PageSize+1 OFFSET offset` / `HasMore`).
- `otherColumns(w)` = every Wiretap field except `time`/`level` (which get their own typed struct fields on `LogRow`); everything else lands in `LogRow.Fields` (map).
- Substring filters are `contains(col, ?)`, **not** `LIKE '%…%'`: DuckDB only rewrites LIKE to the fast `contains` path when the pattern has no `_`, and log needles are full of them (`study_uid`: 160 → 49ms on the 10GB clone), and `_`/`%` typed by the user must stay literal (`TestSearchFiltersAndTimeFallback` pins it).
- Every instance sets `late_materialization_max_rows=256` (`lateMaterializationRows`, DSN) because DuckDB's default cutoff of 50 is below our `LIMIT 101`; with it, `ORDER BY time DESC LIMIT 101` sorts rowids first and fetches the wide columns for the page only (13 → 7ms unfiltered, 70 → 22ms with a common needle). Filter-less Top-N also skips row groups by `time` min/max since 1.5 — another reason to keep `time` the sort key.
