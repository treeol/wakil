package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/counsel"
	"github.com/treeol/wakil/internal/proxy"
)

// --- Part B3: provider-cost wins ---

func TestRecordInferenceCostProviderWins(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Costs = proxy.NewCostTracker()
	app.Client.Model = "m"
	app.Client.SetUsage(proxy.UsageStat{
		InputTok: 100, OutputTok: 50, Exact: true,
		ProviderCostUSD: 0.42, HasProviderCost: true,
	})
	app.RecordInferenceCost()
	ct, _ := app.Costs.SnapshotDetail()
	if ct.Provider != 0.42 || ct.Total != 0.42 {
		t.Errorf("provider-wins = %+v, want provider 0.42", ct)
	}
}

func TestRecordOracleCostProviderWins(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Costs = proxy.NewCostTracker()
	app.RecordOracleCostFor("m", counsel.OracleUsage{
		InputTokens: 10, OutputTokens: 5, ProviderCostUSD: 0.07, HasProviderCost: true,
	})
	ct, _ := app.Costs.SnapshotDetail()
	if ct.Mashura != 0.07 || ct.Provider != 0.07 {
		t.Errorf("oracle provider-wins = %+v", ct)
	}
}

// --- Part B5/B6: subagent fold categorised + session save/restore ---

func TestFoldSubagentCostCategorised(t *testing.T) {
	parent := proxy.NewCostTracker()
	rows := []proxy.CostRow{{
		Source: "inference·openrouter/m", Category: proxy.CostCatMain,
		Calls: 1, InputTok: 100, CostUSD: 0.05, ProviderUSD: 0.05,
		ProviderPriced: true, Priced: true, Confidence: proxy.ConfExact,
	}}
	total := foldSubagentCost(parent, rows)
	if total != 0.05 {
		t.Errorf("fold total = %v", total)
	}
	ct, _ := parent.SnapshotDetail()
	if ct.Subagent != 0.05 || ct.Main != 0 {
		t.Errorf("fold category split = %+v, want subagent 0.05", ct)
	}
}

func TestSessionCostRowsSurviveSaveRestore(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Costs = proxy.NewCostTracker()
	app.Session = &Session{ChatID: "costtest01", Model: "m"}
	app.Conv = []proxy.Message{{Role: "user", Content: StrPtr("hi")}}
	app.Costs.RecordCategorized("inference·openrouter/m", proxy.CostCatMain, 100, 50, 0.30, true, proxy.ConfExact)
	app.Costs.RecordCategorized("mashura·m", proxy.CostCatMashura, 10, 5, 0.02, true, proxy.ConfExact)
	app.SaveSession()
	// Load back.
	s, err := LoadSession("costtest01")
	if err != nil {
		t.Fatalf("LoadSession: %v (dir %s)", err, dir)
	}
	if len(s.CostRows) != 2 {
		t.Fatalf("persisted CostRows = %d, want 2 (%+v)", len(s.CostRows), s.CostRows)
	}
	_ = filepath.Join
	// Restore via InstallSession into a fresh app.
	app2 := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app2.Costs = proxy.NewCostTracker()
	app2.InstallSession(s)
	ct, _ := app2.Costs.SnapshotDetail()
	if ct.Total != 0.32 || ct.Main != 0.30 || ct.Mashura != 0.02 {
		t.Errorf("restored split = %+v", ct)
	}
}

func TestCostStatusLine(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Costs = proxy.NewCostTracker()
	app.Costs.RecordCategorized("inference·openrouter/m", proxy.CostCatMain, 100, 50, 0.30, true, proxy.ConfExact, config.TokenDetail{CachedTok: 40})
	app.Costs.RecordCategorized("mashura·m", proxy.CostCatMashura, 10, 5, 0.02, true, proxy.ConfExact)
	app.Costs.RecordCategorized("search", proxy.CostCatAux, 0, 0, 0.01, true, proxy.ConfModeled)
	line := app.costStatusLine()
	// billed = exact total = 0.32; provider = 0.32; est = 0.01; cache = 40/110.
	if !strings.Contains(line, "$0.3200 provider") || !strings.Contains(line, "cache 36.4%") {
		t.Errorf("costStatusLine = %q", line)
	}
	if !strings.Contains(line, "main $0.3000") || !strings.Contains(line, "mash $0.0200") || !strings.Contains(line, "aux $0.0100") {
		t.Errorf("costStatusLine split missing in %q", line)
	}
	if want := "$0.3200 provider $0.3200 / est $0.0100 · main $0.3000 / sub $0.0000 / mash $0.0200 / aux $0.0100 · cache 36.4%"; line != want {
		t.Errorf("costStatusLine =\n  %q\nwant\n  %q", line, want)
	}
}

