package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/counsel"
	"github.com/treeol/wakil/internal/diag"
	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/safe"
	"github.com/treeol/wakil/internal/workflow"
)

// Mashūra counsel tools. The single oracle__ask tool became this family: each
// tool differs only in how Wakil deterministically assembles the briefing from
// authoritative, Wakil-known context. The model supplies intent; Wakil supplies
// the truth — counsel quality tracks briefing quality, not question phrasing.
//
// They are explicit-call only (the model chooses which), every one is gated
// through the normal confirm flow (auto-approved in /auto mode like other tool
// calls, with a visible ⚡ auto note carrying panel/question/briefing), all share
// the same timeout/cost config and the same shown-vs-recalled honesty
// (oracleSystemPrompt), and each validates fail-closed (missing required
// intent → ERROR, no call made).

const (
	// Source-reading caps. Wakil reads paths from disk; the model never pastes content.
	mashuraSourceFileCap   = 32 * 1024  // per-file bytes before line-level clip
	mashuraSourcesTotal    = 150 * 1024 // total bytes across all sources in one briefing
	mashuraDirFileCap      = 20         // max source files expanded from one directory
	mashuraToolBriefingCap = 200 * 1024 // overall briefing cap for tool-initiated calls

	// mashuraDebugTraceK is how many recent tool calls a debug briefing includes.
	mashuraDebugTraceK = 8
	// mashuraCheckMaxTokens is the default cap for the lightweight check tool when
	// no per-tool override is configured — far less than a full review.
	mashuraCheckMaxTokens = 1024
)

// PathRange selects a line span of a file to include in a briefing.
// StartLine and EndLine are 1-indexed and inclusive; 0 means start/end of file.
type PathRange struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

// mashuraSourceMeta is the read receipt for one file sent to the oracle.
type mashuraSourceMeta struct {
	Path    string
	Bytes   int
	Lines   int  // lines shown
	Total   int  // total lines in file
	Clipped bool // clipped to per-file cap
	Omitted bool // skipped due to total budget
}

// binaryExts are file extensions Wakil skips during directory expansion.
var binaryExts = map[string]bool{
	".o": true, ".a": true, ".so": true, ".dylib": true, ".exe": true,
	".bin": true, ".pyc": true, ".class": true, ".jar": true, ".wasm": true,
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".ico": true,
	".mp4": true, ".mp3": true, ".wav": true, ".pdf": true,
	".zip": true, ".tar": true, ".gz": true, ".tgz": true, ".bz2": true,
}

// mashuraToolDefs returns the four counsel tool definitions, advertised together
// by buildTools when OracleEnabled and the API key is present. Intentionally
// absent from discoveryTools so subagents never receive them.
func mashuraToolDefs() []proxy.Tool {
	str := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "string", "description": desc}
	}
	strArr := func(desc string) map[string]interface{} {
		return map[string]interface{}{
			"type":        "array",
			"items":       map[string]interface{}{"type": "string"},
			"description": desc,
		}
	}
	obj := func(props map[string]interface{}, required ...string) json.RawMessage {
		m := map[string]interface{}{"type": "object", "properties": props}
		if len(required) > 0 {
			m["required"] = required
		}
		b, _ := json.Marshal(m)
		return b
	}
	tool := func(name, desc string, params json.RawMessage) proxy.Tool {
		return proxy.Tool{Type: "function", Function: proxy.ToolFunction{Name: name, Description: desc, Parameters: params}}
	}
	const shared = " Counsel from a more capable external AI; use sparingly — the call is gated and costs money, and the answer is an OPINION to evaluate, not ground truth. Wakil attaches the authoritative context (task, phase, recent step log, named files) automatically — you supply only the intent, never paste what Wakil can read."

	panelParam := str("Optional: named panel to use for this call (overrides the default panel configured for this tool). Omit unless the user explicitly named a panel.")
	pathsParam := strArr("Optional: file or directory paths for Wakil to read from disk and attach as sources. Wakil reads current bytes — never paste content yourself.")
	pathRangesParam := map[string]interface{}{
		"type":        "array",
		"description": "Optional: line ranges to read. Each selects a span of one file — use when only part of a large file is relevant.",
		"items": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":       map[string]interface{}{"type": "string", "description": "File path"},
				"start_line": map[string]interface{}{"type": "integer", "description": "First line (1-indexed; omit for file start)"},
				"end_line":   map[string]interface{}{"type": "integer", "description": "Last line inclusive (1-indexed; omit for file end)"},
			},
			"required": []interface{}{"path"},
		},
	}
	return []proxy.Tool{
		tool("mashura__review",
			"Get counsel on the current plan and work against the task. Wakil reads the named paths from disk and attaches them alongside task, findings, plan, and step log."+shared,
			obj(map[string]interface{}{
				"focus":       str("Optional: what to focus the review on. Defaults to a full review against the task."),
				"paths":       pathsParam,
				"path_ranges": pathRangesParam,
				"panel":       panelParam,
			})),
		tool("mashura__debug",
			"Diagnose the root cause of a failure. Wakil reads the named paths from disk and attaches them alongside the recent tool calls."+shared,
			obj(map[string]interface{}{
				"symptom":     str("What is going wrong — the error, the unexpected behavior, and what you expected instead."),
				"paths":       pathsParam,
				"path_ranges": pathRangesParam,
				"panel":       panelParam,
			}, "symptom")),
		tool("mashura__decide",
			"Get counsel on a decision. Wakil attaches the task and findings as the constraints the decision lives under."+shared,
			obj(map[string]interface{}{
				"question":    str("The decision to make."),
				"options":     str("The options you are weighing, one per line or comma-separated."),
				"paths":       pathsParam,
				"path_ranges": pathRangesParam,
				"panel":       panelParam,
			}, "question", "options")),
		tool("mashura__check",
			"Lightweight single-claim verification — cheaper and lower max_tokens than a full review. Wakil reads the evidence files from disk."+shared,
			obj(map[string]interface{}{
				"claim":       str("The single claim to verify."),
				"paths":       pathsParam,
				"path_ranges": pathRangesParam,
				"panel":       panelParam,
			}, "claim")),
	}
}

// handleMashura dispatches a mashura__* tool call: it parses the tool-specific
// args, resolves the target panel (from config + optional per-call override),
// gates the entire panel with a single confirm prompt, and returns a PLACEHOLDER
// immediately — the panel runs on a worker goroutine and its result
// is injected into the conversation before the next model request, or can be
// retrieved early via check_pending. Cost is committed at worker terminal
// completion; grounding is committed at delivery (drain/check_pending) — both
// exactly once. The legacy oracle__ask alias is handled as a review.
//
// Fallbacks that keep the call synchronous (loudly): the async registry is full
// (asyncMaxActive reached) or stopping. Auto-counsel (maybeSuggestDebug) uses
// runMashuraCore directly with sync=true — its assistant+tool-pair injection
// and cap semantics are unchanged.
func (a *App) handleMashura(ctx context.Context, name string, tc proxy.ToolCall) string {
	// Subagents keep today's synchronous behavior — they have no turn loop to
	// deliver async completions into.
	return a.runMashuraCore(ctx, name, tc, a.IsSubagent)
}

