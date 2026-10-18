# The TUI

Anything typed that isn't a slash command goes to the agent as a task. `@`
opens a picker to attach a file or folder for context.

## Commands

**Session**

```
/new, /reset         fresh conversation (new chat_id, clears viewport)
/handoff [proceed|stop] summarize → memory → fresh session (default: show and wait; proceed: auto-continue)
/compact             summarize older turns now (frees context); in long-session mode requires /compact force (a forced compact materializes collapsed requests and destroys their archive pointers)
/rewind              list checkpoints (turn-level file snapshots)
/rewind <N>          rewind N checkpoints (1 = last turn; restores files, truncates history)
/sessions            list saved sessions (★ = current)
/history             transcript size
/quit, /exit         leave (tears down the container)
```

**Workflow**

```
/plan <task>         start a gather→plan→review→implement workflow for <task>
/plan --oracle=MODE  set per-run review schedule (every-step|on-deviation|phases-only)
/plan status         show current workflow phase and step
/plan approve        approve the plan; force-skip review (logged); advance past pauses
/plan review         retry the counsel plan review (when review is pending/unavailable)
/plan verify         re-run the final review (in verify state after gaps flagged)
/plan abort          cancel the active workflow
```

**Executor and tools**

```
/cwd                 show executor working directory
/mode                show execution backend · context mode
/mode standard       set context mode: full history sent (default; persisted per repo)
/mode long-session   set long-session mode: finished requests collapse to question + answer + note (alias: /mode long)
/mode status         show context mode, collapsed/kept counts, sent vs full sizes, archive dir
/keep <n>            exempt request n from collapse; /keep off <n> removes the exemption
/mcp                 list connected MCP servers and their tools
/mcp reconnect NAME  reconnect a named MCP server
```

**Endpoint and model**

```
/backend             ilm-proxy: show backend selection · openai: list configured endpoints
/backend <name>      ilm-proxy: set proxy backend · openai: switch to named endpoint
/model <name>        switch model (re-resolves context limits); tab-completes from the server's model list
```

**Meta**

```
/learn               ask the proxy to synthesise a fact to remember (ilm-proxy endpoints only —
                     refuses client-side on openai endpoints instead of faking success)
/auto                toggle auto-approve (shown as AUTO in status bar)
/rawtools            toggle full tool output in context (default: capped at 8k chars)
/maxctx <chars>      cap effective context for large models (e.g. 200000 = ~200k chars; 0 = disabled)
/maxctx              show current effective context cap and resulting compaction thresholds
/init                detect project conventions and create AGENTS.md if absent
/review [ref]        review current diff (or diff vs ref) for correctness, tests, security, style
/skill-create <topic> research a topic via a subagent and save it as a skill (asks for confirmation)
/repomap             rebuild and show the repo map (lightweight file-tree outline)
/rewind              list checkpoints or rewind N turns (see Session section above)
/info                toggle the info panel (proxy/model/exec/cwd/costs/grounding)
/help                full command list
```

See also the [`/plan` workflow](workflows.md) documentation.

**Lifecycle hooks**

Hooks are configured in the config file, not via TUI commands. See [features.md](features.md#lifecycle-hooks) for configuration.

## Keybindings

| Key | Action |
|---|---|
| `Enter` | Send input *(Shift+Enter for newline)* |
| `↑` / `↓` | Browse command history *(previous / next)* |
| `Ctrl+R` | Reverse incremental search through command history |
| `Ctrl+E` | Expand/collapse live reasoning while the model is thinking |
| `Ctrl+Y` | Copy the last assistant response (raw source text) to the clipboard. Mid-stream this copies the previous *completed* response, not the in-flight one |
| `Alt+T` | Toggle terminal-selection mode: hands the mouse back to the terminal so its native click-drag selection works (status shows `select:terminal`; press again to restore wakil's mouse handling) |
| `Ctrl+C` | Cancel in-flight turn *(press twice to force-quit)* |
| `Esc` | Cancel in-flight turn |
| `Ctrl+D` | Quit *(when idle)* |
| `y` / `n` | Approve / decline a pending tool call |
| `a` | Allow all read-only calls for this session |
| `@` | Attach a file or folder |

## Copying text

Three ways to copy out of the TUI:

1. **Drag-select in the conversation pane** (mouse) — release copies the
   selection. Copy failures are reported honestly in the status line, with
   the transport used (native tool name, or OSC 52 "unverified").
2. **`Ctrl+Y`** — copies the last assistant response's *raw source* text
   (pre-rendering, blank lines preserved). No mouse needed. A whitespace-only
   response is a no-op (nothing to copy).
3. **`Alt+T` terminal-selection mode** — disables wakil's mouse handling so
   your terminal's own click-drag selection works (includes tmux copy-mode
   users). The status line shows `select:terminal` while active; `Alt+T`
   again restores mouse handling.

The transport follows `clipboard_mode` in the config (`auto` by default:
native tools first, then OSC 52).

### If OSC 52 paste is empty

Over **SSH** (no local clipboard daemon), wakil falls back to the **OSC 52**
terminal escape sequence, which forwards the text through the terminal to
your *local* clipboard. The status hint says "unverified" because the app
cannot know whether the sequence was honored. Enable it:

**tmux** (local or remote): add to `~/.tmux.conf`:

```
set -g set-clipboard on
```

If your tmux version doesn't support `on`, check `man tmux` for the
`set-clipboard` values your version accepts (older versions differ). Nested
tmux (local + remote) requires the setting on **both**.

**Terminal emulators** — OSC 52 clipboard writes are disabled by default in some
terminals. Check your terminal's documentation for how to enable it:

| Terminal | OSC 52 support |
|---|---|
| iTerm2, Alacritty, Kitty, WezTerm | enabled by default |
| xterm | requires `allowWindowOps` or enabling Set/GetSelection in `disallowedWindowOps` — see `man xterm` |
| GNOME Terminal / VTE | recent versions enable it; older VTE blocked it entirely |
| Konsole | check Konsole settings (OSC 52 support was added in a recent release) |

Native clipboard commands (`wl-copy`, `xclip`, `xsel`, `pbcopy`) copy to the
environment where wakil is running. In SSH sessions, OSC 52 is what targets
your local terminal's clipboard — if a native command is found on the remote but
fails (e.g. `xclip` without `DISPLAY`), wakil falls back to OSC 52.
