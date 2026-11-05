package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/treeol/wakil/internal/memory"
	"github.com/treeol/wakil/internal/proxy"
)

// --- B1: an exhausted child costs at most ONE extra turn, and garbage becomes a
// structured partial (remaining_scope), never Uncertainty/findings noise.

func TestExhaustedGarbageFinalTurnYieldsRemainingScopeNotUncertainty(t *testing.T) {
	garbage := strings.Repeat("no json here ", 400) // > 2 KB
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		switch n {
		case 1:
			writeSSE(w, toolCallFrames("r1", "read_file", `{"path":"a.go"}`)...)
		default: // forced final turn, and the single reserve turn: both garbage
			writeSSE(w, contentChunk(garbage))
		}
	}))
	defer srv.Close()

	exec := newFakeExecutor()
	exec.files["a.go"] = "content"
	parent := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })
	parent.subMaxToolIter = 1

	summary, _, _, _, _, _ := parent.dispatchSubagent(context.Background(), "find things", io.Discard, "", "", "", "read", nil)

	if got := calls.Load(); got != 3 {
		t.Fatalf("model calls = %d, want 3 (tool turn, forced final, ONE reserve turn)", got)
	}
	if summary.Status != "incomplete" {
		t.Errorf("status = %q, want incomplete", summary.Status)
	}
	if len(summary.Findings) != 0 {
		t.Errorf("findings = %v, want none for an unparseable exhausted dispatch", summary.Findings)
	}
	if summary.RemainingScope == "" || len(summary.RemainingScope) > subagentRemainingScopeCap {
		t.Errorf("remaining_scope len = %d, want 1..%d", len(summary.RemainingScope), subagentRemainingScopeCap)
	}
	for _, u := range summary.Uncertainty {
		if strings.Contains(u, "no json here") {
			t.Errorf("raw tail leaked into Uncertainty: %q", u)
		}
	}
}

func TestExhaustedValidFinalTurnSpendsNoExtraTurn(t *testing.T) {
	var calls atomic.Int32
	summaryJSON := `{"objective":"x","findings":[{"summary":"f","location":"a.go:1","kind":"fact","weight":"low"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			writeSSE(w, toolCallFrames("r1", "read_file", `{"path":"a.go"}`)...)
			return
		}
		writeSSE(w, contentChunk(summaryJSON))
	}))
	defer srv.Close()

	exec := newFakeExecutor()
	exec.files["a.go"] = "content"
	parent := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })
	parent.subMaxToolIter = 1

	summary, _, _, _, _, _ := parent.dispatchSubagent(context.Background(), "x", io.Discard, "", "", "", "read", nil)
	if got := calls.Load(); got != 2 {
		t.Fatalf("model calls = %d, want 2: a parseable forced final turn must not trigger a reserve turn", got)
	}
	if summary.Status != "incomplete" || len(summary.Findings) != 1 {
		t.Errorf("status/findings = %q/%d, want incomplete/1", summary.Status, len(summary.Findings))
	}
}

func TestTailBytesRuneBoundary(t *testing.T) {
	s := strings.Repeat("é", 3000) // 2 bytes each
	got := tailBytes(s, 2048)
	if len(got) > 2048 || !strings.HasPrefix(got, "é") && got != "" {
		t.Errorf("tail len=%d, must be <=2048 and start on a rune boundary", len(got))
	}
	if tailBytes("short", 2048) != "short" {
		t.Error("short strings must pass through")
	}
}

// --- B2: the class budget never RAISES an explicit operator cap.

func TestCapSubagentWorkBudget(t *testing.T) {
	cases := []struct{ work, explicit, want int }{
		{194000, 0, 194000},      // unset: class budget stands
		{194000, 120000, 120000}, // explicit lowers
		{60000, 120000, 60000},   // explicit never raises
		{60000, 60000, 60000},
	}
	for _, c := range cases {
		if got := capSubagentWorkBudget(c.work, c.explicit); got != c.want {
			t.Errorf("cap(%d,%d) = %d, want %d", c.work, c.explicit, got, c.want)
		}
	}
}

// --- B4 (read side): a tainted session reads back its own quarantined plan,
// and the redirect is exact-key only.

func TestTaintedPlanReadBackAndExactKey(t *testing.T) {
	app, _ := memoryTestApp(t, false)
	app.touchedExternal.Store(true)
	ctx := context.Background()

	put := func(key string) string {
		return app.handleMemoryPut(ctx, proxy.ToolCall{Function: proxy.FunctionCall{Name: "memory_put",
			Arguments: `{"key":"` + key + `","value":"the plan","kind":"plan","ttl_seconds":3600}`}})
	}
	if out := put("plan/current"); !strings.Contains(out, "quarantined") {
		t.Fatalf("tainted plan write not quarantined: %s", out)
	}
	get := app.handleMemoryGet(ctx, proxy.ToolCall{Function: proxy.FunctionCall{Name: "memory_get", Arguments: `{"key":"plan/current"}`}})
	if !strings.Contains(get, "the plan") {
		t.Errorf("tainted session cannot read back its quarantined plan: %s", get)
	}
	// Lookalike key must NOT be redirected into the plan namespace.
	put("plan/currentfoo")
	if _, err := app.getMemoryStore().List(ctx, "plan/tainted/", "", memory.StatusProposed); err != nil {
		t.Fatal(err)
	}
	ents, _ := app.getMemoryStore().List(ctx, "plan/tainted/", "", memory.StatusProposed)
	if len(ents) != 1 {
		t.Errorf("plan/tainted entries = %d, want exactly 1 (plan/currentfoo must not be redirected)", len(ents))
	}
}
