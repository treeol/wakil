package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/treeol/wakil/internal/memory"
	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/sessionhistory"
)

// newSessionHistoryApp builds an App with a real sessionhistory store rooted at
// a temp workspace, and an isolated temp sessions dir (so reconcile never reads
// the developer's real session dir).
func newSessionHistoryApp(t *testing.T, ws string) *App {
	t.Helper()
	app := testHandoffApp()
	app.Session = &Session{ChatID: "current", Workspace: ws}
	app.Client.ChatID = "current"
	// Make SessionWorkspace() resolve to ws.
	app.Cfg.ExecMode = "direct"
	app.Cfg.WorkDir = ws

	dbPath := filepath.Join(t.TempDir(), "sessionhistory", "test.db")
	st, err := sessionhistory.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	app.SessionHistory = st
	return app
}

// writeSessionFile writes a real session JSON file into the temp sessions dir
// (set by the calling test), so reconcileHistory can find it on disk.
func writeSessionFile(t *testing.T, s *Session) {
	t.Helper()
	if s.ChatID == "" {
		t.Fatal("session needs a chat_id")
	}
	if err := WriteSession(s); err != nil {
		t.Fatal(err)
	}
}

func TestStripRetrievalBlock(t *testing.T) {
	// Production format: header + content + END marker + "\n" + real user text.
	block := retrievalBlockHeader + "some injected content" + retrievalBlockEnd + "\nactual user message here"
	got := stripRetrievalBlock(block)
	if got != "actual user message here" {
		t.Errorf("expected stripped text, got %q", got)
	}

	// A block with an embedded blank line inside content still strips fully
	// to the structural end marker.
	multiline := retrievalBlockHeader + "part one\n\npart two\n" + retrievalBlockEnd + "\ntrailing user text"
	got = stripRetrievalBlock(multiline)
	if got != "trailing user text" {
		t.Errorf("embedded blank line must not truncate strip, got %q", got)
	}

	// A user message that merely contains the header mid-string is untouched.
	plain := "the header ## Relevant context from memory is mentioned"
	if got := stripRetrievalBlock(plain); got != plain {
		t.Errorf("mid-string header must not be stripped, got %q", got)
	}

	// Fail-closed: header without the end marker is NOT stripped.
	noEnd := retrievalBlockHeader + "incomplete block without end marker\n\nuser text"
	if got := stripRetrievalBlock(noEnd); got != noEnd {
		t.Errorf("block without end marker must not be stripped (fail-closed), got %q", got)
	}
}

func TestRememberSearchAcrossSessions(t *testing.T) {
	t.Setenv("WAKIL_SESSIONS_DIR", t.TempDir())
	ws := "/tmp/ws-a"
	app := newSessionHistoryApp(t, ws)

	// Two prior sessions written to disk (reconcile reads them from disk).
	s1 := Session{
		ChatID:    "sess-0001",
		Workspace: ws,
		Conv: []proxy.Message{
			{Role: "user", Content: strPtr("fix the io_uring sandbox syscall allowlist")},
			{Role: "assistant", Content: strPtr("edit the seccomp profile and rebuild")},
		},
	}
	s2 := Session{
		ChatID:    "sess-0002",
		Workspace: ws,
		Conv: []proxy.Message{
			{Role: "user", Content: strPtr("set up the CI pipeline for golangci-lint")},
		},
	}
	writeSessionFile(t, &s1)
	writeSessionFile(t, &s2)

	// Pre-index them so reconcile sees matching hashes (no re-parse rewrite).
	if err := app.indexSession(context.Background(), s1, false); err != nil {
		t.Fatal(err)
	}
	if err := app.indexSession(context.Background(), s2, false); err != nil {
		t.Fatal(err)
	}

	out, err := app.RememberSearch(context.Background(), "io_uring sandbox")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(out, "io_uring sandbox syscall allowlist") {
		t.Errorf("expected sess-0001 content to match io_uring, got:\n%s", out)
	}
	if contains(out, "CI pipeline") {
		t.Errorf("sess-0002 should not match io_uring, got:\n%s", out)
	}
}

func TestRememberSearchCurrentSessionExcluded(t *testing.T) {
	t.Setenv("WAKIL_SESSIONS_DIR", t.TempDir())
	ws := "/tmp/ws-b"
	app := newSessionHistoryApp(t, ws)

	cur := Session{ChatID: "current", Workspace: ws, Conv: []proxy.Message{
		{Role: "user", Content: strPtr("io_uring sandbox notes")},
	}}
	writeSessionFile(t, &cur)
	if err := app.indexSession(context.Background(), cur, false); err != nil {
		t.Fatal(err)
	}

	out, err := app.RememberSearch(context.Background(), "io_uring sandbox")
	if err != nil {
		t.Fatal(err)
	}
	if contains(out, "io_uring sandbox notes") {
		t.Errorf("current session must be excluded, got:\n%s", out)
	}
}

