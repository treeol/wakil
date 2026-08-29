# Commit-signing design: mediated SSH signing from the sandbox

> **Status:** design only — implementation is future work (capability roadmap).
> **Scope:** WP-8.2. This document specifies a *mediated* signing path to
> replace the raw SSH agent socket passthrough that exists today.
>
> **Related:**
> - `docs/proposal-ssh-signing-passthrough.md` — the v2 proposal that shipped
>   the current raw-socket-passthrough implementation.
> - `internal/exec/signing.go` — current implementation.
> - `SECURITY.md` — sandbox threat model and trust boundaries.
>
> **Review history:** reviewed by mashūra counsel (3 panels) on 2026-07-17.
> Revisions addressed: exact-byte signing contract, stale-inode
> self-contradiction, `gpg.ssh.program` invocation modes, SSH-push regression,
> repo-binding enforcement, prompt-injection hardening, log-content policy,
> and revocation mechanism precision.

## 1. Background — what exists today

SSH commit signing from the Wakil sandbox was implemented in 2026-07-04
(proposal v2, now marked "implemented"). The design achieves its stated goal —
`git commit` inside the Docker sandbox produces SSH-signed commits that GitHub
verifies, with the private key never entering the container — through **raw
agent socket passthrough**:

1. `DetectSigning` (host-side, `signing.go:35`) resolves the host
   `SSH_AUTH_SOCK`, the literal public key, and host-effective git identity.
2. `signingEnv` (`signing.go:184`) adds a bind-mount of the agent socket to
   `/ssh-agent.sock` inside the container plus `GIT_CONFIG_*` env pairs that
   configure `gpg.format=ssh`, `user.signingkey=key::<pub>`, and optionally
   `commit.gpgsign=true` / `user.name` / `user.email`.
3. The container sees `SSH_AUTH_SOCK=/ssh-agent.sock` and any process can talk
   to the agent over the AF_UNIX socket. Private keys stay on the host.

This is the **mediation gap**: the socket grants the *entire agent* to *every
process* in the sandbox for the *entire container lifetime*. The v2 proposal
acknowledges this residual risk honestly (§Security analysis, residual risk)
and points users at host-side mitigations — `ssh-add -c`, a dedicated signing
agent, and keeping the default `off`.

A secondary benefit v2 notes is that the agent mount also enables `git push`
over SSH from inside the sandbox. **This design deliberately removes raw
`SSH_AUTH_SOCK` passthrough**; the consequence for push is addressed in §4.9
(SSH authentication is a separate capability from commit signing and should be
granted deliberately, not as a side effect of enabling signed commits).

## 2. Threat model

### Asset
A git commit signature attests authorship. Each signature is a credential-grade
attestation that GitHub (and other forges) render as a "Verified" badge. A
coerced signature over an attacker-chosen commit is equivalent to the attacker
authoring that commit under the user's identity. Unlike a leaked key, a
*signature* is bound to a specific object — it cannot be replayed onto a
different commit — but it can be presented as proof for the object it covers,
so the signing oracle must still be constrained.

### Capability granted today
Binding the host `ssh-agent` socket into the sandbox hands the sandbox a
**signing oracle**: while mounted, any process in the container can request
the agent to sign **arbitrary data** with **any key** the agent holds. This is
strictly broader than "sign git commits":

- **SSH authentication signatures** — the same agent holds keys used for SSH
  login to other hosts. A malicious sandboxed process could sign SSH
  authentication challenges and impersonate the user to any host trusting
  those keys. This is not theoretical: ssh-agent signs whatever the socket
  caller asks for, and the agent cannot distinguish a "commit" signature
  request from an "auth" signature request at the protocol level.
- **Any commit, not just the model's** — the oracle is not scoped to a
  particular repo, message, or SHA. A compromised process can sign arbitrary
  commits at any time while the socket is mounted.
- **Broad blast radius** — a single signing key is often the user's identity
  across many repos.

### Adversary model
The sandbox is **convenience-grade isolation, not adversarial-grade**
(SECURITY.md §Sandbox classification): no seccomp/AppArmor. We therefore treat
any code running inside the container — the model's shell commands, tool
output, dependency build steps, `go test` shelling out — as a potential
adversary that may attempt to invoke the signing oracle. The agent socket is
not a trusted channel from the container's perspective.