// runMashuraCore is the shared mashūra execution path. sync=true runs the
// panel inline and returns the formatted result (auto-counsel, registry-full
// fallback, subagents); sync=false enqueues the panel on the async registry
// and returns a placeholder pointing at the op id.
func (a *App) runMashuraCore(ctx context.Context, name string, tc proxy.ToolCall, sync bool) string {
	// Consume the auto-counsel skip-gate flag BEFORE any code that can return
	// early with an error. Previously this was consumed after resolvePanel and
	// mashuraPanelKeys, which meant a panel/key error would skip the clearing
	// and leave the flag set for the next call — a leak that could bypass the
	// confirm gate on a subsequent unrelated call. Moving it to the very first
	// line guarantees the flag is always consumed exactly once per invocation,
	// regardless of which error path is taken.
	skipGate := a.autoCounselSkipGate
	a.autoCounselSkipGate = false

	// Resolve the panel config and preflight API keys BEFORE any tool-specific
	// work (briefing build reads sources from disk). An unavailable selection
	// must fail with zero source reads — previously the briefing was assembled
	// first, wasting evidence gathering on a call that could never run.
	panelOverride := mashuraPanelArg(tc)
	panelName, panel, panelOK := a.resolvePanel(name, panelOverride)
	if !panelOK {
		return "ERROR: panel " + panelName + " not found or has no models"
	}
	apiKeys, keyErr := a.mashuraPanelKeys(panel)
	if keyErr != nil {
		return "ERROR: " + a.mashuraKeyErrorMessage(name, panelName, keyErr)
	}

	var question, briefing string
	var receipts []mashuraSourceMeta
	var err error
	switch name {
	case "mashura__debug":
		question, briefing, receipts, err = a.mashuraDebug(ctx, tc)
	case "mashura__decide":
		question, briefing, receipts, err = a.mashuraDecide(ctx, tc)
	case "mashura__check":
		question, briefing, receipts, err = a.mashuraCheck(ctx, tc)
	default: // mashura__review and the legacy oracle__ask alias
		question, briefing, receipts, err = a.mashuraReview(ctx, tc)
	}
	if err != nil {
		return "ERROR: " + err.Error()
	}

	maxTokens := a.mashuraMaxTokensFor(name)
	detail := counsel.PanelDetail(panelName, panel.Models, panel.Mode, question, briefing, panel.ServerTools)
	// Fail-closed: a nil Confirm must never silently bypass the gate. Production
	// wiring (TUI, headless, host turn) always sets Confirm; nil here is a
	// wiring bug and an error is the correct outcome — a panic would also be
	// acceptable, but an ERROR string keeps the tool-result contract uniform.
	if !skipGate {
		if a.Confirm == nil {
			return "ERROR: no confirmer configured — refusing to send to external AI (wiring bug)"
		}
		if !a.Confirm(name, "Send to external AI?", detail, false) {
			return "[declined by user]"
		}
	}

	// Log read receipts now that the user approved (pre-panel, post-gate).
	a.mashuraLogReceipts(ctx, receipts)

	// Run panel; for fusion mode this is a single OpenRouter call.
	// Keys, briefing snapshot, and config are captured NOW (issue time) — the
	// worker sends exactly what was approved (TOCTOU bound by the pre-gate
	// briefing build).
	ccfg := counsel.PanelCallConfig{
		MaxTokens:          maxTokens,
		TimeoutSeconds:     a.Cfg.OracleTimeoutSeconds,
		AnthropicEndpoint:  a.Cfg.OracleEndpoint,
		FusionJudge:        panel.FusionJudge,
		FusionMaxToolCalls: panel.FusionMaxToolCalls,
		ServerTools:        panel.ServerTools,
		WebSearchEngine:    panel.WebSearchEngine,
		ServerToolMaxCalls: panel.ServerToolMaxCalls,
	}
	models, mode := panel.Models, panel.Mode

	if sync {
		results := counsel.RunPanel(ctx, models, mode, question, briefing, ccfg, apiKeys)
		a.recordMashuraResults(results)
		return counsel.FormatPanelResult(results)
	}

	op, reason := a.enqueueAsyncOpJob(name, panelName, a.mashuraCallTimeout(mode), func(opID, originChatID string) (string, []counselUsageRec, []string, error) {
		// Detached from the turn context on purpose: the paid call was approved
		// and should complete even if the turn is cancelled (D-4;
		// cancellation support is a follow-up). Layer the provider timeout using
		// the SAME authoritative value the watchdog uses (mashuraTimeout), so a
		// "0 = no timeout" config still yields the bounded default — a hung panel
		// can never run unbounded. Debate mode gets 2× up front so
		// runDebate's derived 2× wall-time deadline isn't clipped to 1×.
		callCtx, cancel := context.WithTimeout(context.Background(), a.mashuraCallTimeout(mode))
		defer cancel()

		// Live progress: a bounded, drop-on-full status channel + a SINGLE
		// forwarder goroutine drain it to sendEvent. This decouples the paid
		// panel call from a blocked/stalled Bubble Tea sink: a member goroutine
		// does a non-blocking channel push, so it can never block wg.Wait /
		// terminalization / slot release (mirrors the Phase 1 publishAsyncOp
		// "UI must not stall the async slot" invariant). The single forwarder
		// preserves per-op FIFO.
		//
		// After RunPanel returns we close the channel and WAIT for the forwarder
		// under a bound (asyncJobChunkDrainMax). In the normal case all Chunk
		// sends complete before terminalization (before publishAsyncOp's Done).
		// If the sink is wedged (Program.Send blocks past the bound), we abandon
		// the remaining drain and terminalize anyway — the TUI's t.done guard
		// already discards any Chunk that arrives after Done, so this satisfies
		// "UI can never stall terminalization" while preserving Chunk-before-Done
		// ordering for every Chunk the sink actually accepted.
		type statusMsg struct {
			round int
			model string
			kind  counsel.PanelMemberEventKind
		}
		const statusCap = 256
		chunks := make(chan statusMsg, statusCap)
		fwdClosed := make(chan struct{})
		safe.Go("mashura-chunk-fwd", func() {
			defer close(fwdClosed)
			for sm := range chunks {
				line := renderAsyncJobChunkText(counsel.PanelMemberEvent{Round: sm.round, Model: sm.model, Kind: sm.kind})
				if line == "" {
					continue // unknown/reserved kind — skip, no garbage status line
				}
				a.sendEvent(AsyncJobChunkMsg{OpID: opID, OriginChatID: originChatID, Text: line})
			}
		})
		// Ensure cleanup on ANY path (including a panic in RunPanel/assembly):
		// close the channel, then wait for the forwarder ONLY under a bound.
		// If the sink is wedged (Program.Send blocks) we do NOT wait further —
		// abandoning here guarantees a stuck UI can never stall terminalization
		// or the async slot. The forwarder goroutine may remain blocked in the
		// sink (expendable UI telemetry); any Chunk it eventually delivers is
		// dropped by the TUI's done guard. There is deliberately NO
		// fwdDone.Wait() after the timeout — that would reintroduce the stall.
		defer func() {
			close(chunks)
			select {
			case <-fwdClosed:
				// Forwarder drained; every accepted Chunk was sent before Done.
			case <-time.After(asyncJobChunkDrainMax):
				// Sink wedged — abandon remaining chunks without waiting.
			}
		}()
		// Set the observer on a COPY of ccfg (never mutates the caller's ccfg,
		// so the synchronous fallback below reusing ccfg emits NO chunks).
		liveCfg := ccfg
		liveCfg.OnMemberEvent = func(ev counsel.PanelMemberEvent) {
			// Non-blocking push; drop on full (chunks are ephemeral telemetry).
			sm := statusMsg{round: ev.Round, model: ev.Model, kind: ev.Kind}
			select {
			case chunks <- sm:
			default:
			}
		}

		results := counsel.RunPanel(callCtx, models, mode, question, briefing, liveCfg, apiKeys)
		// Deferred cleanup (close + bounded drain) runs here on the way out,
		// guaranteeing all accepted Chunk sends happen-before terminalization.

		recs := make([]counselUsageRec, 0, len(results))
		var okModels []string
		allErr := true
		for _, r := range results {
			if r.Usage.InputTokens > 0 || r.Usage.OutputTokens > 0 {
				recs = append(recs, counselUsageRec{Model: r.Model, Usage: r.Usage})
			}
			if r.Err == nil {
				okModels = append(okModels, r.Model)
				allErr = false
			}
		}
		if allErr && len(results) > 0 {
			return counsel.FormatPanelResult(results), recs, okModels, fmt.Errorf("all panel members failed (see check_pending result for details)")
		}
		return counsel.FormatPanelResult(results), recs, okModels, nil
	})
	if reason != "" {
		// Registry full or stopping — LOUD synchronous fallback (never
		// silent). Detached context: an approved paid call completes even if
		// the turn is cancelled, consistent with the async worker path.
		why := "async registry full"
		if reason == "stopping" {
			why = "session shutting down"
		}
		fmt.Fprintln(a.Out, Dim("· "+why+" — running mashūra synchronously"))
		// Same bounded provider timeout as the async worker path (mashuraTimeout
		// never returns 0 — ), with the debate 2× up-front so runDebate's
		// derived 2× wall-time deadline isn't clipped to 1×.
		fbCtx, cancel := context.WithTimeout(context.Background(), a.mashuraCallTimeout(mode))
		defer cancel()
		results := counsel.RunPanel(fbCtx, models, mode, question, briefing, ccfg, apiKeys)
		a.recordMashuraResults(results)
		return counsel.FormatPanelResult(results)
	}
	return fmt.Sprintf("queued as %s (%s, panel %s) — the panel is running now; once it completes, its result will be injected into context at the next model request while this turn continues. Keep working; call check_pending(%q) any time to retrieve it early.",
		op.id, name, panelName, op.id)
}

