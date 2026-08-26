-- 001_init.sql — P1 initial schema for the session-host event log.
-- Creates the sessions and events tables only (the subset needed for P1's
-- SQLite-backed event log). Full schema (tenants, users, workspaces, turns, etc.)
-- arrives in P2/P4.
--
-- schema_migrations is owned by the migration runner (migrate.go), NOT this
-- file. The runner creates it before applying migrations.

CREATE TABLE sessions (
  id         TEXT PRIMARY KEY,
  tenant_id  TEXT NOT NULL,
  workspace  TEXT NOT NULL,
  created_by TEXT NOT NULL,
  created_at INTEGER NOT NULL,   -- Unix nanoseconds (time.Time.UnixNano())
  last_seq   INTEGER NOT NULL DEFAULT 0,
  CHECK (last_seq >= 0),
  UNIQUE (tenant_id, id)         -- composite FK target for events
);

CREATE TABLE events (
  tenant_id   TEXT NOT NULL,
  session_id  TEXT NOT NULL,
  seq         INTEGER NOT NULL,
  ts          INTEGER NOT NULL,   -- Unix nanoseconds
  kind        TEXT NOT NULL,
  payload     BLOB NOT NULL,      -- JSON-encoded (encoding='json-v1')
  encoding    TEXT NOT NULL DEFAULT 'json-v1',
  PRIMARY KEY (session_id, seq),
  FOREIGN KEY (tenant_id, session_id) REFERENCES sessions(tenant_id, id),
  CHECK (seq > 0),
  CHECK (encoding IN ('json-v1'))
);
