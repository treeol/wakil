-- 006_agents.sql — P5c: agent management for the UI write-path.
--
-- Agents are named, tenant-scoped agent configurations (design §4.3, §7).
-- Each agent has one or more immutable revisions; a session pins a specific
-- revision at creation time so configuration changes cannot break running
-- sessions.
--
-- All queries are tenant-scoped (design §6.3): every method includes a
-- tenant_id predicate.

CREATE TABLE IF NOT EXISTS agents (
  id            TEXT PRIMARY KEY,           -- agent ID (e.g., "agt_<random>")
  tenant_id     TEXT NOT NULL,              -- owner tenant
  name          TEXT NOT NULL,              -- human-facing name, unique per tenant
  head_rev_id   TEXT NOT NULL DEFAULT '',   -- current revision ID
  created_at    TEXT NOT NULL,              -- RFC 3339
  UNIQUE(tenant_id, name),
  FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_agents_tenant ON agents(tenant_id);

CREATE TABLE IF NOT EXISTS agent_revisions (
  id            TEXT PRIMARY KEY,           -- revision ID (e.g., "rev_<random>")
  tenant_id     TEXT NOT NULL,              -- owner tenant
  agent_id      TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  rev_number    INTEGER NOT NULL,           -- monotonic per agent
  spec          TEXT NOT NULL,              -- JSON: prompt, tools, limits, subagent policy
  spec_hash     TEXT NOT NULL,              -- content-addressed hash
  created_by    TEXT NOT NULL,              -- user ID
  created_at    TEXT NOT NULL,              -- RFC 3339
  UNIQUE(agent_id, rev_number),
  FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_agent_revs_tenant ON agent_revisions(tenant_id);
CREATE INDEX IF NOT EXISTS idx_agent_revs_agent ON agent_revisions(agent_id);
