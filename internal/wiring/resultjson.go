package wiring

// resultjson.go implements the --result-json summary file for `wakil run`.
//
// The bench harness needs per-run tokens, cost and counts in a stable
// machine-readable form. The JSONL transcript already carries tokens and
// cost, but tool/collapse/mashura counts live behind App state the
// transcript does not expose, so a single JSON object at exit is the
// canonical source.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/buildinfo"
	"github.com/treeol/wakil/internal/proxy"
	wtools "github.com/treeol/wakil/internal/tools"
)

// ResultJSON is the machine-readable summary written at exit when
// HeadlessOptions.ResultJSON is set. Pointer fields stay nil (JSON null)
// when the run did not observe the value (e.g. a binary built without
// cost tracking, or counts unavailable).
type ResultJSON struct {
	Version    string `json:"wakil_version"`
	Model      string `json:"model"`
	ExitReason string `json:"exit_reason"`
	Workdir    string `json:"workdir,omitempty"`
	// SandboxBackend is the resolved sandbox backend ("hisn", "docker",
	// "direct"). Workdir is the executor's own view of the workspace: a host
	// path for hisn and direct, a container path for docker, so a reader that
	// compares it with a host folder needs to know which one it is.
	SandboxBackend   string   `json:"sandbox_backend,omitempty"`
	Seconds          float64  `json:"seconds"`
	TokensIn         *int64   `json:"tokens_in"`
	TokensOut        *int64   `json:"tokens_out"`
	TokensCached     *int64   `json:"tokens_cached,omitempty"`
	CostUSD          *float64 `json:"cost_usd"`
	ToolCalls        *int     `json:"tool_calls"`
	Collapses        *int     `json:"collapses"`
	CollapsesTurnEnd *int     `json:"collapses_turn_end"`
	MashuraRounds    *int     `json:"mashura_rounds"`
	DeclinedCalls    *int     `json:"declined_calls"`
	// ContextModules is the context-modules switch state ("on" or "off") for
	// this run. Omitted when the run did not observe it (nil app).
	ContextModules string `json:"context_modules,omitempty"`
	// ModulesLoaded is the semicolon-joined list of module names loaded
	// during the session. Semicolons (not commas) so CSV readers that split
	// on commas are not broken. Omitted when the switch is off or no modules
	// were loaded.
	ModulesLoaded string `json:"modules_loaded,omitempty"`
}

// exitReasonName maps a headless exit code to the bench-facing reason
// string. The terminal JSONL records use the same outcome vocabulary
// ("pass"/"declined"/"backend_failure"); ExitGaps and ExitError keep
// their code names so callers can branch on them.
func exitReasonName(code int) string {
	switch code {
	case ExitOK:
		return "pass"
	case ExitDeclined:
		return "declined"
	case ExitBackendFailure:
		return "backend_failure"
	case ExitGaps:
		return "gaps"
	case ExitError:
		return "error"
	case ExitIncomplete:
		return "incomplete"
	default:
		return fmt.Sprintf("exit_%d", code)
	}
}

// writeResultJSON assembles the summary for app after the run produced
// code and writes it as one JSON object to path. Tool/collapse/mashura
// counts come from the live conversation and collapse records, not the
// saved session snapshot (which only exists with --transcript).
//
// workdir is the resolved workspace folder the session actually ran in;
// empty when unknown (e.g. bootstrap failure before resolution). backend is
// the resolved sandbox backend name, empty when unknown.
func writeResultJSON(path string, app *agent.App, code int, started time.Time, workdir, backend string) error {
	res := ResultJSON{
		Version:        buildinfo.ResolvedVersion(),
		ExitReason:     exitReasonName(code),
		Workdir:        workdir,
		SandboxBackend: backend,
		Seconds:        time.Since(started).Seconds(),
	}
	if app != nil && app.Client != nil {
		res.Model = app.Client.Model
		// Tokens, cost stay null when the run made no model call, so the
		// caller can distinguish "not observed" from a true 0 (unpriced
		// model or zero-cost backend). The sum over rows is the
		// authoritative total; a run that spent money has rows.
		if app.Costs != nil {
			total, rows := app.Costs.Snapshot()
			if len(rows) > 0 {
				var inTok, outTok, cachedTok int64
				for _, r := range rows {
					inTok += r.InputTok
					outTok += r.OutputTok
					cachedTok += r.CachedTok
				}
				res.TokensIn = &inTok
				res.TokensOut = &outTok
				if cachedTok > 0 {
					res.TokensCached = &cachedTok
				}
				res.CostUSD = &total
			}
		}
		toolCalls := 0
		for _, m := range app.Conv {
			toolCalls += len(m.ToolCalls)
		}
		res.ToolCalls = &toolCalls
		midTurn, turnEnd := app.CollapseCounts()
		res.Collapses = &midTurn
		res.CollapsesTurnEnd = &turnEnd
		// Mashura rounds = explicit mashura__* tool calls in the conversation
		// plus automatic rounds fired by Wakil itself (countAutoRounds rule).
		rounds := resultMashuraRounds(app.Conv)
		res.MashuraRounds = &rounds
		// Declined calls: count of tool calls whose result was declined by the
		// user or by policy. Uses a dedicated counter on App (not Conv scanning
		// — collapsed/compacted turns would undercount).
		declined := app.DeclinedCallCount()
		res.DeclinedCalls = &declined
		// Context modules: report the switch state and loaded module names.
		// With the switch off, ContextModules stays "" (omitted by omitempty)
		// and ModulesLoaded stays "". With the switch on, the state is "on"
		// and the loaded names are semicolon-joined (not comma, which would
		// break the bench CSV).
		if app.Modules != nil {
			res.ContextModules = "on"
			loaded := app.Modules.LoadedNames()
			if len(loaded) > 0 {
				res.ModulesLoaded = strings.Join(loaded, ";")
			}
		} else if app.Cfg.ContextModules == "on" {
			res.ContextModules = "on"
		}
	}
	b, err := json.Marshal(res)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// resultMashuraRounds counts the Mashūra rounds of a conversation: the
// model's own mashura__* calls plus the automatic rounds Wakil started
// (agent.AutoRoundsInConv). A legacy synthetic call carries a mashura tool
// name and an automatic-round id; it is counted once, as automatic.
func resultMashuraRounds(conv []proxy.Message) int {
	modelCalls := 0
	for _, m := range conv {
		for _, tc := range m.ToolCalls {
			if !agent.IsAutoRoundCallID(tc.ID) && wtools.IsMashuraTool(tc.Function.Name) {
				modelCalls++
			}
		}
	}
	return modelCalls + agent.AutoRoundsInConv(conv)
}
