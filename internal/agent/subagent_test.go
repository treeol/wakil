package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/tools"
)

// --- Test 1: parent ctx isolation ---
// Parent calls dispatch_subagent; subagent reads a large file; only the ≤4k
// summary enters the parent's Conv — raw file content never touches it.

func TestDispatchSubagentParentCtxIsolated(t *testing.T) {
	rawContent := strings.Repeat("LARGE-RAW-XQ7Z-", 600) // ~9.6k chars

	summaryJSON := `{"objective":"find ToolResultCap","findings":[{"summary":"ToolResultCap set in config.go line 55","location":"config.go:55","kind":"match","weight":"high"}],"checked":[{"path":"config.go","size_k":9,"status":"full"}]}`

	srv := sseServer(t,
		// call 0 — parent: returns dispatch_subagent tool call
		toolCallFrames("d1", "dispatch_subagent", `{"task":"find where ToolResultCap is configured"}`),
		// call 1 — subagent: returns read_file tool call
		toolCallFrames("r1", "read_file", `{"path":"config.go"}`),
		// call 2 — subagent: returns JSON summary (no tool calls → loop exits)
		[]string{contentChunk(summaryJSON)},
		// call 3 — parent: returns final text after receiving summary
		[]string{contentChunk("Found it in config.go:55.")},
	)
	defer srv.Close()

	exec := newFakeExecutor()
	exec.files["config.go"] = rawContent

	app := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })
	final, err := app.Send(context.Background(), "find where ToolResultCap is configured")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(final, "Found it") {
		t.Errorf("unexpected final: %q", final)
	}

	// Parent Conv must NOT contain raw file content.
	for _, m := range app.Conv {
		if strings.Contains(DerefStr(m.Content), "LARGE-RAW-XQ7Z-") {
			t.Errorf("raw file content leaked into parent Conv (role=%q)", m.Role)
		}
	}

	// The tool result in parent Conv must be the ≤4k summary.
	found := false
	for _, m := range app.Conv {
		if m.Role == "tool" && strings.Contains(DerefStr(m.Content), "config.go:55") {
			found = true
			if len(DerefStr(m.Content)) > 4000 {
				t.Errorf("summary in parent Conv is %d chars, want ≤4000", len(DerefStr(m.Content)))
			}
		}
	}
	if !found {
		t.Error("summary not found as tool result in parent Conv")
	}
}

// --- Test 2: malformed JSON degrades to parse-error finding, not an exception ---

func TestDispatchSubagentMalformedJSON(t *testing.T) {
	notJSON := "Here are my results: config.go line 55 has it. Very interesting."
	alsoNotJSON := "Still not JSON, sorry about that."

	srv := sseServer(t,
		[]string{contentChunk(notJSON)},     // subagent first Send → no tool calls → exits
		[]string{contentChunk(alsoNotJSON)}, // subagent retry Send → also not JSON
	)
	defer srv.Close()

	exec := newFakeExecutor()
	parent := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })

	summary, _, _, _, _ := parent.dispatchSubagent(context.Background(), "find something", io.Discard, "")

	if len(summary.Findings) == 0 {
		t.Fatal("expected a degraded finding, got empty Findings")
	}
	if summary.Findings[0].Kind != "parse-error" {
		t.Errorf("kind = %q, want parse-error", summary.Findings[0].Kind)
	}
	if summary.Findings[0].Weight != "low" {
		t.Errorf("weight = %q, want low", summary.Findings[0].Weight)
	}
	if len(summary.Uncertainty) == 0 {
		t.Error("expected uncertainty note for parse failure")
	}
	if summary.Objective != "find something" {
		t.Errorf("objective = %q, want original task", summary.Objective)
	}
}

// --- Test 3: subagent reads file, produces structured summary ---

