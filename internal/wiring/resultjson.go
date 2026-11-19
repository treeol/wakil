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
	"time"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/buildinfo"
	wtools "github.com/treeol/wakil/internal/tools"
)

// ResultJSON is the machine-readable summary written at exit when
// HeadlessOptions.ResultJSON is set. Pointer fields stay nil (JSON null)
// when the run did not observe the value (e.g. a binary built without
// cost tracking, or counts unavailable).
type ResultJSON struct {
	Version       string   `json:"wakil_version"`
	Model         string   `json:"model"`
	ExitReason    string   `json:"exit_reason"`
	Seconds       float64  `json:"seconds"`
	TokensIn      *int64   `json:"tokens_in"`
	TokensOut     *int64   `json:"tokens_out"`
	CostUSD       *float64 `json:"cost_usd"`
	ToolCalls     *int     `json:"tool_calls"`
	Collapses     *int     `json:"collapses"`
	MashuraRounds *int     `json:"mashura_rounds"`
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
	default:
		return fmt.Sprintf("exit_%d", code)
	}
}

// writeResultJSON assembles the summary for app after the run produced
// code and writes it as one JSON object to path. Tool/collapse/mashura
// counts come from the live conversation and collapse records, not the
// saved session snapshot (which only exists with --transcript).
func writeResultJSON(path string, app *agent.App, code int, started time.Time) error {
	res := ResultJSON{
		Version:    buildinfo.ResolvedVersion(),
		ExitReason: exitReasonName(code),
		Seconds:    time.Since(started).Seconds(),
	}
	if app != nil {
		res.Model = app.Client.Model
		if app.Costs != nil {
			var inTok, outTok int64
			total, rows := app.Costs.Snapshot()
			for _, r := range rows {
				inTok += r.InputTok
				outTok += r.OutputTok
			}
			res.TokensIn = &inTok
			res.TokensOut = &outTok
			res.CostUSD = &total
		}
		toolCalls := 0
		mashuraTools := 0
		for _, m := range app.Conv {
			toolCalls += len(m.ToolCalls)
			for _, tc := range m.ToolCalls {
				if wtools.IsMashuraTool(tc.Function.Name) {
					mashuraTools++
				}
			}
		}
		res.ToolCalls = &toolCalls
		collapses := len(app.CollapsedRecords())
		res.Collapses = &collapses
		// Mashura rounds = explicit mashura__* tool calls in the conversation
		// plus automatic rounds fired by Wakil itself (countAutoRounds rule).
		rounds := mashuraTools + agent.AutoRoundsInConv(app.Conv)
		res.MashuraRounds = &rounds
	}
	b, err := json.Marshal(res)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