The mediator and the host `ssh-agent` themselves are **trusted** components —
they run on the host, outside the container's reach. This design does not model
host-side compromise of the mediator or the agent; that is the same trust
boundary as the host kernel.

### Why the current mitigations are incomplete
The v2 proposal lists three mitigations. They are all host-side and all rely
on the user remembering to apply them:

| Mitigation | Gap |
|---|---|
| `ssh-add -c` (confirm per signature) | Requires the user to run it manually; prompts are generic "allow signature?" with no *what* is being signed. Easy to rubber-stamp. Does not exist on all platforms. |
| Dedicated signing agent holding only the signing key | Correct but manual setup; users rarely do this. Still grants the full signing key as an oracle (any commit/message). |
| Default `off` | Strong, but once `ssh_signing=auto` is enabled the socket is mounted for the **whole** container lifetime — there is no narrower mode. |

The core problem is **temporal**: a mount that exists for hours to enable one
intended `git commit -S` also exists during every unrelated tool call in
between.

## 3. Design goal

Replace raw socket passthrough with a **mediated signing helper** that:

1. Never exposes the `ssh-agent` socket to the sandbox.
2. Signs the **exact bytes git produced** — the mediator never reconstructs a
   commit object from partial fields; it parses git-supplied bytes for display
   and signs those same bytes after approval (the "what you see is what gets
   signed" invariant).
3. Requires **explicit user confirmation per signing operation**, with the
   parsed commit fields shown to the user before approval, hardened against
   prompt injection.
4. Is revocable instantly by stopping the mediator — the closed listener (and
   dropped client connections) is the security boundary, not merely the
   unlinked path.
5. Never places key material on the container filesystem and never logs
   private keys — and, by default, does not log commit messages either.
6. **Does not regress `git push` over SSH silently** — SSH authentication is
   decoupled from commit signing and re-enabled deliberately (§4.9).

## 4. Proposed design

### 4.1 Architecture

A small **host-side signing daemon** (the *mediator*) holds a reference to the
host `ssh-agent` socket and exposes a narrow RPC over a restricted channel to
the sandbox. The container talks to the mediator, not to `ssh-agent` directly.
A tiny **shim binary** in the image is configured as `gpg.ssh.program` so that
git's signing/verification backend is the mediator, not a raw agent.

```
┌──────── host ────────────┐         ┌──────── sandbox container ────────┐
│  ssh-agent (AF_UNIX)     │         │  git commit -S                    │
│    ▲                     │         │    │                              │
│    │ ssh-agent protocol  │         │    ▼                              │
│    │                     │         │  gpg.ssh.program = wakil-shim    │
│  ┌─┴──────────────┐      │         │  (shim, replaces ssh-keygen)     │
│  │  mediator      │ ←── UDS (mount socket *directory*, not file) ──→  shim│
│  │  (host daemon) │      │         │    │                              │
│  │  · per-op gate  │      │         │    ▼                              │
│  │  · signs exact   │      │         │  SignGitCommit RPC:              │
│  │    git bytes    │      │         │   {unsignedCommitBuf, keyFp}     │
│  └────────────────┘      │         │                                   │
│        ▲                 │         │   ← signed signature bytes        │
│        │ user y/n prompt │         └───────────────────────────────────┘
│  ┌─────┴─────────┐      │
│  │ TUI (same proc │      │
│  │  as wakil)     │      │
│  └───────────────┘      │
└──────────────────────────┘
```

Key differences from today:

| Aspect | Today (v2) | Proposed (mediated) |
|---|---|--- |
| What the sandbox sees | The raw `ssh-agent` socket (full signing oracle) | A mediator socket with one method, `SignGitCommit` |
| Scope of signatures | Any data, any agent key, any time | The exact bytes git passed to the shim, parsed + bound to a workspace |
| Socket lifetime | Container lifetime | Mediator lifetime — stop it, capability is gone *now* |
| Key material on container FS | None (already good) | None — mediator holds only a *reference* to the host agent |
| SSH auth (`git push`) | Side-effect of signing mount | Removed; re-granted deliberately (§4.9) |

### 4.2 The mediator (host-side)

The mediator is a library/component running **in the wakil process itself**
(not a separate long-lived daemon — see §4.5 on why a separate process re-creates
the stale-mount problem). It:

1. **Resolves** the signing setup with the existing `DetectSigning` logic
   (`signing.go:35`) — same host git-config source of truth, same
   `off|auto|<path>` config key. No new config surface.
2. **Opens** a listening AF_UNIX socket on the **host**, under a per-session
   path in the user's runtime dir (e.g. `$XDG_RUNTIME_DIR/wakil/sign-<pid>.sock`).
3. **Authenticates** every connection with `SO_PEERCRED` (Linux) /
   `getpeereid` (BSD), accepting only the wakil process's own UID. **`chmod`
   is not the control** — the mediator verifies peer credentials on every
   `accept()`. This is the real authorization; filesystem perms are
   defense-in-depth only. (Container UID mapping is environment-dependent;
   §4.8 lists the test that confirms this holds under the target Docker
   user-namespace config.)
4. **Serves** a single RPC: `SignGitCommit(unsignedCommitBytes, keyFingerprint)
   → sshSignature`. The protocol carries the **exact unsigned commit object**
   git handed the shim, plus the fingerprint of the key to sign with.
5. **Gates** every `SignGitCommit` call with a **user confirmation prompt**
   surfaced through the wakil TUI (same process — no cross-process IPC for
   prompts), identical in shape to the existing `run_shell` gate. The user
   answers `y/n`. No auto-approve path — signing is carved out of `/auto` like
   destructive commands already are.
6. **Exits** when wakil exits, closing its listener and all client connections.

### 4.3 The shim (container-side)

Git's SSH signing backend is `gpg.ssh.program` (defaults to `ssh-keygen`). The
container no longer has `SSH_AUTH_SOCK`. Instead, a **shim binary** —
`wakil-shim`, a Go static binary in the image — is configured as
`gpg.ssh.program` via one additional `GIT_CONFIG_*` pair.

The shim **faithfully reproduces the `ssh-keygen -Y` CLI contract** that git
expects (confirmed against the target git/OpenSSH versions at implementation
time — see §4.8). Git invokes `gpg.ssh.program` for **several** operations,
not only signing:

| Invocation | Shim behavior |
|---|---|
| `sign` (commit/tag) | Connect to the mediator, send the exact buffer git passed, receive signature, write it where git expects. |
| `verify` (`git log --show-signature`, `git verify-commit`) | **Delegate to the real `ssh-keygen -Y verify`** on the host-visible `allowedSignersFile`. The shim does not implement verification itself; it passes through. |
| `find-principals` | Same — delegate to `ssh-keygen`. |

The shim is a thin adapter: for signing it talks to the mediator; for
everything else it execs the real `ssh-keygen` so that verification and tag
flows keep working. **Tag signing (`git tag -s`) is also routed through the
mediator** — the shim detects the `sign` subcommand regardless of whether the
caller is `git commit` or `git tag`, and the mediator's per-op gate shows
"signing a git tag" vs "signing a git commit" accordingly.

### 4.4 Per-operation user confirmation — the "what you see is what gets signed" invariant

The central control. The secure invariant is:

> **The mediator parses `unsignedCommitBytes` for display, then signs those
> exact bytes after approval.** Display fields are never supplied by the
> caller separately from the signed payload.

The gate works as follows:

```
sandbox: git commit -S ...
   │
   ▼
shim → SignGitCommit(unsignedCommitBytes, keyFp)  over mediator socket
   │
   ▼
mediator:
  1. parse unsignedCommitBytes → {tree, parents[], author, committer,
     message, timestamps, headers}   // read-only derivation
  2. verify object-DB binding (§4.6)
  3. render sanitized prompt (§4.7)
  4. surface to wakil TUI, block on y/n
   ┌─────────────────────────────────────────────┐
   │ wakil wants to sign a git commit:           │
   │   repo:    ~/projects/wakil (verified)      │
   │   type:    commit                           │
   │   tree:    3f7a9c2…                         │
   │   parents: [a1b2c3d…]                       │
   │   author:  Jane Doe <jane@example.com>      │
   │             2026-07-17T14:03:00Z            │
   │   message: "feat: add mediated signing"     │
   │   key:     ed25519:SHA256:…6f3              │
   │                                              │
   │   [y] approve   [n] reject                   │
   └─────────────────────────────────────────────┘
   │
   ▼
on y: mediator signs the exact unsignedCommitBytes via host ssh-agent
     (ssh-keygen -Y sign -U against the agent), returns signature to shim
on n: mediator returns an error → git aborts (no unsigned fallback)
```

Properties:
- **What you see is what gets signed.** The user sees fields parsed from the
  exact buffer that will be signed. There is no caller-supplied metadata path
  that can diverge from the signed bytes. This is the fix for the
  "reconstruct-from-partial-fields" flaw — the mediator never *builds* the
  object to sign; git already gave it the object.
- **No silent failure.** Rejection aborts the commit loudly — git's existing
  behavior when signing fails. There is no path that produces an unsigned
  commit in a signing-configured repo.
- **No auto-approve bypass.** Signing is carved out of `/auto`, matching the
  existing treatment of destructive commands.
- **Prompt-fidelity hard invariant (acceptance criterion).** The signed bytes
  are byte-identical to the parsed/displayed bytes. Any implementation that
  reconstructs or transforms the buffer before signing fails acceptance.

### 4.5 Revocation — the closed listener is the boundary

Capability revocation is the property "stop granting the capability and the
sandbox immediately loses it." The security boundary is the **closed listener
and dropped client connections**, not the unlinked path. (Unlinking a socket
path does not propagate through a bind mount; an existing open connection
survives until closed.)

The mediator runs **in-process** with wakil. This avoids re-creating the
stale-inode problem the design criticizes in v2: a separate host daemon that
is restarted would bind a *new* socket inode that the already-running
container cannot see through its mount. By running in-process:

| Action | Effect on sandbox signing capability |
|---|---|
| User quits wakil (`/quit`, Ctrl+D) | Mediator listener closes, all client connections drop → shim RPCs get `ECONNREFUSED` → signing impossible |
| User kills wakil | Same — process death closes the listener |
| Mediator logic errors | In-process; wakil logs and the shim's next `SignGitCommit` fails closed |
| Container compromise | The sandbox only ever had the mediator socket. It cannot reach `ssh-agent`. Quitting wakil kills the only path. |
| Host `ssh-agent` killed | Mediator's signing calls to the agent fail → shim RPC fails closed → no signature produced. (This also revokes the v2 raw-passthrough capability, which the v2 doc understated.) |

**Mount strategy to avoid stale inodes:** the container mounts the
**socket's parent directory** (e.g. `$XDG_RUNTIME_DIR/wakil/` →
`/run/wakil/`), not the socket file itself. A bind-mounted *file* pins the
inode; a bind-mounted *directory* lets the process `connect()` to whatever
socket file currently exists at the path inside it. This is the same reason
mounting `/run/user/1000` (rejected by v2 as "contains more than the agent")
is safer here — wakil creates a *dedicated* subdir holding only `sign-<pid>.sock`,
so the directory mount exposes nothing else. (To be confirmed by a Docker
bind-mount test at implementation time — §4.8.)

