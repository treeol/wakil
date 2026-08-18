# The TUI

Anything typed that isn't a slash command goes to the agent as a task. `@`
opens a picker to attach a file or folder for context.

## Commands

**Session**

```
/new, /reset         fresh conversation (new chat_id, clears viewport)
/handoff             summarize session → store in memory → start fresh session with continuation prompt
/compact             summarize older turns now (frees context)
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
/mode                show execution backend
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
/help                full command list
```

See also the [`/plan` workflow](workflows.md) documentation.

## Keybindings

| Key | Action |
|---|---|
| `Enter` | Send input *(Shift+Enter for newline)* |
| `↑` / `↓` | Browse command history *(previous / next)* |
| `Ctrl+R` | Reverse incremental search through command history |
| `Ctrl+E` | Expand/collapse live reasoning while the model is thinking |
| `Ctrl+C` | Cancel in-flight turn *(press twice to force-quit)* |
| `Esc` | Cancel in-flight turn |
| `Ctrl+D` | Quit *(when idle)* |
| `y` / `n` | Approve / decline a pending tool call |
| `a` | Allow all read-only calls for this session |
| `@` | Attach a file or folder |

## Copying text over SSH

Mouse-drag to select text in the conversation pane, then release — wakil copies
the selection to the clipboard. On a **remote host over SSH** (no local
clipboard daemon), it falls back to the **OSC 52** terminal escape sequence,
which forwards the text through the terminal to your *local* clipboard.

If copy-over-SSH doesn't work, the flash message will say
`sent N chars via OSC 52 — if paste is empty, enable terminal/tmux clipboard`.
That means OSC 52 was used but your terminal or tmux is blocking it. Enable it:

**tmux** (local or remote): add to `~/.tmux.conf`:

```
set -g set-clipboard on
```

Depending on your tmux version, `set -g set-clipboard external` may be needed
instead — check `man tmux` for your installed version. Nested tmux (local +
remote) requires the setting on **both**.

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
