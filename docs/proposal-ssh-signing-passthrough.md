# Proposal: SSH commit signing inside the Wakil sandbox — v2

> **Status:** implemented (2026-07-04). Config derived from the host's git
> config (auto-detection), single wakil key as override. See
> internal/exec/signing.go, internal/exec/exec.go (DockerOpts), Dockerfile.
> **Goal:** `git commit` inside the docker sandbox produces SSH-signed commits
> that GitHub verifies — the private key never enters the container.

## Problem

Signed commits work on the host (`gpg.format ssh`, `user.signingkey`, key in
`ssh-agent`). Inside the sandbox they fail because:

1. No `SSH_AUTH_SOCK` — the agent socket is not mounted.
2. Sandbox `HOME` is `~/.wakil/sandbox-home` (exec.go:142) — host
   `~/.gitconfig` (signing settings, identity) is invisible.
3. The image lacks `openssh-client` — git shells out to `ssh-keygen`
   for SSH signatures (git-config(1): `gpg.ssh.program` defaults to
   `ssh-keygen`), so signing fails even with keys present.

## Design principles

1. **Pass the agent, not the key.** Git needs only the *public* key
   (`user.signingkey`) plus an agent holding the private key. Same trust
   shape as the existing `docker_socket` grant: opt-in bind-mount.
2. **The host git config is the source of truth.** Wakil detects whether
   signing is configured at all, and which key, by reading the host's own
   git config. No duplicated key path in wakil's config. One override
   exists for the exceptional case.
