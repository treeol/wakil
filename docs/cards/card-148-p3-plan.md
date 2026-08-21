# P3 — Read-only Web-UI (Implementation Plan)

**Card:** #148
**Branch:** `feature/wakild-daemon`
**Design ref:** `docs/design/wakild-foundation.md` §9

## Goal

Static web frontend served by `wakild`, speaking the same Connect API (HTTP/JSON).
Session list, live session viewer (polling ListEvents), trace browser (GetSessionSnapshot).
No write-path (P5).

## Exit Gate

A running session is live-trackable in the browser, including tool-calls and subagent tree.

## Architecture

- Browser → `wakild` HTTP listener (TCP, localhost-only in P3)
- Connect-Go serves HTTP/JSON natively: `POST /wakil.v1alpha1.SessionService/ListSessions` etc.
- Static files embedded via `//go:embed web` (self-contained binary)
- No build step: vanilla HTML/CSS/JS, no framework
- Live updates via polling `ListEvents` with `after_seq` cursor (500ms interval)
  — simpler than parsing Connect streaming envelopes, near-live, robust

## Chunks

### P3a — HTTP listener + static file serving
- `web/embed.go`: Go package exporting embedded static files via `//go:embed`
- `cmd/wakild/main.go`: add `--http-addr` flag (default empty = disabled)
- `cmd/wakild/server.go`: dual-listener (Unix socket + optional TCP), static file handler at `/`, Connect handlers at their paths
- Security: localhost-only binding, no auth (P4)

### P3b — Web UI (vanilla JS)
- `web/index.html`: SPA shell with three views
- `web/app.js`: app logic
  - Session list: ListSessions → render session cards (state, title, time)
  - Live session viewer: poll ListEvents every 500ms with after_seq cursor → render event timeline
  - Trace browser: GetSessionSnapshot → render historical events
  - Event rendering: messages, tool calls, subagent tree, approvals, errors, turns
- `web/styles.css`: minimal dark-theme styling

### P3c — Tests
- Integration test: start daemon with `--http-addr`, verify:
  - ListSessions over HTTP/JSON
  - GetSessionSnapshot over HTTP/JSON
  - GetServerInfo over HTTP/JSON
  - Static files served at `/`
- Full suite: `go test -race ./...` green, `buf lint` green, `go vet` clean

## No Proto Changes

All required RPCs already exist:
- `SessionService.ListSessions` — session list
- `EventService.ListEvents` — event polling (unary, cursor-based)
- `EventService.GetSessionSnapshot` — trace browser / replay
- `SystemService.GetServerInfo` — server info
- `SystemService.Health` — health check
