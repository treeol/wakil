package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/exec"
	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/tools"
)

// sseServer returns a test server that replies with the given SSE frames.
func sseServer(t *testing.T, framesPerCall ...[]string) *httptest.Server {
	t.Helper()
	call := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			http.NotFound(w, r)
			return
		}
		frames := framesPerCall[0]
		if call < len(framesPerCall) {
			frames = framesPerCall[call]
		}
		call++
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

func contentChunk(s string) string {
	return fmt.Sprintf(`{"choices":[{"delta":{"content":%q},"finish_reason":null}]}`, s)
}

// toolCall frames mimic the proxy: an opening frame with id+name, then
// incremental argument fragments, then a finish frame.
func toolCallFrames(id, name string, argParts ...string) []string {
	frames := []string{
		fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":%q,"arguments":""}}]},"finish_reason":null}]}`, id, name),
	}
	for _, p := range argParts {
		frames = append(frames, fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":%q}}]},"finish_reason":null}]}`, p))
	}
	frames = append(frames, `{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
	return frames
}

func newTestClient(url string) *proxy.Client {
	return &proxy.Client{BaseURL: url, Model: "ilm", ChatID: "test", HTTP: http.DefaultClient}
}

// fakeExecutor records calls and returns canned output.
type fakeExecutor struct {
	shellCalls []string
	writeCalls map[string]string
	files      map[string]string
	dirs       map[string]bool // paths that should report "is a directory" on ReadFile
}

func newFakeExecutor() *fakeExecutor {
	return &fakeExecutor{writeCalls: map[string]string{}, files: map[string]string{}, dirs: map[string]bool{}}
}
func (f *fakeExecutor) RunShell(_ context.Context, c string) (string, error) {
	f.shellCalls = append(f.shellCalls, c)
	return "ran: " + c, nil
}
func (f *fakeExecutor) StatFile(p string) (int64, error) {
	if v, ok := f.files[p]; ok {
		return int64(len(v)), nil
	}
	return 0, fmt.Errorf("no such file: %s", p)
}
func (f *fakeExecutor) ReadFile(p string) (string, error) {
	if f.dirs[p] {
		return "", fmt.Errorf("read %s: is a directory", p)
	}
	if v, ok := f.files[p]; ok {
		return v, nil
	}
	return "", fmt.Errorf("no such file: %s", p)
}
func (f *fakeExecutor) ListDir(p string) (string, error) {
	var names []string
	for k := range f.files {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, "\n"), nil
}
func (f *fakeExecutor) WriteFile(p, c string) (string, error) {
	f.writeCalls[p] = c
	f.files[p] = c
	return fmt.Sprintf("wrote %d bytes to %s", len(c), p), nil
}
func (f *fakeExecutor) Cwd() string           { return "/work" }
func (f *fakeExecutor) WorkspaceRoot() string { return "/work" }
func (f *fakeExecutor) Describe() string      { return "fake" }
func (f *fakeExecutor) Close() error          { return nil }
func (f *fakeExecutor) SandboxTools() string  { return "" }
func (f *fakeExecutor) Generation() int       { return 1 }
func (f *fakeExecutor) ConfinePath(_ context.Context, path string) (string, error) {
	return path, nil
}
func (f *fakeExecutor) DeletePath(_ context.Context, path string) error   { return nil }
func (f *fakeExecutor) MovePath(_ context.Context, src, dst string) error { return nil }
func (f *fakeExecutor) StartBackground(_ context.Context, command, logPath string) (int, int, error) {
	return 1234, 1234, nil
}
func (f *fakeExecutor) KillPgid(_ context.Context, pgid, sig int) error { return nil }
func (f *fakeExecutor) IsProcessAlive(_ context.Context, pid int) bool  { return false }
func (f *fakeExecutor) ReadFileTail(_ context.Context, path string, maxBytes int64) (string, error) {
	return "", nil
}
func (f *fakeExecutor) StartInteractive(_ context.Context, command string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, int, error) {
	return nil, nil, nil, 0, fmt.Errorf("not implemented in fake executor")
}
func (f *fakeExecutor) HostPathToURI(hostPath string) (string, error) {
	return "file://" + hostPath, nil
}
func (f *fakeExecutor) URIToHostPath(uri string) (string, error) {
	return strings.TrimPrefix(uri, "file://"), nil
}

func newTestApp(url string, executor exec.Executor, confirm agent.Confirmer) *agent.App {
	return &agent.App{
		Cfg:     config.DefaultConfig(),
		Client:  newTestClient(url),
		Exec:    executor,
		Tools:   tools.DefaultTools("/work"),
		Out:     io.Discard,
		Confirm: confirm,
	}
}

// 1) Tool-call assembly: incremental argument fragments reconstruct valid JSON.
func TestStreamAssemblesToolCall(t *testing.T) {
	srv := sseServer(t, append(
		[]string{contentChunk("Here are the files:")},
		toolCallFrames("abc123", "run_shell", `{"command":"`, "ls", " -la", `"}`)...,
	))
	defer srv.Close()

	msg, err := newTestClient(srv.URL).Stream(context.Background(), nil, tools.DefaultTools("/work"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if agent.DerefStr(msg.Content) != "Here are the files:" {
		t.Errorf("content = %q", agent.DerefStr(msg.Content))
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("want 1 tool_call, got %d", len(msg.ToolCalls))
	}
	tc := msg.ToolCalls[0]
	if tc.ID != "abc123" || tc.Function.Name != "run_shell" {
		t.Errorf("bad tool_call meta: %+v", tc)
	}
	if tc.Function.Arguments != `{"command":"ls -la"}` {
		t.Errorf("assembled args = %q", tc.Function.Arguments)
	}
}

// 2) Plain-text branch (memory/learn/meta acks) must not crash and yields no tool calls.
func TestStreamPlainTextNoToolCalls(t *testing.T) {
	srv := sseServer(t, []string{
		contentChunk("Learned (manual): my notebook is called rubin"),
	})
	defer srv.Close()

	msg, err := newTestClient(srv.URL).Stream(context.Background(), nil, tools.DefaultTools("/work"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.ToolCalls) != 0 {
		t.Fatalf("expected no tool_calls, got %d", len(msg.ToolCalls))
	}
	if !strings.Contains(agent.DerefStr(msg.Content), "Learned (manual)") {
		t.Errorf("content = %q", agent.DerefStr(msg.Content))
	}
}

// 3) Full agent loop: tool_call -> gate(yes) -> execute -> result fed back -> final text.
func TestAgentLoopExecutesAndFinishes(t *testing.T) {
	srv := sseServer(t,
		toolCallFrames("c1", "run_shell", `{"command":"echo hi"}`), // call 1
		[]string{contentChunk("Done — I ran the command.")},        // call 2
	)
	defer srv.Close()

	fexec := newFakeExecutor()
	app := newTestApp(srv.URL, fexec, func(_, _, _ string, _ bool) bool { return true })

	final, err := app.Send(context.Background(), "run echo hi")
	if err != nil {
		t.Fatal(err)
	}
	if len(fexec.shellCalls) != 1 || fexec.shellCalls[0] != "echo hi" {
		t.Errorf("shell calls = %v", fexec.shellCalls)
	}
	if !strings.Contains(final, "Done") {
		t.Errorf("final = %q", final)
	}
	// Conversation must contain a role:"tool" result for the loop to have closed.
	foundTool := false
	for _, m := range app.Conv {
		if m.Role == "tool" && strings.Contains(agent.DerefStr(m.Content), "ran: echo hi") {
			foundTool = true
		}
	}
	if !foundTool {
		t.Errorf("tool result not threaded into conversation: %+v", app.Conv)
	}
}

// 6) Declining the gate aborts cleanly, feeds back a declined result, loop continues.
func TestConfirmDeclineAborts(t *testing.T) {
	srv := sseServer(t,
		toolCallFrames("c1", "run_shell", `{"command":"rm -rf /"}`),
		[]string{contentChunk("Understood, I won't run it.")},
	)
	defer srv.Close()

	fexec := newFakeExecutor()
	app := newTestApp(srv.URL, fexec, func(_, _, _ string, _ bool) bool { return false })

	final, err := app.Send(context.Background(), "delete everything")
	if err != nil {
		t.Fatal(err)
	}
	if len(fexec.shellCalls) != 0 {
		t.Fatalf("declined command must NOT execute, got %v", fexec.shellCalls)
	}
	if !strings.Contains(final, "won't") {
		t.Errorf("final = %q", final)
	}
	declined := false
	for _, m := range app.Conv {
		if m.Role == "tool" && strings.Contains(agent.DerefStr(m.Content), "declined") {
			declined = true
		}
	}
	if !declined {
		t.Error("expected a [declined by user] tool result in the conversation")
	}
}

// TestNoMemoryWriteFlag verifies that Client.NoMemoryWrite=true transmits both
// the X-Ilm-No-Memory-Write request header and metadata["ilm-no-memory-write"]
// in the request body. This covers the sender side; proxy-side enforcement
// (that the write path is actually gated) must be verified against a live
// proxy by inspecting agent_memory.db before and after a subagent turn.
func TestNoMemoryWriteFlag(t *testing.T) {
	var capturedHeader, capturedMeta string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeader = r.Header.Get("X-Ilm-No-Memory-Write")
		var body struct {
			Metadata map[string]string `json:"metadata"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			capturedMeta = body.Metadata["ilm-no-memory-write"]
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: "+contentChunk("ok")+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	client := &proxy.Client{
		BaseURL:       srv.URL,
		Model:         "ilm",
		ChatID:        "test-nomem",
		NoMemoryWrite: true,
		HTTP:          http.DefaultClient,
	}
	client.Stream(context.Background(), []proxy.Message{{Role: "user", Content: agent.StrPtr("hi")}}, nil, nil, nil)

	if capturedHeader != "true" {
		t.Errorf("X-Ilm-No-Memory-Write header = %q, want %q", capturedHeader, "true")
	}
	if capturedMeta != "true" {
		t.Errorf("metadata[ilm-no-memory-write] = %q, want %q", capturedMeta, "true")
	}
}

// write_file is gated; read_file is not.
func TestGatedToolSet(t *testing.T) {
	for name, want := range map[string]bool{"run_shell": true, "write_file": true, "read_file": false} {
		if tools.GatedTool(name) != want {
			t.Errorf("gatedTool(%q) = %v, want %v", name, tools.GatedTool(name), want)
		}
	}
}

func reasoningChunk(s string) string {
	return fmt.Sprintf(`{"choices":[{"delta":{"reasoning_content":%q},"finish_reason":null}]}`, s)
}

// TestReasoningNotInMessage verifies that reasoning_content deltas reach the
// reasoningSink but are never included in the returned Message content.
func TestReasoningNotInMessage(t *testing.T) {
	const thought = "I need to reason about this carefully"
	const answer = "The answer is 42"

	srv := sseServer(t, []string{
		reasoningChunk(thought),
		contentChunk(answer),
	})
	defer srv.Close()

	var gotReasoning []string
	msg, err := newTestClient(srv.URL).Stream(
		context.Background(), nil, nil,
		nil,
		func(s string) { gotReasoning = append(gotReasoning, s) },
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotReasoning) == 0 || gotReasoning[0] != thought {
		t.Errorf("reasoning sink got %v, want [%q]", gotReasoning, thought)
	}
	if agent.DerefStr(msg.Content) != answer {
		t.Errorf("message content = %q, want %q", agent.DerefStr(msg.Content), answer)
	}
	if msg.Content != nil && strings.Contains(*msg.Content, thought) {
		t.Error("reasoning text must not appear in message content")
	}
}

// TestReasoningNotInConvHistory is the critical invariant test: after a full
// Send() with a server emitting reasoning deltas, a.Conv contains none of it.
func TestReasoningNotInConvHistory(t *testing.T) {
	const thoughtMarker = "SECRET_REASONING_ABCXYZ"
	const answer = "plain answer"

	srv := sseServer(t, []string{
		reasoningChunk(thoughtMarker),
		contentChunk(answer),
	})
	defer srv.Close()

	fexec := newFakeExecutor()
	app := newTestApp(srv.URL, fexec, func(_, _, _ string, _ bool) bool { return true })
	var reasoningDelivered bool
	app.OnReasoning = func(s string) { reasoningDelivered = true }

	_, err := app.Send(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if !reasoningDelivered {
		t.Error("OnReasoning callback was never called")
	}

	// Serialise the full Conv and assert the thought marker is absent.
	raw, _ := json.Marshal(app.Conv)
	if strings.Contains(string(raw), thoughtMarker) {
		t.Errorf("reasoning text %q leaked into Conv history:\n%s", thoughtMarker, raw)
	}
}

// TestBackendHeaderSent verifies that a non-empty Client.Backend is forwarded
// as X-Ilm-Backend and that X-Ilm-Backend-Used is stored in LastUsedBackend.
func TestBackendHeaderSent(t *testing.T) {
	var capturedHeader string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeader = r.Header.Get("X-Ilm-Backend")
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Ilm-Backend-Used", "openrouter")
		fmt.Fprint(w, "data: "+contentChunk("ok")+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	client := &proxy.Client{
		BaseURL: srv.URL,
		Model:   "ilm",
		ChatID:  "test-backend",
		Backend: "openrouter",
		HTTP:    http.DefaultClient,
	}
	_, err := client.Stream(context.Background(), nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if capturedHeader != "openrouter" {
		t.Errorf("X-Ilm-Backend header = %q, want %q", capturedHeader, "openrouter")
	}
	if got := client.LastUsedBackend(); got != "openrouter" {
		t.Errorf("LastUsedBackend = %q, want %q", got, "openrouter")
	}
}

// TestBackendHeaderOmittedWhenEmpty verifies that no X-Ilm-Backend header is
// sent when Client.Backend is empty.
func TestBackendHeaderOmittedWhenEmpty(t *testing.T) {
	var capturedHeader string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeader = r.Header.Get("X-Ilm-Backend")
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: "+contentChunk("ok")+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	client := newTestClient(srv.URL) // Backend is "" by default
	_, err := client.Stream(context.Background(), nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if capturedHeader != "" {
		t.Errorf("X-Ilm-Backend header should not be sent when Backend is empty; got %q", capturedHeader)
	}
	if got := client.LastUsedBackend(); got != "" {
		t.Errorf("LastUsedBackend should be empty when proxy sends no header; got %q", got)
	}
}

// TestEgressGateFiresAndPreventsRequest verifies that Send gates when the
// backend is external, doesn't send when declined, and reverts SelectedBackend.
func TestEgressGateDeclineRevertsBackend(t *testing.T) {
	srv := sseServer(t, []string{contentChunk("this should not be reached")})
	defer srv.Close()

	prompts := 0
	app := newTestApp(srv.URL, newFakeExecutor(), func(toolName, _, _ string, _ bool) bool {
		if toolName == "external_backend" {
			prompts++
		}
		return false // always decline
	})
	app.Cfg.ExternalBackends = []string{"openrouter"}
	app.SelectedBackend = "openrouter"

	result, err := app.Send(context.Background(), "hello")
	if err != nil {
		t.Fatalf("decline should not return an error; got: %v", err)
	}
	if result != "" {
		t.Errorf("declined turn should return empty string; got %q", result)
	}
	if prompts != 1 {
		t.Errorf("expected 1 egress prompt; got %d", prompts)
	}
	if app.SelectedBackend != "" {
		t.Errorf("SelectedBackend should be cleared on decline; got %q", app.SelectedBackend)
	}
}

// TestEgressGateApproveConsentOnce verifies that approval is remembered for
// the session — only one prompt fires even after multiple turns.
func TestEgressGateApproveConsentOnce(t *testing.T) {
	srv := sseServer(t,
		[]string{contentChunk("first")},
		[]string{contentChunk("second")},
	)
	defer srv.Close()

	prompts := 0
	app := newTestApp(srv.URL, newFakeExecutor(), func(toolName, _, _ string, _ bool) bool {
		if toolName == "external_backend" {
			prompts++
		}
		return true // always approve
	})
	app.Cfg.ExternalBackends = []string{"openrouter"}
	app.SelectedBackend = "openrouter"

	// Turn 1: gate fires.
	if _, err := app.Send(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	// Turn 2: already consented, gate must not fire again.
	if _, err := app.Send(context.Background(), "world"); err != nil {
		t.Fatal(err)
	}
	if prompts != 1 {
		t.Errorf("expected exactly 1 consent prompt across 2 turns; got %d", prompts)
	}
}
