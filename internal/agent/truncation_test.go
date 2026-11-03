package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/counsel"

	"github.com/treeol/wakil/internal/proxy"
)

// recordingChatServer is a fake chat-completions backend that records the
// message contents of every request EXACTLY as received on the wire, and
// replies with the given assistant text.
type recordingChatServer struct {
	mu   sync.Mutex
	reqs [][]string // per request: content of each message
	srv  *httptest.Server
}

func newRecordingChatServer(t *testing.T, reply string) *recordingChatServer {
	t.Helper()
	r := &recordingChatServer{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.HasSuffix(req.URL.Path, "/v1/chat/completions") {
			http.NotFound(w, req)
			return
		}
		raw, _ := io.ReadAll(req.Body)
		var body struct {
			Messages []struct {
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(raw, &body)
		var contents []string
		for _, m := range body.Messages {
			var s string
			if err := json.Unmarshal(m.Content, &s); err != nil {
				// content-block array (cache breakpoints): concatenate text parts
				var blocks []struct {
					Text string `json:"text"`
				}
				_ = json.Unmarshal(m.Content, &blocks)
				for _, b := range blocks {
					s += b.Text
				}
			}
			contents = append(contents, s)
		}
		r.mu.Lock()
		r.reqs = append(r.reqs, contents)
		r.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fl.Flush()
		fmt.Fprintf(w, "data: %s\n\n", contentChunk(reply))
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *recordingChatServer) requests() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]string(nil), r.reqs...)
}