// recordMashuraResults commits cost + grounding for a synchronously-run panel
// (auto-counsel, registry-full fallback, subagents). Exactly mirrors the
// async drain path's commitAsyncEffects.
func (a *App) recordMashuraResults(results []counsel.PanelMemberResult) {
	// Record cost per model; add grounding entry per successful member.
	// Record usage even on error — providers bill for truncated/failed calls.
	for _, r := range results {
		if r.Usage.InputTokens > 0 || r.Usage.OutputTokens > 0 {
			a.RecordOracleCostFor(r.Model, r.Usage)
		}
		if r.Err == nil {
			a.addExternalGrounding(proxy.GroundingEntry{Type: "oracle", Label: r.Model})
		}
	}
}

// mashuraPanelArg extracts the optional "panel" field from a tool call's JSON
// arguments. Returns "" when absent or unparseable.
func mashuraPanelArg(tc proxy.ToolCall) string {
	var args struct {
		Panel string `json:"panel"`
	}
	_ = json.Unmarshal([]byte(tc.Function.Arguments), &args) // optional field
	return strings.TrimSpace(args.Panel)
}

// mashuraKeyErrorMessage enriches a panel key preflight failure with the panel
// name, credential-ready alternative panels, and an omission suggestion only
// when the no-override target would pass the same preflight. The goal is that
// a model (or human) can recover without guessing: the error names what failed
// and what would work. Only env var NAMES, panel names, and provider/model
// identifiers are disclosed — never key values. Alternatives are informational
// for the user; per the tool schema the model should not switch panels unless
// the user named one.
func (a *App) mashuraKeyErrorMessage(toolName, panelName string, keyErr error) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s (panel %q)", keyErr.Error(), panelName)

	// Collect panels whose required keys are all present (same preflight).
	ready := make([]string, 0, len(a.Cfg.MashuraPanels))
	for name, p := range a.Cfg.MashuraPanels {
		if len(p.Models) == 0 {
			continue
		}
		if _, err := a.mashuraPanelKeys(p); err == nil {
			ready = append(ready, name)
		}
	}
	sort.Strings(ready)
	if len(ready) > 0 {
		fmt.Fprintf(&sb, "; panels with keys available: %s (ask the user whether to switch)", strings.Join(ready, ", "))
	}

	// Suggest omitting the override only if the effective no-override target
	// passes preflight (and is not the failing panel itself — the tool mapping
	// may route to a panel other than the one the override named).
	if defName, defPanel, ok := a.resolvePanel(toolName, ""); ok && defName != panelName {
		if _, err := a.mashuraPanelKeys(defPanel); err == nil {
			fmt.Fprintf(&sb, "; if you did not intend to select this panel, retry without the panel override (would use %q)", defName)
		}
	}
	return sb.String()
}

