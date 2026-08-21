-- 005_workspaces.sql — P5b: workspace management for the UI write-path.
--
-- Workspaces are project roots / working directories (design §4.3). A session
-- pins a workspace at creation time. The host_path is the filesystem path on
-- the daemon host that the sandbox mounts.
--
-- All queries are tenant-scoped (design §6.3): every method includes a
-- tenant_id predicate. The UNIQUE(tenant_id, name) constraint prevents
-- duplicate workspace names within a tenant.

CREATE TABLE IF NOT EXISTS workspaces (
  id            TEXT PRIMARY KEY,           -- workspace ID (e.g., "wsp_<random>")
  tenant_id     TEXT NOT NULL,              -- owner tenant
  name          TEXT NOT NULL,              -- human-facing name, unique per tenant
  host_path     TEXT NOT NULL,              -- filesystem path on the daemon host
  vcs_remote    TEXT NOT NULL DEFAULT '',   -- optional VCS remote URL
  created_at    TEXT NOT NULL,              -- RFC 3339
  UNIQUE(tenant_id, name),
  FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_workspaces_tenant ON workspaces(tenant_id);
