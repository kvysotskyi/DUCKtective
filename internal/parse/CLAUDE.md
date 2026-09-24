# internal/parse

Turns one NDJSON log line into column values per a Wiretap's field
definitions. Pure, no I/O, no DB — easy to unit test in isolation (see
[parse_test.go](parse_test.go)).

## `Field` and `Line`

`Field{Column, JSONKeys, Required}` — `JSONKeys` is an ordered candidate
list; the first key present with the right-shaped value wins, so one
column can absorb multiple producers' spellings of the same concept
(e.g. `"time"` and `":time"`).

`Line(raw, fields, values) (ts, ok)` — `ok` is `false` **only** when the
line isn't valid JSON at all. A missing or malformed *individual* field
(even one marked `Required` in the Wiretap form) never drops the line —
it just leaves that field's slot `nil` (`NULL` downstream). `Required` is
purely a UI/form-validation concept enforced at Wiretap-creation time,
not at parse/ingest time — nothing here or in `store.LoadFile` checks it.

`values` is caller-owned and positional, aligned 1:1 with `fields` —
`values[i]` is `fields[i]`'s value (`nil` if absent), not a map. This
exists so `internal/store` can pool and reuse these slices across lines
instead of allocating a fresh `map[string]string` per line (see
[internal/store/CLAUDE.md](../store/CLAUDE.md)); `Line` itself has no
pooling logic, it just writes into whatever slice it's given, sized
`len(fields)` by the caller. `values` is left untouched when `ok` is
`false`.

## ⚠️ Timezone handling — do not "fix" this

`parseTimeString` deliberately **discards** any parsed offset (e.g.
`-06:00`) and reconstructs the timestamp using the source's literal
wall-clock digits in `time.UTC`:

```go
wallClock := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(),
    t.Second(), t.Nanosecond(), time.UTC)
```

This is intentional, confirmed via a real DuckDB round-trip test: DuckDB's
`TIMESTAMP` column type has no timezone concept and silently normalizes
any offset to UTC on write — inserting `09:01:28-06:00` naively comes
back as `15:01:28 UTC`, a wrong wall-clock hour if displayed as-is.
Passing the offset through would silently shift every timestamp. See
`TestLinePreservesSourceWallClockOffset`.

`parseEpoch` is unrelated and correct as-is: a bare epoch number has no
source-local wall clock to preserve, so UTC is the only correct
interpretation there (see `TestLineEpochIsUTC`). Values above `1e12` are
treated as milliseconds, otherwise seconds with fractional-second nanos.

`TimeColumn = "time"` is the one field name parsed as a timestamp instead
of a string; it's a fixed constant, not user-configurable.
