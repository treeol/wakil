# Design Spike: Persistent Semantic Code Index

**Status:** Design spike (no implementation). Card: [TMu1N9hO](https://trello.com/c/TMu1N9hO)
**Date:** 2026-07-21
**Deps satisfied:** Multi-language LSP (P2) ✓, Verification runner (P3) ✓

## Problem

No persistent workspace semantic index exists. The agent has:
- `search_files` — grep -rn, returns matching lines with file:line, no symbol awareness
- `lsp_symbols` — workspace symbol search via LSP (7 languages), but requires a running
  language server, slow first-call, not persistent across sessions
- `memory` store — general-purpose KV with FTS5, not a code index (no file-path/line-range/
  symbol-type columns)

Missing: `code_search`, `symbol_graph`, `impact_analysis`, `related_tests`.

## Recommendation: Option D — thin code_search MVP now, SQLite index documented as v2

Ship a thin `code_search` wrapper as the MVP. Document the persistent SQLite index as a
future phase (v2) for when `impact_analysis`/`related_tests` are needed.

### Why not build the SQLite index now (Option B)
- Card says "Do NOT start with impact_analysis/related_tests" — those are the main
  beneficiaries of a persistent index
- Index freshness, incremental updates, and Docker storage add complexity
- The thin wrapper delivers immediate value with zero storage/freshness concerns

### Why not do nothing (Option C)
- LSP `lsp_symbols` requires a running server, slow first-call, not persistent
- `search_files` has no symbol awareness — can't answer "find the definition of X"
- A `code_search` wrapper adds value over both: faster than LSP startup, more
  structured than raw grep

## MVP: code_search wrapper (v1)

### Shape
A `code_search` tool that wraps grep with structured output:
- Query: pattern + optional file glob + optional symbol-type filter
- Returns: file, line, snippet, line range (for context)
- **No enclosing-definition detection in v1** — regex-based boundary detection across 7
  languages is fragile and would disappoint. The LSP tools already handle "jump to
  definition" use cases; `code_search` is for text search with structure.

### Why not ripgrep
Ripgrep (`rg`) is NOT installed in the sandbox image (Dockerfile installs git, jq, make,
build-essential, python3, nodejs, etc. — no ripgrep). The wrapper would use `grep -rn`,
same as the existing `search_files`. The value-add is structured output (file/line/snippet
as JSON, not raw text) and optional symbol-type filtering, not a faster grep backend.

### What it adds over search_files
- Structured JSON output (file, line, snippet, context lines) instead of raw text
- Optional symbol-type filter (function/class/struct/method) using language-specific regex
- Optional context lines (like `grep -A/-B/-C`) — currently search_files returns only the
  matching line

### Privacy
Local only, no cloud indexing. The wrapper runs grep inside the sandbox — no data leaves
the workspace. Same privacy boundary as `search_files`.

## Future: Persistent SQLite index (v2)

### Storage
`.wakil/index.db` — SQLite via `modernc.org/sqlite` (pure Go, already a dependency, no cgo).
Writable in the workspace (same as `.wakil/plan.md`, `.wakil/gotmp`, `.wakil/gocache`).

**Alternative:** host-side path (like the memory store at `~/.local/share/wakil/memory/`).
Host-side avoids polluting the workspace and survives container recreation, but requires
path mapping (host → container) and a separate connection. The memory store already does
this — its pattern could be reused.

**Recommendation:** host-side, keyed by workspace path (same pattern as memory store).
Avoids `.wakil/index.db` in every project directory.

### Schema
```sql
CREATE TABLE symbols (
    id INTEGER PRIMARY KEY,
    workspace TEXT NOT NULL,      -- workspace path (host-side key)
    file TEXT NOT NULL,           -- relative path
    line_start INTEGER NOT NULL,
    line_end INTEGER,
    name TEXT NOT NULL,           -- symbol name
    kind TEXT NOT NULL,           -- function/class/struct/method/variable
    language TEXT NOT NULL,       -- go/rust/python/typescript/...
    file_hash TEXT NOT NULL,      -- for invalidation
    container TEXT,               -- enclosing symbol (e.g. method → class)
    UNIQUE(workspace, file, line_start, name)
);
CREATE INDEX idx_symbols_name ON symbols(workspace, name);
CREATE INDEX idx_symbols_file ON symbols(workspace, file);
CREATE VIRTUAL TABLE symbols_fts USING fts5(name, file, kind);
```

### Invalidation
File hash (FNV-64a or SHA-1) per file. On index refresh:
1. Walk the workspace, compute file hashes
2. For each file: if hash matches the stored hash, skip. If different, re-index that file.
3. Delete entries for files that no longer exist.

The memory store already has `Anchor{Path, Hash}` machinery and staleness checks — this
pattern can be reused.

### Privacy boundary
- Local only, no cloud indexing
- Exclude: `.git/`, `vendor/`, `node_modules/`, `__pycache__/`, `target/`, build dirs
- No secrets indexing: skip `.env`, `*.pem`, `*key*`, `*secret*`, `*credential*`, `*token*`
  (same exclusion patterns as the Mashūra evidence screening)
- The index is workspace-scoped, not shared across projects (same as memory store)

### What NOT to build yet
- `impact_analysis` — needs language-specific call-graph analyzers (tree-sitter or LSP
  textDocument/references). The LSP tools already provide `lsp_references` for this.
- `related_tests` — needs language-specific test-framework awareness. The verification
  runner (P3) + `search_files` can find test files by convention.

## Benchmarking (deferred)
The card asks to "benchmark indexing on a medium repo." This is deferred until the v2
implementation is started — benchmarking a design that doesn't exist yet is premature.

## Path forward
1. **v1 (if prioritized):** `code_search` tool — thin grep wrapper with structured output
2. **v2 (future):** Persistent SQLite index — only when impact_analysis/related_tests are
   needed and LSP `lsp_references` proves insufficient
3. **v3 (future):** `impact_analysis`, `related_tests` — needs language-specific analyzers

## Decision
This is a design spike — no implementation. The recommendation is to NOT build the
persistent index now. If `code_search` is prioritized as a separate card, implement it as
a thin grep wrapper (v1). The SQLite index (v2) is documented here for future reference.

## Mashūra review (3-panel decide)
All three panels recommended Option D (thin wrapper MVP + documented v2):
- OpenAI gpt-5.5: "Choose D: MVP code_search over ripgrep, design v2 SQLite index"
- Anthropic claude-5-fable: "D is the only option that satisfies all of the card's asks"
- Moonshot kimi-k3: "D is the only option consistent with all nine findings"

Key caveats noted:
- ripgrep is NOT in the sandbox → wrapper uses grep (confirmed)
- Symbol-boundary detection via regex is fragile across 7 languages → scope v1 to
  structured search without enclosing-definition detection
- LSP default is OFF (`lsp_enabled` config field) → lsp_symbols is conditional, not
  guaranteed coverage
