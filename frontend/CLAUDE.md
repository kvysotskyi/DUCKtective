# frontend

Plain JS/HTML/CSS — no framework (no React/Vue/etc). Vite is used purely
as the dev-server/build tool (`wails.json`'s `frontend:dev:watcher` /
`frontend:build` hooks run `npm run dev` / `npm run build`), not for any
component model.

## Layout

- [index.html](index.html) — the entire DOM structure (search view + wiretaps view), static.
- [src/main.js](src/CLAUDE.md) — all behavior, plain DOM manipulation.
- [src/style.css](src/CLAUDE.md) — amber/charcoal "detective" theme.
- [src/assets/icon.png](src/assets/icon.png) — app icon (duck detective), also copied to `../build/appicon.png` for the packaged app icon.
- `wailsjs/` — **generated** Go↔JS bindings (`wails generate module`, run automatically by the user's own `wails dev`). Never hand-edit; changes to `*App` method signatures in `internal/app/app.go` regenerate this.
- `dist/` — Vite build output, embedded into the Go binary via `//go:embed all:frontend/dist` in `main.go`. Generated, not source.

## UI rules that came from direct, repeated user rejection

**Never use native `<input type="date">`/`type="time">`/`type="datetime-local">`.**
They render using OS locale — US month/day order, AM/PM — with no way to
override via HTML/CSS. This was tried twice (once with
`datetime-local`, once with a `date`+`time` pair) and rejected both times
before landing on a fully custom-built calendar popup + numeric HH/MM/SS
spinner (`createDatePicker` in `src/main.js`). Any future date/time input
needs the same custom-widget treatment, not a native `<input>`.

**Never use `window.alert()` / `window.confirm()`.** They wedge the
entire webview, not just JS execution — confirmed via a genuinely stuck
window that needed a forced reload to recover. Use inline status text
and, for destructive actions, a two-click confirm pattern (see
`onDeleteWiretap`'s `pendingDeleteId` in `src/main.js`).
