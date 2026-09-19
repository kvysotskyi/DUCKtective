# internal/appdir

One function: `Dir() (string, error)` resolves
`<os.UserConfigDir()>/ducktective` — the per-OS directory the app's
DuckDB file lives in (`store.Open()` creates it and puts
`ducktective.duckdb` there). Doesn't create the directory itself; the
caller does (`store.Open` calls `os.MkdirAll`).

`appName = "ducktective"` is the only thing to change if the app is ever
renamed again — it was previously `"logviewer"`, and that leftover name
caused real confusion (the DB file was silently written to the old
directory) before being caught and fixed.
