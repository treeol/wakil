# Wakil

<p align="center">
  <img src="assets/wakil-braille.png" alt="Wakil" width="280">
</p>

Wakil is a terminal coding assistant. It reads and edits your code, runs your commands, and keeps memory between sessions — with the tool calls running in a sandbox and asks-first safeguards on the gates.

[![CI](https://github.com/treeol/wakil/actions/workflows/ci.yml/badge.svg?branch=master)](https://github.com/treeol/wakil/actions/workflows/ci.yml)
[![govulncheck](https://github.com/treeol/wakil/actions/workflows/govulncheck.yml/badge.svg?branch=master)](https://github.com/treeol/wakil/actions/workflows/govulncheck.yml)
[![Release](https://img.shields.io/github/v/release/treeol/wakil)](https://github.com/treeol/wakil/releases)
[![License](https://img.shields.io/github/license/treeol/wakil)](LICENSE)
[![Go version](https://img.shields.io/github/go-mod/go-version/treeol/wakil)](go.mod)
[![Release](https://img.shields.io/github/actions/workflow/status/treeol/wakil/release.yml?label=release%20workflow)](https://github.com/treeol/wakil/actions/workflows/release.yml)

## Install

Quick install (Linux and macOS, amd64 and arm64):

```sh
curl -fsSL https://github.com/treeol/wakil/releases/latest/download/install.sh | sh
```

The script installs the latest release into `~/.local/bin`, verifies its checksum before extracting anything, and runs `wakil init` if no config exists. To read the script first:

```sh
curl -fsSL https://github.com/treeol/wakil/releases/latest/download/install.sh -o install.sh && sh install.sh
```

Manual install: take a `.tar.gz`, `.deb`, `.rpm` or `.apk` from the [releases page](https://github.com/treeol/wakil/releases), or build with Go:

```sh
go install github.com/treeol/wakil/cmd/wakil@latest
```

Then install [hisn](https://github.com/treeol/hisn) 0.6.7 or newer (`hisn check` must pass), or have Docker available. Wakil uses hisn when it is usable, otherwise Docker; `--sandbox direct` runs with no sandbox. Hisn 0.6.7 is the minimum because it gates signing by seat role (owner/member sign, observer refused); older hisn is refused at startup naming the found and required versions.

## First run

```sh
wakil init
```

It asks for a provider (OpenRouter, or any OpenAI-compatible server), a key, and a model, writes `~/.config/wakil/config.json`, and makes one test call. Then:

```sh
cd your-project && wakil
```

- `/new` starts a fresh conversation.
- `/help` lists every slash command.

## What you can do

- **Ask and edit.** Read files, write and edit code, search the codebase, and run shell commands — the model calls the tools, and Wakil executes them.
- **Run in the background.** Start long-running jobs with `run_background`, read their output with `read_process_log`, and stop them with `kill_process`.
- **Delegate with subagents.** `dispatch_subagent` spawns a bounded worker (read-only discovery, file-editing, or tools/MCP-capable) to search the codebase, run a build, or fan out parallel tasks.
- **Remember across sessions.** Durable memory entries and global skills persist per project and across projects; new sessions start with what earlier ones learned. `/remember` searches prior sessions, `/compact` shrinks the active context, and `/resume` returns to a saved conversation.
- **Check your project.** `/verify` runs the tests, lint, or build you configured; `wakil doctor` checks the container runtime, config and image.
- **Use the headless mode.** `wakil run` runs one task without the TUI and exits with a code. `/plan` walks a task through gather, plan, review and implement.
- **Browse and search.** SearXNG web search (`searxng_search`, `searxng_url_read`) when `searxng_url` is configured, and `browser_*` tools for headless-browser verification when Chromium is available.
- **Connect MCP servers.** List them under `mcp_servers` in the config; `/mcp` shows their tools.

## How it keeps you safe

- **Sandbox.** By default tools run in hisn (rootless) or Docker — not directly on your host. `--sandbox direct` opts out; the confirmation prompt stays on.
- **Approvals.** The shell, file-write and background-process tools ask for confirmation before running. `/auto` turns on auto-approval, but destructive commands and counsel calls still gate. Skill and template writes always ask.
- **Egress consent.** Sending session context or a Mashūra briefing to an external host asks first, once per session — even in `/auto` mode.
- **Data policy.** `/zdr` turns on OpenRouter zero-data-retention enforcement; `/region` pins OpenRouter traffic to a regional host. `trusted_local_hosts` treats listed host names as local.

## Second opinions (Mashūra)

Mashūra sends a review, a debugging question, or a decision to models other than the one you are talking to, so you get a second opinion on the same briefing. The models are configured in the `mashura` block (`level` plus `members`), and the model calls the `mashura__review`, `mashura__debug`, `mashura__decide` and `mashura__check` tools.

- `/mashura off|low|medium|high` changes the level for the current repo: `off` closes the gate, `low` suggests only, `medium` enables auto-counsel when the model struggles, `high` adds debate.
- Cost: each call is billed per-token by the members' rates; caps (`mashura_review_budget_usd`) stop a panel before it spends more than you set.

## Configuration and docs

The config file is JSON at `~/.config/wakil/config.json` (`WAKIL_CONFIG` or `--config` point elsewhere). All keys, flags and subcommands are in [docs/configuration.md](docs/configuration.md); [config.example.json](config.example.json) is a commented reference. The rest of the documentation is indexed in [docs/README.md](docs/README.md).

## Project status and versioning

Patch releases are cut from master with `vX.Y.Z` tags; [CHANGELOG.md](CHANGELOG.md) lists what each release changed, and the release page body is the matching section of that file. CI gates are in [CONTRIBUTING.md](CONTRIBUTING.md).

## License

Apache License 2.0. See [LICENSE](LICENSE).
