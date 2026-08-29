package agent

// Parallel subagent dispatch: the fan-out core shared by the per-turn
// contiguous-block path (Send) and the dispatch_subagents batch tool.
//
// Execution model (see .wakil/parallel-subagents-plan.md):
//
//	Phase A — prepare (MAIN GOROUTINE): parse args, resolve backend once,
//	  run the egress consent gate once, mint all ChatIDs, and send ALL
//	  SubagentStartMsg events before any worker spawns. This guarantees the
//	  Start-before-Chunk invariant: the TUI has a tab for every ChatID
//	  before the first tagged chunk can arrive.
//	Phase B — dispatch (WORKER GOROUTINES, bounded by MaxParallelSubagents):
//	  dispatchSubagent only. Workers write only their own results slot and
//	  emit tagged events via sendEvent (Program.Send is goroutine-safe).
//	  No a.Out writes, no consent-map writes, no Conv/trace/budget touches.
//	Phase C — finalize (MAIN GOROUTINE, original call order): Done events,
//	  spill, warning lines, and the caller's Conv/trace/cap bookkeeping.

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/treeol/wakil/internal/proxy"
	wtools "github.com/treeol/wakil/internal/tools"
)

// subagentJob is one prepared dispatch: an immutable snapshot handed to a
// worker goroutine. Index points back at the originating tool call so results
// can be finalized in original call order.
type subagentJob struct {
	Index      int
	Task       string
	ChatID     string
	Capability string
	Model      string // per-dispatch model override ("" or "inherit" = use session-global routing)
}

// subagentJobResult carries one worker's outcome back to the main goroutine.
type subagentJobResult struct {
	Summary      SubagentSummary
	Grounding    []proxy.GroundingEntry
	CtxSize      int
	UsedBackend  string
	CostRows     []proxy.CostRow // child's own priced rows; folded into a.Costs in Phase C only
	FilesChanged []string        // mechanical record of canonical paths touched (edit-tier only)
}

// cancelledJobResult is the truthful summary for a job that never ran (or was
// cut short) because ctx was cancelled. The tool_call still gets a response —
// an unanswered tool_call would invalidate the next API request.
func cancelledJobResult(task string) subagentJobResult {
	return subagentJobResult{Summary: SubagentSummary{
		Objective:   task,
		Status:      "incomplete",
		Uncertainty: []string{"subagent cancelled before completion"},
	}}
}

// panicJobResult converts a recovered worker panic into an error summary so
// one crashing child never takes down the parent turn or its siblings.
func panicJobResult(task string, r interface{}) subagentJobResult {
	return subagentJobResult{Summary: SubagentSummary{
		Objective:   task,
		Status:      "incomplete",
		Findings:    []Finding{{Summary: Truncate(fmt.Sprintf("subagent panic: %v", r), 200), Kind: "error", Weight: "low"}},
		Uncertainty: []string{"subagent worker panicked"},
	}}
}

