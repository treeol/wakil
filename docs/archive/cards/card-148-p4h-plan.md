# P4h — Cross-Tenant Pentest (P4 Exit Gate)

## Goal

Design doc §9 P4 exit criterion (line 556):
> Zwei Mandanten auf einem Daemon; ein Penetrationstest-Skript versucht
> systematisch Cross-Tenant-Zugriff über jeden RPC und schlägt überall fehl.

Two tenants on one daemon; a penetration test script systematically attempts
cross-tenant access over every RPC and fails everywhere.

## Scope

Exercise every tenant-scoped RPC at the Connect handler level (HTTP →
headerInjector → Connect handler → principalResolver → core service → store)
with two tenants on the same daemon, verifying that cross-tenant access
fails everywhere.

## Vulnerability Found & Fixed

**RevokeJoinToken cross-tenant revocation** — The `RevokeJoinToken` store
query (`tokenstore.go`) used `WHERE id = ? AND revoked_at IS NULL` with no
`tenant_id` predicate. An admin from tenant B could revoke tenant A's join
token by knowing its ID.

**Fix**: Added `tenantID` parameter to `Store.RevokeJoinToken`,
`Issuer.Revoke`, and the `AuthHandler.RevokeJoinToken` call site. The store
now uses `WHERE id = ? AND tenant_id = ? AND revoked_at IS NULL`, and the
existence check is also tenant-scoped. A cross-tenant revocation returns
`ErrJoinTokenNotFound` (no existence leak).

## Pentest Coverage

### SessionService (8 RPCs)
- **CreateSession** — tenant A creates; B creates own (no cross-tenant issue)
- **GetSession** — B→A returns CodeNotFound ✅
- **ListSessions** — B's list excludes A's sessions ✅
- **DeleteSession** — B→A returns CodeNotFound ✅
- **SubmitInput** — B→A returns CodeNotFound ✅
- **RespondToApproval** — B→A returns CodeNotFound ✅
- **Interrupt** — B→A returns CodeNotFound ✅
- **CloseSession** — B→A returns CodeNotFound ✅

### EventService (3 RPCs)
- **StreamEvents** — B→A returns CodeNotFound ✅
- **ListEvents** — B→A returns CodeNotFound ✅
- **GetSessionSnapshot** — B→A returns CodeNotFound ✅

### AuthService (11 RPCs)
- **CreateJoinToken** — B issuing for tenant A returns PermissionDenied ✅
- **ListJoinTokens** — B's list excludes A's tokens ✅
- **RevokeJoinToken** — B revoking A's token returns NotFound ✅ (fixed)
- **ExchangeJoinToken** — public, but token is tenant-bound at exchange
- **WhoAmI** — returns caller's own principal, no cross-tenant data
- **Logout** — revokes caller's own session cookie
- **CreateAPIToken** — scoped to caller's tenant
- **ListAPITokens** — B's list excludes A's tokens ✅
- **RevokeAPIToken** — tenant-scoped at store level ✅
- **GetOIDCAuthURL** — public, no tenant data
- **ExchangeOIDCCode** — public, resolves to caller's tenant

### BackendService (4 RPCs)
- **CreateBackend** — scoped to caller's tenant
- **ListBackends** — B's list excludes A's backends ✅
- **UpdateBackend** — B→A returns CodeNotFound ✅
- **DeleteBackend** — B→A returns CodeNotFound ✅

### SystemService (2 RPCs)
- **GetServerInfo** — no tenant-scoped data
- **Health** — no tenant-scoped data

### Store-Level Defense-in-Depth
- `backendstore.Get/Update/Delete` — all have `WHERE tenant_id = ?` ✅
- `backendstore` AAD binding prevents cross-tenant ciphertext decryption ✅

## Control Phase

After B's attack attempts, tenant A verifies it can still:
- GetSession ✅
- ListSessions (includes A's session) ✅
- ListJoinTokens (A's token not revoked by B) ✅
- ListAPITokens (A's tokens intact) ✅
- ListBackends (A's backend unchanged) ✅

## Files Changed

- `internal/auth/tokenstore/tokenstore.go` — `RevokeJoinToken` gains `tenantID` param
- `internal/auth/jointoken/jointoken.go` — `Issuer.Revoke` gains `tenantID` param
- `internal/auth/jointoken/jointoken_test.go` — updated calls to pass tenant ID
- `internal/server/connect/auth_handler.go` — passes `p.TenantID` to `issuer.Revoke`
- `internal/server/connect/cross_tenant_pentest_test.go` — comprehensive pentest (new)

## Verification

- `go build ./...` ✅
- `go vet ./...` ✅
- `go test -race ./internal/...` ✅ (all packages)
- `gofmt` ✅
- `buf lint` ✅
- `buf breaking` ✅
