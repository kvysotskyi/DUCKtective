# internal/store

Owns the single embedded DuckDB file. One dynamic table per Wiretap
(`w_<id>`), plus two fixed metadata tables (`_meta_wiretaps`,
`_meta_ingested_files`, created in [db.go](db.go)'s `migrate()`). No SQL
ever reaches the UI — every query is built here from typed Go structs.

## Files

- `db.go` — `Open`/`OpenAt`, migrations.
- `wiretap.go` — Wiretap CRUD, field/schema validation, DDL.
- `ingest.go` — `LoadFile`: parse + bulk insert.
- `search.go` — `Search`, `DistinctLevels`, `RawLine`.
- `retention.go` — `DeleteOlderThan`.
- `compact.go` — `CompactWiretap`: rewrite-and-swap to reclaim disk space `DeleteOlderThan` leaves behind.
- `schema.go` — `sanitizeIdent` (Wiretap name → safe table/id suffix).

## ⚠️ Table column order is load-bearing

`CreateWiretap`'s DDL puts bookkeeping columns **first** — `file_hash,
raw, source_file, source_line, ingested_at` — then the Wiretap's `Fields`
after. This is not cosmetic: `LoadFile` inserts via DuckDB's Appender,
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

`LoadFile` (ingest.go) parses all lines of a file concurrently
(`parseLinesConcurrently`, worker pool sized `runtime.NumCPU()+2`), then
bulk-loads the parsed rows via `github.com/marcboeker/go-duckdb`'s native
**Appender** API — not a batched multi-row SQL `INSERT`. That was tried
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

**`LoadFile` does not dedupe.** There is no per-row uniqueness
constraint on `file_hash` (there used to be a `PRIMARY KEY` +
`ON CONFLICT DO NOTHING`, which was *also* measured and found to make no
difference — DuckDB's conflict-check path is the slow one regardless).
Calling `LoadFile` twice for the same file inserts every row twice.
Callers must check `IsFileLoaded` first and skip files already loaded
(see `App.LoadFilesNow` / `App.autoLoadNewFiles` in
[internal/app](../app/CLAUDE.md)) — safe because each file loads inside
one all-or-nothing transaction, so "already fully loaded" is the only
duplicate scenario that can occur.

## compact.go — why DELETE/VACUUM don't shrink the file

Confirmed empirically against a real 13.7GB installation: `pragma_database_size()`
showed ~41% of the file was blocks DuckDB had already freed from past
`DeleteOlderThan` calls but never returned to the OS. DuckDB only frees a
storage block once *every* row in it is dead; `DeleteOlderThan`'s deletes are
scattered across blocks (rows land wherever `LoadFile`'s Appender happened to
write them, not clustered by `time`), so in practice almost no block is ever
100% dead, and neither an automatic checkpoint (runs on `db.Close`) nor an
explicit `VACUUM` reclaims the space. `CompactWiretap` sidesteps this
entirely by rewriting the table (`CREATE TABLE ... AS SELECT * FROM ...`,
`DROP`, `RENAME`, all in one transaction) — every block the rewrite produces
holds only live rows, so the following `CHECKPOINT` reliably shrinks the
file. Retention only marks rows dead; compacting is what actually reclaims
the space.

## search.go conventions

- Always named-column `SELECT`s, never `SELECT *` — must stay correct regardless of physical column order (see above).
- `Filters` combine with AND; `PageSize = 100`, paged via `Offset` (frontend does infinite scroll, not page buttons — see `LIMIT PageSize+1 OFFSET offset` / `HasMore`).
- `otherColumns(w)` = every Wiretap field except `time`/`level` (which get their own typed struct fields on `LogRow`); everything else lands in `LogRow.Fields` (map).
