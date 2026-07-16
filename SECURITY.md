# Security Policy

## Threat model

Wakil is a terminal coding agent that executes shell commands, reads/writes
files, and optionally runs inside a Docker sandbox. The primary attack
surface is **tool execution**: the model can request arbitrary shell
commands, file writes, and background processes.

### Trust boundaries

| Boundary | Risk | Mitigation |
|---|---|---|
| **Shell execution** | The model can run arbitrary commands | Per-call `y/n` confirmation gate; destructive commands gated even in auto mode |
| **File access** | Read/write within the workspace | Path confinement (all paths resolved and checked against workspace root); write/edit/delete gated |
| **Docker socket** | Host-root-equivalent if mounted | Opt-in only (`docker_socket: true`, defaults to `false`) |
| **Memory injection** | `memory_put` is ungated; poisoned tool results could write instruction-shaped entries | Taint signal on entries from sessions touching external content; mid-tier TTL auto-expires |

### Sandbox classification

- **Default Docker mode** applies basic hardening: `--cap-drop=ALL`,
  `--security-opt=no-new-privileges`, `--read-only` rootfs, resource limits,
  and writable tmpfs for `/tmp` and `/etc`. This is convenience-grade
  isolation — it prevents accidental damage and raises the bar for casual
  escapes, but is **not** adversarial-grade. Seccomp/AppArmor profiles are
  not applied. Optional host integrations (Docker socket, SSH signing socket)
  materially weaken isolation when enabled.
- **Direct mode** (`--exec direct`) runs on the host with no container
  isolation. The confirmation gate is the only defense.

Even hardened, the sandbox is **not** a substitute for the confirmation gate
when running untrusted tasks.

### Docker socket

The host Docker socket (`/var/run/docker.sock`) is **not** bind-mounted by
default. Enabling `docker_socket: true` (or `--docker-sock`) gives the agent
full access to the host Docker daemon — this is **host-root-equivalent**.
Only enable when you need the agent to run `docker` / `docker compose`
commands against your real daemon, and treat it with the same caution as
root access.

### Seccomp / AppArmor

Seccomp and AppArmor profiles are **not** currently applied. Adding a
seccomp profile that blocks `mount`, `pivot_root`, `reboot`, and other
container-escape syscalls is planned future work. The current hardening
(`--cap-drop=ALL`, `--read-only`, `--security-opt=no-new-privileges`)
provides defense-in-depth but is not a complete container isolation solution.

### Hardening flags

The following flags are always applied in Docker mode:

| Flag | Purpose |
|---|---|
| `--cap-drop=ALL` | Drop all Linux capabilities |
| `--security-opt=no-new-privileges` | Prevent privilege escalation |
| `--read-only` | Read-only root filesystem |
| `--tmpfs=/tmp` | Writable temp directory (100 MB) |
| `--tmpfs=/etc` | Writable /etc for passwd entries (1 MB) |

Configurable via `config.json`:

| Field | Default | Purpose |
|---|---|---|
| `docker_caps` | `[]` (none) | Capabilities to re-add after cap-drop (e.g. `["CHOWN"]` if `go build` fails) |
| `docker_memory` | `"2g"` | Container memory limit |
| `docker_pids_limit` | `512` | Max processes in the container |

## Disclosure

If you discover a security vulnerability in wakil, please report it
responsibly:

1. **Do not** open a public GitHub issue.
2. Open a [GitHub Private Security Advisory](https://github.com/treeol/wakil/security/advisories/new),
   or email `security@treeol.dev`.
3. Include a proof of concept and affected versions.
4. Allow reasonable time for a fix before public disclosure.

## Hardening checklist for untrusted tasks

- [ ] Keep the confirmation gate **on** (do not use `--auto` unattended)
- [ ] Use `--exec direct` in a disposable VM, or hardened Docker mode
- [ ] Do **not** enable `docker_socket` unless you need Docker access
- [ ] Audit memory entries (`memory_list`) after operating on untrusted content
- [ ] Run against an endpoint and model you trust
