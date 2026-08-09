package agent

// Async discovery subagents (card #122 Phase 1).
//
// Pure-discovery dispatch_subagent / dispatch_subagents blocks run through the
// async funnel: the turn goroutine prepares (Phase A), children run on a worker
// goroutine (Phase B, detached from the turn context so they complete even if
// the turn ends), and per-child model-visible effects are committed + delivered
// on the turn goroutine at drain (Phase C).
//
// SECURITY SCOPE: only PURE-DISCOVERY blocks go asynchronous. A discovery child
// is read-only, so detaching it cannot race parent workspace writes. Any block
// containing an edit- or tools-capable child stays synchronous (the child-vs-
// parent mutation invariant requires the parent to remain blocked while a
// writing child runs). This is enforced here by refusing to queue any block
// where pureDiscovery is false.

import (
	"context"
	"fmt"
	"strings"

	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/safe"
)

// ensureSubagentGlobalSem returns the App's GLOBAL subagent concurrency semaphore,
// creating it lazily at the requested size (maxPar = the effective /maxpar cap).
// The global sem bounds total concurrent subagent children across ALL overlapping
// batches (synchronous AND detached async discovery), so a batch's own /maxpar
// clamp is respected globally. Sized once; a later larger maxPar cannot grow the
// channel (the clamp is already applied at admission inside runSubagentJobs, and
// re-homestzing here is best-effort). Safe to call concurrently.
func (a *App) ensureSubagentGlobalSem(maxPar int) chan struct{} {
	// Fast path: already sized to at least the requested cap.
	if s := a.subagentGlobalSem; s != nil && cap(s) >= maxPar {
		return s
	}
	a.subagentSemMu.Lock()
	defer a.subagentSemMu.Unlock()
	if a.subagentGlobalSem == nil || cap(a.subagentGlobalSem) < maxPar {
		a.subagentGlobalSem = make(chan struct{}, maxPar)
	}
	return a.subagentGlobalSem
}

