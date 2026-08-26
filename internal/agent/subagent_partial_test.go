package agent

// Card #146: a subagent that fails (context deadline, stream error) must not
// lose the work it already did. These tests verify the salvage path added to
// dispatchSubagent's error branch: the child's in-memory transcript (tool
// calls + results) is flushed to a spill file under the parent's chatID and
// pointed at from the error summary, so the parent can recover partial
// findings instead of receiving a bare error.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/treeol/wakil/internal/proxy"
	wtools "github.com/treeol/wakil/internal/tools"
)

// sseToolRound writes one complete tool-call stream (a read_file call), letting
// the subagent do one tool round before the next handler runs.
func sseToolRound(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, f := range toolCallFrames("r1", "read_file", `{"path":"config.go"}`) {
		fmt.Fprintf(w, "data: %s\n\n", f)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// abruptSSE writes a valid tool-call frame stream but then closes the response
// WITHOUT a [DONE] marker — the "stream disconnected mid-generation" failure
// (not a clean HTTP error status).
func abruptSSE(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n")
	// Deliberately no [DONE] — the connection just ends.
}

// --- pure renderer tests ---

func TestSubagentTranscriptUnit(t *testing.T) {
	conv := []proxy.Message{
		{Role: "system", Content: StrPtr(subagentSystemPrompt), Pinned: true},
		{Role: "user", Content: StrPtr("the task"), Pinned: true},
		{Role: "assistant", ToolCalls: []proxy.ToolCall{{Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"a.go"}`}}}},
		{Role: "tool", Name: "read_file", Content: StrPtr("package main")},
		{Role: "assistant", ToolCalls: []proxy.ToolCall{{Function: proxy.FunctionCall{Name: "search_files", Arguments: `{"pattern":"x"}`}}}},
		{Role: "tool", Name: "search_files", Content: StrPtr("a.go:3: match")},
	}

	got := subagentTranscript(conv)
	if got == "" {
		t.Fatal("expected non-empty transcript for a child that did tool work")
	}
	for _, want := range []string{
		"tool call: read_file", `{"path":"a.go"}`,
		"tool result: read_file", "package main",
		"tool call: search_files", "tool result: search_files", "a.go:3: match",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("transcript missing %q:\n%s", want, got)
		}
	}
	// Pinned system prompt + task must be skipped.
	if strings.Contains(got, "the task") || strings.Contains(got, "focused discovery subagent") {
		t.Errorf("transcript must skip the system prompt and task message:\n%s", got)
	}

	// No tool work → empty.
	if got := subagentTranscript([]proxy.Message{
		{Role: "system", Content: StrPtr("sys")},
		{Role: "user", Content: StrPtr("task")},
		{Role: "assistant", Content: StrPtr("just prose")},
	}); got != "" {
		t.Errorf("expected empty transcript when no tool work happened, got %q", got)
	}
}

// TestSubagentTranscriptCapsLongResults verifies oversized tool results are
// capped with a truncation marker, and that truncation is on a UTF-8 boundary.
func TestSubagentTranscriptCapsLongResults(t *testing.T) {
	// Fill with a multibyte rune so a naive byte-slice would split it.
	long := strings.Repeat("界", subagentTranscriptCap)
	got := subagentTranscript([]proxy.Message{
		{Role: "assistant", ToolCalls: []proxy.ToolCall{{Function: proxy.FunctionCall{Name: "read_file", Arguments: "{}"}}}},
		{Role: "tool", Name: "read_file", Content: StrPtr(long)},
	})
	if !strings.Contains(got, "truncated") {
		t.Errorf("expected truncation marker in capped transcript")
	}
	if !strings.Contains(got, "界") {
		t.Errorf("expected valid multibyte content preserved")
	}
	if !utf8Valid(got) {
		t.Errorf("transcript contains invalid UTF-8 — truncation split a rune")
	}
	if len(got) > subagentTranscriptCap+1000 {
		t.Errorf("transcript unexpectedly large: %d chars", len(got))
	}
}

// TestSubagentTranscriptTotalCap verifies the aggregate transcript is bounded:
// many large results are kept tail-first, dropping the earliest entries.
func TestSubagentTranscriptTotalCap(t *testing.T) {
	var conv []proxy.Message
	for i := 0; i < 20; i++ {
		conv = append(conv,
			proxy.Message{Role: "assistant", ToolCalls: []proxy.ToolCall{{Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"x.go"}`}}}},
			proxy.Message{Role: "tool", Name: "read_file", Content: StrPtr(strings.Repeat("y", 4000))},
		)
	}
	got := subagentTranscript(conv)
	if len(got) > subagentTranscriptTotalCap+2000 {
		t.Errorf("aggregate transcript not bounded: %d bytes (cap %d)", len(got), subagentTranscriptTotalCap)
	}
}

