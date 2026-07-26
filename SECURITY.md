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
  and writable tmpfs for `/tmp` and `/etc`. Docker's default seccomp profile
  is applied by the daemon (blocks `io_uring_setup`, `keyctl`, `bpf`, etc.).
  This is convenience-grade isolation — it prevents accidental damage and
  raises the bar for casual escapes, but is **not** adversarial-grade.
  Optional host integrations (Docker socket, SSH signing socket, io_uring)
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

### Root in the sandbox

The sandbox container runs as **root by design**. This is an intentional
tradeoff, not an oversight:

- **`/etc/passwd` management**: wakil creates entries for workspace users
  (`ensurePasswdEntry`) so file ownership maps correctly across the host ↔
  container boundary. This requires write access to `/etc/passwd`.
- **Docker-in-Docker**: the sandbox runs `docker exec` / `docker cp` against
  child containers for shell execution, file I/O, and LSP servers. The Docker
  CLI requires root (or the `docker` group, which is root-equivalent on most
  systems).
- **Process management**: wakil kills background processes by process-group ID
  (`KillPgid`), which requires `CAP_KILL`.
- **System tool installation**: the sandbox installs gopls, language servers,
  and other tools into `/usr/local/bin` at build time.

A non-root user would require granting `CAP_SYS_ADMIN`, `CAP_KILL`,
`CAP_SETUID`, and `CAP_SETGID` — capabilities that are effectively
root-equivalent for this use case. Instead, the sandbox relies on Docker's own
isolation (`--cap-drop=ALL` on child containers, `--read-only` rootfs,
`--tmpfs=/etc`) to prevent escalation from the sandbox to the host. The root
user inside the sandbox cannot escalate to the host root because the sandbox
container itself runs with `--cap-drop=ALL` and `--security-opt=no-new-privileges`.

### Seccomp

Docker's **default seccomp profile** is applied by the daemon in Docker mode.
It blocks `io_uring_setup`/`io_uring_enter`/`io_uring_register`, `keyctl`,
`bpf`, `mount`, `pivot_root`, `reboot`, and other container-escape syscalls.

The `docker_io_uring` config option (default: `false`, Docker-only) replaces
the default profile with a custom one derived from moby's default
([profiles/seccomp/default.json](https://github.com/moby/moby/blob/v28.0.0/profiles/seccomp/default.json),
pinned at moby v28.0.0) with the three io_uring syscalls added to the allow
list. All other baseline denials remain in effect. `seccomp=unconfined` is
**never** used by this feature.

**Security implications of `docker_io_uring: true`:**
- io_uring is a large kernel subsystem with a history of CVEs (Google,
  ChromeOS, and GKE disable it by default).
- Operations submitted through the ring bypass seccomp per-op filtering —
  `openat`, `read`, `write`, `connect`, etc. executed via SQEs are not
  individually mediated by the filter.
- `--cap-drop=ALL` does not mitigate io_uring's kernel attack surface.
- The opt-in is container-wide: everything the agent runs via `docker exec`
  inherits the relaxed profile.
- SQPOLL rings require `CAP_SYS_NICE` (dropped by `--cap-drop=ALL`), so
  this feature guarantees non-SQPOLL io_uring only.
- Success depends on the host kernel, Docker runtime, and
  `kernel.io_uring_disabled` sysctl (0 = allowed; 1 = CAP_SYS_ADMIN only;
  2 = fully disabled).

A `+iouring` badge appears in `Describe()` and a warning is printed to stderr
at startup when enabled.

AppArmor is not explicitly configured by wakil; its application depends on
the host's Docker/AppArmor setup.

### Hardening flags

The following flags are always applied in Docker mode:

| Flag | Purpose |
|---|---|
| `--cap-drop=ALL` | Drop all Linux capabilities |
| `--security-opt=no-new-privileges` | Prevent privilege escalation |
| `--read-only` | Read-only root filesystem |
| `--tmpfs=/tmp` | Writable temp directory (default 4g, configurable via `docker_tmpfs_size`) |
| `--tmpfs=/etc` | Writable /etc for passwd entries (1 MB) |

Configurable via `config.json`:

| Field | Default | Purpose |
|---|---|---|
| `docker_caps` | `[]` (none) | Capabilities to re-add after cap-drop (e.g. `["CHOWN"]` if `go build` fails) |
| `docker_memory` | `"4g"` | Container memory limit |
| `docker_pids_limit` | `512` | Max processes in the container |
| `docker_tmpfs_size` | `""` (→ 4g) | /tmp tmpfs size override |
| `docker_io_uring` | `false` | Enable io_uring via custom seccomp profile (increases kernel attack surface; see [Seccomp](#seccomp)) |

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
