package connect

import (
	"net/http"

	"github.com/treeol/wakil/internal/auth"
)

// headerInjector is an HTTP middleware that injects the request headers
// into the context so the token/cookie resolver can read Cookie and
// Authorization headers. It wraps the Connect handler.
//
// This is the seam that lets the PrincipalResolver interface stay as
// Resolve(ctx) without adding an http.Header parameter — the middleware
// stashes headers in the context before the handler calls resolvePrincipal.
func headerInjector(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := auth.WithHTTPHeaders(r.Context(), r.Header)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// OriginValidator is an HTTP middleware that validates the Origin header
// for cookie-authenticated mutating requests (POST). It rejects requests
// from unapproved origins to prevent CSRF.
//
// AllowedOrigins is a set of origin URLs (scheme://host:port). If empty,
// all origins are allowed (development mode). In production (hosted mode,
// P4f), this must be set to the configured frontend URL(s).
//
// CSRF enforcement is scoped to requests that carry a Cookie header — i.e.,
// browser sessions. Non-browser clients (CLI, CI, API tokens) use Bearer
// auth, do not send cookies, and are not subject to this check. This
// prevents the middleware from blocking legitimate API clients that don't
// send an Origin header.
//
// Connect RPCs use POST with application/json or application/proto content
// types, which are "non-simple" and trigger CORS preflight. SameSite=Strict
// cookies also block cross-site requests. This Origin check is defense-in-
// depth on top of SameSite.
func OriginValidator(allowedOrigins map[string]bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(allowedOrigins) == 0 {
				// No allowlist configured — allow all (dev mode).
				next.ServeHTTP(w, r)
				return
			}

			// Only validate cookie-authenticated mutating requests.
			// Bearer-authenticated clients (API tokens, OIDC JWTs) do not
			// send cookies and are not subject to CSRF — skip them.
			hasCookie := r.Header.Get("Cookie") != ""
			if !hasCookie {
				next.ServeHTTP(w, r)
				return
			}

			// Only validate mutating methods. GET (static files) is safe.
			if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodDelete || r.Method == http.MethodPatch {
				origin := r.Header.Get("Origin")
				if origin == "" {
					// A cookie-authenticated POST without an Origin header
					// is suspicious — real browsers always send Origin on
					// POST. Reject to be safe.
					http.Error(w, "origin required", http.StatusForbidden)
					return
				}
				if !allowedOrigins[origin] {
					http.Error(w, "origin not allowed", http.StatusForbidden)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}
