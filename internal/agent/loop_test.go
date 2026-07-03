package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/tools"
)

// The iteration cap is the hard backstop against a runaway tool loop: once
// MaxToolIterations is reached, tools are dropped and the model is forced to
// answer, so a model that never stops calling tools still terminates.
func TestMaxToolIterationsForcesFinish(t *testing.T) {
	// A server that always replies with the same tool call — i.e. a model that
	// never stops. Without the cap this loops forever.
	srv := sseServer(t, toolCallFrames("c1", "run_shell", `{"command":"echo hi"}`))
	defer srv.Close()

	exec := newFakeExecutor()
	app := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.MaxToolIterations = 3

	done := make(chan struct{})
	go func() {
		_, _ = app.Send(context.Background(), "go")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Send did not terminate — iteration cap not enforced")
	}

	// Tools run on iters 0,1,2; iter 3 is the forced tool-less finish.
	if len(exec.shellCalls) != 3 {
		t.Fatalf("shell executed %d times, want 3 (== MaxToolIterations)", len(exec.shellCalls))
	}
	var injected bool
	for _, m := range app.Conv {
		if m.Role == "user" && DerefStr(m.Content) == ToolLimitPrompt {
			injected = true
		}
	}
	if !injected {
		t.Error("tool-limit wrap-up prompt was not injected on the forced finish")
	}
}

// list_dir returns the directory entries to the model.
func TestListDirTool(t *testing.T) {
	srv := sseServer(t,
		toolCallFrames("l1", "list_dir", `{"path":"."}`),
		[]string{contentChunk("done")},
	)
	defer srv.Close()

	exec := newFakeExecutor()
	exec.files["a.go"] = "x"
	exec.files["b.go"] = "y"
	app := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })
	if _, err := app.Send(context.Background(), "list it"); err != nil {
		t.Fatal(err)
	}

	var got string
	for _, m := range app.Conv {
		if m.Role == "tool" && m.Name == "list_dir" {
			got = DerefStr(m.Content)
		}
	}
	if !strings.Contains(got, "a.go") || !strings.Contains(got, "b.go") {
		t.Fatalf("list_dir result = %q, want both files", got)
	}
}

// Reading a directory redirects the model to list_dir/search_files instead of
// returning a raw errno it would otherwise retry against (a known loop trigger).
func TestReadFileOnDirectoryRedirects(t *testing.T) {
	srv := sseServer(t,
		toolCallFrames("r1", "read_file", `{"path":"src"}`),
		[]string{contentChunk("ok")},
	)
	defer srv.Close()

	exec := newFakeExecutor()
	exec.dirs["src"] = true
	app := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })
	if _, err := app.Send(context.Background(), "read src"); err != nil {
		t.Fatal(err)
	}

	var got string
	for _, m := range app.Conv {
		if m.Role == "tool" && m.Name == "read_file" {
			got = DerefStr(m.Content)
		}
	}
	if !strings.Contains(got, "is a directory") || !strings.Contains(got, "list_dir") {
		t.Fatalf("read_file on a directory = %q, want a redirect message", got)
	}
}

// Dedup keys collapse trivial path variants and are insensitive to argument
// key ordering.
func TestToolDedupKeyNormalizes(t *testing.T) {
	app := &App{Exec: newFakeExecutor()} // fake Cwd() == "/work"

	base := app.toolDedupKey("read_file", `{"path":"."}`)
	for _, variant := range []string{`{"path":"./"}`, `{"path":"/work"}`, `{"path":"/work/"}`, `{"path":"./x/.."}`} {
		if k := app.toolDedupKey("read_file", variant); k != base {
			t.Errorf("variant %s -> %q, want %q", variant, k, base)
		}
	}
	if app.toolDedupKey("read_file", `{"path":"other.go"}`) == base {
		t.Error("distinct paths must not collapse")
	}
	a := app.toolDedupKey("search_files", `{"pattern":"x","path":"."}`)
	b := app.toolDedupKey("search_files", `{"path":".","pattern":"x"}`)
	if a != b {
		t.Errorf("argument key order changed the dedup key: %q vs %q", a, b)
	}
}

// A second, equivalent tool call returns the dedup notice instead of executing.
func TestToolDedupHitOnEquivalentPath(t *testing.T) {
	exec := newFakeExecutor()
	exec.files["a.go"] = "content"
	app := &App{Exec: exec, ToolCache: map[string]bool{}, Out: io.Discard}

	r1 := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"a.go"}`}})
	if strings.Contains(r1, "already called") {
		t.Fatalf("first call should execute, got %q", r1)
	}
	r2 := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"./a.go"}`}})
	if !strings.Contains(r2, "already called") {
		t.Fatalf("equivalent path should be deduped, got %q", r2)
	}
}

// No tool may serialize "required" as null: the backend's template parser
// rejects it ("type must be array, but is null"). Tools with no required
// params must omit the key entirely.
func TestToolSchemasNeverNullRequired(t *testing.T) {
	sets := map[string][]proxy.Tool{
		"defaultTools":   tools.DefaultTools("/work"),
		"discoveryTools": tools.DiscoveryTools("/work"),
	}
	for name, tools := range sets {
		for _, tl := range tools {
			b, err := json.Marshal(tl.Function.Parameters)
			if err != nil {
				t.Fatalf("%s/%s: marshal: %v", name, tl.Function.Name, err)
			}
			if strings.Contains(string(b), `"required":null`) {
				t.Errorf("%s/%s emits null required: %s", name, tl.Function.Name, b)
			}
			// list_dir has no required params → the key must be absent.
			if tl.Function.Name == "list_dir" && strings.Contains(string(b), `"required"`) {
				t.Errorf("%s/list_dir should omit required, got: %s", name, b)
			}
		}
	}
}

// When InjectDate is on, every request must carry a leading system message
// stating the current date — otherwise the model defaults to its training-era
// year. When off (subagents/tests), no such message is added.
func TestInjectDateSystemMessage(t *testing.T) {
	var captured struct {
		Messages []struct {
			Role    string  `json:"role"`
			Content *string `json:"content"`
		} `json:"messages"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + contentChunk("ok") + "\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	year := strconv.Itoa(time.Now().Year())

	// InjectDate on → leading system message with the current year.
	on := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	on.InjectDate = true
	if _, err := on.Send(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if len(captured.Messages) == 0 || captured.Messages[0].Role != "system" {
		t.Fatalf("first message should be a system date preamble, got %+v", captured.Messages)
	}
	if !strings.Contains(DerefStr(captured.Messages[0].Content), year) {
		t.Errorf("date preamble missing current year %s: %q", year, DerefStr(captured.Messages[0].Content))
	}
	// It must not be persisted into the transcript.
	for _, m := range on.Conv {
		if m.Role == "system" && strings.Contains(DerefStr(m.Content), "Current date") {
			t.Error("date preamble leaked into stored Conv")
		}
	}

	// InjectDate off → no synthetic system message.
	off := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	if _, err := off.Send(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if len(captured.Messages) > 0 && captured.Messages[0].Role == "system" {
		t.Errorf("no date preamble expected when InjectDate is off, got %q", DerefStr(captured.Messages[0].Content))
	}
}
