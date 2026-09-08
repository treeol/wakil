# Wakil Telegram Confirmation Trigger — Design Plan

> Status: planning only (2026-09-08). No code written yet.

## Goal

When Wakil needs human input (approval gate), ping you on Telegram so you can
approve/decline from your phone — even when you're away from the keyboard and the
TUI is sitting at a confirm prompt waiting for you.

## Concept

Not a full Telegram bot frontend. Just a **confirmation trigger**: when the
`Confirmer` gate fires (the same gate that shows the TUI's y/n approval prompt),
it also sends a Telegram message with inline buttons. You tap a button on your
phone, the response feeds back into the agent, and the turn continues.

```
                    ┌─────────────────┐
                    │  Wakil TUI      │
                    │  (confirm prompt │
                    │   y/n blinking)  │
                    └────────┬────────┘
                             │
                     ┌───────▼────────┐
                     │  Confirmer gate │
                     │  (blocks agent  │
                     │   goroutine)    │
                     └───┬─────────┬───┘
                         │         │
                    ┌────▼───┐  ┌──▼──────────────┐
                    │  TUI   │  │  Telegram MCP   │
                    │  y/n   │  │  send_confirm    │
                    │ prompt │  │  → TG message    │
                    └────┬───┘  └──┬───────────────┘
                         │         │
                    first response wins
                         │         │
                    ┌────▼─────────▼───┐
                    │  ConfirmChoice   │
                    │  → agent resumes │
                    └─────────────────┘
```

## How the Confirmer works today (verified)

The `Confirmer` type (`internal/agent/app.go:34`):

```go
type Confirmer func(toolName, headline, detail string, readAction bool) bool
```

The TUI sets `app.Confirm = tuiConfirmer(app)` before each turn
(`internal/agent/tui_cmds.go:53`). When a tool needs approval, the agent goroutine
calls `app.Confirm(...)` which **blocks the agent goroutine** until the user
responds:

1. `tuiConfirmer` posts a `ConfirmReqMsg{ToolName, Headline, Detail, ReadAction, RespCh}`
   into the TUI event loop (`internal/agent/commands.go:130`)
2. Blocks on `<-ch` (a buffered(1) channel)
3. The TUI's `handleKey` answers by sending a `ConfirmChoice` into `ch`
   (`internal/tui/tui.go:822-850`)
4. The confirmer returns `true` (approve) or `false` (decline)

Key insight: **the confirmer is a blocking function on the agent goroutine**.
Anything that can feed a `ConfirmChoice` into the channel — TUI keypress, Telegram
button press, a timer — can unblock the agent.

## Design: MCP tool as Telegram bridge

### Why MCP?

The user suggested implementing it as an MCP tool. This is clean because:
- **No wakil core changes** — the confirmer already exists; we just wrap it to also
  call an MCP tool
- **Pluggable** — enabled/disabled via MCP server config in `wakil.yaml`; no
  process changes when it's not configured
- **Secret isolation** — the Telegram bot token stays in the MCP server's env, not
  in wakil's config
- **Separate binary** — the Telegram bridge is a standalone process (stdio
  transport), not embedded in wakil

### The MCP server

A standalone binary (`telegram-bridge`) that:
1. Runs a Telegram bot long-polling loop (bot token from env)
2. Exposes one MCP tool: `send_confirmation`
3. When called with `(tool_name, headline, detail, read_action)`:
   - Sends a Telegram message to the allowlisted chat with the approval details
   - Attaches inline keyboard buttons: ✅ Approve / ❌ Decline (and optionally
     📖 Allow reads for read-only actions)
   - Blocks on the callback until the user taps a button (or timeout)
   - Returns the choice as the tool result

### The composite confirmer

A new `Confirmer` that races two channels:

```go
func telegramCompositeConfirmer(app *App, tgConfirm Confirmer) Confirmer {
    return func(toolName, headline, detail string, readAction bool) bool {
        // Fast path: if auto-approve handles it, skip Telegram entirely
        if app.Consent().AutoApprove {
            reason := SuspendAuto(toolName, app, detail)
            if reason == "" {
                app.sendEvent(SysNoteMsg{Text: "⚡ auto: " + headline})
                return true
            }
        }

        // Fire both the TUI prompt and the Telegram notification in parallel.
        // First response wins; cancel the other.
        tuiCh := make(chan ConfirmChoice, 1)
        app.sendEvent(ConfirmReqMsg{
            ToolName:   toolName,
            Headline:   headline,
            Detail:     detail,
            ReadAction:  readAction,
            RespCh:      tuiCh,
        })

        tgCh := make(chan ConfirmChoice, 1)
        go func() {
            result := tgConfirm(toolName, headline, detail, readAction)
            if result {
                tgCh <- ChoiceApprove
            } else {
                tgCh <- ChoiceDecline
            }
        }()

        select {
        case choice := <-tuiCh:
            return choice != ChoiceDecline
        case choice := <-tgCh:
            // Telegram responded first — cancel the TUI prompt
            // (the TUI's pending approval is cleared by the event system)
            return choice != ChoiceDecline
        case <-time.After(10 * time.Minute):
            app.sendEvent(SysNoteMsg{Text: "⏱ approval timed out (10 min)"})
            return false
        }
    }
}
```

**Wait — this is more complex than needed.** The TUI confirmer already posts a
`ConfirmReqMsg` and blocks on a channel. If we can feed the same channel from
Telegram, we don't need a composite at all — we just need a second event source
that writes to the same `RespCh`.

### Simpler approach: Telegram writes to the same channel

The `ConfirmReqMsg` carries a `RespCh chan ConfirmChoice`. The TUI event loop
writes to it on keypress. If a Telegram sidecar can also write to that channel,
the first writer wins and the agent resumes — no composite confirmer needed.

But the `RespCh` is created by `tuiConfirmer` inside the agent goroutine and sent
to the TUI via `sendEvent`. The Telegram sidecar doesn't have access to it.

**Two clean approaches:**

**A) Wrap the confirmer (composite, above).** The Telegram confirmer calls the MCP
tool. The composite races both. First wins. Clean but requires a new confirmer
type in the wiring layer.