### 4.6 Repo binding — enforced, not caller-asserted

A git commit object does not encode a repository. If the RPC trusted a
caller-supplied `repoPath` for the prompt, a malicious sandboxed process could
craft a commit for a different repo while displaying the workspace path. The
mediator therefore **does not trust `repoPath`**:

1. The shim sends only `unsignedCommitBytes` and `keyFingerprint` — no repo
   path in the protocol.
2. The mediator resolves the workspace from wakil's own session state (the
   bind-mount mapping wakil already knows), not from the container.
3. The mediator **verifies the commit's tree (and parents) resolve in the
   workspace's object database** before prompting. A commit whose tree is not
   present in the workspace repo is rejected without a prompt. This binds the
   signature to the actual workspace without trusting the caller's claim.
4. The prompt renders the verified repo path and marks it `(verified)`.

### 4.7 Prompt-injection hardening

The confirmation prompt renders attacker-controlled strings (commit message,
author name). Without sanitization, ANSI escapes or newlines could spoof the
prompt box (e.g., a commit message containing fake `[y] approve` lines). The
mediator:

- **Strips ANSI/control sequences** from all rendered fields.
- **Collapses newlines** in the message preview to a single line, with a
  truncation marker if the message is multi-paragraph.
- **Draws the prompt box after** the sanitized fields, so a field cannot
  "draw" over the approve/reject line.
