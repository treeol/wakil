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

	"github.com/treeol/wakil/internal/proxy"
	wtools "github.com/treeol/wakil/internal/tools"
)

// subagentJob is one prepared dispatch: an immutable snapshot handed to a
// worker goroutine. Index points back at the originating tool call so results
// can be finalized in original call order.
type subagentJob struct {
	Index  int
	Task   string
	ChatID string
}

// subagentJobResult carries one worker's outcome back to the main goroutine.
type subagentJobResult struct {
	Summary     SubagentSummary
	Grounding   []proxy.GroundingEntry
	CtxSize     int
	UsedBackend string
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
//     read-only, so no workspace write races from workers.
//   - Costs: the child App's Costs tracker is nil and its Client is fresh, so
//     RecordInferenceCost inside a child Send is a no-op on parent state.
//     Subagent inference cost is NOT folded into the parent ledger (documented
//     limitation, unchanged from sequential dispatch).
//   - consentedBackends: workers receive a snapshot copy; only Phase A writes
//     the parent map.
func (a *App) runSubagentJobs(ctx context.Context, jobs []subagentJob, backend string) []subagentJobResult {
	results := make([]subagentJobResult, len(jobs))
	maxPar := a.Cfg.MaxParallelSubagents
	if maxPar < 1 {
		maxPar = 1
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
			summary, grounding, ctxSize, usedBackend := a.dispatchSubagent(
				ctx, jobs[i].Task, subagentProgressOut(a, jobs[i].ChatID), backend, jobs[i].ChatID)
			results[i] = subagentJobResult{
				Summary:     summary,
				Grounding:   grounding,
				CtxSize:     ctxSize,
				UsedBackend: usedBackend,
			}
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
			Task string `json:"task"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			out[i] = fmt.Sprintf("ERROR: could not parse arguments: %v", err)
			continue
		}
		if args.Task == "" {
			out[i] = "ERROR: task is required"
			continue
		}
		jobs = append(jobs, subagentJob{Index: i, Task: args.Task, ChatID: NewChatID()})
	}
	if len(jobs) == 0 {
		return out
	}

	backend := ResolveSubagentBackend(a.SelectedBackend, a.Cfg.SubagentBackend)
	if !a.ensureSubagentConsent(backend) {
		for _, j := range jobs {
			out[j.Index] = declinedSubagentSummary(j.Task, backend).Render()
		}
		return out
	}

	fmt.Fprintln(a.Out, Dim(fmt.Sprintf("· %d subagents in parallel (cap %d)", len(jobs), a.Cfg.MaxParallelSubagents)))
	// All Start events BEFORE any worker spawns (Start-before-Chunk invariant).
	for _, j := range jobs {
		fmt.Fprintln(a.Out, Dim("· subagent: "+Truncate(j.Task, 60)))
		a.sendEvent(SubagentStartMsg{Task: j.Task, ChatID: j.ChatID, Backend: backend})
	}

	// ---- Phase B: concurrent dispatch ----
	results := a.runSubagentJobs(ctx, jobs, backend)

	// ---- Phase C: finalize in original order (main goroutine) ----
	for k, j := range jobs {
		r := results[k]
		a.sendEvent(SubagentDoneMsg{
			ChatID:       j.ChatID,
			Grounding:    r.Grounding,
			CtxSize:      r.CtxSize,
			HardMaxBytes: subagentHardMaxBytes,
			UsedBackend:  r.UsedBackend,
		})
		fullJSON := r.Summary.Render()
		result := fullJSON
		if spillPath := wtools.SpillToCache(a.chatID(), "dispatch_subagent", fullJSON); spillPath != "" {
			result = fullJSON + fmt.Sprintf("\n[subagent summary at: %s]", spillPath)
		}
		if r.Summary.Status == "incomplete" {
			fmt.Fprintln(a.Out, Yellow("⚠ subagent ran out of budget on task: "+Truncate(j.Task, 80)))
			fmt.Fprintln(a.Out, Yellow("  partial findings returned — consider re-dispatching narrower or taking over"))
		}
		out[j.Index] = result
	}
	return out
}
