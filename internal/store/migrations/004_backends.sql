-- 004_backends.sql — P4g: encrypted LLM backend credential storage.
--
-- Stores per-tenant LLM backend configurations with envelope-encrypted API
-- keys (design §6.4). The API key is encrypted with AES-256-GCM; the DEK is
-- wrapped by the master key. key_id identifies the master key version for
-- future rotation. The plaintext key is never stored; only the ciphertext
-- components and last_four (for display) are persisted.
--
-- The API never returns the encrypted key material; only id, label,
-- last_four, and timestamps are exposed (design §6.4).

CREATE TABLE IF NOT EXISTS backends (
  id               TEXT PRIMARY KEY,           -- backend ID (e.g., "be_<random>")
  tenant_id        TEXT NOT NULL,              -- owner tenant
  label            TEXT NOT NULL,             -- human-facing name
  backend_type     TEXT NOT NULL,              -- "openai", "anthropic", "custom", etc.
  base_url         TEXT NOT NULL DEFAULT '',   -- API endpoint

  -- Envelope-encrypted API key (AES-256-GCM):
  api_key_cipher   BLOB NOT NULL,              -- encrypted payload (includes GCM tag)
  api_key_dek      BLOB NOT NULL,              -- wrapped DEK (includes GCM tag)
  api_key_data_nonce  BLOB NOT NULL,           -- GCM nonce for payload encryption
  api_key_dek_nonce   BLOB NOT NULL,           -- GCM nonce for DEK wrapping
  api_key_key_id   TEXT NOT NULL,              -- master key version tag
  api_key_last_four TEXT NOT NULL DEFAULT '',  -- last 4 chars (display only)

  created_at       TEXT NOT NULL,              -- RFC 3339
  updated_at       TEXT NOT NULL,              -- RFC 3339

  FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_backends_tenant ON backends(tenant_id);
