package agent

import "sync"

// subagent_state.go: subagent dispatch and tracking state for App (WP-6.3
// extraction). Embedded in App so all field access is unchanged via Go's
// promoted-field access.

type subagentState struct {
	// exhausted is set by Send when the subagent hit MaxToolIterations
	// (forceFinish) or enforceHardMax dropped content during the turn. It is
	// read by dispatchSubagent after Send returns to produce a truthful
	// Status:"incomplete" summary instead of relying on the model's final
	// response — which may be a lobotomized generic message if compaction
	// fired. Only meaningful for subagents; the parent ignores it.
	exhausted bool

	// stopReason records why the subagent stopped, set at the exact site where
	// exhaustion occurs. Values: "iteration_limit", "hard_max_shed",
	// "confinement_breaker". Empty = no stop reason (normal completion).
	// Captured before the retry Send (which resets it) and ORed across both
	// Sends, exactly like exhausted. Only meaningful for subagents.
	stopReason string

	// turnBudgetStubbed is a sticky per-App flag set inside CapOrStub when the
	// per-turn tool budget is exhausted and a result is stubbed to a spill
	// pointer. Used by dispatchSubagent to set StopReason="turn_budget_exhausted"
	// when no other stop reason fired (the model may stop naturally after being
	// starved of content, without hitting the iteration cap).
	turnBudgetStubbed bool

	// filesChanged is the recorder for edit-tier subagents: tracks canonical
	// paths touched by successful edit-category tool calls during the child's
	// Send loop. nil for the parent and discovery-tier children. Populated by
	// ExecuteToolCall when it detects an edit-tool success; read by
	// dispatchSubagent after Send returns to produce the mechanical
	// files_changed list on the done message.
	filesChanged *filesChangedRecorder

	// pendingRestore holds the fileRecorder from a failed edit-tier subagent
	// dispatch, for /restore to use. Set by dispatchSubagent when the child
	// returns incomplete/exhausted. Cleared on successful /restore or on the
	// next edit-tier dispatch. TUI-only — headless auto-restores immediately.
	pendingRestore *filesChangedRecorder

	// externalActions is the recorder for tools-tier subagents: tracks every
	// MCP tool call the child makes (server, tool, status). nil for the parent
	// and discovery/edit-tier children. Populated by ExecuteToolCall when it
	// routes an MCP tool call; read by dispatchSubagent after Send returns to
	// fold the mechanical external_calls list into the summary.
	externalActions *externalActionsRecorder

	// confinementTripped is set by Send when the path-confinement circuit
	// breaker fires: confinementBreakerThreshold consecutive ConfinePath
	// rejections within the turn. ConfinePath failures are a deterministic
	// error class — the same path fails identically on every retry — so this
	// is distinct from generic budget exhaustion: the model is stopped early
	// (well before MaxToolIterations) and told plainly which path(s) are
	// unreachable, rather than being left to burn its whole iteration budget
	// retrying a doomed path. Read by dispatchSubagent to attach a precise
	// "inaccessible" Skipped entry. confinementPathsHit holds the distinct
	// path arguments observed during the tripped streak, for reporting.
	confinementTripped  bool
	confinementPathsHit []string

	// pinUserMessage marks the user message appended by Send as Pinned, so it
	// survives compaction and hard-max dropping. Set by dispatchSubagent for
	// the subagent's task instruction — the subagent must never forget its own
	// task mid-run. The parent does not set this.
	pinUserMessage bool

	// subMaxToolIter overrides the subagentMaxToolIter constant when non-zero.
	// Used by tests to force exhaustion through the real dispatchSubagent path
	// without changing the package-level constant. Zero = use the constant.
	subMaxToolIter int

	// Card #122 Phase 1: GLOBAL subagent concurrency cap across ALL overlapping
	// batches (synchronous + async discovery). Sized lazily by MaxParallelSubagents;
	// bounded by a wire in runSubagentJobs so total concurrent children never
	// exceeds /maxpar even when async batches detach and overlap. nil until first use.
	subagentGlobalSem chan struct{}
	// subagentSemMu guards lazy (re)size of subagentGlobalSem.
	subagentSemMu sync.Mutex

	// subagentLimitsCachePtr backs a singleflight cache for context-limit
	// probes against overridden subagent endpoints: concurrent dispatch_subagent
	// workers targeting the same endpoint+backend fire at most one /props or
	// /v1/ilm/limits request. A pointer field (not an embedded mutex value)
	// avoids adding another lock to App's memory layout.
	//
	// MAIN GOROUTINE ONLY to set: ensureSubagentLimitsCache() populates it
	// before any worker goroutine spawns. Go's memory model guarantees a value
	// written on the main goroutine before a `go` statement is visible inside
	// that goroutine without extra synchronization, so workers may safely read
	// (never write) this field after being spawned.
	subagentLimitsCachePtr *subagentLimitsCache
}