// resolvePanel returns the panel name and config to use for a mashura tool call.
// Resolution order: explicit override → tool mapping in MashuraToolPanels → "default"
// panel → built-in single-model fallback using OracleModel/MashuraFallbackModel.
//
// An explicit override that names a nonexistent panel is an error (returns
// ok=false) — a typo in the panel argument should not silently fall back to a
// different panel. A missing tool-mapping target is also an error.
//
// The built-in fallback is provider-aware: it uses MashuraFallbackModel if set
// (must be explicitly prefixed, e.g. "openrouter:anthropic/claude-sonnet-4"),
// otherwise auto-detects based on which API key env var is set. This fixes the
// bug where OpenRouter-only users saw tools advertised but every call failed
// because the fallback hardcoded "anthropic:" + OracleModel.
func (a *App) resolvePanel(toolName, override string) (name string, panel config.MashuraPanelConfig, ok bool) {
	name = override
	if name == "" {
		short := strings.TrimPrefix(toolName, "mashura__")
		if toolName == "oracle__ask" {
			short = "review"
		}
		if a.Cfg.MashuraToolPanels != nil {
			name = a.Cfg.MashuraToolPanels[short]
		}
	}
	// If an explicit name was provided (override or tool mapping), it must
	// resolve to a configured panel — UNLESS the name is "default", which
	// is eligible for the built-in fallback if no configured "default" panel
	// exists (so /mashura map review default works even without a configured
	// default panel). A typo like panel="resilient" still fails closed.
	if name != "" && name != "default" {
		if a.Cfg.MashuraPanels != nil {
			if p, exists := a.Cfg.MashuraPanels[name]; exists && len(p.Models) > 0 {
				if p.Mode == "" {
					p.Mode = "panel"
				}
				return name, p, true
			}
		}
		// Named panel not found or empty — fail-closed.
		return name, config.MashuraPanelConfig{}, false
	}
	// No override, no tool mapping, or name=="default" → try the "default" panel.
	if name == "" {
		name = "default"
	}
	if a.Cfg.MashuraPanels != nil {
		if p, exists := a.Cfg.MashuraPanels[name]; exists && len(p.Models) > 0 {
			if p.Mode == "" {
				p.Mode = "panel"
			}
			return name, p, true
		}
	}
	// Built-in fallback: provider-aware.
	model := a.fallbackModel()
	return name, config.MashuraPanelConfig{
		Models: []string{model},
		Mode:   "panel",
	}, true
}

// fallbackModel returns the prefixed model string for the built-in fallback
// panel when no "default" panel is configured. Resolution:
//  1. MashuraFallbackModel (config) if set — must be explicitly prefixed.
//  2. OracleModel if it already contains a provider prefix (colon).
//  3. Auto-detect: if only the OpenRouter key is set, use "openrouter:" + the
//     OpenRouter default model constant. If only the Anthropic key is set, use
//     "anthropic:" + OracleModel. If both, prefer "anthropic:" + OracleModel
//     (backward compat). If neither, use "anthropic:" + OracleModel (the key
//     check in mashuraPanelKeys will catch the missing key and fail-closed).
func (a *App) fallbackModel() string {
	if fm := a.Cfg.MashuraFallbackModel; fm != "" {
		return fm
	}
	// If OracleModel already has a provider prefix (e.g. "openrouter:anthropic/..."),
	// use it as-is to avoid double-prefixing. We check for known prefixes rather
	// than any colon, because OpenRouter model IDs can contain colons in variant
	// suffixes (e.g. "anthropic/claude-sonnet-4:free").
	if strings.HasPrefix(a.Cfg.OracleModel, "anthropic:") || strings.HasPrefix(a.Cfg.OracleModel, "openrouter:") {
		return a.Cfg.OracleModel
	}
	// Auto-detect based on available keys.
	hasAnthropic := os.Getenv(a.Cfg.OracleAPIKeyEnv) != ""
	hasOpenRouter := os.Getenv(a.Cfg.OpenRouterAPIKeyEnv) != ""
	if !hasAnthropic && hasOpenRouter {
		// OpenRouter-only: use the OpenRouter default model.
		return "openrouter:" + defaultOpenRouterModel
	}
	// Anthropic key present (or neither) → backward-compatible Anthropic fallback.
	return "anthropic:" + a.Cfg.OracleModel
}

// defaultOpenRouterModel is the model used when auto-detecting an OpenRouter-only
// fallback and no explicit model is configured. It must be a valid OpenRouter
// model ID (vendor/model format).
const defaultOpenRouterModel = "anthropic/claude-sonnet-4"

// defaultPanel returns the "default" panel config, used by workflow-phase oracle
// calls that are not model-initiated mashura tool calls.
func (a *App) defaultPanel() (string, config.MashuraPanelConfig, bool) {
	return a.resolvePanel("", "")
}

// mashuraPanelKeys collects the API key for each provider referenced by the
// panel, checking that every required key is present before the gate fires.
// anthropic → OracleAPIKeyEnv (default: ANTHROPIC_API_KEY)
// openrouter → OpenRouterAPIKeyEnv (default: OPENROUTER_API_KEY)
// fusion  → only OpenRouterAPIKeyEnv (all analysis models route through OpenRouter)
func (a *App) mashuraPanelKeys(panel config.MashuraPanelConfig) (map[string]string, error) {
	if panel.Mode == "fusion" {
		key := os.Getenv(a.Cfg.OpenRouterAPIKeyEnv)
		if key == "" {
			return nil, fmt.Errorf("mashūra API key not set (%s) for fusion mode", a.Cfg.OpenRouterAPIKeyEnv)
		}
		return map[string]string{"openrouter": key}, nil
	}
	keys := map[string]string{}
	for _, model := range panel.Models {
		prov, _ := counsel.ParseModelPrefix(model)
		if _, already := keys[prov]; already {
			continue
		}
		var envVar string
		switch prov {
		case "anthropic":
			envVar = a.Cfg.OracleAPIKeyEnv
		case "openrouter":
			envVar = a.Cfg.OpenRouterAPIKeyEnv
		default:
			return nil, fmt.Errorf("unknown provider %q in model %q", prov, model)
		}
		key := os.Getenv(envVar)
		if key == "" {
			return nil, fmt.Errorf("mashūra API key not set (%s) for provider %q", envVar, prov)
		}
		keys[prov] = key
	}
	return keys, nil
}