3. **No mounted key file.** git accepts a literal public key as
   `user.signingkey` with the `key::` prefix (git-config(1),
   `gpg.ssh.defaultKeyCommand`: "a valid ssh public key prefixed with
   `key::`"). Wakil reads the `.pub` on the host and injects the literal —
   one less mount, nothing on disk in the container.

## Configuration surface

One config key, string:

```jsonc
// ~/.config/wakil/config.json
"ssh_signing": "auto"     // "off" (default) | "auto" | "<path to .pub>"
```

- `"off"` — default. No agent mount, no signing config injected. Capability
  grants stay explicit, consistent with `docker_socket`.
- `"auto"` — detect from host git config (below). If signing isn't
  configured on the host, log one line and continue unsigned — never fail
  startup.
- explicit path — skip detection of the key, use this `.pub` file; still
  requires the agent socket. For the "I sign with a different key in wakil"
  case.

Flag: `--ssh-signing=auto` next to `--docker-sock` (config.go ~421).

## Host-side detection (`"auto"` mode)

New file `internal/exec/signing.go`, runs on the **host** before the
container starts:

```go
// SigningSetup is resolved on the host and consumed by NewDockerExecutor.
type SigningSetup struct {
    Enabled   bool
    AgentSock string // resolved host SSH_AUTH_SOCK
    PublicKey string // literal "ssh-ed25519 AAAA… comment"
    AutoSign  bool   // host commit.gpgsign
}

func DetectSigning(mode, repoDir string) (SigningSetup, string /*skip reason*/)
```

Detection steps (all read-only, `git -C <workspace> config get …` so
repo-local config is respected):

1. `gpg.format` — must be `ssh`. Anything else (unset, `openpgp`, `x509`)
   → skip with reason "host git signing is not ssh-format". GPG passthrough
   is explicitly out of scope (gpg-agent forwarding is a different, messier
   problem).
2. `user.signingkey` — resolve to a literal public key:
   - value starts with `key::` → use as-is.
   - path ending in `.pub` → read the file (host), use contents.
   - path *not* ending in `.pub` (people commonly configure the private
     key path, e.g. `~/.ssh/id_ed25519`) → read `<path>.pub` if it exists.
     **Never open the private key file itself.** If no `.pub` sibling →
     skip with reason.
   - unset but `gpg.ssh.defaultKeyCommand` set → v1: skip with reason
     (running arbitrary host commands from detection is a bigger decision;
     see open questions).
3. `commit.gpgsign` — **decided (revised): mirror the host-effective value
   for this repo.** Detection runs `git -C <workspace> config get
   commit.gpgsign` on the host, which resolves global + repo-local scope
   with repo winning. Inject `commit.gpgsign=true` only when that resolves
   to true; inject nothing otherwise. This reproduces host behavior
   exactly: repos configured to sign do so automatically (no `-S` needed —
   git-config(1): commit.gpgSign signs all commits when true), repos that
   don't stay unsigned, and a repo that locally overrides a global
   `gpgsign=true` to false is respected. Note: repo-local `.git/config`
   travels with the workspace mount and is visible in-container anyway;
   the injection matters for the global-scope case, which the container
   cannot see (sandbox HOME ≠ host HOME).
4. `SSH_AUTH_SOCK` must be set and the socket must exist → else skip with
   reason "no ssh-agent".

`DetectSigning` returns a skip reason string so startup can log exactly why
signing is inactive — no silent degradation.

Explicit-path mode runs only steps 2 (with the given path) and 4.

## Container wiring (`NewDockerExecutor`)

The constructor is at 4 positional params and this adds more → introduce an
options struct now (answering v1 open question #2):

```go
type DockerOpts struct {
    Image, Workdir, HostMount string
    DockerSock                bool
    Signing                   SigningSetup
}
func NewDockerExecutor(opts DockerOpts) (*DockerExecutor, error)
```

When `Signing.Enabled`:

```go
args = append(args,
    "-v", signing.AgentSock+":/ssh-agent.sock",
    "-e", "SSH_AUTH_SOCK=/ssh-agent.sock",
    "-e", "GIT_CONFIG_COUNT=3",
    "-e", "GIT_CONFIG_KEY_0=gpg.format",
    "-e", "GIT_CONFIG_VALUE_0=ssh",
    "-e", "GIT_CONFIG_KEY_1=user.signingkey",
    "-e", "GIT_CONFIG_VALUE_1=key::"+signing.PublicKey,
    "-e", "GIT_CONFIG_KEY_2=commit.gpgsign",
    "-e", "GIT_CONFIG_VALUE_2=true",
)
```

Why env injection, not a written `.gitconfig`:

- `GIT_CONFIG_{COUNT,KEY,VALUE}` is the documented "command" scope
  (git-config(1) ENVIRONMENT): overrides all config files, no file to
  create/clean in the persistent sandbox-home, no drift between sessions.
- The socket mounts at a fixed neutral path — host paths like
  `/run/user/1000/…` don't exist in the container namespace.
- `key::` literal means the container never sees a key *file* at all.

`Describe()` gains `+sign` so the active grant is always visible:
`docker[img → /path] +docker +sign`.

Note `user.name`/`user.email` are a separate concern — commits need them
too, but that's identity, not signing. Cheapest fix: two more
`GIT_CONFIG_*` pairs read from host `user.name`/`user.email` during the
same detection pass. Included in v1 since detection is already reading the
host config (brings the pair count to 5).

## DirectExecutor

No change — inherits host env and config; signing already works there.

## Dockerfile

Add `openssh-client` to the apt list. Needed for `ssh-keygen` (signing
backend) and as a bonus enables `git push` over SSH via the same agent.

## Verification (acceptance)

```sh
# inside the sandbox:
ssh-add -l                                   # agent reachable, key listed
git config get user.signingkey               # shows key::ssh-…  (scope: command)
git commit --allow-empty -m sign-test        # signs via commit.gpgsign=true
git cat-file commit HEAD | grep -c SSH       # signature block present
```

GitHub-side: push a signed commit, check the **Verified** badge (server
verifies against the signing key registered on the account). Local
`git log --show-signature` additionally needs
`gpg.ssh.allowedSignersFile` — out of scope for v1.

## Security analysis

| Option | Private key exposure | Verdict |
|---|---|---|
| **Agent socket + `key::` literal (this)** | Key never in container; container can *request* signatures while mounted | ✅ chosen |
| Bind-mount private key | Readable by any sandboxed process | ❌ tier-1 violation |
| Copy key into sandbox-home | Persists on disk across sessions | ❌ |
| Host-side signing proxy | Cleanest, but requires command interception on the host | ⏸ overkill |

Residual risk, stated honestly: while mounted, anything in the sandbox can
ask the agent to sign arbitrary data with **any** key the agent holds —
including SSH authentication, not just commits. It cannot extract keys.
Mitigations (documented for the user):

1. `ssh-add -c` on the host — per-signature confirmation prompt. Best fit
   for a notebook with a desktop session.
2. A dedicated agent holding only the signing key (future
   `ssh_agent_sock` override if wanted).
3. Default is `"off"`; the grant is visible in `Describe()` every session.

## Resolved decisions

1. `commit.gpgsign`: **mirror the host-effective per-repo value** (user
   decision, 2026-07-04 — revised from always-true). Signing activates
   only where the host git config says so; no `-S` needed in signing
   repos because `commit.gpgsign=true` signs every commit automatically
   (git-config(1)). The agent socket + `gpg.format` + `user.signingkey`
   env pairs are still injected whenever `ssh_signing` is enabled, so a
   manual `git commit -S` also works in repos that don't auto-sign.
   Fail-mode in signing repos is unchanged: if the agent is unreachable
   at commit time, git aborts the commit loudly rather than committing
   unsigned.

## Open questions

1. `gpg.ssh.defaultKeyCommand` support in auto-detection — run the host
   command and consume its `key::` output? Deferred; skip-with-reason in v1.
2. Stale socket after host agent restart: bind mounts pin the inode, so a
   restarted agent means a dead socket until the sandbox container is
   recreated. Accept for v1 (document "restart wakil"), or mount the
   socket's parent directory instead? Directory-mount is fragile
   (`/run/user/1000` contains much more than the agent) — proposal: accept
   and document.