// runSubagentJobs is Phase B: run the prepared jobs concurrently, bounded by
// MaxParallelSubagents, and return results indexed like jobs.
//
// Caller contract (Phase A, main goroutine, BEFORE this call): backend
// resolved, ensureSubagentConsent passed, ChatIDs minted, all
// SubagentStartMsg events sent.
//
// wg.Wait here is deliberate and safe: every blocking operation inside
// dispatchSubagent is ctx-aware (the HTTP stream uses the request context),
// and semaphore acquisition selects on ctx.Done. Returning before all workers
// finish would race on the results slice, so we always join fully.
//
// Concurrency audit (step 6/7 of the parallel-subagents plan):
//   - Executor: shared with workers. RunShell/ReadFile/ListDir compose fresh
//     commands per call (runFromRoot); the one lazily-written cache
//     (SandboxTools probe) is sync.Once-guarded. Discovery tools are
//     read-only, so no workspace write races from discovery workers. Edit-
//     tier children are serialized by subagentWriterMu (at most one edit
//     child executing at a time); discovery children still parallelize freely,
//     including alongside one running edit child.
//   - Costs: each child App gets its OWN fresh CostTracker (never a.Costs, the
//     parent's pointer) — RecordInferenceCost inside a child Send writes only
//     to that private tracker, so no worker ever touches parent-shared cost
//     state. dispatchSubagent returns the child's priced rows in the result;
//     Phase C (main goroutine, after wg.Wait) folds them into a.Costs — see
//     foldSubagentCost. This is the only point subagent cost touches the
//     parent ledger, and it happens strictly after all workers have joined.
//   - Limits: the child's CtxLimit is resolved by dispatchSubagent itself
//     (inherit: a.CtxLimit directly, zero requests; override: through
//     a.subagentLimitsCache, which is mutex-guarded and singleflights
//     concurrent probes for the same endpoint+backend key — safe to call from
//     every worker without duplicating probes).
//   - consentedBackends: workers receive a snapshot copy; only Phase A writes
//     the parent map.
func (a *App) runSubagentJobs(ctx context.Context, jobs []subagentJob, backend string) []subagentJobResult {
	results := make([]subagentJobResult, len(jobs))
	// MaxParallelSubagents is turn-stable per batch — read once at batch
	// start under stateMu.RLock, used for the whole batch.
	a.stateMu.RLock()
	maxPar := a.Cfg.MaxParallelSubagents
	a.stateMu.RUnlock()
	if maxPar < 1 {
		maxPar = 1
	}
	// Clamp to job count: a huge config value (e.g. /maxpar 64 with 2 jobs)
	// would allocate an oversized semaphore for no benefit.
	if maxPar > len(jobs) {
		maxPar = len(jobs)
	}
	// Card #122 Phase 1: the GLOBAL semaphore bounds total concurrent subagent
	// children ACROSS all overlapping batches (incl. detached async discovery),
	// not just within this invocation — so async batches cannot balloon
	// parallelism beyond /maxpar. Sized once by maxPar.
	sem := a.ensureSubagentGlobalSem(maxPar)
	var wg sync.WaitGroup
	for i := range jobs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					results[i] = panicJobResult(jobs[i].Task, r)
				}
			}()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[i] = cancelledJobResult(jobs[i].Task)
				return
			}
			if ctx.Err() != nil {
				results[i] = cancelledJobResult(jobs[i].Task)
				return
			}
			// Slot acquired — this subagent is now actually running (was queued).
			// sendEvent is goroutine-safe (Program.Send), same as chunk events.
			a.sendEvent(SubagentActiveMsg{ChatID: jobs[i].ChatID})
			summary, grounding, ctxSize, usedBackend, costRows, filesChanged := a.dispatchSubagent(
				ctx, jobs[i].Task, subagentProgressOut(a, jobs[i].ChatID), backend, jobs[i].Capability, jobs[i].Model, jobs[i].ChatID)
			results[i] = subagentJobResult{
				Summary:      summary,
				Grounding:    grounding,
				CtxSize:      ctxSize,
				UsedBackend:  usedBackend,
				CostRows:     costRows,
				FilesChanged: filesChanged,
			}
			// Early display-only completion event: emitted from the worker the
			// moment the child returns, before the result enters the results
			// slice and before Phase C's cost fold. The TUI uses this to flip
			// the tab to done-state at actual completion time; SubagentDoneMsg
			// in Phase C remains the authoritative event carrying the folded
			// state. No parent-state mutation here — CostUSD is the child's own
			// total (from its fresh CostTracker), display data only.
			a.sendSubagentFinished(jobs[i].ChatID, results[i])
		}(i)
	}
	wg.Wait()
	return results
}

// runParallelSubagentBlock executes a contiguous block of dispatch_subagent
// tool calls through the three-phase model and returns one result string per
// call, in block order. MAIN GOROUTINE ONLY (Phases A and C run here).
//
// Observability: prints a one-line concurrency note so it is visible when the
// model actually batched dispatches (parallelism is model-dependent and can
// silently degrade to sequential — this line is the receipt that it fired).
//
// This is the SYNCHRONOUS path (edit/tools-capable blocks, and blocks that
// fail the pure-discovery fast path). Pure-discovery blocks may instead be
// dispatched asynchronously via queueAsyncDiscoveryBlock — see that function.
func (a *App) runParallelSubagentBlock(ctx context.Context, block []proxy.ToolCall) []string {
	// ---- Phase A: prepare (main goroutine) — resolve jobs, collect per-call
	// immediate results (parse/consent errors). Also determines whether the
	// block is pure discovery (async-capable).
	jobs, out, backend, pureDiscovery, prepared := a.prepareSubagentBlock(block)
	if !prepared {
		return out
	}
	a.announceSubagentBlock(jobs, backend)
	if pureDiscovery {
		// Card #122 Phase 1: route pure-discovery blocks through the async
		// funnel. queueAsyncDiscoveryBlock returns per-call placeholders on
		// success, or explicit per-call rejections on refusal (never silent sync).
		results, ok := a.queueAsyncDiscoveryBlock(block, jobs, backend)
		if ok || results != nil {
			return results
		}
	}
	// Mixed / non-discovery / refused: synchronous path (child-vs-parent mutation
	// invariant preserved; no silent async downgrade on refusal — refusal above
	// returns explicit rejections already).
	syncResults := a.runPreparedSubagents(ctx, jobs, backend)
	return a.finalizeSubagentBlock(jobs, syncResults, out)
}

