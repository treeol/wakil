package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/tools"
)

// Subagent context budget constants. Conservative for a 32k-token backend;
// raise HardMaxBytes after checking --ctx-size on the proxy machine.
const (
	subagentHardMaxBytes   = 70_000 // ~23k tokens @ 3 chars/tok; safe floor for 32k backend
	subagentCompactAt      = 55_000
	subagentKeepBytes      = 45_000
	subagentSummaryBytes   = 8_000
	subagentToolResultCap  = 12_000 // larger per-file view than parent's 8k
	subagentTurnToolBudget = 50_000 // generous: HardMax is the real ceiling
	subagentToolResultTTL  = -1     // never evict; ephemeral ctx is a license to keep
	subagentMaxToolIter    = 16     // hard loop backstop: force a summary after this many tool round-trips
)

// SubagentSummary is the structured return value of dispatchSubagent.
// The parent acts on this blind to raw content, so every gap must be visible.
type SubagentSummary struct {
	Objective   string        `json:"objective"`
	Status      string        `json:"status,omitempty"` // "incomplete" when the subagent hit a budget/iteration wall; empty = complete
	Findings    []Finding     `json:"findings,omitempty"`
	Checked     []CheckedItem `json:"checked,omitempty"`
	Skipped     []SkippedItem `json:"skipped,omitempty"`
	Uncertainty []string      `json:"uncertainty,omitempty"`
	SpillRefs   []SpillRef    `json:"spill_refs,omitempty"`
}

// Finding is one discrete result the parent should consider acting on.
type Finding struct {
	Summary  string `json:"summary"`  // ≤200 chars
	Location string `json:"location"` // "file:line" or spill path — must be actionable
	Kind     string `json:"kind"`     // match|pattern|error|fact|ref|parse-error
	Weight   string `json:"weight"`   // high|medium|low
}

// CheckedItem records a file that was examined and at what fidelity.
type CheckedItem struct {
	Path   string `json:"path"`
	SizeK  int    `json:"size_k"` // chars read ÷ 1000 — coverage signal for the parent
	Status string `json:"status"` // full|truncated|stub-only
}

// SkippedItem records something the subagent could not or chose not to read.
type SkippedItem struct {
	Path   string `json:"path,omitempty"`
	Reason string `json:"reason"` // budget-exhausted|inaccessible|out-of-scope|declined
}

// SpillRef is a pointer to spilled content the parent can read_file if needed.
type SpillRef struct {
	ToolName string `json:"tool_name"`
	Path     string `json:"path"`   // cache path written by capToolResult/stubToolResult
	SizeK    int    `json:"size_k"` // size hint so parent can weigh the retrieval cost
}

// Render marshals the summary to JSON, trimming findings from the tail one by one
// until it fits under 4000 chars. Always returns valid JSON ≤ 4000 chars.
func (s SubagentSummary) Render() string {
	for {
		b, err := json.Marshal(s)
		if err != nil {
			return `{"objective":"","findings":[{"summary":"render error","location":"","kind":"error","weight":"low"}]}`
		}
		if len(b) <= 4000 {
			return string(b)
		}
		if len(s.Findings) > 1 {
			s.Findings = s.Findings[:len(s.Findings)-1]
			continue
		}
		// Cannot trim further — hard truncate (rare: even a single finding is >4k).
		return string(b[:3997]) + "…"
	}
}

// subagentSystemPrompt instructs the subagent to emit only a SubagentSummary JSON.
const subagentSystemPrompt = `You are a focused discovery subagent. Use list_dir and find_files to navigate, search_files to grep, and read_file (offset/limit for large files) or read_file_full (complete file in one call, up to ~256 KB) to read, then respond with ONLY a valid JSON object — no prose, no markdown, no code fences.

Required schema (omit empty arrays):
{"objective":"<task echoed>","status":"<omit unless incomplete>","findings":[{"summary":"<≤200 chars>","location":"<file:line or path>","kind":"match|pattern|error|fact|ref","weight":"high|medium|low"}],"checked":[{"path":"<path>","size_k":<int>,"status":"full|truncated|stub-only"}],"skipped":[{"path":"<path>","reason":"budget-exhausted|inaccessible|out-of-scope|declined"}],"uncertainty":["<≤100 chars>"],"spill_refs":[{"tool_name":"<name>","path":"<spill-path>","size_k":<int>}]}

Rules:
- Emit ONLY the JSON object. Nothing before {, nothing after }.
- Keep total rendered JSON under 4000 characters.
- List every file you examined in checked[]; set status: full|truncated|stub-only.
- List files you could not reach in skipped[].
- Make gaps explicit in uncertainty[] — do not imply complete coverage you did not achieve.`