// --- Part B2: forced-final keeps cache + single fallback ---

func TestForceFinishKeepsToolsAndTextEnds(t *testing.T) {
	// MaxToolIterations=1: iter0 runs tools, iter1 is forced. Server: call0
	// tool-call, call1 text answer.
	toolFrames := toolCallFrames("c1", "run_shell", `{"command":"echo hi"}`)
	textFrames := []string{`{"choices":[{"delta":{"content":"final answer"},"finish_reason":null}]}`}
	srv, getCaptured := capturingServer(t, toolFrames, textFrames)
	defer srv.Close()

	fe := newFakeExecutor()
	app := newTestApp(srv.URL, fe, func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.MaxToolIterations = 1
	_, err := app.Send(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	captured := getCaptured()
	if len(captured) != 2 {
		t.Fatalf("want 2 requests (forced text, no fallback), got %d", len(captured))
	}
	var forced struct {
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal([]byte(captured[1]), &forced); err != nil {
		t.Fatal(err)
	}
	if len(forced.Tools) == 0 {
		t.Error("forced request must carry tools (cache preservation), got none")
	}
	if len(fe.shellCalls) != 1 {
		t.Errorf("tools executed %d times, want 1 (no execution on forced turn)", len(fe.shellCalls))
	}
}

func TestForceFinishToolAnswerSingleFallback(t *testing.T) {
	// call0 tool, call1 (forced) tool-call again, call2 (fallback) text.
	toolFrames := toolCallFrames("c1", "run_shell", `{"command":"echo hi"}`)
	toolFrames2 := toolCallFrames("c2", "run_shell", `{"command":"echo hi"}`)
	textFrames := []string{`{"choices":[{"delta":{"content":"fallback answer"},"finish_reason":null}]}`}
	srv, getCaptured := capturingServer(t, toolFrames, toolFrames2, textFrames)
	defer srv.Close()

	fe := newFakeExecutor()
	app := newTestApp(srv.URL, fe, func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.MaxToolIterations = 1
	final, err := app.Send(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	captured := getCaptured()
	if len(captured) != 3 {
		t.Fatalf("want exactly 3 requests (forced + 1 fallback), got %d", len(captured))
	}
	var fb struct {
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal([]byte(captured[2]), &fb); err != nil {
		t.Fatal(err)
	}
	if len(fb.Tools) != 0 {
		t.Errorf("fallback request must have no tools, got %d", len(fb.Tools))
	}
	if final != "fallback answer" {
		t.Errorf("final = %q, want fallback answer", final)
	}
}

// --- counsel provider cost lives in counsel's own tests
// (provider-cost parse on the OpenRouter SSE path is covered by
// counsel.TestParseOpenRouterSSECost; here we assert the agent-side hop:
// an OracleUsage with HasProviderCost records provider USD, above. ---

// TestAuxRecordNeverRecountsMain: an aux record must count only the client it
// was handed. A nil client records nothing, and a separate aux client's usage
// lands once under the aux category without touching the main row.
func TestAuxRecordNeverRecountsMain(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Costs = proxy.NewCostTracker()
	app.Client.Model = "m"
	app.Client.SetUsage(proxy.UsageStat{InputTok: 100, OutputTok: 50, Exact: true, ProviderCostUSD: 0.40, HasProviderCost: true})
	app.RecordInferenceCost()

	// Nil client: must not fall back to the main client's usage.
	app.RecordAuxInferenceCost(nil, proxy.CostSourceInference)
	ct, _ := app.Costs.SnapshotDetail()
	if ct.Aux != 0 || ct.Total != 0.40 {
		t.Fatalf("nil aux client re-counted main: %+v (want aux 0, total 0.40)", ct)
	}

	// Separate aux client, usage recorded once.
	aux := newTestClient("http://unused")
	aux.Model = "m"
	aux.SetUsage(proxy.UsageStat{InputTok: 10, OutputTok: 5, Exact: true, ProviderCostUSD: 0.05, HasProviderCost: true})
	app.RecordAuxInferenceCost(aux, proxy.CostSourceInference)
	ct, rows := app.Costs.SnapshotDetail()
	if ct.Aux != 0.05 || ct.Main != 0.40 || ct.Total != 0.45 {
		t.Fatalf("aux split = %+v, want main 0.40 aux 0.05 total 0.45", ct)
	}
	var auxRows int
	for _, r := range rows {
		if r.Category == proxy.CostCatAux {
			auxRows++
		}
	}
	if auxRows == 0 {
		t.Error("aux call did not land under the aux category")
	}
}
