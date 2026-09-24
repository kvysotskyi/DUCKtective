# internal/appdir

One function: `Dir() (string, error)` resolves
`<os.UserConfigDir()>/ducktective` — the per-OS directory the app's
DuckDB files live in: `ducktective.duckdb` (the catalog), a `wiretaps/`
subdirectory with one `<id>.duckdb` per Wiretap, and — after a one-time
migration from the older single-file layout — `ducktective.legacy.duckdb`,
a backup the user can delete. `store.Open()` creates the directories;
this package doesn't.

`appName = "ducktective"` is the only thing to change if the app is ever
renamed again — it was previously `"logviewer"`, and that leftover name
caused real confusion (the DB file was silently written to the old
directory) before being caught and fixed.
