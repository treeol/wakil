package wiring

// resultjson_test.go: the --result-json summary file — shape, counts, and
// unknown-value nulls, driven through the real headless path against the
// fake SSE backend.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
)

func TestResultJSON_SingleTask(t *testing.T) {
	toolFrame := toolCallFrames("call_1", "read_file", `{"path":"a.go"}`)
	answerFrame := []string{contentChunk("done\n")}
	srv := sseServer(t, toolFrame, answerFrame, answerFrame)
	defer srv.Close()

	app := fakeApp(srv.URL)
	app.Client.Model = "ilm"
	resultPath := filepath.Join(t.TempDir(), "result.json")

	var out strings.Builder
	start := time.Now()
	code := RunHeadlessApp(context.Background(), app, "read a.go", false,
		HeadlessOptions{Auto: true, ResultJSON: resultPath}, &out)
	if code != ExitOK {
		// A tool call may need a second turn; accept any non-error code.
		if code == ExitError || code == ExitBackendFailure {
			t.Fatalf("exit = %d, want non-error; out:\n%s", code, out.String())
		}
	}
	_ = start

	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("result file: %v", err)
	}
	var res ResultJSON
	if err := json.Unmarshal(data, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if res.ExitReason == "" {
		t.Error("exit_reason empty")
	}
	if res.Model != "ilm" {
		t.Errorf("model = %q, want ilm (resolved from the client)", res.Model)
	}
	if res.Version == "" {
		t.Error("wakil_version empty")
	}
	if res.Seconds < 0 {
		t.Errorf("seconds = %v, want >= 0", res.Seconds)
	}
	if res.ToolCalls == nil || *res.ToolCalls < 1 {
		t.Errorf("tool_calls = %v, want >= 1 (one read_file ran)", res.ToolCalls)
	}
	if res.Collapses == nil {
		t.Error("collapses nil, want a count (0 when none)")
	}
	if res.CollapsesTurnEnd == nil {
		t.Error("collapses_turn_end nil, want a count (0 when none)")
	}
	if res.MashuraRounds == nil {
		t.Error("mashura_rounds nil, want a count (0 when none)")
	}
	// Tokens/cost may be null when the fake backend reports no usage —
	// the contract is null, not zero, for unknown values.
	if res.TokensIn != nil && *res.TokensIn < 0 {
		t.Errorf("tokens_in negative: %d", *res.TokensIn)
	}
}