- The full, unmodified commit message is available on-demand (e.g., press `d`
  for details) but is never auto-rendered at full length in the gate.

### 4.8 Git/OpenSSH compatibility contract (to confirm at implementation)

`gpg.ssh.program` is invoked by git with an `ssh-keygen -Y`-shaped command line.
The exact argv, stdin/stdout, and temp-file behavior is git-version-dependent
and must be **traced and confirmed** before implementation, not assumed:

```sh
GIT_TRACE=1 GIT_TRACE_SETUP=1 git commit -S ...
GIT_TRACE=1 git tag -s ...
GIT_TRACE=1 git log --show-signature    # verify path
```

Acceptance for this item: the shim's argv/IO handling is validated against the
target git and `openssh-client` versions in the sandbox image, and a
compatibility test exercises `commit -S`, `tag -s`, and `verify-commit`
through the shim (sign path via mediator; verify path via passthrough to
`ssh-keygen`). If a future git version changes the `gpg.ssh.program` contract,
the shim must be updated — this is a known maintenance surface.

**Other items to confirm at implementation (not design-blocking):**

1. **Socket auth under Docker user namespaces.** `SO_PEERCRED` UID must match
   the wakil UID under the image's user-namespace config. Confirm with a test
   container that attempts `connect()` to the mediator socket as the mapped
   UID.