// mashuraCore returns the authoritative context shared by every mashura tool,
// persistence-ordered (most-static first): task and workflow position from
// WorkflowState (never model-edited text), the working directory, and the recent
// step log read by Wakil from plan.md. Empty sections are omitted; outside a
// workflow only the working directory is present.
func (a *App) mashuraCore(ctx context.Context) string {
	var sb strings.Builder
	if a.Workflow != nil {
		if t := strings.TrimSpace(a.Workflow.Task); t != "" {
			fmt.Fprintf(&sb, "## Task\n\n%s\n\n", t)
		}
		fmt.Fprintf(&sb, "## Workflow position\n\nphase: %s; step %d of %d\n\n",
			a.Workflow.PhaseName(), a.Workflow.StepIdx, a.Workflow.StepCount)
	}
	if a.Exec != nil {
		if cwd := a.Exec.Cwd(); cwd != "" {
			fmt.Fprintf(&sb, "## Working directory\n\n%s\n\n", cwd)
		}
	}
	if entries := a.mashuraRecentStepLog(ctx); len(entries) > 0 {
		sb.WriteString("## Step log (recent)\n\n")
		sb.WriteString(strings.Join(entries, "\n\n"))
		sb.WriteString("\n\n")
	}
	return sb.String()
}

// mashuraRecentStepLog reads the recent step-log entries from plan.md (Wakil's
// record), or nil when there is no workflow / executor / readable plan.
func (a *App) mashuraRecentStepLog(ctx context.Context) []string {
	if a.Workflow == nil || a.Exec == nil {
		return nil
	}
	content, err := a.Exec.ReadFile(ctx, a.Workflow.PlanPath)
	if err != nil {
		return nil
	}
	return workflow.RecentStepEntries(workflow.ExtractPlanSection(content, "## Step log"), workflow.WFStepLogK)
}

// mashuraPlanSections returns the ## Findings and ## Plan section bodies from
// plan.md (empty when unavailable).
func (a *App) mashuraPlanSections(ctx context.Context) (findings, plan string) {
	if a.Workflow == nil || a.Exec == nil {
		return "", ""
	}
	content, err := a.Exec.ReadFile(ctx, a.Workflow.PlanPath)
	if err != nil {
		return "", ""
	}
	return workflow.ExtractPlanSection(content, "## Findings"), workflow.ExtractPlanSection(content, "## Plan")
}

// mashuraCap tail-truncates an assembled tool briefing to mashuraToolBriefingCap.
// This is larger than the WF oracle cap — tool calls can attach full source files.
//
// If the briefing contains a Sources section, the tail-truncation would drop the
// evidence first (Sources are appended last). To avoid this, we split on the
// Sources header and truncate the non-sources portion separately, preserving
// as much evidence as fits within the overall cap.
func mashuraCap(s string) string {
	if len(s) <= mashuraToolBriefingCap {
		return s
	}
	// The Sources header is "## Sources (read by Wakil)\n" — match on the
	// stable prefix "## Sources" to find where evidence begins.
	const marker = "## Sources"
	idx := strings.Index(s, marker)
	if idx < 0 {
		// No Sources section — simple tail truncation.
		return s[:mashuraToolBriefingCap] + "\n[briefing truncated at overall cap]"
	}
	// Split into body (before Sources) and sources (from marker onward).
	body := s[:idx]
	sources := s[idx:]
	// Reserve space for truncation notices (worst case: two notices ~60 bytes).
	usable := mashuraToolBriefingCap - 80
	bodyCap := usable * 6 / 10 // 60% to body
	srcCap := usable - bodyCap // 40% to sources
	if len(sources) <= srcCap {
		// Sources fit — give the remainder to body.
		bodyCap = usable - len(sources)
	} else if len(body) <= bodyCap {
		// Body fits — give the remainder to sources.
		srcCap = usable - len(body)
	}
	var result string
	if len(body) > bodyCap {
		result = body[:bodyCap] + "\n[briefing body truncated]\n"
	} else {
		result = body
	}
	if len(sources) > srcCap {
		result += sources[:srcCap] + "\n[sources truncated]"
	} else {
		result += sources
	}
	return result
}

// mashuraReview builds the review briefing: core + findings + plan + model focus
// + Wakil-read sources. The model supplies a focus; Wakil fetches the evidence.
func (a *App) mashuraReview(ctx context.Context, tc proxy.ToolCall) (question, briefing string, receipts []mashuraSourceMeta, err error) {
	var args struct {
		Focus      string      `json:"focus"`
		Question   string      `json:"question"` // legacy oracle__ask alias
		Paths      []string    `json:"paths"`
		PathRanges []PathRange `json:"path_ranges"`
	}
	if e := json.Unmarshal([]byte(tc.Function.Arguments), &args); e != nil {
		return "", "", nil, fmt.Errorf("could not parse arguments: %w", e)
	}
	focus := strings.TrimSpace(args.Focus)
	if focus == "" {
		focus = strings.TrimSpace(args.Question)
	}
	findings, plan := a.mashuraPlanSections(ctx)
	if focus == "" && plan == "" && len(args.Paths) == 0 && len(args.PathRanges) == 0 {
		return "", "", nil, fmt.Errorf("nothing to review: provide a focus, paths, or run inside a workflow with a plan")
	}

	sourceSec, receipts, srcErr := a.mashuraReadSources(ctx, args.Paths, args.PathRanges)
	if srcErr != nil {
		return "", "", nil, srcErr
	}

	var sb strings.Builder
	sb.WriteString(a.mashuraCore(ctx))
	if findings != "" {
		fmt.Fprintf(&sb, "## Findings\n\n%s\n\n", workflow.CapFindings(findings))
	}
	if plan != "" {
		fmt.Fprintf(&sb, "## Plan\n\n%s\n\n", plan)
	}
	if focus != "" {
		fmt.Fprintf(&sb, "## Focus\n\n%s\n\n", focus)
	}
	if sourceSec != "" {
		sb.WriteString(sourceSec)
	}
	q := "Critically review the plan and work against the task. Identify missing steps, " +
		"incorrect assumptions, unclear acceptance criteria, risks, and contradictions. " +
		"Distinguish what the shown context demonstrates from what you recall."
	return q, mashuraCap(sb.String()), receipts, nil
}

// mashuraDebug builds the debug briefing: core + symptom + recent tool calls +
// Wakil-read sources.
func (a *App) mashuraDebug(ctx context.Context, tc proxy.ToolCall) (question, briefing string, receipts []mashuraSourceMeta, err error) {
	var args struct {
		Symptom    string      `json:"symptom"`
		Paths      []string    `json:"paths"`
		PathRanges []PathRange `json:"path_ranges"`
	}
	if e := json.Unmarshal([]byte(tc.Function.Arguments), &args); e != nil {
		return "", "", nil, fmt.Errorf("could not parse arguments: %w", e)
	}
	symptom := strings.TrimSpace(args.Symptom)
	if symptom == "" {
		return "", "", nil, fmt.Errorf("symptom is required")
	}

	sourceSec, receipts, srcErr := a.mashuraReadSources(ctx, args.Paths, args.PathRanges)
	if srcErr != nil {
		return "", "", nil, srcErr
	}

	var sb strings.Builder
	sb.WriteString(a.mashuraCore(ctx))
	fmt.Fprintf(&sb, "## Symptom\n\n%s\n\n", symptom)
	if tr := a.mashuraDebugTraces(); tr != "" {
		fmt.Fprintf(&sb, "## Recent tool calls\n\n%s\n\n", tr)
	}
	if sourceSec != "" {
		sb.WriteString(sourceSec)
	}
	q := "Diagnose the root cause of the symptom. Distinguish what the shown evidence " +
		"demonstrates from what you recall. Propose the single cheapest next experiment to " +
		"confirm or refute the leading hypothesis."
	return q, mashuraCap(sb.String()), receipts, nil
}