// queueAsyncDiscoveryBlock enqueues a pure-discovery block as ONE async op and
// returns the per-call placeholder result strings (one per tool_call_id, per
// the protocol-closure invariant — every tool_call_id gets exactly one tool
// result). When the block is refused (registry full, stopping, or not pure
// discovery) it returns ok=false so the caller can fall back to the
// synchronous path. Never a silent synchronous fallback: the full/invalid cases
// return explicit errors via ok=false with a clear reason in the result.
//
// Caller contract: this runs on the TURN goroutine. Phase A (prepare) has
// already run and ALL SubagentStartMsg events have been sent. This function
// only reserves the async slot, starts the worker that runs Phase B, and returns
// placeholders for Phase C delivery.
func (a *App) queueAsyncDiscoveryBlock(block []proxy.ToolCall, jobs []subagentJob, backend string) ([]string, bool) {
	// One batch op. jobs are guaranteed pure discovery (checked by caller via
	// prepareSubagentBlock's pureDiscovery flag).
	op, reason := a.registerAsyncOp("dispatch_subagents", fmt.Sprintf("%d discovery subagents", len(jobs)))
	if reason != "" {
		// Refused — return a clear rejection, never a silent sync fallback.
		// Also send SubagentDoneMsg with Err for every child so TUI tabs
		// don't spin forever (SubagentStartMsg was already sent in Phase A).
		out := make([]string, len(block))
		why := "async registry full"
		if reason == "stopping" {
			why = "session shutting down"
		}
		for i, tc := range block {
			out[i] = fmt.Sprintf("ERROR: async %s %q rejected (%s) — re-run when capacity frees", tc.Function.Name, tc.ID, why)
		}
		for _, j := range jobs {
			a.sendEvent(SubagentDoneMsg{
				ChatID: j.ChatID,
				Err:    "async registry refused: " + why,
			})
		}
		return out, false
	}

	// Store child ChatIDs + tasks on the op so the watchdog can synthesize
	// per-child SubagentDoneMsg events if the worker doesn't return.
	op.childChatIDs = make([]string, len(jobs))
	op.childTasks = make([]string, len(jobs))
	for i, j := range jobs {
		op.childChatIDs[i] = j.ChatID
		op.childTasks[i] = j.Task
	}

	// Arm the watchdog BEFORE starting the worker so there's no window
	// where a stuck worker has no timeout protection.
	timeout := a.subagentTimeout()
	a.armSubagentWatchdog(op, timeout)

	safe.Go("async-subagent-block", func() {
		defer close(op.done)
		defer a.cancelWatchdog(op) // cancel the watchdog if the worker returns normally
		// Finalizer: if ANYTHING in the body panics after the children ran
		// (rendering, cost fold, indexing), still terminalize as an error and
		// publish — so the registered active slot is never leaked and the
		// registry stays consistent (review finding #5). Idempotent via
		// publishAsyncOp / the op.terminal guard.
		defer func() {
			if r := recover(); r != nil {
				op.mu.Lock()
				shouldSend := false
				if !op.terminal {
					op.terminal = true
					op.err = fmt.Errorf("async discovery worker panic: %v", r)
					// Synthesize per-child error results so tabs don't spin.
					subs := make([]asyncSubagentResult, 0, len(op.childChatIDs))
					for i, cid := range op.childChatIDs {
						task := ""
						if i < len(op.childTasks) {
							task = op.childTasks[i]
						}
						subs = append(subs, asyncSubagentResult{
							ChatID: cid,
							Task:   task,
							Err:    "worker panicked",
						})
					}
					op.subagents = subs
					// Atomically claim event ownership under the lock so the
					// watchdog can't publish and trigger drain-time sends
					// in the gap between unlock and our send loop.
					op.subagentEffectsCommitted = true
					shouldSend = true
				}
				op.mu.Unlock()
				if !shouldSend {
					// Watchdog (or a prior panic) already claimed effects.
					// Don't double-fire; just ensure we publish (idempotent).
					a.commitAsyncCost(op)
					a.publishAsyncOp(op)
					return
				}
				// Send per-child SubagentDoneMsg with Err for every child so TUI
				// tabs don't spin. subagentEffectsCommitted was already set
				// under the lock above so drain won't re-send.
				for _, cid := range op.childChatIDs {
					a.sendEvent(SubagentDoneMsg{
						ChatID: cid,
						Err:    "subagent worker panicked",
					})
				}
				a.commitAsyncCost(op)
				a.publishAsyncOp(op)
			}
		}()
		// Detached from the turn context: discovery children are read-only and
		// should complete even if the turn is cancelled (card #121 D-4 pattern).
		// But bounded by a timeout: if the child's HTTP stream hangs (network
		// stall, rate-limit), ctx cancellation kills the request and the worker
		// returns normally. The watchdog is the safety net for non-cooperative
		// blocking paths.
		var workCtx context.Context
		if timeout > 0 {
			var cancel context.CancelFunc
			workCtx, cancel = context.WithTimeout(context.Background(), timeout)
			defer cancel()
		} else {
			workCtx = context.Background()
		}

		// Phase B runs on this worker. runSubagentJobs is safe off the turn
		// goroutine (see its concurrency audit); it resolves the child's own
		// limits and never touches parent Conv/trace/budget. The worker emits
		// tagged events (sendEvent is goroutine-safe) and Start events were
		// already sent in Phase A on the turn goroutine.
		results := a.runPreparedSubagents(workCtx, jobs, backend)

		// Build the terminal record: per-child effects + a flattened rendered
		// result (merged) + op-level error if any child failed.
		subs := make([]asyncSubagentResult, 0, len(results))
		var merged strings.Builder
		firstFailed := ""
		for k, j := range jobs {
			r := results[k]
			rendered := renderSubagentResult(a, j.Task, r.Summary, r.FilesChanged)
			if merged.Len() > 0 {
				merged.WriteString("\n")
			}
			fmt.Fprintf(&merged, "[%s]", j.ChatID)
			merged.WriteString(rendered)
			s := asyncSubagentResult{
				ChatID:       j.ChatID,
				Task:         j.Task,
				Result:       rendered,
				Grounding:    r.Grounding,
				CostRows:     r.CostRows,
				FilesChanged: r.FilesChanged,
				CtxSize:      r.CtxSize,
				UsedBackend:  r.UsedBackend,
			}
			if r.Summary.Status == "incomplete" {
				s.Err = "subagent incomplete (budget/cancelled)"
			}
			if s.Err != "" && firstFailed == "" {
				firstFailed = s.Err
			}
			subs = append(subs, s)
		}
		var opErr error
		if firstFailed != "" {
			opErr = fmt.Errorf("%s", firstFailed)
		}

		op.mu.Lock()
		if op.terminal {
			op.mu.Unlock()
			return
		}
		op.terminal = true
		op.result = merged.String()
		op.err = opErr
		op.subagents = subs // published frozen; workers never mutate after this
		op.mu.Unlock()

		// Cost folded at terminal (worker-safe: foldSubagentCost → mutex-guarded
		// tracker). Grounding/SubagentDoneMsg/LSP deferred to drain (turn goroutine).
		// publishAsyncOp releases the slot + publishes to the inbox exactly once
		// (idempotent; suppresses on shutdown).
		a.commitAsyncCost(op)
		a.publishAsyncOp(op)
	})

	// Per-call placeholders (protocol closure). Each tool_call_id gets exactly
	// one tool result. Capacity was reserved; deliverable when the batch drains.
	out := make([]string, len(block))
	for i := range block {
		out[i] = fmt.Sprintf("queued as %s (discovery subagent) — running now; result will be injected into context when it completes. Call check_pending(%q) to retrieve early.",
			op.id, op.id)
	}
	return out, true
}