// subagentRetryPrompt is sent on parse failure to request a clean JSON retry.
const subagentRetryPrompt = `Your previous response was not valid JSON. Respond with ONLY the JSON object — no text before {, no text after }. Start directly with { and end with }.`

// extractJSON strips markdown fences and extracts the outermost {...} object from s.
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	for _, fence := range []string{"```json", "```"} {
		if i := strings.Index(s, fence); i >= 0 {
			inner := s[i+len(fence):]
			if j := strings.Index(inner, "```"); j >= 0 {
				inner = inner[:j]
			}
			s = strings.TrimSpace(inner)
			break
		}
	}
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end <= start {
		return s
	}
	return s[start : end+1]
}

// subagentProgressOut returns a writer that forwards subagent output to the
// TUI's subagent sidebar panel, tagged with the dispatch's chatID so the TUI
// can route concurrent streams to the right tab. Returns io.Discard when the
// parent app has no EventSink set (CLI invocation or tests).
func subagentProgressOut(parent *App, chatID string) io.Writer {
	if parent == nil || parent.EventSink == nil {
		return io.Discard
	}
	return NewProgWriter(func(m StreamChunkMsg) {
		parent.sendEvent(SubagentChunkMsg{ChatID: chatID, Text: m.Text})
	})
}

// readOnlyConfirmer auto-approves read actions and silently declines mutations.
// discoveryTools contains no mutating tools; this is belt-and-suspenders.
func readOnlyConfirmer() Confirmer {
	return func(_, _, _ string, readAction bool) bool { return readAction }
}

// ensureSubagentConsent runs the egress consent gate for resolvedBackend on
// behalf of a subagent dispatch. Returns true when the dispatch may proceed
// (backend is local, already consented, or the user just approved).
//
// MAIN GOROUTINE ONLY: this reads and writes a.consentedBackends and may fire
// an interactive Confirm prompt. It must be called before any dispatch worker
// goroutine spawns — never from inside one. Workers receive an immutable
// snapshot of the consent state instead (see dispatchSubagent).
func (a *App) ensureSubagentConsent(resolvedBackend string) bool {
	if resolvedBackend == "" || !IsExternalBackend(a.BackendList, a.Cfg, resolvedBackend) {
		return true
	}
	if a.consentedBackends != nil && a.consentedBackends[resolvedBackend] {
		return true
	}
	detail := fmt.Sprintf(
		"This subagent's briefing (including session context, grounding, and "+
			"learned notes) will be sent to external backend %q. Proceed?",
		resolvedBackend)
	if !a.Confirm("external_backend",
		"⚠ Send subagent context to external backend "+resolvedBackend+"?",
		detail, false) {
		return false
	}
	if a.consentedBackends == nil {
		a.consentedBackends = make(map[string]bool)
	}
	a.consentedBackends[resolvedBackend] = true
	return true
}

// declinedSubagentSummary is the summary returned when the egress gate for a
// subagent dispatch is declined: no request is made, the parent learns why.
func declinedSubagentSummary(task, resolvedBackend string) SubagentSummary {
	return SubagentSummary{
		Objective:   task,
		Uncertainty: []string{"subagent declined: external backend " + resolvedBackend + " not consented"},
	}
}

// dispatchSubagentGated is the consent-then-dispatch sequence used by the
// sequential (single-dispatch) path: run the egress gate, then dispatch.
// MAIN GOROUTINE ONLY (it calls ensureSubagentConsent). The parallel path
// must instead call ensureSubagentConsent once in its prepare phase and then
// invoke dispatchSubagent directly from workers.
func (a *App) dispatchSubagentGated(ctx context.Context, task string, progressOut io.Writer, resolvedBackend string, chatID ...string) (SubagentSummary, []proxy.GroundingEntry, int, string) {
	if !a.ensureSubagentConsent(resolvedBackend) {
		return declinedSubagentSummary(task, resolvedBackend), nil, 0, ""
	}
	return a.dispatchSubagent(ctx, task, progressOut, resolvedBackend, chatID...)
}