// mashuraDebugTraces formats the last mashuraDebugTraceK rolling traces. EXIT≠0
// entries get their generous error tail appended — this consumer overrides the
// step log's 4-line cap.
func (a *App) mashuraDebugTraces() string {
	traces := a.recentTraces
	if len(traces) > mashuraDebugTraceK {
		traces = traces[len(traces)-mashuraDebugTraceK:]
	}
	if len(traces) == 0 {
		return ""
	}
	var lines []string
	for _, e := range traces {
		lines = append(lines, FormatTraceEntry(e))
		if e.ExitErr && e.ErrorTail != "" {
			lines = append(lines, e.ErrorTail)
		}
	}
	return strings.Join(lines, "\n")
}

// mashuraReadSources reads paths (files or directories) and path_ranges from the
// executor and assembles a ## Sources section. Returns the section text, per-file
// receipts, and a pre-API error for bad (not found) paths.
//
// Capping rules:
//   - Per file: clipped to mashuraSourceFileCap bytes with an explicit marker.
//   - Total budget: whole files are dropped (not partially clipped) once exceeded;
//     each omission is reported explicitly.
func (a *App) mashuraReadSources(ctx context.Context, paths []string, ranges []PathRange) (section string, receipts []mashuraSourceMeta, err error) {
	if len(paths) == 0 && len(ranges) == 0 {
		return "", nil, nil
	}
	if a.Exec == nil {
		return "", nil, fmt.Errorf("no executor — cannot read source files")
	}

	type entry struct {
		path  string
		start int // 0 = file start
		end   int // 0 = file end
	}
	var entries []entry
	var dirMarkers []string

	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		expanded, isDir, expErr := a.mashuraExpandPath(ctx, p)
		if expErr != nil {
			return "", nil, fmt.Errorf("path %q: %w", p, expErr)
		}
		if isDir {
			for _, fp := range expanded {
				// Skip the cap-omission marker — it's informational text
				// appended by mashuraExpandDir, not a real file path.
				// We'll include it in the briefing output later.
				if strings.HasPrefix(fp, "[+") {
					dirMarkers = append(dirMarkers, fp)
					continue
				}
				entries = append(entries, entry{path: fp})
			}
		} else {
			entries = append(entries, entry{path: p})
		}
	}
	for _, pr := range ranges {
		pr.Path = strings.TrimSpace(pr.Path)
		if pr.Path == "" {
			continue
		}
		entries = append(entries, entry{path: pr.Path, start: pr.StartLine, end: pr.EndLine})
	}

	var sb strings.Builder
	sb.WriteString("## Sources (read by Wakil)\n\n")
	// Include any directory-expansion omission markers so the oracle knows
	// that some files were not read.
	for _, m := range dirMarkers {
		fmt.Fprintf(&sb, "%s\n\n", m)
	}
	total := 0

	for _, e := range entries {
		body, readErr := a.Exec.ReadFile(ctx, e.path)
		if readErr != nil {
			return "", nil, fmt.Errorf("path %q: %w", e.path, readErr)
		}

		allLines := strings.Split(body, "\n")
		totalLines := len(allLines)

		// Apply line range if requested.
		start, end := 0, totalLines
		if e.start > 1 {
			start = e.start - 1
		}
		if e.end > 0 && e.end < totalLines {
			end = e.end
		}
		if start > totalLines {
			start = totalLines
		}
		if end < start {
			end = start
		}
		shownLines := allLines[start:end]
		shownBody := strings.Join(shownLines, "\n")

		// Per-file cap: clip and mark.
		clipped := false
		if len(shownBody) > mashuraSourceFileCap {
			clipped = true
			clipped_body := shownBody[:mashuraSourceFileCap]
			// Back up to a newline so we don't split mid-line.
			if nl := strings.LastIndexByte(clipped_body, '\n'); nl >= 0 {
				clipped_body = clipped_body[:nl]
			}
			clippedCount := strings.Count(clipped_body, "\n") + 1
			shownBody = clipped_body + fmt.Sprintf("\n[%s: truncated, %d of %d lines shown]", e.path, clippedCount, totalLines)
			shownLines = allLines[start : start+clippedCount]
		}

		// Total budget: drop whole file if it won't fit.
		if total+len(shownBody) > mashuraSourcesTotal {
			receipts = append(receipts, mashuraSourceMeta{Path: e.path, Total: totalLines, Omitted: true})
			fmt.Fprintf(&sb, "── %s ──\n[%s: omitted — total sources cap reached]\n\n", e.path, e.path)
			continue
		}

		total += len(shownBody)
		meta := mashuraSourceMeta{
			Path:    e.path,
			Bytes:   len(shownBody),
			Lines:   len(shownLines),
			Total:   totalLines,
			Clipped: clipped,
		}
		receipts = append(receipts, meta)

		label := e.path
		if e.start > 0 || e.end > 0 {
			label = fmt.Sprintf("%s (lines %d–%d)", e.path, e.start, e.end)
		}
		fmt.Fprintf(&sb, "── %s (%d lines) ──\n%s\n\n", label, len(shownLines), shownBody)
	}

	if len(receipts) == 0 {
		return "", nil, nil
	}
	return sb.String(), receipts, nil
}

// mashuraExpandPath returns the file paths for a given path:
//   - If it reads as a file: returns (nil, false, nil) — caller uses path directly.
//   - If it's a directory: returns the expanded source file list.
//   - If it's neither: returns a pre-API error.
func (a *App) mashuraExpandPath(ctx context.Context, path string) (files []string, isDir bool, err error) {
	_, readErr := a.Exec.ReadFile(ctx, path)
	if readErr == nil {
		return nil, false, nil // readable file
	}
	_, listErr := a.Exec.ListDir(ctx, path)
	if listErr != nil {
		return nil, false, fmt.Errorf("path %q not found (read: %w, list: %w)", path, readErr, listErr)
	}
	// It's a directory.
	expanded, expErr := a.mashuraExpandDir(ctx, path)
	if expErr != nil {
		return nil, true, expErr
	}
	return expanded, true, nil
}

