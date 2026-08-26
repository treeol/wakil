# Card #148 — P4c: Join Token System + Session Cookies

**Branch:** `feature/wakild-daemon`
**Prerequisites:** P4a (schema `958c226`), P4b (SO_PEERCRED `55e2459`)
**Mashūra reviewed:** 3 panels (gpt-5.6-sol, claude-fable-5, glm-5.2)

---

## Scope

P4c implements the **join token onboarding flow** and **browser session cookies**:

1. Admin (local owner via SO_PEERCRED) issues join tokens scoped to tenant+role
2. Clients present a join token → exchange it for a **browser session cookie**
3. The web UI becomes functional over TCP with cookie-based auth
4. CLI/CI API token issuance via exchange is included; standalone API token management deferred to P4d

**Not in P4c:** OIDC/Zitadel (P4e), TLS (P4f), credential encryption (P4g), standalone API token CRUD (P4d).

---

## Architecture (Mashūra-informed)

### Transport-aware resolver dispatch (not a blind chain)

- **Unix socket**: `LocalResolver` only (SO_PEERCRED). Rejects cookie/bearer.
- **TCP**: `WebSessionResolver` (cookie) or `APITokenResolver` (P4d, Bearer header).
- Invalid credentials **hard-fail** (no fallthrough). Only "no credentials of this type" may try next.
- New error types: `ErrCredentialAbsent` (try next) vs `ErrInvalidCredential` (hard fail).

### Server-side DB sessions (not stateless signed cookies)

- `web_sessions` table: opaque random token (256-bit), SHA-256 hashed at rest
- DB lookup per request is acceptable (single-node SQLite, non-goal for v1)
- Immediate revocation, session listing, role-change propagation
- Cookie: `HttpOnly`, `SameSite=Strict`, `Secure` (P4f TLS), `Path=/`
- Resolver reads **current** membership role from DB (not cached in session row)

### AuthService RPCs

```
CreateJoinToken   — owner/admin only, returns plaintext token once
ListJoinTokens    — admin visibility (metadata only, no hashes/secrets)
RevokeJoinToken   — admin/owner, sets revoked_at
ExchangeJoinToken — PUBLIC (unauthenticated), rate-limited, sets Set-Cookie
WhoAmI            — authenticated, returns principal info
Logout            — authenticated, revokes session, clears cookie
```

No `Login`/`Refresh` (those are OIDC/P4e). No `TokenPair` (cookie via `Set-Cookie` header).

### AuthMethod values

Add to `core.AuthMethod`:
- `AuthSession` — browser session cookie
- `AuthAPIToken` — API token (P4d, but constant defined now)

### Join token exchange flow (atomic transaction)

1. Hash plaintext join token (SHA-256)
2. `UPDATE join_tokens SET used_at = ? WHERE token_hash = ? AND used_at IS NULL AND expires_at > ? AND revoked_at IS NULL` — exactly 1 row affected
3. If `user_id IS NULL`: create new user (email, display_name from request; `auth_subject = NULL`, `password_hash = NULL`)
4. Create membership (tenant_id, user_id, role from token)
5. Create `web_sessions` row (opaque token, hash, tenant_id, user_id, expiry)
6. Commit transaction
7. Return `Set-Cookie` header + principal metadata (NOT the session secret)

### Security measures

- Join token: 256-bit CSPRNG, `jnt_` prefix, base64url-encoded
- Web session token: 256-bit CSPRNG, `wst_` prefix, base64url-encoded
- SHA-256 hash at rest (acceptable for high-entropy tokens, not passwords)
- `auth_subject` NEVER from exchange request — only from OIDC flow (P4e)
- Role-based issuance: only `owner` can issue `owner`-role tokens; `owner`+`admin` for others
- `expires_at` checked in SQL conditional UPDATE, not just in Go
- Origin validation for CSRF (in addition to SameSite=Strict)
- Rate limiting on `ExchangeJoinToken` (per-IP, simple token bucket)
- Generic error messages (no enumeration of expired vs used vs revoked)
- No secrets in logs, traces, URLs, or ListJoinTokens response

---

## Deliverables

### New files

1. `internal/store/migrations/003_web_sessions.sql` — `web_sessions` table + `join_tokens.revoked_at` column + fix `ON DELETE SET NULL` → `CASCADE`
2. `api/proto/wakil/v1alpha1/auth.proto` — AuthService messages
3. `api/proto/wakil/v1alpha1/auth_service.proto` — AuthService definition
4. `api/gen/wakil/v1alpha1/auth.pb.go` — generated (buf)
5. `api/gen/wakil/v1alpha1/wakilv1alpha1connect/auth_service.connect.go` — generated (buf)
6. `internal/auth/tokenstore/tokenstore.go` — DB queries for join_tokens, web_sessions
7. `internal/auth/jointoken/jointoken.go` — token generation, issuance, exchange
8. `internal/auth/websession/websession.go` — session cookie creation, validation, revocation
9. `internal/auth/tokenresolver/tokenresolver.go` — WebSessionResolver (cookie auth)
10. `internal/server/connect/auth_handler.go` — Connect handler for AuthService
11. `internal/server/connect/auth_interceptor.go` — cookie/header extraction middleware + Origin validation
12. Tests for all new packages

### Modified files

1. `internal/core/service.go` — Add `AuthSession`, `AuthAPIToken` to `AuthMethod`
2. `internal/auth/principal.go` — Add `ErrCredentialAbsent`, `ErrInvalidCredential`
3. `internal/auth/context.go` — Add HTTP header context key for cookie/bearer extraction
4. `internal/server/connect/server.go` — Add auth handler, mount on both Unix+TCP
5. `internal/server/connect/errors.go` — Add `CodePermissionDenied` mapping (if needed)
6. `cmd/wakild/server.go` — Wire token store, web session resolver on TCP, mount AuthService, add Connect RPC to TCP with auth middleware
7. `internal/server/connect/system_handler.go` — Update `auth_method` field

---

## Exit Criteria

1. Admin can create a join token via `CreateJoinToken` RPC
2. Client can exchange join token for browser session cookie via `ExchangeJoinToken`
3. Browser with cookie can call Session/Event RPCs over TCP
4. `WhoAmI` returns the caller's principal
5. `Logout` revokes the session and clears the cookie
6. Expired/used/revoked tokens are rejected with generic error (no enumeration)
7. Concurrent double-exchange of the same token produces exactly one success
8. Invalid cookie does not fall through to another resolver
9. Unix-socket local mode continues to work without cookies (P4b preserved)
10. `go test -race` passes for all affected packages
11. `buf lint` + `buf breaking` clean
12. Cross-tenant token exchange is rejected