func TestResultJSON_NilApp(t *testing.T) {
	p := filepath.Join(t.TempDir(), "r.json")
	if err := writeResultJSON(p, nil, ExitError, time.Now(), ""); err != nil {
		t.Fatalf("writeResultJSON: %v", err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var res ResultJSON
	if err := json.Unmarshal(data, &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if res.ExitReason != "error" {
		t.Errorf("exit_reason = %q, want error", res.ExitReason)
	}
	if res.Model != "" || res.TokensIn != nil || res.ToolCalls != nil {
		t.Errorf("nil app must leave unknowns null/empty, got %+v", res)
	}
}

func TestExitReasonName(t *testing.T) {
	cases := map[int]string{
		ExitOK:             "pass",
		ExitDeclined:       "declined",
		ExitBackendFailure: "backend_failure",
		ExitGaps:           "gaps",
		ExitError:          "error",
		ExitIncomplete:     "incomplete",
	}
	for code, want := range cases {
		if got := exitReasonName(code); got != want {
			t.Errorf("exitReasonName(%d) = %q, want %q", code, got, want)
		}
	}
}

func TestResultJSON_CountAutoRoundsFromConv(t *testing.T) {
	header := "[automatic mashura round — started by Wakil] ..."
	userText := "prompt " + header
	conv := []proxy.Message{
		{Role: "user", Content: &userText, ToolCalls: []proxy.ToolCall{
			{ID: "auto-counsel-7", Function: proxy.FunctionCall{Name: "mashura__debug", Arguments: "{}"}},
		}},
	}
	if got := agent.AutoRoundsInConv(conv); got != 2 {
		t.Errorf("AutoRoundsInConv = %d, want 2 (header + legacy auto-counsel id)", got)
	}
}

func TestResultJSON_CollapsesSplitAndTokensCached(t *testing.T) {
	app := fakeApp("http://unused.invalid")
	app.Client.Model = "openrouter/test"
	app.Costs = &proxy.CostTracker{}
	// Record cost with cached input tokens.
	app.Costs.Record("inference·openrouter/test", 100000, 2000, 0.01234, true, proxy.ConfExact, config.TokenDetail{CachedTok: 75000})
	// Simulate 1 turn-end collapse (Partial: false) and 1 mid-turn collapse (Partial: true).
	app.RestoreTurnState(&agent.Session{
		Collapsed: []agent.CollapseRecord{
			{Turn: 1, Partial: false, Note: "turn 1 done"},
			{Turn: 2, Partial: true, Note: "turn 2 mid-turn"},
		},
	})

	p := filepath.Join(t.TempDir(), "r.json")
	if err := writeResultJSON(p, app, ExitOK, time.Now(), "/tmp/ws"); err != nil {
		t.Fatalf("writeResultJSON: %v", err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var res ResultJSON
	if err := json.Unmarshal(data, &res); err != nil {
		t.Fatal(err)
	}
	if res.Collapses == nil || *res.Collapses != 1 {
		t.Errorf("collapses (mid-turn) = %v, want 1", res.Collapses)
	}
	if res.CollapsesTurnEnd == nil || *res.CollapsesTurnEnd != 1 {
		t.Errorf("collapses_turn_end = %v, want 1", res.CollapsesTurnEnd)
	}
	if res.TokensCached == nil || *res.TokensCached != 75000 {
		t.Errorf("tokens_cached = %v, want 75000", res.TokensCached)
	}

	// Without cached tokens reported, tokens_cached must be omitted / nil.
	app2 := fakeApp("http://unused.invalid")
	app2.Client.Model = "openrouter/test"
	app2.Costs = &proxy.CostTracker{}
	app2.Costs.Record("inference·openrouter/test", 1000, 100, 0.001, true, proxy.ConfExact)
	p2 := filepath.Join(t.TempDir(), "r2.json")
	if err := writeResultJSON(p2, app2, ExitOK, time.Now(), "/tmp/ws"); err != nil {
		t.Fatal(err)
	}
	data2, err := os.ReadFile(p2)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data2), `"tokens_cached"`) {
		t.Errorf("tokens_cached must be omitted when provider reported none; got %s", string(data2))
	}
}

func TestResultJSON_DeclinedCalls(t *testing.T) {
	app := fakeApp("http://unused.invalid")
	app.Client.Model = "openrouter/test"
	app.Costs = &proxy.CostTracker{}
	// Simulate 3 declined tool calls by setting the counter directly via
	// the exported test helper.
	app.SetDeclinedCallCount(3)

	p := filepath.Join(t.TempDir(), "r.json")
	if err := writeResultJSON(p, app, ExitOK, time.Now(), "/tmp/ws"); err != nil {
		t.Fatalf("writeResultJSON: %v", err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var res ResultJSON
	if err := json.Unmarshal(data, &res); err != nil {
		t.Fatal(err)
	}
	if res.DeclinedCalls == nil || *res.DeclinedCalls != 3 {
		t.Errorf("declined_calls = %v, want 3", res.DeclinedCalls)
	}

	// Zero declines: the field is present (pointer to 0), not nil.
	app2 := fakeApp("http://unused.invalid")
	app2.Client.Model = "openrouter/test"
	app2.Costs = &proxy.CostTracker{}
	p2 := filepath.Join(t.TempDir(), "r2.json")
	if err := writeResultJSON(p2, app2, ExitOK, time.Now(), "/tmp/ws"); err != nil {
		t.Fatal(err)
	}
	data2, err := os.ReadFile(p2)
	if err != nil {
		t.Fatal(err)
	}
	var res2 ResultJSON
	if err := json.Unmarshal(data2, &res2); err != nil {
		t.Fatal(err)
	}
	if res2.DeclinedCalls == nil || *res2.DeclinedCalls != 0 {
		t.Errorf("declined_calls = %v, want 0 (present, not nil)", res2.DeclinedCalls)
	}
}
