// Package sessionclient defines the agent-free facade contract the TUI consumes
// (card #148 chunk 7b1, plan D26).
//
// It is the client-facing interface between internal/tui and the session host +
// agent loop. The TUI imports this package and event/proxy/core leaf types ONLY
// — never internal/agent. The implementation lives in internal/wiring (7b3).
//
// Design (plan D26):
//   - The interface lives here, not in internal/wiring, so tui → wiring → agent
//     is never a transitive import leak that launders agent types through wiring's
//     API surface. sessionclient imports only event, proxy, and core; wiring
//     implements.
//   - The TUI owns a view-model (ClientSnapshot) — an immutable, version-stamped
//     snapshot of session state — not live getters. Consent is the one live-read
//     exception (it can change mid-turn via a deferred grant).
//   - Turn-driving goes through SessionService (SubmitInput/Interrupt/
//     RespondToApproval); the TUI observes via EventReader (Subscribe).
//   - Slash commands produce an agent-free CommandResult (D23), never an
//     agent.Cmd (which is `func() Msg` where `Msg = any` — returning it makes the
//     TUI import agent to call it and type-switch on agent.Msg types).
//
// 7b1 scope: this package defines the contract and the neutral DTOs. The
// implementation (wiring-side facade that bridges to *agent.App) lands in 7b3.
// In 7b1 the package compiles, the DTOs are complete, and the command
// classification matrix is defined — but nothing consumes it yet (Gate #1 stays
// red; the TUI keeps driving *agent.App until 7b3).
package sessionclient