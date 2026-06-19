package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"wakil/internal/config"
	"wakil/internal/proxy"
	"wakil/internal/tools"
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
	Path   string `json:"path"`
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
const subagentSystemPrompt = `You are a focused discovery subagent. Use list_dir and find_files to navigate, search_files to grep, and read_file to read (pass offset/limit for large files), then respond with ONLY a valid JSON object — no prose, no markdown, no code fences.

Required schema (omit empty arrays):
{"objective":"<task echoed>","findings":[{"summary":"<≤200 chars>","location":"<file:line or path>","kind":"match|pattern|error|fact|ref","weight":"high|medium|low"}],"checked":[{"path":"<path>","size_k":<int>,"status":"full|truncated|stub-only"}],"skipped":[{"path":"<path>","reason":"budget-exhausted|inaccessible|out-of-scope|declined"}],"uncertainty":["<≤100 chars>"],"spill_refs":[{"tool_name":"<name>","path":"<spill-path>","size_k":<int>}]}

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
// TUI's subagent sidebar panel. Returns io.Discard when the parent app has no
// EventSink set (CLI invocation or tests).
func subagentProgressOut(parent *App) io.Writer {
	if parent == nil || parent.EventSink == nil {
		return io.Discard
	}
	return NewProgWriter(func(m StreamChunkMsg) {
		parent.sendEvent(SubagentChunkMsg{Text: m.Text})
	})
}

// readOnlyConfirmer auto-approves read actions and silently declines mutations.
// discoveryTools contains no mutating tools; this is belt-and-suspenders.
func readOnlyConfirmer() Confirmer {
	return func(_, _, _ string, readAction bool) bool { return readAction }
}

// dispatchSubagent runs a bounded read-only discovery subagent for task and
// returns a structured SubagentSummary. The subagent:
//   - shares the parent's Executor (same filesystem, read-only toolset only)
//   - uses a fresh Client (new ChatID, NoMemoryWrite=true)
//   - routes to resolvedBackend (X-Ilm-Backend), inheriting the parent's
//     consent map so no duplicate egress prompt fires inside the sub-turn
//   - runs cap/evict/compact/enforceHardMax internally — bounded by its own HardMaxBytes
//   - is completely silent (Out=io.Discard)
//
// Only the ≤4k rendered summary enters the parent's transcript; raw file content
// examined by the subagent never touches the parent's Conv.
//
// The fourth return value is the X-Ilm-Backend-Used header from the subagent's
// last Stream call (empty when the proxy didn't send it).
func (a *App) dispatchSubagent(ctx context.Context, task string, progressOut io.Writer, resolvedBackend string, chatID ...string) (SubagentSummary, []proxy.GroundingEntry, int, string) {
	// Egress consent: if the subagent's backend is external, gate via the parent's
	// confirm mechanism (shared session consent, never auto-approved).
	if resolvedBackend != "" && IsExternalBackend(a.BackendList, a.Cfg, resolvedBackend) {
		if a.consentedBackends == nil || !a.consentedBackends[resolvedBackend] {
			detail := fmt.Sprintf(
				"This subagent's briefing (including session context, grounding, and "+
					"learned notes) will be sent to external backend %q. Proceed?",
				resolvedBackend)
			if !a.Confirm("external_backend",
				"⚠ Send subagent context to external backend "+resolvedBackend+"?",
				detail, false) {
				return SubagentSummary{
					Objective:   task,
					Uncertainty: []string{"subagent declined: external backend " + resolvedBackend + " not consented"},
				}, nil, 0, ""
			}
			if a.consentedBackends == nil {
				a.consentedBackends = make(map[string]bool)
			}
			a.consentedBackends[resolvedBackend] = true
		}
	}

	subChatID := NewChatID()
	if len(chatID) > 0 && chatID[0] != "" {
		subChatID = chatID[0]
	}
	subClient := &proxy.Client{
		BaseURL:         a.Client.BaseURL,
		Model:           a.Client.Model,
		ChatID:          subChatID,
		AuthHeader:      a.Client.AuthHeader,
		NoMemoryWrite:   true,
		HTTP:            a.Client.HTTP,
		Backend:         resolvedBackend, // propagate X-Ilm-Backend (the P31 bug fix)
		MaxRequestBytes: a.Client.MaxRequestBytes,
	}

	cfg := config.DefaultConfig()
	cfg.HardMaxBytes      = subagentHardMaxBytes
	cfg.CompactAt         = subagentCompactAt
	cfg.KeepBytes         = subagentKeepBytes
	cfg.SummaryBytes      = subagentSummaryBytes
	cfg.ToolResultCap     = subagentToolResultCap
	cfg.TurnToolBudget    = subagentTurnToolBudget
	cfg.ToolResultTTL     = subagentToolResultTTL
	cfg.MaxToolIterations = subagentMaxToolIter

	if progressOut == nil {
		progressOut = io.Discard
	}
	sub := &App{
		Cfg:             cfg,
		Client:          subClient,
		Exec:            a.Exec,
		Tools:           tools.DiscoveryTools(a.Exec.Cwd()),
		Confirm:         readOnlyConfirmer(),
		Out:             progressOut,
		Session:         nil,
		ToolCache:       map[string]bool{},
		IsSubagent:      true,
		SelectedBackend: resolvedBackend,
		BackendList:     a.BackendList,
		// Share the parent's consent map — consent granted for the subagent's
		// backend above is immediately visible here, preventing a duplicate prompt
		// inside sub.Send when it repeats the egress check.
		consentedBackends: a.consentedBackends,
	}
	sub.Conv = []proxy.Message{{Role: "system", Content: StrPtr(subagentSystemPrompt)}}

	raw, err := sub.Send(ctx, task)
	grounding := sub.Client.Grounding()
	ctxSize := TranscriptSize(sub.Conv)
	usedBackend := sub.Client.LastUsedBackend()

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
	return summary, grounding, ctxSize, usedBackend
}