2. **Directory-mount socket semantics.** Confirm a bind-mounted *directory*
   lets an in-container `connect()` reach a host socket that was recreated at
   the same path after a mediator restart (the stale-inode avoidance claim).
3. **`ssh-keygen -Y sign -U` agent usage.** The mediator signs via the host
   agent using the `-U` (agent) flag; confirm the target `openssh-client`
   version supports this and that it signs the exact buffer provided.

### 4.9 SSH authentication is decoupled — `git push` over SSH

Removing `SSH_AUTH_SOCK` from the container is a **functional regression for
in-container `git push` over SSH**, which v2 enabled as a side effect of the
signing mount. This design makes that regression explicit and deliberate:

- **Commit signing does not grant SSH authentication.** These are different
  capabilities with different threat profiles. Conflating them is how v2 ended
  up mounting the full agent.
- **`git push` over SSH** (to remotes requiring key auth) is re-enabled by a
  **separate, deliberate config** — e.g. `ssh_push: true` or a dedicated
  `ssh_agent_sock` override — that mounts the agent socket *only* for push,
  with its own `Describe()` marker (`+ssh-push`) and its own threat-model note.
  This is out of scope for WP-8.2 (design-only) but is flagged so the
  regression is not silent.
- **HTTPS push and SSH push to password/key-less remotes** are unaffected.

### 4.10 Configuration surface

No new config key for signing. The design reuses `ssh_signing`
(`off | auto | <path>`):

- `off` — no mediation, no signing. Default.
- `auto` — `DetectSigning` resolves the host signing key; the mediator starts
  and the shim is configured as `gpg.ssh.program`. Container never sees
  `ssh-agent`.
- `<path>` — same, with the explicit `.pub` override. Same behavior.

This is a **backward-compatible behavior change**: users who had
`ssh_signing=auto` still get signed commits, but the mechanism is mediated
rather than raw passthrough. The threat surface shrinks without a config
migration. `Describe()` changes from `+sign` to `+sign(mediated)` to make the
mechanism visible.

**Changes to existing code** (implementation handoff, not done in this WP):

- `SigningSetup`: `AgentSock` becomes internal to the mediator (the host
  agent socket is never mounted). The struct gains the in-process mediator
  listener state and the shim path. `signingEnv` drops the `-v AgentSock`
  mount and the `SSH_AUTH_SOCK` env, adds the directory mount and the
  `gpg.ssh.program=wakil-shim` `GIT_CONFIG_*` pair.
- `Dockerfile`: add the `wakil-shim` binary (built from a new tiny package)
  alongside the existing `openssh-client`.
- `internal/exec/exec.go` (`DockerOpts`): the signing field carries mediator
  setup, not a raw socket path.

### 4.11 Open questions (deferred to implementation)

1. **Protocol framing for `SignGitCommit`.** JSON over the AF_UNIX socket vs.
   length-prefixed binary framing. Low stakes — pick at implementation time.
   The buffer can be large (commit + message); enforce a max request size.