func TestDispatchSubagentDirectly(t *testing.T) {
	fileContent := "const CompactAt = 145000 // trigger compaction threshold\n"
	summaryJSON := `{"objective":"find CompactAt","findings":[{"summary":"CompactAt defined as 145000","location":"config.go:55","kind":"match","weight":"high"}],"checked":[{"path":"config.go","size_k":1,"status":"full"}],"uncertainty":["only one file checked"]}`

	srv := sseServer(t,
		toolCallFrames("r1", "read_file", `{"path":"config.go"}`),
		[]string{contentChunk(summaryJSON)},
	)
	defer srv.Close()

	exec := newFakeExecutor()
	exec.files["config.go"] = fileContent

	parent := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })
	summary, _, _, _, _ := parent.dispatchSubagent(context.Background(), "find CompactAt", io.Discard, "")

	if summary.Objective == "" {
		t.Error("objective not populated")
	}
	if len(summary.Findings) == 0 {
		t.Error("no findings")
	}
	if summary.Findings[0].Kind != "match" {
		t.Errorf("kind = %q, want match", summary.Findings[0].Kind)
	}
	if summary.Findings[0].Location != "config.go:55" {
		t.Errorf("location = %q", summary.Findings[0].Location)
	}
	if len(summary.Checked) == 0 {
		t.Error("no checked items")
	}
	if len(summary.Uncertainty) == 0 {
		t.Error("no uncertainty — subagent should acknowledge partial coverage")
	}
}

// --- Test 4: Render() caps at 4000 chars and stays valid JSON ---

func TestSubagentSummaryRenderCap(t *testing.T) {
	s := SubagentSummary{
		Objective: "test cap",
		Findings:  make([]Finding, 30),
	}
	for i := range s.Findings {
		s.Findings[i] = Finding{
			Summary:  strings.Repeat("a", 200),
			Location: "file.go:1",
			Kind:     "match",
			Weight:   "high",
		}
	}
	rendered := s.Render()
	if len(rendered) > 4000 {
		t.Errorf("Render() = %d chars, want ≤4000", len(rendered))
	}
	var check SubagentSummary
	if err := json.Unmarshal([]byte(rendered), &check); err != nil {
		t.Errorf("Render() is not valid JSON: %v\n%s", err, rendered[:min(len(rendered), 200)])
	}
	if len(check.Findings) == 0 {
		t.Error("Render() trimmed all findings")
	}
}

// --- Test 5: NoMemoryWrite=true on subagent's client ---

func TestDispatchSubagentNoMemoryWrite(t *testing.T) {
	var capturedNoMemHeader string

	// Capture server: records the X-Ilm-No-Memory-Write header then returns a summary
	summaryJSON := `{"objective":"check","findings":[{"summary":"done","location":"","kind":"fact","weight":"low"}]}`
	captureSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedNoMemHeader = r.Header.Get("X-Ilm-No-Memory-Write")
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: "+contentChunk(summaryJSON)+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer captureSrv.Close()

	exec := newFakeExecutor()
	parent := &App{
		Cfg:     config.DefaultConfig(),
		Client:  newTestClient(captureSrv.URL),
		Exec:    exec,
		Tools:   tools.DefaultTools("/work"),
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
	}

	parent.dispatchSubagent(context.Background(), "check something", io.Discard, "")

	if capturedNoMemHeader != "true" {
		t.Errorf("X-Ilm-No-Memory-Write = %q, want %q", capturedNoMemHeader, "true")
	}
}

// --- P31 tests: subagent backend resolution, header propagation, egress gate ---

// TestResolveSubagentBackendInherit verifies "inherit" (and "") both yield the
// parent's current backend.
func TestResolveSubagentBackendInherit(t *testing.T) {
	for _, cfg := range []string{"inherit", ""} {
		got := ResolveSubagentBackend("openrouter", cfg)
		if got != "openrouter" {
			t.Errorf("cfg=%q: ResolveSubagentBackend(\"openrouter\", %q) = %q, want %q",
				cfg, cfg, got, "openrouter")
		}
	}
	// Inheriting an empty parent gives empty.
	got := ResolveSubagentBackend("", "inherit")
	if got != "" {
		t.Errorf("inherit from empty parent: got %q, want %q", got, "")
	}
}

// TestResolveSubagentBackendDefault verifies "default" always gives empty string
// (no X-Ilm-Backend header sent — proxy uses its default).
func TestResolveSubagentBackendDefault(t *testing.T) {
	got := ResolveSubagentBackend("openrouter", "default")
	if got != "" {
		t.Errorf("\"default\": got %q, want \"\" (no header)", got)
	}
}

// TestResolveSubagentBackendPinned verifies a named config value is used
// regardless of the parent's current backend.
func TestResolveSubagentBackendPinned(t *testing.T) {
	for _, parent := range []string{"openrouter", "", "together"} {
		got := ResolveSubagentBackend(parent, "llama")
		if got != "llama" {
			t.Errorf("pinned: parent=%q, got %q, want \"llama\"", parent, got)
		}
	}
}