**B) Event-based (RespondToApproval).** The event system already has
`ApprovalRequested` with an `ApprovalID` and `RespondToApproval` on the host
(`internal/core/service.go:329`). A Telegram sidecar subscribes to the event stream,
sees `ApprovalRequested`, sends a Telegram message, and on button press calls
`facade.RespondToApproval(ctx, principal, ApprovalDecision{ApprovalID, Outcome})`.
The host resolves the approval → the TUI's `ConfirmReqMsg` is answered via the
existing shim. **No confirmer changes at all** — the Telegram sidecar is purely
an event-stream consumer + approval responder.

**B is simpler.** It requires:
- No changes to the confirmer
- No changes to the agent goroutine
- Just a Telegram bot that subscribes to events and responds to approvals
- The existing `RespondToApproval` is non-blocking and idempotent (duplicate
  decisions are tolerated)

### Chosen approach: B (event-based Telegram sidecar)

```
┌─────────────────────────────────────────────────────┐
│  Wakil Process (TUI)                                 │
│                                                      │
│  agent goroutine ──► Confirmer ──► ConfirmReqMsg     │
│                                      │               │
│                            event stream              │
│                                      │               │
│  event pump ──► TUI event loop      │               │
│  event pump ──► Telegram sidecar ───┘               │
│                      │                               │
│                      │ ApprovalRequested event       │
│                      │ → send Telegram message       │
│                      │ → inline buttons              │
│                      │                               │
│                      │ button press                  │
│                      │ → RespondToApproval()          │
│                      │ → host resolves approval       │
│                      │ → TUI's ConfirmReqMsg answered │
│                      │ → agent goroutine resumes     │
│ └────────────────────────────────────────────────────┘
```

The Telegram sidecar runs **inside the wakil process** as a second event-pump
consumer (like the TUI's event pump, but routing to Telegram instead of the
screen). It subscribes to the same event stream the TUI does.

Wait — but the user said "even thought only to implement it as mcp tool." So they
want it as an MCP tool, not as an in-process event consumer. Let me reconcile:

### MCP tool approach (user's suggestion)

The Telegram bridge is an **MCP server** (separate process, stdio transport). It
exposes a tool `send_confirmation` that the **confirmer** calls (not the agent).