// TestSubagentTranscriptScrubsSecrets verifies sensitive argument values are
// redacted and content-bearing tool args are omitted.
func TestSubagentTranscriptScrubsSecrets(t *testing.T) {
	conv := []proxy.Message{
		{Role: "assistant", ToolCalls: []proxy.ToolCall{{Function: proxy.FunctionCall{
			Name:      "run_shell",
			Arguments: `{"command":"curl -H 'Authorization: Bearer sekret123' https://x"}`,
		}}}},
		{Role: "tool", Name: "run_shell", Content: StrPtr("ran: curl ...")},
		{Role: "assistant", ToolCalls: []proxy.ToolCall{{Function: proxy.FunctionCall{
			Name:      "read_file",
			Arguments: `{"path":".env"}`,
		}}}},
		{Role: "tool", Name: "read_file", Content: StrPtr("API_KEY=topsecret\nDB_PASS=hunter2")},
		{Role: "assistant", ToolCalls: []proxy.ToolCall{{Function: proxy.FunctionCall{
			Name:      "write_file",
			Arguments: `{"path":"big.go","content":"func main(){}"}`,
		}}}},
		{Role: "tool", Name: "write_file", Content: StrPtr("wrote 20 bytes")},
	}

	got := subagentTranscript(conv)

	// 1. run_shell args are content-bearing → omitted entirely.
	if strings.Contains(got, "sekret123") {
		t.Errorf("shell command leaked into transcript:\n%s", got)
	}
	if !strings.Contains(got, "arguments omitted") {
		t.Errorf("expected content-bearing arg omission marker:\n%s", got)
	}
	// 2. Tool RESULTS are kept verbatim (they carry the findings) — this is
	// intentional; the review's concern was structured args, not results. The
	// .env read result is NOT scrubbed because it is the child's observed
	// content, but we must not have *invented* a redaction guarantee we don't
	// make. Assert the result is present (documenting current behavior).
	if !strings.Contains(got, "API_KEY=topsecret") {
		t.Errorf("tool result content should be preserved verbatim (documented behavior):\n%s", got)
	}
	// 3. write_file args are content-bearing → omitted.
	if strings.Contains(got, "func main(){}") {
		t.Errorf("write_file body leaked into transcript:\n%s", got)
	}
}