// TestSubagentBackendHeaderPropagates verifies that the resolved backend is
// forwarded as X-Ilm-Backend on the subagent's outgoing request (the P31 bug fix).
func TestSubagentBackendHeaderPropagates(t *testing.T) {
	var capturedHeader string
	summaryJSON := `{"objective":"check","findings":[{"summary":"done","location":"","kind":"fact","weight":"low"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeader = r.Header.Get("X-Ilm-Backend")
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: "+contentChunk(summaryJSON)+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	parent := &App{
		Cfg:     config.DefaultConfig(),
		Client:  newTestClient(srv.URL),
		Exec:    newFakeExecutor(),
		Tools:   tools.DefaultTools("/work"),
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
	}

	parent.dispatchSubagent(context.Background(), "check", io.Discard, "openrouter")

	if capturedHeader != "openrouter" {
		t.Errorf("X-Ilm-Backend = %q, want %q (backend not propagated)", capturedHeader, "openrouter")
	}
}

// TestSubagentHeaderOmittedWhenEmpty verifies no X-Ilm-Backend header is sent
// when the resolved backend is empty (proxy-default routing).
func TestSubagentHeaderOmittedWhenEmpty(t *testing.T) {
	var capturedHeader string
	summaryJSON := `{"objective":"check","findings":[{"summary":"done","location":"","kind":"fact","weight":"low"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeader = r.Header.Get("X-Ilm-Backend")
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: "+contentChunk(summaryJSON)+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	parent := &App{
		Cfg:     config.DefaultConfig(),
		Client:  newTestClient(srv.URL),
		Exec:    newFakeExecutor(),
		Tools:   tools.DefaultTools("/work"),
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
	}

	parent.dispatchSubagent(context.Background(), "check", io.Discard, "")

	if capturedHeader != "" {
		t.Errorf("X-Ilm-Backend should be absent for empty backend; got %q", capturedHeader)
	}
}

// TestSubagentUsedBackendReturned verifies dispatchSubagent returns the
// X-Ilm-Backend-Used header value from the subagent's last Stream call.
func TestSubagentUsedBackendReturned(t *testing.T) {
	summaryJSON := `{"objective":"check","findings":[{"summary":"done","location":"","kind":"fact","weight":"low"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Ilm-Backend-Used", "openrouter")
		fmt.Fprint(w, "data: "+contentChunk(summaryJSON)+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	parent := &App{
		Cfg:     config.DefaultConfig(),
		Client:  newTestClient(srv.URL),
		Exec:    newFakeExecutor(),
		Tools:   tools.DefaultTools("/work"),
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
	}

	_, _, _, usedBackend, _ := parent.dispatchSubagent(context.Background(), "check", io.Discard, "openrouter")
	if usedBackend != "openrouter" {
		t.Errorf("usedBackend = %q, want %q", usedBackend, "openrouter")
	}
}

// TestSubagentEgressGateFiresForExternal verifies that dispatching a subagent
// to an unconsented external backend triggers the egress gate.
func TestSubagentEgressGateFiresForExternal(t *testing.T) {
	summaryJSON := `{"objective":"check","findings":[{"summary":"done","location":"","kind":"fact","weight":"low"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: "+contentChunk(summaryJSON)+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	prompts := 0
	parent := &App{
		Cfg: func() config.Config {
			c := config.DefaultConfig()
			c.ExternalBackends = []string{"openrouter"}
			return c
		}(),
		Client: newTestClient(srv.URL),
		Exec:   newFakeExecutor(),
		Tools:  tools.DefaultTools("/work"),
		Out:    io.Discard,
		Confirm: func(toolName, _, _ string, _ bool) bool {
			if toolName == "external_backend" {
				prompts++
			}
			return true // approve
		},
	}

	parent.dispatchSubagentGated(context.Background(), "check", io.Discard, "openrouter")
	if prompts != 1 {
		t.Errorf("expected 1 egress prompt for external subagent backend; got %d", prompts)
	}
	// Second dispatch: already consented, no new prompt.
	parent.dispatchSubagentGated(context.Background(), "check again", io.Discard, "openrouter")
	if prompts != 1 {
		t.Errorf("second dispatch to same consented backend should not re-prompt; got %d total prompts", prompts)
	}
}

// TestSubagentEgressDeclineReturnsUncertainty verifies that declining the
// egress gate results in a summary with uncertainty and no request is made.
func TestSubagentEgressDeclineReturnsUncertainty(t *testing.T) {
	requestCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	parent := &App{
		Cfg: func() config.Config {
			c := config.DefaultConfig()
			c.ExternalBackends = []string{"openrouter"}
			return c
		}(),
		Client:  newTestClient(srv.URL),
		Exec:    newFakeExecutor(),
		Tools:   tools.DefaultTools("/work"),
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return false }, // always decline
	}

	summary, _, _, _, _ := parent.dispatchSubagentGated(context.Background(), "check", io.Discard, "openrouter")
	if requestCount != 0 {
		t.Errorf("no request should be sent after egress decline; got %d", requestCount)
	}
	if len(summary.Uncertainty) == 0 {
		t.Error("declined egress should result in uncertainty note")
	}
	if !strings.Contains(summary.Uncertainty[0], "declined") {
		t.Errorf("uncertainty should mention decline; got %q", summary.Uncertainty[0])
	}
}

// TestSubagentInheritFromMain verifies the full dispatch path: when
// subagent_backend="inherit" and main has a selected backend, the subagent's
// request carries that backend header.
func TestSubagentInheritFromMain(t *testing.T) {
	var capturedHeaders []string
	summaryJSON := `{"objective":"check","findings":[{"summary":"done","location":"","kind":"fact","weight":"low"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeaders = append(capturedHeaders, r.Header.Get("X-Ilm-Backend"))
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: "+contentChunk(summaryJSON)+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	cfg := config.DefaultConfig()
	cfg.SubagentBackend = "inherit"
	parent := &App{
		Cfg:             cfg,
		Client:          newTestClient(srv.URL),
		Exec:            newFakeExecutor(),
		Tools:           tools.DefaultTools("/work"),
		Out:             io.Discard,
		Confirm:         func(_, _, _ string, _ bool) bool { return true },
		SelectedBackend: "openrouter",
	}

	subBackend := ResolveSubagentBackend(parent.SelectedBackend, parent.Cfg.SubagentBackend)
	parent.dispatchSubagent(context.Background(), "check", io.Discard, subBackend)

	if len(capturedHeaders) == 0 {
		t.Fatal("no request made")
	}
	if capturedHeaders[0] != "openrouter" {
		t.Errorf("subagent X-Ilm-Backend = %q, want \"openrouter\" (inherited from main)", capturedHeaders[0])
	}
}

// TestSubagentPinnedIgnoresMain verifies that a pinned subagent_backend is used
// regardless of the main session's current backend.
func TestSubagentPinnedIgnoresMain(t *testing.T) {
	var capturedHeader string
	summaryJSON := `{"objective":"check","findings":[{"summary":"done","location":"","kind":"fact","weight":"low"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeader = r.Header.Get("X-Ilm-Backend")
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: "+contentChunk(summaryJSON)+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	cfg := config.DefaultConfig()
	cfg.SubagentBackend = "llama"
	parent := &App{
		Cfg:             cfg,
		Client:          newTestClient(srv.URL),
		Exec:            newFakeExecutor(),
		Tools:           tools.DefaultTools("/work"),
		Out:             io.Discard,
		Confirm:         func(_, _, _ string, _ bool) bool { return true },
		SelectedBackend: "openrouter", // main is on OR
	}

	subBackend := ResolveSubagentBackend(parent.SelectedBackend, parent.Cfg.SubagentBackend)
	parent.dispatchSubagent(context.Background(), "check", io.Discard, subBackend)

	if capturedHeader != "llama" {
		t.Errorf("pinned subagent backend: X-Ilm-Backend = %q, want \"llama\"", capturedHeader)
	}
}

// --- Test 6: extractJSON handles fences and bare objects ---

func TestExtractJSON(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`{"a":1}`, `{"a":1}`},
		{"```json\n{\"a\":1}\n```", `{"a":1}`},
		{"```\n{\"a\":1}\n```", `{"a":1}`},
		{"Here is the result:\n{\"a\":1}\nThat's it.", `{"a":1}`},
		{"no json here", "no json here"},
	}
	for _, c := range cases {
		got := extractJSON(c.in)
		if got != c.want {
			t.Errorf("extractJSON(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
