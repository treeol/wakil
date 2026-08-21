-- 002_auth_tenancy.sql — P4a: auth + tenancy control-plane tables.
-- Creates tenants, users, memberships, api_tokens, and join_tokens per the
-- schema sketch in docs/design/wakild-foundation.md §4.3. Seeds the default
-- tenant (tnt_local), default user (usr_local), and owner membership so
-- existing embedded/local mode continues to work unchanged.
--
-- This migration does NOT alter the existing sessions/events tables (they
-- already carry tenant_id from 001_init.sql). Adding FKs from sessions→tenants
-- would require table recreation in SQLite; tenant_id validation for the
-- sessions table stays at the application layer (accepted gap, documented).
-- The new tables do carry FKs among themselves.
--
-- Design notes for P4c (not implemented here):
--   - join_tokens with user_id=NULL are "create user on exchange" tokens; the
--     new user's email/display_name/auth_subject come from the exchange
--     request, not the token row. Email collision is an exchange-time error.
--   - Browser session cookies (for the web UI) are NOT stored in this table;
--     P4c will introduce a web_sessions table or use signed cookies.
--   - One-time-use of join_tokens is enforced at the application layer via a
--     conditional UPDATE (WHERE used_at IS NULL) in a transaction.

-- ── tenants ──────────────────────────────────────────────────────────────
CREATE TABLE tenants (
  id            TEXT PRIMARY KEY,
  slug          TEXT NOT NULL UNIQUE,
  display_name  TEXT NOT NULL,
  status        TEXT NOT NULL DEFAULT 'active',  -- active | suspended
  created_at    INTEGER NOT NULL,
  CHECK (status IN ('active', 'suspended'))
);

-- ── users ────────────────────────────────────────────────────────────────
-- Email is globally unique (not per-tenant): one identity, multiple
-- memberships. Case-normalization is the application layer's responsibility
-- (SQLite TEXT comparison is binary by default).
-- auth_subject is the OIDC 'sub' claim. A partial unique index enforces
-- uniqueness for non-NULL values (local/SO_PEERCRED users have NULL).
CREATE TABLE users (
  id            TEXT PRIMARY KEY,
  email         TEXT NOT NULL UNIQUE,
  display_name  TEXT NOT NULL,
  auth_subject  TEXT,                             -- OIDC sub; NULL for local accounts
  password_hash TEXT,                             -- argon2id; NULL when no password (SO_PEERCRED / token-only)
  status        TEXT NOT NULL DEFAULT 'active',   -- active | suspended
  created_at    INTEGER NOT NULL,
  CHECK (status IN ('active', 'suspended'))
);

CREATE UNIQUE INDEX idx_users_auth_subject ON users(auth_subject) WHERE auth_subject IS NOT NULL;

-- ── memberships ─────────────────────────────────────────────────────────
CREATE TABLE memberships (
  tenant_id  TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role       TEXT NOT NULL,                       -- owner | admin | member | viewer
  created_at INTEGER NOT NULL,
  PRIMARY KEY (tenant_id, user_id),
  CHECK (role IN ('owner', 'admin', 'member', 'viewer'))
);

CREATE INDEX idx_memberships_user_id ON memberships(user_id);

-- ── api_tokens ───────────────────────────────────────────────────────────
-- Machine/CI credentials: scoped, expiring, sha256-hashed (never plaintext).
-- token_hash is UNIQUE: one plaintext secret maps to exactly one token row,
-- and the unique index serves as the auth lookup index.
-- ON DELETE CASCADE on the membership FK: deleting a membership (or its
-- tenant/user) removes the tokens. Revocation is app-layer (set revoked_at);
-- the row is retained for audit.
CREATE TABLE api_tokens (
  id           TEXT PRIMARY KEY,
  tenant_id    TEXT NOT NULL,
  user_id      TEXT NOT NULL,
  name         TEXT NOT NULL,
  token_hash   TEXT NOT NULL UNIQUE,               -- sha256 hex of the secret
  scopes       TEXT NOT NULL DEFAULT '[]',         -- JSON array of scope strings (validated app-side)
  expires_at   INTEGER,                            -- NULL = no expiry
  last_used_at INTEGER,
  revoked_at   INTEGER,                            -- NULL = not revoked
  created_at   INTEGER NOT NULL,
  FOREIGN KEY (tenant_id, user_id) REFERENCES memberships(tenant_id, user_id) ON DELETE CASCADE
);

CREATE INDEX idx_api_tokens_tenant_user ON api_tokens(tenant_id, user_id);

-- ── join_tokens ──────────────────────────────────────────────────────────
-- One-time-use tokens issued by an owner/admin for onboarding new users or
-- devices. Exchanged for an api_token (CLI/CI) or session cookie (browser).
-- token_hash is sha256 hex of the plaintext join secret (never stored).
--
-- user_id semantics:
--   NULL  = "create user on exchange" (P4c supplies user details at exchange)
--   non-NULL = bind to an existing user (must be a member of the tenant)
--
-- created_by has a composite FK to memberships: the issuer must be a member
-- of the tenant the token is scoped to. Authorization to issue a particular
-- role (e.g. only owners can issue owner tokens) is app-layer.
CREATE TABLE join_tokens (
  id           TEXT PRIMARY KEY,
  tenant_id    TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  user_id      TEXT REFERENCES users(id) ON DELETE SET NULL,
  role         TEXT NOT NULL,                      -- role assigned on exchange
  token_hash   TEXT NOT NULL UNIQUE,               -- sha256 hex of the plaintext secret
  created_by   TEXT NOT NULL,                      -- user who issued the token
  expires_at   INTEGER NOT NULL,                   -- join tokens MUST expire
  used_at      INTEGER,                            -- NULL = unused
  created_at   INTEGER NOT NULL,
  CHECK (role IN ('owner', 'admin', 'member', 'viewer')),
  FOREIGN KEY (tenant_id, created_by) REFERENCES memberships(tenant_id, user_id) ON DELETE RESTRICT
);

CREATE INDEX idx_join_tokens_tenant ON join_tokens(tenant_id);

-- ── default tenant + user bootstrap ─────────────────────────────────────
-- These INSERTs are plain (not INSERT OR IGNORE) because the tables are
-- created earlier in this same migration, so the rows cannot pre-exist.
-- The migration runner (migrate.go) wraps each migration in a transaction
-- and records the version only after success, so re-running is impossible.
-- created_at = 0 is a sentinel for "bootstrap, before first real operation".
INSERT INTO tenants (id, slug, display_name, status, created_at)
  VALUES ('tnt_local', 'local', 'Local', 'active', 0);

INSERT INTO users (id, email, display_name, auth_subject, password_hash, status, created_at)
  VALUES ('usr_local', 'local@localhost', 'Local User', NULL, NULL, 'active', 0);

INSERT INTO memberships (tenant_id, user_id, role, created_at)
  VALUES ('tnt_local', 'usr_local', 'owner', 0);
