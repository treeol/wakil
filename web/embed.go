// Package web embeds the static web UI files (card #148 P3).
// The files are served by wakil's HTTP listener — the same listener
// that serves the Connect RPC handlers (HTTP/JSON).
package web

import "embed"

// StaticFiles holds the embedded web UI assets (index.html, app.js, styles.css).
//
//go:embed index.html app.js styles.css
var StaticFiles embed.FS