// mashuraExpandDir lists source files inside dirPath, respecting .gitignore
// (via git ls-files) and capping at mashuraDirFileCap files.
func (a *App) mashuraExpandDir(ctx context.Context, dirPath string) ([]string, error) {
	q := "'" + strings.ReplaceAll(dirPath, "'", `'\''`) + "'"
	// git ls-files respects .gitignore; fall back to find ONLY if git fails
	// (not if it succeeds with empty output — empty means no tracked files,
	// which is a valid result, not a signal to bypass .gitignore with find).
	cmd := `git ls-files --cached --others --exclude-standard -- ` + q + ` 2>/dev/null | sort`
	out, err := a.Exec.RunShell(ctx, cmd)
	if err != nil {
		// git failed (not in a repo, git not installed) — fall back to find.
		cmd = `find ` + q + ` -type f ! -path '*/.git/*' ! -path '*/node_modules/*' | sort`
		out, err = a.Exec.RunShell(ctx, cmd)
		if err != nil {
			return nil, fmt.Errorf("directory expansion failed: %w", err)
		}
	}

	var files []string
	total := 0
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if binaryExts[strings.ToLower(filepath.Ext(line))] {
			continue
		}
		files = append(files, line)
		total++
		if len(files) >= mashuraDirFileCap {
			break
		}
	}
	// Count how many were skipped for the cap marker.
	all := 0
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !binaryExts[strings.ToLower(filepath.Ext(line))] {
			all++
		}
	}
	if all > mashuraDirFileCap {
		files = append(files, fmt.Sprintf("[+%d files omitted from %s — use explicit paths for full coverage]",
			all-mashuraDirFileCap, dirPath))
	}
	return files, nil
}

// mashuraLogReceipts emits read receipts to the step log (if in a workflow) and
// to a.Out as a dim status line. Called after the user approves the gate.
func (a *App) mashuraLogReceipts(ctx context.Context, receipts []mashuraSourceMeta) {
	if len(receipts) == 0 {
		return
	}
	var parts []string
	for _, r := range receipts {
		switch {
		case r.Omitted:
			parts = append(parts, r.Path+" (omitted — cap)")
		case r.Clipped:
			parts = append(parts, fmt.Sprintf("%s (%d bytes, clipped)", r.Path, r.Bytes))
		default:
			parts = append(parts, fmt.Sprintf("%s (%d bytes)", r.Path, r.Bytes))
		}
	}
	msg := "[mashura] sent: " + strings.Join(parts, ", ")
	fmt.Fprintln(a.Out, Dim("· "+msg))
	if a.Workflow != nil && a.Exec != nil {
		if content, e := a.Exec.ReadFile(ctx, a.Workflow.PlanPath); e == nil {
			if _, err := a.Exec.WriteFile(ctx, a.Workflow.PlanPath, workflow.WFAppendToStepLog(content, msg)); err != nil {
				diag.Printf("warning: step log write failed: %v\n", err)
			}
		}
	}
}

// mashuraDecide builds the decide briefing: core + findings + decision question
// + options + Wakil-read sources.
func (a *App) mashuraDecide(ctx context.Context, tc proxy.ToolCall) (question, briefing string, receipts []mashuraSourceMeta, err error) {
	var args struct {
		Question   string      `json:"question"`
		Options    string      `json:"options"`
		Paths      []string    `json:"paths"`
		PathRanges []PathRange `json:"path_ranges"`
	}
	if e := json.Unmarshal([]byte(tc.Function.Arguments), &args); e != nil {
		return "", "", nil, fmt.Errorf("could not parse arguments: %w", e)
	}
	decision := strings.TrimSpace(args.Question)
	options := strings.TrimSpace(args.Options)
	if decision == "" {
		return "", "", nil, fmt.Errorf("question is required")
	}
	if options == "" {
		return "", "", nil, fmt.Errorf("options are required")
	}

	sourceSec, receipts, srcErr := a.mashuraReadSources(ctx, args.Paths, args.PathRanges)
	if srcErr != nil {
		return "", "", nil, srcErr
	}

	findings, _ := a.mashuraPlanSections(ctx)
	var sb strings.Builder
	sb.WriteString(a.mashuraCore(ctx))
	if findings != "" {
		fmt.Fprintf(&sb, "## Findings\n\n%s\n\n", workflow.CapFindings(findings))
	}
	fmt.Fprintf(&sb, "## Decision\n\n%s\n\n", decision)
	fmt.Fprintf(&sb, "## Options\n\n%s\n\n", options)
	if sourceSec != "" {
		sb.WriteString(sourceSec)
	}
	q := "Which option best fits the shown constraints (task and findings)? State what " +
		"evidence would change your answer. Flag any option that contradicts a stated finding."
	return q, mashuraCap(sb.String()), receipts, nil
}

// mashuraCheck builds the lightweight check briefing: core + the single claim +
// Wakil-read evidence sources.
func (a *App) mashuraCheck(ctx context.Context, tc proxy.ToolCall) (question, briefing string, receipts []mashuraSourceMeta, err error) {
	var args struct {
		Claim      string      `json:"claim"`
		Paths      []string    `json:"paths"`
		PathRanges []PathRange `json:"path_ranges"`
	}
	if e := json.Unmarshal([]byte(tc.Function.Arguments), &args); e != nil {
		return "", "", nil, fmt.Errorf("could not parse arguments: %w", e)
	}
	claim := strings.TrimSpace(args.Claim)
	if claim == "" {
		return "", "", nil, fmt.Errorf("claim is required")
	}

	sourceSec, receipts, srcErr := a.mashuraReadSources(ctx, args.Paths, args.PathRanges)
	if srcErr != nil {
		return "", "", nil, srcErr
	}

	var sb strings.Builder
	sb.WriteString(a.mashuraCore(ctx))
	fmt.Fprintf(&sb, "## Claim\n\n%s\n\n", claim)
	if sourceSec != "" {
		sb.WriteString(sourceSec)
	}
	q := "Verify the single claim against the shown evidence only. Answer SUPPORTED, " +
		"CONTRADICTED, or INSUFFICIENT EVIDENCE, then one sentence why. Do not rely on recall."
	return q, mashuraCap(sb.String()), receipts, nil
}

// mashuraMaxTokensFor returns the max_tokens for a mashura tool: a per-tool config
// override if present, else a tool default (check is lighter than the rest), else
// the shared base (OracleMaxTokens).
func (a *App) mashuraMaxTokensFor(name string) int {
	short := strings.TrimPrefix(name, "mashura__")
	if name == "oracle__ask" {
		short = "review"
	}
	if v := a.Cfg.MashuraToolMaxTokens[short]; v > 0 {
		return v
	}
	base := a.Cfg.OracleMaxTokens
	if base <= 0 {
		base = 4096
	}
	if short == "check" && base > mashuraCheckMaxTokens {
		return mashuraCheckMaxTokens
	}
	return base
}