// dispatchSubagent runs a bounded read-only discovery subagent for task and
// returns a structured SubagentSummary. The subagent:
//   - shares the parent's Executor (same filesystem, read-only toolset only)
//   - uses a fresh Client (new ChatID, NoMemoryWrite=true)
//   - routes to resolvedBackend (X-Ilm-Backend); egress consent must already
//     have been granted by the caller via ensureSubagentConsent — this
//     function never prompts and never touches a.consentedBackends
//   - runs cap/evict/compact/enforceHardMax internally — bounded by its own HardMaxBytes
//   - is completely silent (Out=io.Discard)
//
// CONCURRENCY: safe to call from multiple goroutines at once, provided the
// caller ran ensureSubagentConsent on the main goroutine first. It reads only
// immutable parent fields (Client config, Cfg, Exec, BackendList) and builds
// a fully isolated child App. The child receives a snapshot COPY of the
// consent map, so no goroutine ever writes parent-shared state.
//
// Only the ≤4k rendered summary enters the parent's transcript; raw file content
// examined by the subagent never touches the parent's Conv.
//
// The fourth return value is the X-Ilm-Backend-Used header from the subagent's
// last Stream call (empty when the proxy didn't send it).
func (a *App) dispatchSubagent(ctx context.Context, task string, progressOut io.Writer, resolvedBackend string, chatID ...string) (SubagentSummary, []proxy.GroundingEntry, int, string) {
	subChatID := NewChatID()
	if len(chatID) > 0 && chatID[0] != "" {
		subChatID = chatID[0]
	}
	subClient := &proxy.Client{
		BaseURL:         a.Client.BaseURL,
		Model:           a.Client.Model,
		Kind:            a.Client.Kind,            // endpoint kind gates the proxy-specific request shape
		ConfiguredModel: a.Client.ConfiguredModel, // plain endpoints always send the configured model
		Temperature:     a.Client.Temperature,
		TopP:            a.Client.TopP,
		MaxTokens:       a.Client.MaxTokens,
		ChatID:          subChatID,
		AuthHeader:      a.Client.AuthHeader,
		NoMemoryWrite:   true,
		HTTP:            a.Client.HTTP,
		Backend:         resolvedBackend, // propagate X-Ilm-Backend (the P31 bug fix)
		MaxRequestBytes: a.Client.MaxRequestBytes,
	}

	cfg := config.DefaultConfig()
	cfg.HardMaxBytes = subagentHardMaxBytes
	cfg.CompactAt = subagentCompactAt
	cfg.KeepBytes = subagentKeepBytes
	cfg.SummaryBytes = subagentSummaryBytes
	cfg.ToolResultCap = subagentToolResultCap
	cfg.TurnToolBudget = subagentTurnToolBudget
	cfg.ToolResultTTL = subagentToolResultTTL
	cfg.MaxToolIterations = subagentMaxToolIter
	if a.subMaxToolIter > 0 {
		cfg.MaxToolIterations = a.subMaxToolIter
	}

	if progressOut == nil {
		progressOut = io.Discard
	}
	// Snapshot the parent's consent map. The child must see the consent that
	// ensureSubagentConsent just granted (so sub.Send's egress check doesn't
	// re-prompt), but it must NOT share the parent's map: concurrent dispatch
	// workers reading a map the parent could write is a fatal data race.
	consentSnapshot := make(map[string]bool, len(a.consentedBackends))
	for k, v := range a.consentedBackends {
		consentSnapshot[k] = v
	}

	sub := &App{
		Cfg:               cfg,
		Client:            subClient,
		Exec:              a.Exec,
		Tools:             tools.DiscoveryTools(a.Exec.Cwd()),
		Confirm:           readOnlyConfirmer(),
		Out:               progressOut,
		Session:           nil,
		ToolCache:         map[string]bool{},
		IsSubagent:        true,
		pinUserMessage:    true, // pin the task instruction so it survives compaction
		SelectedBackend:   resolvedBackend,
		BackendList:       a.BackendList,
		consentedBackends: consentSnapshot,
	}
	sub.Conv = []proxy.Message{{Role: "system", Content: StrPtr(subagentSystemPrompt), Pinned: true}}

	raw, err := sub.Send(ctx, task)
	grounding := sub.Client.Grounding()
	ctxSize := TranscriptSize(sub.Conv)
	usedBackend := sub.Client.LastUsedBackend()

	// Capture exhaustion from the first Send before the retry path runs.
	// The retry Send resets sub.exhausted at its start, so we must OR this
	// with whatever the retry produces — otherwise a first-Send exhaustion
	// is silently masked by a clean retry.
	exhaustedFirstSend := sub.exhausted
	// Same capture-before-retry pattern for the path-confinement breaker: the
	// retry Send does not reset confinementTripped (only Send's exhausted
	// reset is inside Send itself — confinementTripped is set once per turn
	// and never cleared, so this is belt-and-suspenders, not strictly needed,
	// but keeps the two flags symmetric and equally safe against future
	// changes to either reset path).
	confinementFirstSend := sub.confinementTripped
	confinementPathsFirstSend := sub.confinementPathsHit

	if err != nil {
		return SubagentSummary{
			Objective:   task,
			Findings:    []Finding{{Summary: Truncate("subagent error: "+err.Error(), 200), Kind: "error", Weight: "low"}},
			Uncertainty: []string{"subagent failed with error"},
		}, grounding, ctxSize, usedBackend
	}

	// Parse — two-stage fallback
	var summary SubagentSummary
	parseErr := json.Unmarshal([]byte(extractJSON(raw)), &summary)
	if parseErr != nil {
		// Drop tools for the retry: it asks only for clean JSON, so the model must
		// not be able to spin up another tool loop instead of answering.
		sub.Tools = nil
		retryRaw, retryErr := sub.Send(ctx, subagentRetryPrompt)
		if retryErr == nil {
			parseErr = json.Unmarshal([]byte(extractJSON(retryRaw)), &summary)
		}
		if retryErr != nil || parseErr != nil {
			summary = SubagentSummary{
				Objective:   task,
				Findings:    []Finding{{Summary: Truncate(raw, 200), Kind: "parse-error", Weight: "low"}},
				Uncertainty: []string{"summary schema not followed"},
			}
		}
	}
	if summary.Objective == "" {
		summary.Objective = task
	}

	// Truthful return on the path-confinement breaker: distinct from generic
	// budget exhaustion below — this is a deterministic, structural dead end
	// (a path outside the sandbox root can never become readable on retry),
	// not a "ran out of budget, might succeed with more room" situation. The
	// parent gets the exact unreachable path(s) so it knows re-dispatching the
	// same task narrower will NOT help — the parent must either accept the
	// gap or dispatch against a different, reachable path/repo.
	if confinementFirstSend || sub.confinementTripped {
		summary.Status = "incomplete"
		paths := confinementPathsFirstSend
		if len(sub.confinementPathsHit) > 0 {
			paths = sub.confinementPathsHit
		}
		if len(paths) == 0 {
			summary.Skipped = append(summary.Skipped, SkippedItem{Reason: "inaccessible"})
		} else {
			for _, p := range paths {
				summary.Skipped = append(summary.Skipped, SkippedItem{Path: p, Reason: "inaccessible"})
			}
		}
		note := "path(s) outside the sandboxed workspace — permanently unreachable, not a budget issue: re-dispatching narrower will not help"
		if len(summary.Uncertainty) == 0 || summary.Uncertainty[len(summary.Uncertainty)-1] != note {
			summary.Uncertainty = append(summary.Uncertainty, note)
		}
	} else if exhaustedFirstSend || sub.exhausted {
		// Truthful return on generic exhaustion: if the subagent hit its
		// iteration limit or enforceHardMax shed content during EITHER the
		// first Send or the retry, override the status to "incomplete". The
		// model's response may look normal but be based on a lobotomized
		// context. The parent must know the subagent ran out of budget so it
		// can re-dispatch narrower or take over, rather than trusting
		// potentially-incomplete findings.
		summary.Status = "incomplete"
		summary.Skipped = append(summary.Skipped, SkippedItem{
			Reason: "budget-exhausted",
		})
		if len(summary.Uncertainty) == 0 || summary.Uncertainty[len(summary.Uncertainty)-1] != "subagent hit budget/iteration limit — findings may be incomplete" {
			summary.Uncertainty = append(summary.Uncertainty, "subagent hit budget/iteration limit — findings may be incomplete")
		}
	}

	return summary, grounding, ctxSize, usedBackend
}