The flow:
1. The agent goroutine hits the confirmer gate
2. The confirmer (a new `telegramConfirmer`) calls the MCP tool
   `telegram-bridge.send_confirmation(tool_name, headline, detail, read_action)`
3. The MCP server sends a Telegram message with inline buttons
4. The MCP server blocks on the button press (long-polling Telegram)
5. The user taps a button → the MCP server returns the result
6. The confirmer returns the result → the agent goroutine resumes

**But who calls the MCP tool?** The confirmer is in the agent package and has
access to `app.MCP` (the MCP manager). So:

```go
func telegramConfirmer(app *App) Confirmer {
    return func(toolName, headline, detail string, readAction bool) bool {
        // Auto-approve / SuspendAuto logic stays the same
        if app.Consent().AutoApprove {
            reason := SuspendAuto(toolName, app, detail)
            if reason == "" {
                app.sendEvent(SysNoteMsg{Text: "⚡ auto: " + headline})
                return true
            }
        }

        // Call the Telegram MCP tool — it blocks until the user responds
        args, _ := json.Marshal(map[string]any{
            "tool_name":   toolName,
            "headline":     headline,
            "detail":      detail,
            "read_action": readAction,
        })
        result := app.MCP.CallTool(ctx, "telegram-bridge.send_confirmation",
            string(args), app.Confirm, false)

        // Parse result → bool
        return strings.Contains(result, "approved")
    }
}
```

**Problem:** this replaces the TUI confirmer entirely. When at the keyboard,
you'd want the TUI prompt, not Telegram. So it needs to be a **composite** that
races both:

1. Fire the TUI `ConfirmReqMsg` AND the MCP `send_confirmation` call in parallel
2. First response wins
3. Cancel the other

This is the composite confirmer from approach A above, but the Telegram half is
implemented as an MCP tool call rather than an in-process event consumer.

### Hybrid: MCP server + composite confirmer (recommended)

**MCP server** (`telegram-bridge` binary):
- Long-polls Telegram Bot API
- Exposes `send_confirmation(tool_name, headline, detail, read_action) → result`
- Sends inline keyboard message, blocks on callback, returns result
- Configured in `wakil.yaml` as an MCP server (stdio transport)
- Token + allowed user IDs in the MCP server's env

**Composite confirmer** (in `internal/wiring/` or `internal/agent/`):
- Wraps `tuiConfirmer` + calls `telegram-bridge.send_confirmation` in parallel
- First response wins
- Timeout after N minutes (configurable; default 10)
- When the Telegram MCP server is not configured, falls back to `tuiConfirmer`
  alone (zero behavior change)

**This is the cleanest approach because:**
- The Telegram bot is a separate binary (MCP server via stdio)
- No wakil core changes — the composite confirmer is a wiring-layer concern
- Pluggable: no MCP server configured → no Telegram, no behavior change
- The TUI still shows the prompt (you can respond from keyboard or phone)
- The bot token stays in the MCP server's env

## How the composite confirmer works

```go
// telegramCompositeConfirmer wraps tuiConfirmer and adds a Telegram
// notification via an MCP tool. First response (TUI or Telegram) wins.
// When the Telegram MCP server is not configured, falls back to tuiConfirmer.
func telegramCompositeConfirmer(app *App, tuiConf Confirmer, tgMCPName string) Confirmer {
    return func(toolName, headline, detail string, readAction bool) bool {
        // Auto-approve + SuspendAuto logic (unchanged from tuiConfirmer)
        // ... (reuse existing logic) ...

        // Race TUI + Telegram
        tuiCh := make(chan ConfirmChoice, 1)
        // ... fire tuiConf asynchronously, sending result to tuiCh ...

        tgCh := make(chan bool, 1)
        // ... fire MCP call asynchronously, sending result to tgCh ...

        select {
        case choice := <-tuiCh:
            return choice != ChoiceDecline
        case approved := <-tgCh:
            return approved
        case <-time.After(approvalTimeout):
            return false
        }
    }
}
```