// displayAbbrev renders a trace entry's tool abbreviation for symptoms. An
// empty Abbrev means the tool-call name was lost in transit (stream chunk
// dropped before the name fragment arrived) — say so instead of printing a
// misleading `shell ""` / `subagent ""`.
func displayAbbrev(abbrev string) string {
	if abbrev == "" {
		return "tool (name lost — stream corruption?)"
	}
	return abbrev
}

// detectStruggle scans recent tool traces for a deterministic struggle signal and,
// when found, returns a prefilled symptom string for mashura__debug. Signals: a
// streak of two consecutive failures; the same command run twice in a row; or the
// same file written/edited three or more times in the window. It only offers — the
// caller never auto-calls the tool, and the human gate always applies.
func DetectStruggle(traces []ToolTraceEntry) (symptom string, detected bool) {
	if len(traces) < 2 {
		return "", false
	}
	n := len(traces)
	last := traces[n-1]
	prev := traces[n-2]

	// EXIT≠0 streak: the two most recent calls both failed.
	if last.ExitErr && prev.ExitErr {
		s := fmt.Sprintf("repeated failures: %s %q keeps exiting non-zero", displayAbbrev(last.Abbrev), last.Command)
		if last.FirstLine != "" {
			s += " — " + last.FirstLine
		}
		return s, true
	}
	// Same command twice in a row with no intervening progress.
	if last.Command != "" && last.Command == prev.Command && last.Abbrev == prev.Abbrev {
		return fmt.Sprintf("ran %s %q twice with no progress", displayAbbrev(last.Abbrev), last.Command), true
	}
	// Repeated rewrites of the same file across the window.
	counts := map[string]int{}
	for _, e := range traces {
		if (e.Abbrev == "write" || e.Abbrev == "edit") && e.Command != "" {
			counts[e.Command]++
			if counts[e.Command] >= 3 {
				return fmt.Sprintf("rewrote %q %d times — the fix may not be landing", e.Command, counts[e.Command]), true
			}
		}
	}
	return "", false
}

// mashuraAvailable reports whether mashura counsel tools are available
// (oracle enabled AND the resolved default panel's API keys are present).
// WP-7.10d: single predicate.
func (a *App) mashuraAvailable() bool {
	if !a.Cfg.OracleEnabled {
		return false
	}
	// Check that the resolved default panel's keys are actually present.
	// This fixes the bug where tools were advertised when ANY key was set,
	// but the fallback always required the Anthropic key.
	_, panel, ok := a.resolvePanel("", "")
	if !ok {
		return false
	}
	_, err := a.mashuraPanelKeys(panel)
	return err == nil
}

// maybeSuggestDebug offers mashura__debug when the rolling trace shows a struggle
// signal. Behavior is governed by the effective counsel mode:
//
//	"off"     — note the detection dimly but never consult
//	"suggest" — hint only; the human decides whether to call mashura__debug
//	"auto"    — fire the call directly up to MaxCounsel times per turn, then
//	            fall back to suggest; each fire announced before the call
//
// Mode resolution: CounselMode (TUI, set by /counsel) takes precedence; falls
// back to AutoCounsel bool for the headless benchmark path.
// Each distinct symptom is deduplicated (struggleSuggested); that map is reset
// at the start of each Send() when CounselMode is set (per-turn cap in TUI).
func (a *App) maybeSuggestDebug(ctx context.Context) {
	if !a.mashuraAvailable() {
		return // no mashūra available
	}
	symptom, ok := DetectStruggle(a.recentTraces)
	if !ok {
		return
	}
	if a.struggleSuggested == nil {
		a.struggleSuggested = map[string]bool{}
	}
	if a.struggleSuggested[symptom] {
		return
	}
	a.struggleSuggested[symptom] = true

	// Resolve effective mode: CounselMode (TUI path) wins; fall back to the
	// legacy AutoCounsel bool for backward-compatible headless behaviour.
	mode := a.CounselMode
	if mode == "" {
		if a.AutoCounsel {
			mode = "auto"
		} else {
			mode = "off"
		}
	}

	switch mode {
	case "off":
		return // truly silent — no output

	case "auto":
		if a.MaxCounsel > 0 && a.counselCalls < a.MaxCounsel {
			a.counselCalls++
			// Announce loudly with cost label BEFORE the call so it's visible.
			fmt.Fprintln(a.Out, fmt.Sprintf("⚡ auto-counsel %d/%d · mashura__debug · %s",
				a.counselCalls, a.MaxCounsel, symptom))
			// Bypass the confirm gate when the session is also in /auto mode.
			a.autoCounselSkipGate = a.Consent().AutoApprove
			fakeTC := proxy.ToolCall{
				ID:   fmt.Sprintf("auto-counsel-%d", a.counselCalls),
				Type: "function",
				Function: proxy.FunctionCall{
					Name:      "mashura__debug",
					Arguments: fmt.Sprintf(`{"symptom":%q}`, "auto-counsel: "+symptom),
				},
			}
			// auto-counsel stays SYNCHRONOUS — its diagnosis must be
			// injected immediately as the assistant+tool pair below (cap/dedup
			// semantics depend on the result being in hand right now).
			result := a.runMashuraCore(ctx, "mashura__debug", fakeTC, true)
			a.autoCounselSkipGate = false // ensure cleared even on early return
			// Inject as assistant+tool pair so the model sees the diagnosis.
			a.Conv = append(a.Conv,
				proxy.Message{Role: "assistant", Content: nil, ToolCalls: []proxy.ToolCall{fakeTC}},
				proxy.Message{Role: "tool", ToolCallID: fakeTC.ID, Name: fakeTC.Function.Name, Content: StrPtr(result)},
			)
			fmt.Fprintln(a.Out, Dim(fmt.Sprintf("· auto-counsel %d/%d: %s",
				a.counselCalls, a.MaxCounsel, Truncate(result, 120))))
			if a.counselCalls >= a.MaxCounsel {
				fmt.Fprintln(a.Out, Dim(fmt.Sprintf(
					"· counsel cap reached (%d/%d) — reverting to suggest for this turn",
					a.counselCalls, a.MaxCounsel)))
			}
			return
		}
		// Cap reached — fall through to suggest behavior.
		fallthrough

	default: // "suggest"
		fmt.Fprintln(a.Out, Dim("· struggle detected — mashura__debug can diagnose; symptom: "+symptom))
	}
}
