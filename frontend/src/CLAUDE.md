# frontend/src

`main.js` is one file, organized into `// ---------- Section ----------`
comment banners (grep for those to navigate — they're the real table of
contents). Roughly, top to bottom: small formatting helpers → searchable
picker widget → view switching → auth → Search view → Wiretaps list view
→ Wiretap create/edit form → wire-up (`wireUpStaticControls`, the entry
point that attaches every event listener on load).

## Timezone: two different formatters, do not mix them up

- `formatLogTime(iso)` — **UTC getters only**, used *exclusively* for a log line's own `time` field in search results/detail view. Log timestamps must display the literal wall-clock digits the source wrote (see [internal/parse/CLAUDE.md](../../internal/parse/CLAUDE.md)) — converting to local time here would silently shift the displayed hour.
- `formatIsoLocal(iso)` — local getters, used for the app's *own* operational timestamps (Wiretap `lastPolledAt`, a GCS object's `lastModified` in the Browse-files table). These are legitimately local-time, unlike log content.

`dateTimeBoundary(dateValue, h, m, s, endOfDay)` builds search filter
boundaries with `Date.UTC(...)`, not a local `Date`, for the same reason
— re-introducing local-time conversion here was a repeat mistake earlier
in this UI's development.

## Date/time picker

`createDatePicker(inputEl, popupEl)` — self-drawn calendar (month grid,
Mo–Su header, prev/next nav), returns `{clear()}`. Paired with plain
`<input type="number">` HH/MM/SS spinners (no AM/PM). This exists because
native date/time inputs are locale-dependent and explicitly disallowed —
see [frontend/CLAUDE.md](../CLAUDE.md). Don't replace this with a native
input as a "simplification."

## Search results: infinite scroll, not pagination

`onSearch()` resets `currentOffset` and replaces the results `<tbody>`;
`loadMoreResults()` appends; `onWindowScroll()` triggers a fetch near the
bottom of the page. There are no page-number buttons — this was an
explicit user requirement ("must be not in page, just offset limit that
i can scroll infinitively").

## Other conventions

- `createSearchablePicker(inputEl, listEl, onSelect)` — generic substring-filtered dropdown, reused for both the GCP project picker and the GCS bucket picker in the Wiretap form (real accounts have dozens of each).
- Enter anywhere inside `#search-main` triggers Search (`wireUpStaticControls`) — not scoped to just the query input.
- Destructive actions (delete Wiretap) use a two-click confirm (`pendingDeleteId`), never `window.confirm` (see [frontend/CLAUDE.md](../CLAUDE.md)).
- `style.css` uses CSS custom properties for the amber/charcoal theme (`--accent`, `--bg`, `--bg-panel`, ...) — change the theme by editing the `:root` block, not scattered hex literals. Note the WebKit-specific `input:disabled` fix (`-webkit-text-fill-color`) — without it, populated-but-disabled form fields render as blank text in the packaged app's webview even though the value is present.