2. **Multiple concurrent commits.** If two tool calls both `git commit -S` at
   once, the mediator must queue prompts, not interleave them, and the user
   must be able to cancel the queue. The existing `bgMu`/`bgRegistry` pattern
   in `internal/agent/` is a model. Auto-sign storms (host
   `commit.gpgsign=true` mirrored → many commits → many prompts) are a UX
   cost to assess; a "approve next N" UX may help but must not become an
   auto-approve path for signing.
3. **Prompt fidelity fields.** Exactly which fields to render by default
   (full diffstat? just the message?) is a UX decision. Minimum: repo, type,
   tree, parents, author, message, key fingerprint.
4. **Direct mode.** In `--exec direct` there is no container boundary; the
  mediator provides no isolation benefit and `git commit -S` should fall back
   to the host's own `ssh-agent` directly (as today). `DirectExecutor` is
   unchanged by this design.
5. **`gpg.ssh.allowedSignersFile`.** Verification (not signing) of signatures
   inside the sandbox is out of scope, as in v2; the shim delegates verify to
   `ssh-keygen` regardless.
6. **Logging policy.** By default, log only: repo, type, tree hash, key
   fingerprint, approve/reject, timestamp. **Do not log the commit message by
   default** — commit messages can contain secrets. A debug verbosity could
   include the message subject (first line) only, never the body. No raw-frame
   debug logging of the unsigned buffer or the signature.
7. **Temp-file handling.** If the mediator shells out to `ssh-keygen -Y sign`,
   it may use temp files for the payload/signature. These contain commit
   contents (not keys) — use `os.CreateTemp` under a 0700 dir and clean up on
   every exit path.

## 5. Acceptance

Per WP-8.2, **acceptance is the existence of this design document**.
Implementation is explicitly future work (capability roadmap).

For the future implementation WP, the **hard acceptance criteria** are:

1. The mediator signs the **exact bytes git passed to the shim** — it parses
   for display, never reconstructs. (§4.4 invariant)
2. Prompt fields are **derived from the signed bytes**, not caller-supplied.
3. Rejection **fails closed** — git aborts, no unsigned fallback.
4. Revocation works via the **closed listener + dropped connections**, and
   quitting wakil immediately revokes the capability. (§4.5)
5. Repo binding is **enforced by object-DB verification**, not caller-asserted
   path. (§4.6)
6. Prompts are **sanitized** against ANSI/newline injection. (§4.7)
7. The `gpg.ssh.program` contract is **traced and confirmed** against the
   target git/OpenSSH versions; `verify` and `tag -s` paths are exercised.
   (§4.8)
8. No private key file is read or mounted; `resolvePublicKey`'s
   never-open-private-key invariant is preserved. (§4.6 of v2)
9. No private key material is logged; commit messages are not logged by
   default. (§4.11 #6)
10. `git push` over SSH regression is **documented and decided**, not silent.
    (§4.9)

## 6. Summary of security improvement

| Property | v2 (raw passthrough) | This design (mediated) |
|---|---|---|
| Sandbox can sign arbitrary data | yes — full oracle | no — exact git-provided commit bytes only |
| Sandbox can use SSH auth keys | yes | no — mediator is commit/tag-only |
| Per-operation user confirmation | only via `ssh-add -c` (generic) | yes — shows parsed repo + message + tree + parents |
| "What you see is what gets signed" | n/a (no gate) | hard invariant — display parsed from signed bytes |
| Revocation latency | container lifetime (mount pins inode) | instant (close listener → `ECONNREFUSED`) |
| Stale-socket-after-restart fragility | yes (inode-pinned mount) | avoided (in-process + directory mount) |
| Key material on container FS | none (good) | none (preserved) |
| Key material logged | n/a | never (operation + fingerprint only) |
| Commit messages logged | n/a | no, by default (may contain secrets) |
| Auto-approve bypass | n/a (no gate) | blocked — carved out of `/auto` |
| `git push` over SSH | side-effect of signing mount | removed; re-granted deliberately (§4.9) |
| `git tag -s` | worked (raw agent) | works (mediator, sign path) |
| `git verify-commit` / `log --show-signature` | worked (raw agent) | works (shim delegates to `ssh-keygen`) |
