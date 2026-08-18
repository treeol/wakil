# Provisioning a Remote Host for Wakil

A repeatable runbook for bringing up wakil on a fresh remote host. Distilled
from a real "works on one host, silently broken on another" debug session —
every item below was a failure that surfaced on a second host after passing
clean on the first.

**Goal:** copy config + this checklist → fully working wakil in one pass.

## Prerequisites

- Go 1.26+ (build from source)
- Docker (for the default sandbox mode; skip with `--exec direct`)
- An OpenAI-compatible endpoint or ilm proxy to point at

## Checklist

### 1. Config file

Copy your existing config to the remote:

```sh
scp ~/.config/wakil/config.json remote:~/.config/wakil/config.json
```

If the remote has no config yet, wakil auto-creates a minimal template on
first run — edit it to add your endpoint.

### 2. Mashūra API key (ENV, not config)

The Mashūra counsel API key is read via `os.Getenv` at call time
(`internal/agent/mashura.go`), **never** from `config.json`
(`internal/config/config.go` — "The API key is read from OracleAPIKeyEnv
at call time, never stored here or in session files").

Export it on the remote and persist in your shell rc:

```sh
# Anthropic (default oracle_api_key_env)
export ANTHROPIC_API_KEY="sk-ant-..."
# And/or OpenRouter (for fusion panels)
export OPENROUTER_API_KEY="sk-or-..."
```

Add the `export` lines to `~/.bashrc` or `~/.zshrc` so they survive re-login.

**Symptom if missed:** pressing F2 → "mashura: no key".

### 3. Docker present and reachable

Install Docker, start the daemon, and add your user to the docker group:

```sh
# Install Docker (distro-specific — e.g. dnf install docker-ce on Fedora)
sudo systemctl start docker
sudo usermod -aG docker $USER
# Re-login for the group change to take effect
```

Verify:

```sh
docker info   # should print server info, no errors
```

### 4. Build the wakil-dev sandbox image

The config copy does **not** bring the Docker image. Build it from the repo
on the remote:

```sh
cd /path/to/wakil
docker build -t wakil-dev .
```

Wakil's preflight (`internal/exec/exec.go`, `checkDockerImage`) checks for
the image at startup and fails with a clear message if it's missing.

### 5. SELinux hosts (Fedora / RHEL-family)

On SELinux-Enforcing hosts, wakil's sandbox container cannot reach the
bind-mounted host Docker socket. DAC permissions are correct
(`--group-add <socket-gid>` is already applied), but SELinux MAC denies
the `connectto` operation:

```
avc: denied { connectto }
    scontext=system_u:system_r:container_t:s0:cNNN,cNNN
    tcontext=system_u:system_r:container_runtime_t:s0
    tclass=unix_stream_socket  permissive=0
```

This is **not** a `:z` issue and **not** a GID issue — it's host-side SELinux
type-enforcement. There is no clean fix in wakil's `docker run` args.

**Fix — check for an existing boolean first:**

```sh
getsebool -a | grep -iE "container.*(connect|docker|virt)"
sesearch -A -s container_t -t container_runtime_t -c unix_stream_socket -p connectto
```

**Otherwise, generate a targeted policy module:**

```sh
sudo ausearch -m avc -ts recent | grep connectto | audit2allow -M wakil_dockersock
sudo semodule -i wakil_dockersock.pp
# This adds: allow container_t container_runtime_t:unix_stream_socket connectto;
```

**Fallback:** run wakil with `--exec direct` on SELinux hosts to skip the
sandbox entirely.

### 6. MCP server absolute paths

Any `mcp_servers` stdio entry with an absolute `command` path (e.g.
`/home/valon/mcp-trello/trello-mcp`) must exist and be executable on the
remote host. Wakil passes the command string directly to `exec.Command`
with no path resolution — if the binary isn't there, the server won't
start.

Additionally, any `.mcp.json` the server needs must be in wakil's launch
cwd or `$HOME`.

**Symptom if missed:** the server's own ".mcp.json not found" error (this
is the MCP server's error, not a wakil message).

## Verification

After completing all steps, verify on the remote:

```sh
# 1. Mashūra works (F2 in the TUI, or check the key is set)
echo "$ANTHROPIC_API_KEY"   # should be non-empty

# 2. Docker works inside the sandbox
docker info   # on the host — should succeed
# Inside wakil's sandbox, docker commands should also work
# (or use --exec direct on SELinux hosts)

# 3. MCP servers connect
# Start wakil and run /mcp — should list connected servers and their tools
```

## Related

- [SSH copy via OSC 52](tui.md#copying-text-over-ssh) — clipboard
  forwarding over SSH
- SELinux detection — wakil's preflight should detect and surface
  SELinux denials with actionable error messages
