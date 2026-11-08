# Open follow-ups

Known deferred work items, kept here so deleting their original proposal docs
doesn't lose them. Each entry lists the decision that closed the original
proposal and what remains.

## Prompt architecture

- **Redaction tool** (from the prompt-corrective-revisions review, §2.1(b)):
  the agent.txt screening rules say "a tested redaction tool is the only
  sanctioned redactor; until one exists, withhold". `internal/scrub`
  (PatternScrubber) exists but is the store write-path scrubber, not an
  agent-exposed tool. Until a dedicated redaction tool exists and is tested,
  the withhold rule stays in force.
- **Harness-enforcement follow-ons** (same review, §5): encode the
  non-emitting screening contract (grep -c / exit-status-only checks) as
  harness tests rather than relying on prompt compliance alone.
- Prompt **trimming** (docs/prompt-trimming-proposal.md, deleted) and
  **session-scoped progressive prompt loading**
  (docs/session-scoped-prompt-proposal.md, deleted) were both **rejected**
  (measured 97.6% prompt-cache hit rate made the economics moot; the safety
  kernel computes to 14–19 KB, above the go/no-go threshold). Note: the
  separately shipped `/prompt core` mode (lean preamble + lazy-loaded
  extension, `prompt_mode` config) is a different, implemented design.
  Any future prompt-size proposal must start from the go/no-go measurement
  template of the deleted session-scoped proposal: kernel size, cache-hit
  rate, section usage distribution (recoverable via
  `git log --follow -- docs/session-scoped-prompt-proposal.md`).

## TUI rotation

- **Handoff stage labels** ("generating summary…"): needs a progress callback
  on `RunHandoffPipeline` → `ConversationManager` interface change →
  stale-stage guard. Deferred — the pulsing indicator conveys ~90% of the
  value; revisit only if "frozen" reports persist.
- **Rotation timeout**: `beginRotate` (internal/tui/tui.go) passes
  `context.Background()` and `RunHandoffPipeline` does not add the 120s
  timeout its legacy path had — a hung backend can block rotation
  indefinitely. Fix is a separate change.

## Long-session mode

- **Tool-cache retention**: age-based retention exists
  (`tool_cache_retention_days`, default 30, pruned once at startup; 0 =
  never). Size-based cleanup of the tool cache remains open.
- **Model-proposed partial keep** (`context_keep`-style tool with
  tool-call/sibling closure) — collapse is currently deterministic.
- **Rolling summarization of old answers** so the base stays bounded on very
  long sessions (tail-cap degrade bounds the collapsed-turn answer
  component only; uncapped questions, kept turns and never-collapsed turns
  still grow).

## Continuous mode

- CM2 (coordinator-owned goal completion), CM3 (durable state, leases,
  reconciliation), CM4 (dispatch-time side-effect ledger) — see
  [continuous-mode.md](continuous-mode.md).

## Truncation audit

- **Mashūra review token accounting** (A.8, truncation-damage audit): the
  review footer line (`[mashūra: … — ~N tokens sent, M part(s), …]`) reports
  tokens sent to each reviewer, not tokens in the document being reviewed.
  A reviewer that errors (rate-limit, 400) still reports its token count;
  the count is the question+context tokens, not the answer. The first session
  where this was observable was the design note round 1 (its content is in docs/adr/0011-cleanup-pass-invariants.md)
  (2026-11-03): gpt-6-astra errored (rate-limited) and its footer reported
  ~2141 tokens sent; claude-fable-5.1 succeeded with ~2141 tokens sent. The
  prior sandbox-backends.md round (same session) reported ~11479 tokens sent.
  No evidence of under-counting — the footer accurately reflects the tokens
  dispatched.

## Sandbox (hisn)

- **A.9: hisn policy configurability** — `buildPolicy`
  (`internal/sandbox/hisn/policy.go:40`) hardcodes `Network: "host"` and
  `Shm: true`. These are correct defaults for Wakil's use case (builds need
  network; Chrome/Node need /dev/shm) but could be made configurable via
  `sandbox.hisn.network` and `sandbox.hisn.shm` keys if a use case arises.
  Currently only `shm_size_mb`, `tmp_size_mb`, `env_pass`, `grantable_rw`,
  `binary`, `home_root`, `kvr_binary` and `exec_via_cli` are configurable.

## hisn 0.3.1 integration (v0.4.0)

These items predate the 0.4.1 minimum and are carried as written. Deferred from Mashūra round 1/2 of the 0.3.1 integration (non-blocking):

- **N1 — `.wakil` parent rename vs. the mask.** `mounts.mask` of
  `.wakil/sandbox.json` makes the file unwritable inside the sandbox, but
  whether a sandboxed command can `rename(".wakil", …)` (moving the mask with
  it) and create a fresh file is not verified against a real hisn. The approval
  gate (a changed file is unapproved) is the actual control; verify the mask
  behaviour in a real session.
- **N4 — dropping `--network host` from the CLI.** In profile mode the network
  comes from the profile's `[policy]`. Confirm on a host that a generated
  profile session really has host networking (the integration tests do not
  assert it).
- **N7 — peer-credential check for the kvr socket.** After `connect`, compare
  `SO_PEERCRED` (uid, and pid against the service pid in `inspect --json
  services[]`) to close the symlink-swap window the path guard only narrows.
- **One `risk_ack` per project.** The string is copied into every
  `[[forward]]`/`[[host_services]]`; labels that differ per entry need a named
  profile (`hisn.profile`).
- **Named-profile mode** has no mask for the project file, no kvr service, and
  identifies the instance by supervisor pid + start time.
- **Built-in profile names** need a `profiles/` directory next to the hisn
  binary; the release binary ships none, so `hisn.profile` names resolve only
  via `$HISN_PROFILES`.
- **`kvr-server` discovery** only looks at `PATH`/`sandbox.hisn.kvr_binary`; a
  binary outside `/usr`, `/opt`, `/nix` (e.g. `~/.cargo/bin`) needs its
  directory in `hisn.mounts_ro`.
- **Single-pass risk labels.** `risk_ack` and the label check run at start; a
  profile edited while a session runs is not re-checked until the next start.
- **Round-2 non-blocking items** (design note §11): the docker/backend executors'
  protected-file guard is lexical (a `run_shell` symlink could alias the path;
  the approval gate holds); `wakil doctor` says hisn "will run" without
  checking the project-file/`--workdir`/state-dir reasons that keep docker at
  start; `project.Load` Lstats only the leaf (a symlinked `.wakil` directory is
  accepted, the hash gate still applies); `hisn check` runs twice at startup
  (default probe + preflight); a `$HISN_PROFILES` directory inside the
  workspace would let the agent edit a named profile (whether hisn applies
  `E_PROFILE_UNTRUSTED` to names is unknown).
