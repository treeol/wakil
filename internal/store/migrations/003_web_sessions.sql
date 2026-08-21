-- 003_web_sessions.sql — P4c: browser session storage + join_token fixes.
--
-- Creates the web_sessions table for server-side browser sessions (opaque
-- random token, SHA-256 hashed at rest). The cookie value is a 256-bit
-- CSPRNG secret with a 'wst_' prefix; only its SHA-256 hash is stored.
--
-- Also fixes two issues in the join_tokens table (002_auth_tenancy.sql):
--   1. user_id ON DELETE SET NULL → ON DELETE CASCADE: SET NULL would
--      convert a bound token into a "create user on exchange" token when
--      the bound user is deleted, escalating its capability. CASCADE
--      deletes the token instead.
--   2. Adds a revoked_at column: unused tokens can now be manually revoked
--      before expiry (admin issued to wrong person, etc.).
--
-- SQLite cannot ALTER TABLE to change FK behavior or add columns with
-- constraints, so join_tokens is recreated via the standard SQLite
-- migration pattern: create new table, copy data, drop old, rename.

-- ── web_sessions ──────────────────────────────────────────────────────────
-- Opaque server-side browser sessions. The cookie value sent to the browser
-- is a 256-bit random secret (wst_<base64url>); only its SHA-256 hash is
-- stored here. A DB lookup per request resolves the session to a principal.
--
-- The session stores tenant_id and user_id but NOT role: the resolver reads
-- the current membership role from the memberships table at request time, so
-- role changes and suspensions take effect immediately.
CREATE TABLE web_sessions (
  id               TEXT PRIMARY KEY,
  token_hash       TEXT NOT NULL UNIQUE,              -- sha256 hex of the session secret
  tenant_id        TEXT NOT NULL,
  user_id          TEXT NOT NULL,
  created_at       INTEGER NOT NULL,
  last_seen_at     INTEGER NOT NULL,                  -- updated on use (rate-limited)
  idle_expires_at  INTEGER NOT NULL,                  -- sliding expiry
  expires_at       INTEGER NOT NULL,                  -- absolute expiry
  revoked_at       INTEGER,                           -- NULL = active
  FOREIGN KEY (tenant_id, user_id) REFERENCES memberships(tenant_id, user_id) ON DELETE CASCADE
);

CREATE INDEX idx_web_sessions_tenant_user ON web_sessions(tenant_id, user_id);
CREATE INDEX idx_web_sessions_expires ON web_sessions(expires_at);

-- ── join_tokens recreation ────────────────────────────────────────────────
-- Recreate join_tokens with ON DELETE CASCADE (not SET NULL) and add
-- revoked_at. The composite FK (tenant_id, created_by) → memberships stays
-- ON DELETE RESTRICT (an admin's issued tokens block membership deletion;
-- revoke the tokens first).

CREATE TABLE join_tokens_new (
  id           TEXT PRIMARY KEY,
  tenant_id    TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  user_id      TEXT REFERENCES users(id) ON DELETE CASCADE,
  role         TEXT NOT NULL,
  token_hash   TEXT NOT NULL UNIQUE,
  created_by   TEXT NOT NULL,
  expires_at   INTEGER NOT NULL,
  used_at      INTEGER,
  revoked_at   INTEGER,                               -- NULL = not revoked (NEW)
  created_at   INTEGER NOT NULL,
  CHECK (role IN ('owner', 'admin', 'member', 'viewer')),
  FOREIGN KEY (tenant_id, created_by) REFERENCES memberships(tenant_id, user_id) ON DELETE RESTRICT
);

INSERT INTO join_tokens_new (id, tenant_id, user_id, role, token_hash, created_by, expires_at, used_at, revoked_at, created_at)
  SELECT id, tenant_id, user_id, role, token_hash, created_by, expires_at, used_at, NULL, created_at
  FROM join_tokens;

DROP TABLE join_tokens;
ALTER TABLE join_tokens_new RENAME TO join_tokens;

CREATE INDEX idx_join_tokens_tenant ON join_tokens(tenant_id);
