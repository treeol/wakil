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
	"time"

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
	// Card #165: Pre-populate op.subagents with placeholder slots (ChatID+Task
	// only, no result) and op.subagentCheckpointed with false for every child.
	// As each child completes, the checkpoint callback in runSubagentJobs
	// writes the real result into op.subagents[i] and sets
	// subagentCheckpointed[i]=true. The watchdog then salvages completed
	// children and only synthesizes "timed out" for uncheckpointed ones.
	op.childChatIDs = make([]string, len(jobs))
	op.childTasks = make([]string, len(jobs))
	op.subagents = make([]asyncSubagentResult, len(jobs))
	op.subagentCheckpointed = make([]bool, len(jobs))
	for i, j := range jobs {
		op.childChatIDs[i] = j.ChatID
		op.childTasks[i] = j.Task
		op.subagents[i] = asyncSubagentResult{
			ChatID: j.ChatID,
			Task:   j.Task,
		}
	}

	// Arm the watchdog BEFORE starting the worker so there's no window
	// where a stuck worker has no timeout protection.
	// Card #164: The watchdog timeout must account for multi-wave execution
	// under the global semaphore. With maxPar=2 and 6 jobs, children run in
	// ceil(6/2)=3 waves, each needing up to childTimeout. The watchdog is
	// armed at waves×childTimeout + grace so it doesn't force-terminalize
	// a legitimately-running multi-wave batch before all children have had
	// their full per-child budget. The per-child context (created in
	// runSubagentJobs after semaphore acquisition) bounds individual children;
	// the watchdog bounds the BATCH as a whole.
	batchTimeout := a.subagentBatchTimeout(len(jobs))
	a.armSubagentWatchdog(op, batchTimeout)

	safe.Go("async-subagent-block", func() {
		// Set startedAt under lock — but only if not already terminalized
		// (watchdog could fire before the goroutine starts).
		op.mu.Lock()
		if !op.terminal && op.startedAt.IsZero() {
			op.startedAt = time.Now()
		}
		op.mu.Unlock()
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
					op.finishedAt = time.Now()
					op.err = fmt.Errorf("async discovery worker panic: %v", r)
					// Card #165: Salvage checkpointed children. Same logic as
					// the watchdog: preserve completed children's real results,
					// synthesize "worker panicked" only for uncheckpointed ones.
					// Also build op.result from salvaged summaries so they're
					// visible to the model (Mashūra finding #3).
					subs := make([]asyncSubagentResult, len(op.subagents))
					var merged strings.Builder
					for i := range op.subagents {
						if i < len(op.subagentCheckpointed) && op.subagentCheckpointed[i] {
							subs[i] = op.subagents[i]
							if merged.Len() > 0 {
								merged.WriteString("\n")
							}
							fmt.Fprintf(&merged, "[%s]", subs[i].ChatID)
							merged.WriteString(subs[i].Result)
							continue
						}
						cid := ""
						task := ""
						if i < len(op.childChatIDs) {
							cid = op.childChatIDs[i]
						}
						if i < len(op.childTasks) {
							task = op.childTasks[i]
						}
						subs[i] = asyncSubagentResult{
							ChatID: cid,
							Task:   task,
							Err:    "worker panicked",
						}
					}
					op.subagents = subs
					op.result = merged.String()
					// Card #165: Do NOT set subagentEffectsCommitted — let
					// drain handle per-child effects (grounding, Done events,
					// LSP) for both salvaged and panicked children, same as
					// the watchdog path (Mashūra finding #2).
					shouldSend = true
				}
				op.mu.Unlock()
				if !shouldSend {
					// Watchdog (or a prior panic) already terminalized.
					// Don't double-fire; just ensure we publish (idempotent).
					a.commitAsyncCost(op)
					a.publishAsyncOp(op)
					return
				}
				// Card #165: Don't send Done events here — drain-time
				// commitAsyncSubagentEffects handles per-child Done events,
				// grounding, and LSP for both salvaged and panicked children.
				// This avoids double-firing (Mashūra finding #2).
				a.commitAsyncCost(op)
				a.publishAsyncOp(op)
			}
		}()
		// Detached from the turn context: discovery children are read-only and
		// should complete even if the turn is cancelled (card #121 D-4 pattern).
		// Card #164: The batch-level workCtx bounds the TOTAL batch wall time
		// (queue + multi-wave execution). It is sized to waves×childTimeout so
		// all children get their full per-child budget. Individual children get
		// a FRESH per-child context inside runSubagentJobs (after semaphore
		// acquisition), so queue wait time doesn't eat into a child's work budget.
		// The watchdog (armed above with batchTimeout) is the safety net for
		// non-cooperative blocking paths.
		var workCtx context.Context
		if batchTimeout > 0 {
			var cancel context.CancelFunc
			workCtx, cancel = context.WithTimeout(context.Background(), batchTimeout)
			defer cancel()
		} else {
			workCtx = context.Background()
		}

		// Phase B runs on this worker. runSubagentJobs is safe off the turn
		// goroutine (see its concurrency audit); it resolves the child's own
		// limits and never touches parent Conv/trace/budget. The worker emits
		// tagged events (sendEvent is goroutine-safe) and Start events were
		// already sent in Phase A on the turn goroutine.
		// Card #164: Pass the per-child timeout so each child gets a fresh
		// execution context AFTER semaphore acquisition (not at batch start).
		// Card #165: Pass a checkpoint callback that writes each child's result
		// into op.subagents[i] under op.mu as it completes. The watchdog reads
		// these to salvage completed children instead of synthesizing "timed
		// out" for every child. The callback returns false if the watchdog
		// already terminalized (worker suppresses the SubagentFinishedMsg).
		checkpoint := func(index int, sub asyncSubagentResult) bool {
			op.mu.Lock()
			defer op.mu.Unlock()
			if op.terminal {
				// Watchdog already terminalized — reject the checkpoint.
				// The worker should suppress the SubagentFinishedMsg.
				return false
			}
			if index < len(op.subagents) {
				op.subagents[index] = sub
				op.subagentCheckpointed[index] = true
			}
			return true
		}
		results := a.runPreparedSubagents(workCtx, jobs, backend, a.subagentTimeout(), checkpoint)

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
		op.finishedAt = time.Now()
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
