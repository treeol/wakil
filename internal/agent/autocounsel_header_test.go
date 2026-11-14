package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
)

// TestAutoCounselResultHeader pins Group 5: the result of an automatic
// mashura__debug round (which the model did not call) must begin with one
// line stating that Wakil started it automatically.
func TestAutoCounselResultHeader(t *testing.T) {
	want := "[automatic mashura round — Wakil started this round automatically; you did not call it]\n\n"
	if autoCounselResultHeader != want {
		t.Fatalf("autoCounselResultHeader = %q, want %q", autoCounselResultHeader, want)
	}
	if !strings.HasPrefix(autoCounselResultHeader+`REPORT BODY`, want) {
		t.Fatal("header must prefix the result verbatim")
	}
	// The header is one line: exactly one \n before the report starts.
	lines := strings.SplitN(strings.TrimSuffix(want, "\n"), "\n", 2)
	if len(lines) != 2 || lines[1] != "" {
		t.Fatalf("header must be one line plus blank line, got %q", want)
	}
	_ = proxy.Message{}
	_ = config.Config{}
}

// TestAutoCounselInjectedAsWakilMessage pins Group B rule 3: after an
// automatic round, the outgoing request payload contains no assistant
// message with a mashura tool call for that round, and the header text is
// present exactly once per round inside a message from Wakil.
func TestAutoCounselInjectedAsWakilMessage(t *testing.T) {
	counselServer(t)
	t.Setenv("MASHURA_TEST_KEY", "x")
	t.Setenv("MASHURA_TEST_OR_KEY", "x")

	app := &App{
		Cfg: config.Config{
			OracleAPIKeyEnv:      "MASHURA_TEST_KEY",
			OpenRouterAPIKeyEnv:  "MASHURA_TEST_OR_KEY",
			OracleModel:          "anthropic:test-model",
			OracleMaxTokens:      4096,
			OracleEnabled:        true,
			OracleTimeoutSeconds: 5,
		},
		Client:      &proxy.Client{},
		Exec:        newFakeExecutor(),
		Out:         &strings.Builder{},
		Confirm:     func(_, _, _ string, _ bool) bool { return true },
		AutoCounsel: true,
		MaxCounsel:  3,
	}
	app.recentTraces = buildStruggleTraces()

	app.maybeSuggestDebug(context.Background())

	// The outgoing payload is the projection the request is built from.
	proj := app.contextProjection()
	headerCount := 0
	for _, m := range proj {
		for _, tc := range m.ToolCalls {
			if strings.HasPrefix(tc.Function.Name, "mashura__") {
				t.Fatalf("outgoing payload holds an assistant mashura tool call (%q) the model did not make: %+v", tc.Function.Name, m)
			}
		}
		if m.Content != nil {
			headerCount += strings.Count(*m.Content, autoCounselResultHeader)
			// A tool-result message carrying the header would be a tool
			// result for a call the model never made.
			if m.Role == "tool" && strings.Contains(*m.Content, autoCounselResultHeader) {
				t.Fatalf("automatic round delivered as a tool result; want a message from Wakil")
			}
		}
	}
	if headerCount != 1 {
		t.Fatalf("header text present %d times in the outgoing payload, want exactly once", headerCount)
	}
}

// TestAutoCounselTabEvents pins Group B rule 2: an automatic round emits
// the same AsyncJobStartMsg/AsyncJobDoneMsg the async requested path
// emits, marked as started by Wakil, and the cost is recorded once.
func TestAutoCounselTabEvents(t *testing.T) {
	counselServer(t)
	t.Setenv("MASHURA_TEST_KEY", "x")
	t.Setenv("MASHURA_TEST_OR_KEY", "x")
	col := &collectEvents{}
	app := &App{
		Cfg: config.Config{
			OracleAPIKeyEnv:      "MASHURA_TEST_KEY",
			OpenRouterAPIKeyEnv:  "MASHURA_TEST_OR_KEY",
			OracleModel:          "anthropic:test-model",
			OracleMaxTokens:      4096,
			OracleEnabled:        true,
			OracleTimeoutSeconds: 5,
		},
		Client:      &proxy.Client{},
		Exec:        newFakeExecutor(),
		Out:         &strings.Builder{},
		EventSink:   col.sink,
		Confirm:     func(_, _, _ string, _ bool) bool { return true },
		AutoCounsel: true,
		MaxCounsel:  3,
	}
	app.recentTraces = buildStruggleTraces()
	app.Costs = proxy.NewCostTracker()

	app.maybeSuggestDebug(context.Background())

	if app.CounselCallsCount() != 1 {
		t.Fatalf("CounselCallsCount = %d, want 1", app.CounselCallsCount())
	}
	var start *AsyncJobStartMsg
	var done *AsyncJobDoneMsg
	for _, ev := range col.snapshot() {
		switch e := ev.(type) {
		case AsyncJobStartMsg:
			s := e
			start = &s
		case AsyncJobDoneMsg:
			d := e
			done = &d
		}
	}
	if start == nil {
		t.Fatal("no AsyncJobStartMsg for the automatic round — the Mashūra tab would stay empty")
	}
	if done == nil {
		t.Fatal("no AsyncJobDoneMsg for the automatic round")
	}
	if start.OpID != done.OpID || start.OpID == "" {
		t.Fatalf("start/done OpID mismatch: %q vs %q", start.OpID, done.OpID)
	}
	if !strings.Contains(start.Label, "started by wakil") {
		t.Fatalf("start label %q must mark the round as started by Wakil", start.Label)
	}
	if !strings.Contains(start.ToolName, "automatic") {
		t.Fatalf("start tool name %q must mark the round automatic", start.ToolName)
	}
	if done.Result == "" {
		t.Fatal("done result is empty")
	}
	// Cost is counted where requested rounds are counted, once: the sync
	// panel's own recordMashuraResults is the single commit. The events
	// are display-only and must not add a second cost entry. (A usable
	// result without a priced rate records an unpriced row, so the row
	// count — not the dollar total — is the invariant.)
	if rows := app.Costs.ExportRows(); len(rows) != 1 {
		t.Fatalf("cost rows = %d, want exactly 1 (no double count)", len(rows))
	}
}

// TestCollapseNoteSeparatesAutoRounds pins Group B rule 3, second half:
// the collapse note does not count an automatic round among the model's
// tool calls; it lists it separately. Automatic rounds now arrive as a
// user message from Wakil (carrying the header line), and sessions
// resumed from before 0.8.3 may still carry the old synthetic
// auto-counsel tool call — both are listed separately, never counted
// as the model's tool calls.
func TestCollapseNoteSeparatesAutoRounds(t *testing.T) {
	w := []proxy.Message{
		tcMsg("c1", "write_file", `{"path":"answer.txt"}`),
		trMsg("c1", "write_file", "ok"),
		{Role: "user", Content: StrPtr(autoCounselResultHeader + "diagnosis")},
		tcMsg("auto-counsel-1", "mashura__debug", `{"symptom":"auto-counsel: rewrote"}`),
		trMsg("auto-counsel-1", "mashura__debug", autoCounselResultHeader+"diagnosis"),
	}
	note := buildCollapseNote(1, w, "/fake/archive")
	if strings.Contains(note, "mashura__debug×1") {
		t.Fatalf("collapse note counts the automatic round as a model tool call:\n%s", note)
	}
	if !strings.Contains(note, "automatic mashura rounds (started by wakil): 2") {
		t.Fatalf("collapse note must list the automatic rounds separately:\n%s", note)
	}
	if !strings.Contains(note, "write_file×1") {
		t.Fatalf("model tool calls still counted:\n%s", note)
	}
}