func TestSessionToIndexInputStripsRetrievalBlock(t *testing.T) {
	ws := "/tmp/ws-c"
	s := Session{
		ChatID:    "sess-9",
		Workspace: ws,
		Conv: []proxy.Message{
			{Role: "user", Content: strPtr(retrievalBlockHeader + "injected memory" + retrievalBlockEnd + "\nwhat I actually asked")},
		},
	}
	in := sessionToIndexInput(s)
	found := false
	for _, turn := range in.Turns {
		if turn.Role == "user" {
			if contains(turn.Text, "injected memory") {
				t.Fatal("retrieval block was indexed — feedback-loop guard failed")
			}
			if !contains(turn.Text, "what I actually asked") {
				t.Fatalf("real user text lost after strip: %q", turn.Text)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("expected a user turn in input")
	}
}

func TestSessionToIndexInputExcludesToolOutput(t *testing.T) {
	ws := "/tmp/ws-d"
	s := Session{
		ChatID:    "sess-10",
		Workspace: ws,
		Conv: []proxy.Message{
			{Role: "user", Content: strPtr("show me the config")},
			{Role: "assistant", Content: strPtr("here it is"), ToolCalls: []proxy.ToolCall{
				{ID: "t1", Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"x"}`}},
			}},
			{Role: "tool", Name: "read_file", ToolCallID: "t1", Content: strPtr("SECRET KEY MATERIAL NOT TO BE INDEXED")},
		},
	}
	in := sessionToIndexInput(s)
	for _, turn := range in.Turns {
		if contains(turn.Text, "SECRET KEY MATERIAL") {
			t.Fatal("tool output was indexed — security boundary violated")
		}
		// Assistant text (not tool-call args) should be indexed.
		if turn.Role == "assistant" && !contains(turn.Text, "here it is") {
			t.Fatalf("assistant text missing from index: %q", turn.Text)
		}
	}
}

func TestFinalizeSummarizesAboveThreshold(t *testing.T) {
	ws := "/tmp/ws-e"
	app := newSessionHistoryApp(t, ws)
	// Custom summarizer that returns a fixed summary (avoids network).
	app.Summarize = func(_ context.Context, _ string) (string, error) {
		return "END SUMMARY of the session", nil
	}

	conv := []proxy.Message{}
	for i := 0; i < 5; i++ {
		conv = append(conv,
			proxy.Message{Role: "user", Content: strPtr("question " + string(rune('a'+i)))},
			proxy.Message{Role: "assistant", Content: strPtr("answer " + string(rune('a'+i)))},
		)
	}
	old := Session{ChatID: "sess-old", Workspace: ws, Conv: conv}
	app.finalizeSessionHistory(context.Background(), old)

	// The generated summary must be retrievable via /remember on a distinct term
	// present only in the summary.
	res, err := app.SessionHistory.Search(context.Background(), "END SUMMARY", ws, "current", 8)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range res {
		if r.ChatID == "sess-old" && contains(r.Summary, "END SUMMARY") {
			found = true
		}
	}
	if !found {
		t.Fatal("generated end-of-session summary not stored/searchable")
	}
}

func TestFinalizeSkipsBelowThreshold(t *testing.T) {
	ws := "/tmp/ws-f"
	app := newSessionHistoryApp(t, ws)
	called := false
	app.Summarize = func(_ context.Context, _ string) (string, error) {
		called = true
		return "SHOULD NOT BE CALLED", nil
	}

	// Only 2 user turns — below minSummaryTurns (3).
	old := Session{ChatID: "sess-tiny", Workspace: ws, Conv: []proxy.Message{
		{Role: "user", Content: strPtr("q1")}, {Role: "assistant", Content: strPtr("a1")},
		{Role: "user", Content: strPtr("q2")}, {Role: "assistant", Content: strPtr("a2")},
	}}
	app.finalizeSessionHistory(context.Background(), old)
	if called {
		t.Fatal("summarizer should not be called for a session below the turn threshold")
	}
}

func TestTruncateUTF8Valid(t *testing.T) {
	// CJK/emoji must never be split, and output must never exceed n bytes.
	s := "配置 io_uring 🚀 sandbox"
	for n := 1; n <= len(s); n++ {
		got := truncateUTF8(s, n)
		if !utf8.ValidString(got) {
			t.Fatalf("truncateUTF8(%q, %d) produced invalid UTF-8: %q", s, n, got)
		}
		if len(got) > n {
			t.Fatalf("truncateUTF8(%q, %d) exceeded %d bytes: %q", s, n, n, got)
		}
	}
	// Short strings pass through unchanged.
	if got := truncateUTF8("hello", 100); got != "hello" {
		t.Fatalf("expected passthrough, got %q", got)
	}
}

func TestStripControlRemovesInjection(t *testing.T) {
	// ANSI/ESC sequences must be removed; newlines/tabs preserved.
	in := "normal \x1b[31mred\x1b[0m text\nline2\tok"
	got := stripControl(in)
	if contains(got, "\x1b") {
		t.Fatalf("ESC not stripped: %q", got)
	}
	if !contains(got, "\n") || !contains(got, "\t") {
		t.Fatalf("newline/tab should be preserved: %q", got)
	}
	if !contains(got, "red") {
		t.Fatalf("text after ESC sequence lost: %q", got)
	}
}

func TestFormatRetrievedMarkerSurvivesCap(t *testing.T) {
	// Even with a tiny cap, the structural end marker must be present so the
	// feedback-loop strip never fails open.
	for _, capv := range []int{40, 100, 500, 2000} {
		entries := []*memory.Entry{
			{Key: "k", Value: "some fairly long value that may push the cap", Tainted: memory.TaintTrue},
		}
		got := formatRetrievedContext(entries, capv)
		if !strings.Contains(got, retrievalBlockEnd) {
			t.Fatalf("cap=%d: end marker missing from retrieval block: %q", capv, got)
		}
		if !strings.HasPrefix(got, retrievalBlockHeader) {
			t.Fatalf("cap=%d: header missing", capv)
		}
	}
}

func TestStripControlRemovesC1(t *testing.T) {
	// C1 range (U+0080–U+009F) must be stripped.
	got := stripControl("a\u009bb\u009dc")
	if contains(got, "\u009b") || contains(got, "\u009d") {
		t.Fatalf("C1 controls not stripped: %q", got)
	}
}