**Wiring:** in `internal/wiring/bootstrap_tui.go`, after constructing the facade,
check if the `telegram-bridge` MCP server is configured. If so, wrap the confirmer.
If not, use `tuiConfirmer` as-is. Zero behavior change when Telegram is not
configured.

## Telegram message format

```
🔧 Wakil needs approval

Tool: run_shell
Action: Run shell command?
Detail: $ rm -rf /tmp/build-cache

[✅ Approve]  [❌ Decline]
```

For read-only actions:
```
Tool: read_file
Action: Read file?
Detail: /mnt/wakil/internal/agent/app.go

[✅ Approve]  [📖 Allow reads]  [❌ Decline]
```

`callback_data` = `approve` or `decline` or `allow_reads` (fits easily in 64 bytes).

## Security

1. **Allowed Telegram user IDs:** configured in the MCP server's env
   (`TELEGRAM_ALLOWED_USERS=123456789,987654321`). Numeric IDs, not usernames
   (usernames are reassignable). Any callback from an unallowlisted user is
   ignored.

2. **Bot token:** `TELEGRAM_BOT_TOKEN` env var on the MCP server process. Never
   logged, never passed to wakil core.

3. **Timeout:** default 10 min. If the user doesn't respond, the confirmer
   auto-declines and the agent reports "approval timed out." Configurable via
   env (`TELEGRAM_APPROVAL_TIMEOUT_MINUTES=10`).

4. **Destructive commands:** always require approval — the SuspendAuto carve-outs
   (destructive, external backend) fire before the composite, so Telegram
   notification only happens for approvals that would have blocked the TUI too.

## Files to create

```
telegram-bridge/                   # standalone MCP server binary
  main.go                          # entry point: parse env, start bot + MCP server
  bot.go                           # Telegram Bot API client, long-polling
  confirm.go                       # send_confirmation MCP tool handler
  config.go                        # env parsing (token, allowed users, timeout)
  go.mod                           # standalone module (deps: mcp-go, telegram bot lib)

internal/wiring/
  telegram_confirmer.go            # composite confirmer (tuiConfirmer + Telegram MCP)
```

## Files to modify

- `internal/wiring/bootstrap_tui.go` — after facade construction, check for
  `telegram-bridge` MCP server and wrap the confirmer
- `wakil.yaml` (or config docs) — document the `telegram-bridge` MCP server
  configuration

## What does NOT change

- `internal/core/` — untouched (the event system, facade, and core types are
  unchanged)
- `internal/agent/` — untouched (the Confirmer type, tuiConfirmer, and the
  approval flow are unchanged; the composite is a wiring-layer wrapper)
- `internal/tui/` — untouched (the TUI still shows the prompt; it just might
  get answered from Telegram before the user presses y/n)
- `cmd/wakil/main.go` — untouched

## Open questions

1. **Telegram library:** `go-telegram/bot` (v6, typed) or
   `go-telegram-bot-api/telegram-bot-api` (widely used). Check both for
   long-polling, inline keyboards, and callback handling.

2. **MCP SDK:** Wakil uses `github.com/treeol/wakil/internal/agent/gosdkmcp`
   (the MCP Go SDK). The telegram-bridge would use the same SDK to expose
   its tool. Check the SDK's server-side API.

3. **TUI prompt cleanup when Telegram wins:** if the Telegram button is pressed
   first, the TUI's confirm prompt is still showing. The composite confirmer
   needs to clear it (post a `ConfirmReqMsg` response or use
   `RespondToApproval`). Need to verify the TUI clears the prompt cleanly when
   the approval is resolved externally.

4. **Approval timeout:** 10 min default? Configurable? The agent goroutine
   blocks until answered — a long timeout means a hung session if the user
   is away from both phone and keyboard.

5. **Multiple pending approvals:** if the agent fires several approvals in
   rapid succession (unlikely but possible with parallel subagent blocks),
   each gets its own Telegram message. The callback_data needs to identify
   which approval — use a short UUID or the ApprovalID.

6. **Should the composite confirmer or the event-based approach be used?**
   The composite confirmer (MCP tool) is the user's suggestion and keeps
   the Telegram logic in a separate binary. The event-based approach (B)
   is simpler to implement but runs the Telegram bot inside the wakil
   process. The MCP approach is recommended for separation of concerns.