func anyContains(msgs []string, s string) bool {
	for _, m := range msgs {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

// A 20k-token current user message (≈80 KB) is sent WHOLE, while the prior
// turns that no longer fit are reduced by summary (never by cutting the
// current message), and the reduction is visible.
func TestCurrentMessageSentWholePriorTurnsCompacted(t *testing.T) {
	srv := newRecordingChatServer(t, "ack")
	app := newTestApp(srv.srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	var out strings.Builder
	app.Out = &out
	if err := app.SetContextMode("standard"); err != nil {
		t.Fatal(err)
	}
	app.Cfg.HardMaxBytes = 120_000
	app.Cfg.CompactAt = 100_000
	app.Cfg.KeepBytes = 60_000
	app.Cfg.SummaryBytes = 2_000
	app.Summarize = func(_ context.Context, _ string) (string, error) { return "SUMMARY-OF-EARLIER-TURNS", nil }

	// Prior history: 4 turns of ~25 KB each (100 KB) — together with the big
	// brief they exceed the 120 KB ceiling.
	for i := 1; i <= 4; i++ {
		app.turnSeq++
		app.curTurn = app.turnSeq
		app.appendConv(
			proxy.Message{Role: "user", Content: StrPtr(fmt.Sprintf("PRIOR-USER-%d %s", i, strings.Repeat("u", 12_000)))},
			proxy.Message{Role: "assistant", Content: StrPtr(fmt.Sprintf("PRIOR-ANSWER-%d %s", i, strings.Repeat("a", 12_000)))},
		)
	}

	// The brief: ~20k tokens (80 KB) with a unique head, middle and tail.
	var brief strings.Builder
	brief.WriteString("BRIEF-HEAD ")
	for i := 0; brief.Len() < 80_000; i++ {
		fmt.Fprintf(&brief, "para-%05d lorem ipsum dolor sit amet consectetur. ", i)
		if i == 700 {
			brief.WriteString("BRIEF-MIDDLE ")
		}
	}
	brief.WriteString(" BRIEF-TAIL")
	text := brief.String()

	if _, err := app.Send(context.Background(), text); err != nil {
		t.Fatalf("send: %v", err)
	}

	reqs := srv.requests()
	if len(reqs) == 0 {
		t.Fatal("nothing reached the backend")
	}
	last := reqs[len(reqs)-1]
	if !anyContains(last, text) {
		t.Fatal("the current message did not arrive byte-exact (it was cut or altered)")
	}
	for _, marker := range []string{"BRIEF-HEAD", "BRIEF-MIDDLE", "BRIEF-TAIL"} {
		if !anyContains(last, marker) {
			t.Errorf("%s missing from the request", marker)
		}
	}
	// Prior turns were reduced, and by summary.
	total := 0
	for _, m := range last {
		total += len(m)
	}
	if total > app.Cfg.HardMaxBytes+2000 {
		t.Errorf("request %d bytes exceeds the %d-byte ceiling", total, app.Cfg.HardMaxBytes)
	}
	if !anyContains(last, "SUMMARY-OF-EARLIER-TURNS") {
		t.Error("prior turns must be compacted by SUMMARY, not silently cut")
	}
	o := out.String()
	if !strings.Contains(o, "compacted earlier turns") && !strings.Contains(o, "hard-max shed") {
		t.Errorf("no visible note about the reduction of prior turns:\n%s", o)
	}
}

// When the current turn ALONE exceeds the limit: a clear error naming size and
// limit, and NOTHING is sent (no prefix).
func TestCurrentTurnTooLargeFailsLoudly(t *testing.T) {
	srv := newRecordingChatServer(t, "ack")
	app := newTestApp(srv.srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Out = io.Discard
	if err := app.SetContextMode("standard"); err != nil {
		t.Fatal(err)
	}
	app.Cfg.HardMaxBytes = 50_000
	app.Cfg.CompactAt = 40_000
	app.Cfg.KeepBytes = 20_000

	huge := strings.Repeat("x", 90_000)
	_, err := app.Send(context.Background(), huge)
	if err == nil || !errors.Is(err, ErrCurrentTurnTooLarge) {
		t.Fatalf("want ErrCurrentTurnTooLarge, got %v", err)
	}
	for _, want := range []string{"90", "50000", "nothing was sent"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should name %q", err, want)
		}
	}
	if n := len(srv.requests()); n != 0 {
		t.Fatalf("%d request(s) reached the backend; an over-limit current turn must send nothing", n)
	}
}

// protectedStart is turn-field based: an injected user-role message (async
// envelope, wrap-up prompt) inside the current turn must not become the
// "current turn start" and expose the real brief to compaction.
func TestProtectedStartIgnoresInjectedUserMessages(t *testing.T) {
	conv := []proxy.Message{
		{Role: "system", Content: StrPtr("preamble")}, // Turn 0
		{Role: "user", Turn: 1, Content: StrPtr("old")},
		{Role: "assistant", Turn: 1, Content: StrPtr("old answer")},
		{Role: "user", Turn: 2, Content: StrPtr("THE BRIEF")},
		{Role: "assistant", Turn: 2, ToolCalls: []proxy.ToolCall{{ID: "c1", Function: proxy.FunctionCall{Name: "x", Arguments: "{}"}}}},
		{Role: "tool", Turn: 2, ToolCallID: "c1", Content: StrPtr("result")},
		{Role: "user", Turn: 2, Content: StrPtr("## Async task results (injected)")},
	}
	if got := protectedStart(conv, 2); got != 3 {
		t.Fatalf("protectedStart = %d, want 3 (the real brief), not the injected user message", got)
	}
	if got := protectedStart(conv, 0); got != 6 {
		t.Fatalf("fallback without a turn counter = %d, want the last user message (6)", got)
	}
}

// Wakil's own outbound report: 30k tokens (≈120 KB) of assistant output must
// reach the user whole — the user-facing output stream is never collapsed.
func TestOutboundReportReachesUserWhole(t *testing.T) {
	var sb strings.Builder
	for i := 0; sb.Len() < 120_000; i++ {
		fmt.Fprintf(&sb, "report line %06d with some padding text to make it long\n", i)
	}
	sb.WriteString("REPORT-END-MARKER")
	report := sb.String()

	srv := newRecordingChatServer(t, report)
	app := newTestApp(srv.srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	var out strings.Builder
	app.Out = &out
	final, err := app.Send(context.Background(), "write the report")
	if err != nil {
		t.Fatal(err)
	}
	if final != report {
		t.Fatalf("returned report differs (len %d vs %d)", len(final), len(report))
	}
	if !strings.Contains(out.String(), "REPORT-END-MARKER") {
		t.Error("the streamed output lost the end of the report")
	}
	if strings.Contains(out.String(), "collapsed request") {
		t.Errorf("a collapse notice appeared for a fresh report:\n%.300s", out.String())
	}
	// And it stays whole in the next request's history (prior assistant
	// reports are not capped by default).
	if _, err := app.Send(context.Background(), "thanks"); err != nil {
		t.Fatal(err)
	}
	reqs := srv.requests()
	if !anyContains(reqs[len(reqs)-1], "REPORT-END-MARKER") {
		t.Error("the prior report was cut in the next request")
	}
}

// A VOID reviewer reply — including one whose single retry after length
// truncation fails with an HTTP error — must never count as a round: auto-
// counsel refunds counselCalls, and runWFPanel returns available=false so a
// workflow consult does not consume its one consultation.
func TestVoidReplyNeverCountsAsARound(t *testing.T) {
	t.Setenv("MASHURA_TEST_KEY", "x")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every call after the first is an HTTP error: tests the
		// max_tokens → retry fails → still VOID path.
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), "[This is part") {
			http.Error(w, `{"error":{"message":"overloaded"}}`, 529)
			return
		}
		// Return SSE with max_tokens stop_reason (truncation).
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
		fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"cut o\"}}\n\n")
		fmt.Fprintf(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":{\"output_tokens\":1}}\n\n")
		fmt.Fprintf(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	app, _ := mashuraTestApp(t, strings.Repeat("## Section\nbody text here\n\n", 800), "1. step", "done")
	app.Cfg.OracleAPIKeyEnv = "MASHURA_TEST_KEY"
	app.Cfg.OracleEndpoint = srv.URL
	app.Cfg.MashuraPanels = map[string]config.MashuraPanelConfig{
		"default": {Models: []string{"anthropic:test-model"}, Mode: "panel"},
	}
	app.Confirm = func(_, _, _ string, _ bool) bool { return true }
	app.MaxCounsel = 2
	app.CounselMode = "auto"

	// Auto-counsel path: a VOID reply refunds the call so counselCalls stays 0.
	app.counselCalls = 1
	app.autoCounselSkipGate = true
	res := app.runMashuraCore(context.Background(), "mashura__debug",
		proxy.ToolCall{ID: "c1", Function: proxy.FunctionCall{Name: "mashura__debug", Arguments: `{"symptom":"boom"}`}}, true)
	if !app.lastPanelVoid || !strings.Contains(res, "NOT COMPLETED") {
		t.Fatalf("expected VOID panel result, got lastPanelVoid=%v:\n%s", app.lastPanelVoid, res)
	}
	if app.lastPanelVoid {
		app.counselCalls--
	}
	if app.counselCalls != 0 {
		t.Errorf("counselCalls = %d, want 0 (VOID must not count as a spent round)", app.counselCalls)
	}

	// Workflow consult path: runWFPanel returns available=false when any
	// member is VOID, so the round is not marked as held.
	panel := app.Cfg.MashuraPanels["default"]
	out, raw, ok := runWFPanel(context.Background(), app, "review", "q", strings.Repeat("## Section\nbody\n\n", 800), "default", panel)
	if ok || !counsel.AnyVoid(raw) || !strings.Contains(out, "NOT COMPLETED") {
		t.Fatalf("runWFPanel must report unavailable on VOID: ok=%v out=%s", ok, out)
	}
}

// collapse_context is an explicit model action that may collapse only the
// model's own earlier work in the turn — NEVER the user's message or its @-
// attached files.
func TestCollapseContextNeverCollapsesUserMessageOrAttachments(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	if err := app.SetContextMode("long-session"); err != nil {
		t.Fatal(err)
	}
	userWithAttachment := "Please review this.\n\n--- @spec.md (40 KB) ---\n```md\n" +
		strings.Repeat("USER-BRIEF-ATTACHMENT-LINE\n", 1500) + "ATTACHMENT-END\n```"
	app.turnSeq = 1
	app.curTurn = 1
	app.appendConv(
		proxy.Message{Role: "user", Content: StrPtr(userWithAttachment)},
		proxy.Message{Role: "assistant", ToolCalls: []proxy.ToolCall{{ID: "r1", Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"a.go"}`}}}},
		proxy.Message{Role: "tool", ToolCallID: "r1", Name: "read_file", Content: StrPtr(strings.Repeat("TOOL-WORK-LINE\n", 400))},
		proxy.Message{Role: "assistant", ToolCalls: []proxy.ToolCall{{ID: "cc", Function: proxy.FunctionCall{Name: "collapse_context", Arguments: `{"summary":"read a.go"}`}}}},
	)
	got := app.handleCollapseContext(proxy.ToolCall{ID: "cc", Function: proxy.FunctionCall{Name: "collapse_context", Arguments: `{"summary":"read a.go"}`}})
	if !strings.Contains(got, "collapsed request 1 working set") {
		t.Fatalf("expected collapse to succeed on the working set, got %q", got)
	}
	rec := app.collapsed[1]
	archBytes, err := os.ReadFile(rec.Archive)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(archBytes), "USER-BRIEF-ATTACHMENT-LINE") {
		t.Fatal("the user's message/attachment ended up in the collapsed working-set archive")
	}
	proj := app.contextProjection()
	if len(proj) == 0 || !strings.HasPrefix(DerefStr(proj[0].Content), userWithAttachment+"\n\n[assistant checkpoint") {
		t.Fatal("the user's message/attachment did not survive collapse_context byte-exact in the projection")
	}

	// And if a turn ever lacks a leading user message, collapse_context refuses
	// rather than risk treating the first message as collapsible work.
	app2 := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	_ = app2.SetContextMode("long-session")
	app2.turnSeq = 1
	app2.curTurn = 1
	app2.appendConv(
		proxy.Message{Role: "assistant", ToolCalls: []proxy.ToolCall{{ID: "r1", Function: proxy.FunctionCall{Name: "read_file", Arguments: "{}"}}}},
		proxy.Message{Role: "tool", ToolCallID: "r1", Content: StrPtr(strings.Repeat("x", 2000))},
		proxy.Message{Role: "assistant", ToolCalls: []proxy.ToolCall{{ID: "cc", Function: proxy.FunctionCall{Name: "collapse_context", Arguments: `{"summary":"s"}`}}}},
	)
	refused := app2.handleCollapseContext(proxy.ToolCall{ID: "cc", Function: proxy.FunctionCall{Name: "collapse_context", Arguments: `{"summary":"s"}`}})
	if !strings.Contains(refused, "refused") || len(app2.collapsed) != 0 {
		t.Fatalf("expected refusal when turn does not start with user message, got %q", refused)
	}
}

// Destructive removal of prior turns (Compact and enforceHardMax) is gated on a
// durable archive: when the toolcache cannot be written (here: XDG_DATA_HOME is
// a regular file, not a directory), the original messages remain in a.Conv.
func TestArchiveFailurePreventsDestructiveRemovalOfPriorTurns(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_DATA_HOME", blocker)
	t.Setenv("WAKIL_SESSIONS_DIR", "")

	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	_ = app.SetContextMode("standard")
	app.Cfg.CompactAt = 500
	app.Cfg.KeepBytes = 200
	app.Cfg.HardMaxBytes = 600
	app.Summarize = func(_ context.Context, _ string) (string, error) { return "summary", nil }
	app.Conv = []proxy.Message{
		{Role: "user", Turn: 1, Content: StrPtr("PRIOR-1 " + strings.Repeat("a", 500))},
		{Role: "assistant", Turn: 1, Content: StrPtr("ANS-1 " + strings.Repeat("b", 500))},
		{Role: "user", Turn: 2, Content: StrPtr("CURRENT " + strings.Repeat("c", 100))},
	}
	app.curTurn = 2

	if ok, err := app.Compact(context.Background(), app.summarizeFn(), true); err == nil || ok {
		t.Fatalf("Compact must abort when the archive cannot be written: ok=%v err=%v", ok, err)
	}
	app.enforceHardMax(context.Background(), app.Cfg.HardMaxBytes)
	if len(app.Conv) != 3 || !strings.Contains(DerefStr(app.Conv[0].Content), "PRIOR-1") {
		t.Fatalf("original prior turn was lost without an archive: %+v", app.Conv)
	}
}
