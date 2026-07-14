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
	maxPar := a.Cfg.MaxParallelSubagents
	if maxPar < 1 {
		maxPar = 1
	}
	// Clamp to job count: a huge config value (e.g. /maxpar 64 with 2 jobs)
	// would allocate an oversized semaphore for no benefit.
	if maxPar > len(jobs) {
		maxPar = len(jobs)
	}
	sem := make(chan struct{}, maxPar)
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
				ctx, jobs[i].Task, subagentProgressOut(a, jobs[i].ChatID), backend, jobs[i].Capability, jobs[i].ChatID)
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
func (a *App) runParallelSubagentBlock(ctx context.Context, block []proxy.ToolCall) []string {
	out := make([]string, len(block))

	// ---- Phase A: prepare (main goroutine) ----
	jobs := make([]subagentJob, 0, len(block))
	for i, tc := range block {
		var args struct {
			Task       string `json:"task"`
			Capability string `json:"capability"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			out[i] = fmt.Sprintf("ERROR: could not parse arguments: %v", err)
			continue
		}
		if args.Task == "" {
			out[i] = "ERROR: task is required"
			continue
		}
		capability := args.Capability
		if capability == "" {
			capability = wtools.CapabilityDiscovery
		}
		if !wtools.ValidCapability(capability) {
			out[i] = fmt.Sprintf("ERROR: unknown capability %q — valid values: %q (default), %q, %q",
				args.Capability, wtools.CapabilityDiscovery, wtools.CapabilityEdit, wtools.CapabilityTools)
			continue
		}
		// Consent gate (same as sequential path at app.go — the two must move
		// together): edit capability requires session write consent. The parent's
		// write predicate for edit-category tools is AutoApprove alone (see the
		// verbatim trace in the comment at app.go's dispatch_subagent case).
		// INVARIANT: child may write iff parent may write.
		// Tools capability also requires AutoApprove (session consent for
		// external tool access — same gate, different rationale).
		if (capability == wtools.CapabilityEdit || capability == wtools.CapabilityTools) && !a.AutoApprove {
			out[i] = fmt.Sprintf("ERROR: %s capability requires /auto or --auto (session consent). "+
				"Re-dispatch with capability \"discovery\" (the default) for read-only research.",
				capability)
			continue
		}
		jobs = append(jobs, subagentJob{Index: i, Task: args.Task, ChatID: NewChatID(), Capability: capability})
	}
	if len(jobs) == 0 {
		return out
	}

	// Backend resolution only applies when the child's resolved endpoint is
	// kind ilm-proxy; for kind openai there is no backend-routing concept, so
	// skip resolution entirely rather than compute an inert value.
	backend := a.resolveSubagentBackendForEndpoint(a.resolvedSubagentEndpointKind())
	a.ensureSubagentLimitsCache()
	if !a.ensureSubagentConsent(backend) {
		for _, j := range jobs {
			out[j.Index] = declinedSubagentSummary(j.Task, backend).Render()
		}
		return out
	}

	// Display the effective cap, not the raw config value — runSubagentJobs
	// clamps to len(jobs), so the user sees what actually bounded parallelism.
	dispCap := a.Cfg.MaxParallelSubagents
	if dispCap < 1 {
		dispCap = 1
	}
	if dispCap > len(jobs) {
		dispCap = len(jobs)
	}
	fmt.Fprintln(a.Out, Dim(fmt.Sprintf("· %d subagents in parallel (cap %d)", len(jobs), dispCap)))
	// All Start events BEFORE any worker spawns (Start-before-Chunk invariant).
	// Resolve the display model once — all jobs in this batch share the same
	// endpoint, so the model is identical. This runs in Phase A (main goroutine)
	// before any worker spawns.
	displayModel := a.resolvedSubagentDisplayModel()
	// Resolve the tool names once — all jobs in this batch share the same
	// capability, so the tool list is identical. This runs in Phase A (main
	// goroutine) before any worker spawns.
	toolNames := a.subagentToolNames(jobs[0].Capability)
	for _, j := range jobs {
		fmt.Fprintln(a.Out, Dim("· subagent: "+Truncate(j.Task, 60)))
		a.sendEvent(SubagentStartMsg{
			Task:       j.Task,
			ChatID:     j.ChatID,
			Backend:    backend,
			Capability: j.Capability,
			Model:      displayModel,
			ToolNames:  toolNames,
		})
	}

	// ---- Phase B: concurrent dispatch ----
	results := a.runSubagentJobs(ctx, jobs, backend)

	// ---- Phase C: finalize in original order (main goroutine) ----
	// Cost fold happens HERE, after wg.Wait() in Phase B has fully joined every
	// worker — parent-state mutation (a.Costs) is safe only on this side of the
	// goroutine boundary. No worker ever calls foldSubagentCost itself.
	for k, j := range jobs {
		r := results[k]
		subagentCostUSD := foldSubagentCost(a.Costs, r.CostRows)
		a.sendEvent(SubagentDoneMsg{
			ChatID:       j.ChatID,
			Grounding:    r.Grounding,
			CtxSize:      r.CtxSize,
			HardMaxBytes: subagentHardMaxBytes,
			UsedBackend:  r.UsedBackend,
			CostUSD:      subagentCostUSD,
			FilesChanged: r.FilesChanged,
		})
		fullJSON := r.Summary.Render()
		result := fullJSON
		if spillPath := wtools.SpillToCache(a.chatID(), "dispatch_subagent", fullJSON); spillPath != "" {
			result = fullJSON + fmt.Sprintf("\n[subagent summary at: %s]", spillPath)
		}
		// Append the mechanical files_changed list (ground truth) for edit-tier.
		if len(r.FilesChanged) > 0 {
			result += renderFilesChanged(r.Summary.FilesChanged, r.FilesChanged)
		}
		if r.Summary.Status == "incomplete" {
			fmt.Fprintln(a.Out, Yellow("⚠ subagent ran out of budget on task: "+Truncate(j.Task, 80)))
			fmt.Fprintln(a.Out, Yellow("  partial findings returned — consider re-dispatching narrower or taking over"))
		}
		out[j.Index] = result
	}
	return out
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
