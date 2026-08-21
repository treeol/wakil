# P4g — Credential Encryption + Scrubbing

**Branch:** `feature/wakild-daemon`
**Prior commit:** `84c361a` (P4f — TLS termination)
**Design ref:** `docs/design/wakild-foundation.md` §6.4 (Secrets), §6.5 (Scrubbing)
**Status:** Implemented

## What was done

### Part A: Envelope Encryption (`internal/crypto`)

AES-256-GCM envelope encryption with AAD binding:
- Master key from `--master-key-file <path>` (file must be 0600, regular file)
- `--generate-master-key <path>` to create a new key and exit
- `WAKILD_MASTER_KEY` env var (not implemented — deferred per Mashūra)
- NO `--master-key <hex>` CLI flag (removed per Mashūra: shell history risk)
- DEK wrapped by master key, both use AAD = `v1\x00{purpose}\x00{tenantID}\x00{rowID}`
- AAD prevents cross-row ciphertext swapping
- File permission checks: rejects group/world-readable key files, non-regular files
- WriteMasterKeyFile uses O_EXCL (refuses overwrite)

### Part B: Secret Scrubbing (`internal/scrub`)

Pattern-based scrubbing applied to JSON-marshaled event payloads at write time:
- Standard level: Bearer tokens, OpenAI/Anthropic/GitHub/Google/AWS API keys, private keys, JWTs, connection string passwords
- Aggressive level: adds generic high-entropy detection with keyword prefix
- `--scrub-level off|standard|aggressive` (default: standard)
- Applied in sqlstore.Append: marshal → scrub JSON → store (comprehensive, not field-by-field)

### Part C: Backends Table + Encrypted Credential Storage

- Migration `004_backends.sql`: per-tenant backends with envelope-encrypted API keys
- Proto: `backend.proto` + `backend_service.proto` (CreateBackend, ListBackends, UpdateBackend, DeleteBackend)
- Handler: `backend_handler.go` — owner/admin only, tenant-scoped, never returns API key
- Store: `backendstore.go` — CRUD with tenant_id predicate, encrypt/decrypt helpers with AAD
- API returns only: id, label, backend_type, base_url, last_four, timestamps

### Part D: Log/Error Hardening

- `oidcresolver.go`: validator error no longer propagates (could contain raw JWT)
- Scrubber catches secrets in error messages within event payloads (comprehensive JSON scrub)

## Threat model

Protects against database-only disclosure (copied DB file or backup). Does NOT protect against full host compromise or attacker with filesystem/process access.

## What remains unverified / deferred

- Key rotation workflow (multiple active master keys) — infrastructure supports key_id, but no rotation API
- KMS integration (AWS KMS, GCP KMS) — file-based key only for now
- Existing plaintext `Config.APIKey` in config file — not migrated to encrypted storage (documented as known gap)
- PII detection beyond pattern-based (no NLP/ML)
- Audit log of scrubbing actions