// TestSubagentTranscriptOmitsMCP verifies MCP tool calls + results are omitted
// entirely (highest leakage risk; already audited via ExternalCalls).
func TestSubagentTranscriptOmitsMCP(t *testing.T) {
	conv := []proxy.Message{
		{Role: "assistant", ToolCalls: []proxy.ToolCall{{Function: proxy.FunctionCall{
			Name:      "trello__add_comment",
			Arguments: `{"CardID":"c1","Text":"secret-note"}`,
		}}}},
		{Role: "tool", Name: "trello__add_comment", Content: StrPtr(`{"ok":true}`)},
		{Role: "assistant", ToolCalls: []proxy.ToolCall{{Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"a.go"}`}}}},
		{Role: "tool", Name: "read_file", Content: StrPtr("package a")},
	}
	got := subagentTranscript(conv)
	if strings.Contains(got, "trello") || strings.Contains(got, "secret-note") {
		t.Errorf("MCP tool traffic must be omitted from salvage:\n%s", got)
	}
	if !strings.Contains(got, "read_file") {
		t.Errorf("non-MCP tool traffic should still be present:\n%s", got)
	}
}

// --- end-to-end error-path tests ---

// TestDispatchSubagentErrorFlushesPartialTranscript: a child reads a file, then
// the backend returns 500 → Send errors → partial transcript is salvaged.
func TestDispatchSubagentErrorFlushesPartialTranscript(t *testing.T) {
	tmpDir := t.TempDir()
	// Clear WAKIL_SESSIONS_DIR so XDG_DATA_HOME controls the cache dir (see
	// TestSubagentSummaryWrittenToDisk for the TestMain interaction).
	t.Setenv("WAKIL_SESSIONS_DIR", "")
	t.Setenv("XDG_DATA_HOME", tmpDir)

	fileContent := "package main\n\nfunc main() {}\n"
	srv := errorServer(t, sseToolRound, http500)
	defer srv.Close()

	exec := newFakeExecutor()
	exec.files["config.go"] = fileContent

	parent := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })

	summary, _, _, _, _, _ := parent.dispatchSubagent(context.Background(), "find main", io.Discard, "", "")

	if summary.Status != "incomplete" {
		t.Errorf("status = %q, want 'incomplete' on error", summary.Status)
	}
	if summary.StopReason != "error" {
		t.Errorf("stop_reason = %q, want 'error'", summary.StopReason)
	}
	if len(summary.Findings) == 0 {
		t.Fatal("expected an error finding")
	}
	f := summary.Findings[0]
	if f.Kind != "error" {
		t.Errorf("finding kind = %q, want 'error'", f.Kind)
	}
	if f.Location == "" {
		t.Fatal("expected a spill path in the error finding location")
	}
	if !strings.Contains(f.Summary, "partial work flushed to") {
		t.Errorf("finding summary should point at the partial work: %q", f.Summary)
	}

	if !wtools.IsToolCacheHostPath(f.Location) {
		t.Errorf("spill path %q is not under the toolcache root", f.Location)
	}
	data, err := os.ReadFile(f.Location)
	if err != nil {
		t.Fatalf("spill file unreadable at %q: %v", f.Location, err)
	}
	if !strings.Contains(string(data), "func main() {}") {
		t.Errorf("spill file missing the child's tool result:\n%s", string(data))
	}
	if !strings.Contains(string(data), "tool result: read_file") {
		t.Errorf("spill file missing tool result header:\n%s", string(data))
	}
	// Canonical containment under the parent's chatID dir ("test").
	expectedDir := filepath.Join(tmpDir, "wakil", "toolcache", "test")
	assertWithinDir(t, f.Location, expectedDir)
}

// TestDispatchSubagentErrorMidStreamDisconnect: the child completes one tool
// round, then the next stream disconnects without [DONE] — the "stream
// failure" case (not a clean HTTP status). Salvage must still work.
func TestDispatchSubagentErrorMidStreamDisconnect(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)

	fileContent := "package main\n\nvar X = 1\n"
	srv := errorServer(t, sseToolRound, abruptSSE)
	defer srv.Close()

	exec := newFakeExecutor()
	exec.files["config.go"] = fileContent

	parent := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })

	summary, _, _, _, _, _ := parent.dispatchSubagent(context.Background(), "find X", io.Discard, "", "")

	if summary.Status != "incomplete" || summary.StopReason != "error" {
		t.Errorf("status/stop_reason = %q/%q, want incomplete/error", summary.Status, summary.StopReason)
	}
	if len(summary.Findings) == 0 || summary.Findings[0].Location == "" {
		t.Fatal("expected a salvaged spill path after mid-stream disconnect")
	}
	data, err := os.ReadFile(summary.Findings[0].Location)
	if err != nil {
		t.Fatalf("spill unreadable: %v", err)
	}
	if !strings.Contains(string(data), "var X = 1") {
		t.Errorf("spill missing the child's tool result after disconnect:\n%s", string(data))
	}
}

// TestDispatchSubagentErrorNoToolWorkNoSpill: bare error preserved when the
// child failed before any tool work — no empty spill, no location pointer.
func TestDispatchSubagentErrorNoToolWorkNoSpill(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)

	srv := errorServer(t, http500)
	defer srv.Close()

	parent := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })

	summary, _, _, _, _, _ := parent.dispatchSubagent(context.Background(), "find main", io.Discard, "", "")

	if summary.Status != "incomplete" || summary.StopReason != "error" {
		t.Errorf("status/stop_reason = %q/%q, want incomplete/error", summary.Status, summary.StopReason)
	}
	if len(summary.Findings) == 0 {
		t.Fatal("expected an error finding")
	}
	if summary.Findings[0].Location != "" {
		t.Errorf("expected no spill path when the child did no tool work, got %q", summary.Findings[0].Location)
	}
	if strings.Contains(summary.Findings[0].Summary, "partial work flushed") {
		t.Errorf("must not claim partial work was flushed: %q", summary.Findings[0].Summary)
	}
	dir := filepath.Join(tmpDir, "wakil", "toolcache", "test")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			t.Fatalf("ReadDir(%s) unexpected error: %v", dir, err)
		}
		return // dir never created — cleanest possible state
	}
	if len(entries) != 0 {
		t.Errorf("expected no spill files, found %d under %s", len(entries), dir)
	}
}

// TestDispatchSubagentErrorSpillWriteFailure: when the toolcache dir cannot be
// created, salvage must degrade to the bare error WITHOUT a false "flushed to"
// claim. XDG_DATA_HOME points at a path whose parent is a read-only file, so
// MkdirAll fails.
func TestDispatchSubagentErrorSpillWriteFailure(t *testing.T) {
	// Clear WAKIL_SESSIONS_DIR so XDG_DATA_HOME controls the cache dir (see
	// TestSubagentSummaryWrittenToDisk for the TestMain interaction).
	t.Setenv("WAKIL_SESSIONS_DIR", "")

	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	// A file named "blocker" blocks the directory "blocker/..." from being created.
	t.Setenv("XDG_DATA_HOME", filepath.Join(blocker, "wakil"))

	fileContent := "package main\n"
	srv := errorServer(t, sseToolRound, http500)
	defer srv.Close()

	exec := newFakeExecutor()
	exec.files["config.go"] = fileContent

	parent := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })

	summary, _, _, _, _, _ := parent.dispatchSubagent(context.Background(), "find main", io.Discard, "", "")

	if len(summary.Findings) == 0 {
		t.Fatal("expected an error finding")
	}
	if summary.Findings[0].Location != "" {
		t.Errorf("spill must fail when cache dir is unwritable; got location %q", summary.Findings[0].Location)
	}
	if strings.Contains(summary.Findings[0].Summary, "flushed to") {
		t.Errorf("must not claim 'flushed to' when the spill write failed: %q", summary.Findings[0].Summary)
	}
	// The original child error must still be reported.
	if !strings.Contains(summary.Findings[0].Summary, "subagent error:") {
		t.Errorf("original error must be preserved: %q", summary.Findings[0].Summary)
	}
}

// TestSubagentTranscriptEmptyOnAllPinned: pinned-only conv renders empty.
func TestSubagentTranscriptEmptyOnAllPinned(t *testing.T) {
	conv := []proxy.Message{
		{Role: "system", Content: StrPtr(subagentSystemPrompt), Pinned: true},
		{Role: "user", Content: StrPtr("task"), Pinned: true},
	}
	if got := subagentTranscript(conv); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

// --- helpers ---

// assertWithinDir asserts path is canonically inside dir (no ../ or absolute
// escapes), using filepath.Rel rather than a substring match.
func assertWithinDir(t *testing.T, path, dir string) {
	t.Helper()
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		t.Fatalf("filepath.Rel(%q, %q): %v", dir, path, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Errorf("path %q escapes dir %q (rel=%q)", path, dir, rel)
	}
}

// utf8Valid reports whether s is valid UTF-8 (no mid-rune splits).
func utf8Valid(s string) bool {
	return utf8.ValidString(s)
}