// prepareSubagentBlock is Phase A (MAIN GOROUTINE): parse args, run the egress
// consent gate once, mint all ChatIDs, and send ALL SubagentStartMsg events
// before any worker spawns. Returns the prepared jobs, per-call immediate
// results (for parse/consent/validation failures), the resolved backend, and
// pureDiscovery (true if EVERY job is discovery-capable — the only capability
// safe to detach asynchronously).
func (a *App) prepareSubagentBlock(block []proxy.ToolCall) ([]subagentJob, []string, string, bool, bool) {
	out := make([]string, len(block))
	jobs := make([]subagentJob, 0, len(block))
	pureDiscovery := true
	for i, tc := range block {
		var args struct {
			Task       string `json:"task"`
			Capability string `json:"capability"`
			Model      string `json:"model"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			out[i] = fmt.Sprintf("ERROR: could not parse arguments: %v", err)
			pureDiscovery = false
			continue
		}
		if args.Task == "" {
			out[i] = "ERROR: task is required"
			pureDiscovery = false
			continue
		}
		capability := args.Capability
		if capability == "" {
			capability = wtools.CapabilityDiscovery
		}
		if !wtools.ValidCapability(capability) {
			out[i] = fmt.Sprintf("ERROR: unknown capability %q — valid values: %q (default), %q, %q",
				args.Capability, wtools.CapabilityDiscovery, wtools.CapabilityEdit, wtools.CapabilityTools)
			pureDiscovery = false
			continue
		}
		// Consent gate: edit/tools capability requires session write consent (the
		// parent's own write predicate). INVARIANT: child may write iff parent may.
		if (capability == wtools.CapabilityEdit || capability == wtools.CapabilityTools) && !a.Consent().AutoApprove {
			out[i] = fmt.Sprintf("ERROR: %s capability requires /auto or --auto (session consent). "+
				"Re-dispatch with capability \"discovery\" (the default) for read-only research.",
				capability)
			pureDiscovery = false
			continue
		}
		if capability != wtools.CapabilityDiscovery {
			pureDiscovery = false
		}
		jobs = append(jobs, subagentJob{Index: i, Task: args.Task, ChatID: NewChatID(), Capability: capability, Model: args.Model})
	}
	if len(jobs) == 0 {
		return nil, out, "", pureDiscovery, false
	}
	// Backend/toolset/consent infra is resolved once for the whole block.
	backend := a.resolveSubagentBackendForEndpoint(a.resolvedSubagentEndpointKind())
	a.ensureSubagentLimitsCache()
	if !a.ensureSubagentConsent(backend) {
		for _, j := range jobs {
			out[j.Index] = declinedSubagentSummary(j.Task, backend).Render()
		}
		return nil, out, backend, false, false
	}
	return jobs, out, backend, len(jobs) > 0 && pureDiscovery, true
}

// announceSubagentBlock prints the one-line concurrency receipt and sends all
// SubagentStartMsg events (Start-before-Chunk invariant). MAIN GOROUTINE ONLY,
// before any worker spawns.
func (a *App) announceSubagentBlock(jobs []subagentJob, backend string) {
	dispCap := a.MaxParallelLocked()
	if dispCap < 1 {
		dispCap = 1
	}
	if dispCap > len(jobs) {
		dispCap = len(jobs)
	}
	fmt.Fprintln(a.Out, Dim(fmt.Sprintf("· %d subagents in parallel (cap %d)", len(jobs), dispCap)))
	sessionModel := a.resolvedSubagentDisplayModel()
	for _, j := range jobs {
		fmt.Fprintln(a.Out, Dim("· subagent: "+Truncate(j.Task, 60)))
		// Per-job model: if the job has a per-dispatch override, show it;
		// otherwise fall back to the session-global resolved model.
		dispModel := sessionModel
		if j.Model != "" && j.Model != "inherit" {
			dispModel = j.Model
		}
		// Per-job tool names: each job can have a different capability in a
		// mixed block, so resolve tool names per-job, not once for the block.
		a.sendEvent(SubagentStartMsg{
			Task:       j.Task,
			ChatID:     j.ChatID,
			Backend:    backend,
			Capability: j.Capability,
			Model:      dispModel,
			ToolNames:  a.subagentToolNames(j.Capability),
		})
	}
}

// runPreparedSubagents is Phase B: run the prepared jobs concurrently (bounded
// by MaxParallelSubagents). Safe to call from a worker goroutine (see the
// concurrency audit on runSubagentJobs). Returns results indexed like jobs.
func (a *App) runPreparedSubagents(ctx context.Context, jobs []subagentJob, backend string) []subagentJobResult {
	return a.runSubagentJobs(ctx, jobs, backend)
}

// finalizeSubagentBlock is Phase C (MAIN GOROUTINE): fold costs, emit Done
// events, spill, and assemble per-call result strings in original order.
// Returns result strings indexed by job.Index's orig-position (in out).
func (a *App) finalizeSubagentBlock(jobs []subagentJob, results []subagentJobResult, out []string) []string {
	for k, j := range jobs {
		r := results[k]
		subagentCostUSD := foldSubagentCost(a.Costs, r.CostRows)
		doneErr := ""
		if r.Summary.Status == "incomplete" {
			doneErr = "subagent incomplete (budget/cancelled)"
		}
		a.sendEvent(SubagentDoneMsg{
			ChatID:       j.ChatID,
			Grounding:    r.Grounding,
			CtxSize:      r.CtxSize,
			HardMaxBytes: subagentHardMaxBytes,
			UsedBackend:  r.UsedBackend,
			CostUSD:      subagentCostUSD,
			FilesChanged: r.FilesChanged,
			Err:          doneErr,
		})
		warnSubagentIncomplete(a, j.Task, r.Summary)
		out[j.Index] = renderSubagentResult(a, j.Task, r.Summary, r.FilesChanged)
	}
	return out
}

// renderSubagentResult assembles a single child's result string: the ≤4k JSON
// summary + spill marker + files_changed list. PURE (no a.Out writes) so it is
// safe to call from a worker goroutine (async discovery path). Callers emit the
// incomplete-summary warning themselves (they know whether they're on the turn
// goroutine).
func renderSubagentResult(a *App, task string, summary SubagentSummary, filesChanged []string) string {
	fullJSON := summary.Render()
	result := fullJSON
	if spillPath := wtools.SpillToCache(a.chatID(), "dispatch_subagent", fullJSON); spillPath != "" {
		result = fullJSON + fmt.Sprintf("\n[subagent summary at: %s]", spillPath)
	}
	if len(filesChanged) > 0 {
		result += renderFilesChanged(summary.FilesChanged, filesChanged)
	}
	return result
}

// warnSubagentIncomplete prints the loud incomplete warning. MAIN
// GOROUTINE ONLY (writes a.Out — must not run on a worker).
func warnSubagentIncomplete(a *App, task string, summary SubagentSummary) {
	if summary.Status != "incomplete" {
		return
	}
	fmt.Fprintln(a.Out, Yellow("⚠ subagent incomplete on task: "+Truncate(task, 80)))
	fmt.Fprintln(a.Out, Yellow("  the child ran out of budget or was cancelled — consider re-dispatching narrower or taking over"))
}

// sendSubagentFinished emits the display-only early completion event from the
// worker goroutine. It is called the moment dispatchSubagent returns — before
// the result enters the results slice and before Phase C's cost fold. The
// TUI uses this to flip the tab to done-state at actual completion time.
//
// Display data only: CostUSD is the child's own priced total (summed from its
// fresh CostTracker's priced rows, known worker-side), FilesChanged is the
// mechanical record, SummaryPreview is a short rendering. No parent-state
// mutation happens here — the authoritative cost fold and all parent-state
// bookkeeping stay in Phase C's SubagentDoneMsg.
//
// nil-safe: sendEvent is a no-op when EventSink is unset (tests, CLI).
func (a *App) sendSubagentFinished(chatID string, r subagentJobResult) {
	a.sendEvent(SubagentFinishedMsg{
		ChatID:         chatID,
		Status:         r.Summary.Status,
		CostUSD:        sumPricedRows(r.CostRows),
		FilesChanged:   r.FilesChanged,
		SummaryPreview: summaryPreview(r.Summary),
		FinishedAt:     time.Now(),
	})
}

// sumPricedRows totals the priced USD across the child's own cost rows. This
// mirrors the arithmetic foldSubagentCost performs in Phase C (sum of
// r.Priced rows), but without touching the parent's CostTracker — display only.
func sumPricedRows(rows []proxy.CostRow) float64 {
	var total float64
	for _, r := range rows {
		if r.Priced {
			total += r.CostUSD
		}
	}
	return total
}

// summaryPreview extracts a short display string from the child's summary —
// the objective line, truncated. Gives the user a one-line "what landed"
// preview in the sidebar the moment the child finishes.
func summaryPreview(s SubagentSummary) string {
	if s.Objective != "" {
		return Truncate(s.Objective, 80)
	}
	if len(s.Findings) > 0 && s.Findings[0].Summary != "" {
		return Truncate(s.Findings[0].Summary, 80)
	}
	return ""
}
