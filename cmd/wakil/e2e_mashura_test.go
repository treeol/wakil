package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/core/event"
)

// e2eCounselFake is a fake Mashura transport that answers every panel
// call with a cited reply and records each request body.
type e2eCounselFake struct {
	mu     sync.Mutex
	hits   int
	bodies [][]byte
	srv    *httptest.Server
}

// newE2ECounselFake installs the fake through the oracle endpoint hook.
func newE2ECounselFake(t *testing.T) *e2eCounselFake {
	t.Helper()
	f := &e2eCounselFake{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.hits++
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		text := "diagnosis: fix the loop exit condition\nLAST SECTION READ: Recent tool calls"
		f.writeCitedReply(w, r, text, `{"input_tokens":100,"output_tokens":50}`)
	}))
	agent.SetOracleEndpointHook(f.srv.URL)
	t.Cleanup(func() {
		agent.SetOracleEndpointHook("")
		f.srv.Close()
	})
	return f
}

// count returns how many panel calls the fake served.
func (f *e2eCounselFake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits
}

// requestBodies returns a copy of every recorded request body.
func (f *e2eCounselFake) requestBodies() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.bodies...)
}

// writeCitedReply answers like a diligent reviewer by ending its text
// with the mandatory citation.
func (f *e2eCounselFake) writeCitedReply(w http.ResponseWriter, r *http.Request, text, usageJSON string) {
	raw, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.bodies = append(f.bodies, append([]byte(nil), raw...))
	f.mu.Unlock()
	var body struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(raw, &body)
	user := ""
	if len(body.Messages) > 0 {
		user = body.Messages[len(body.Messages)-1].Content
	}
	if i := strings.Index(user, "── Round 1 responses from other panel members ──"); i >= 0 {
		user = user[:i]
	}
	last, inFence := "", false
	for _, l := range strings.Split(user, "\n") {
		if strings.HasPrefix(l, "```") {
			inFence = !inFence
		}
		if inFence {
			continue
		}
		if strings.HasPrefix(l, "#") {
			last = strings.TrimSpace(strings.TrimLeft(l, "# "))
		} else if strings.HasPrefix(l, "── ") && strings.HasSuffix(l, " ──") {
			last = strings.TrimSuffix(strings.TrimPrefix(l, "── "), " ──")
		}
	}
	if last != "" {
		text += "\nLAST SECTION READ: " + last
	}
	var usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	}
	_ = json.Unmarshal([]byte(usageJSON), &usage)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "data: %s\n\n", e2eMustJSONChunk(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": text}, "finish_reason": nil}}}))
	fmt.Fprintf(w, "data: %s\n\n", e2eMustJSONChunk(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}}}))
	fmt.Fprintf(w, "data: %s\n\n", e2eMustJSONChunk(map[string]any{"choices": []any{}, "usage": map[string]any{"prompt_tokens": usage.InputTokens, "completion_tokens": usage.OutputTokens}}))
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// e2eMustJSONChunk marshals one SSE chunk payload.
func e2eMustJSONChunk(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// e2eAnswerApprovals polls the durable log and answers pending approvals
// until the target number of turns has completed.
func e2eAnswerApprovals(t *testing.T, e *e2e, targetTurns int) {
	t.Helper()
	answered := map[event.ApprovalID]time.Time{}
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		evs, err := e.events()
		if err != nil {
			t.Fatalf("events() read error (a finding, not swallowed): %v", err)
		}
		requested, resolved, turns := 0, 0, 0
		var latest *event.ApprovalRequested
		for _, ev := range evs {
			switch ev.Kind {
			case event.KindSessionError:
				t.Fatalf("unexpected session_error event: %+v", ev.Payload)
			case event.KindApprovalRequested:
				p := ev.Payload.(event.ApprovalRequested)
				requested++
				latest = &p
			case event.KindApprovalResolved:
				resolved++
			case event.KindTurnCompleted:
				turns++
			}
		}
		if turns >= targetTurns {
			return
		}
		// Persistence precedes TUI delivery. Retry an unanswered approval
		// after a bounded delay if the first key arrived before the gate.
		if requested > resolved && latest != nil && time.Since(answered[latest.ApprovalID]) > time.Second {
			answered[latest.ApprovalID] = time.Now()
			if latest.ToolName == "write_file" {
				e.key("g")
			} else {
				e.key("y")
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d turn(s); screen tail:\n%s", targetTurns, e.tail(2000))
}

// TestE2EAutoMashuraOneRoundPerSymptom reproduces the user's observation. Before the fix this reported two rounds: rule repeat after the second write and rule rewrites after the third.
func TestE2EAutoMashuraOneRoundPerSymptom(t *testing.T) {
	counsel := newE2ECounselFake(t)
	t.Setenv("MASHURA_TEST_OR_KEY", "x")
	t.Setenv("MASHURA_TEST_KEY", "x")
	// Dummy values only, never real keys.
	// The harness neutralising loop blanks a fixed list of ambient env
	// names that does not include these two, so they survive startE2E.
	// The ordered script (not Turns) is used because e2eLastUserText
	// reads only the last user message, and an automatic round injects
	// a user message beginning with "[automatic mashura round" that
	// would hide "fix the answer" from Turns matching.
	script := [][]string{
		e2eToolCallFrames("w1", "write_file", `{"path":"answer.txt","content":"same"}`),
		e2eToolCallFrames("w2", "write_file", `{"path":"answer.txt","content":"same"}`),
		e2eToolCallFrames("w3", "write_file", `{"path":"answer.txt","content":"same"}`),
		e2eToolCallFrames("w4", "write_file", `{"path":"answer.txt","content":"same"}`),
		e2eToolCallFrames("s1", "run_shell", `{"command":"./check.sh"}`),
		{e2eContentChunk("finished")},
		{e2eContentChunk("ok")},
	}
	e := startE2E(t, e2eOpts{
		script: script,
		width:  200,
		extra: map[string]any{
			"mashura": map[string]any{
				"level":   "high",
				"members": []string{"openrouter:test-model-a", "openrouter:test-model-b"},
			},
			"openrouter_api_key_env":     "MASHURA_TEST_OR_KEY",
			"oracle_api_key_env":         "MASHURA_TEST_KEY",
			"oracle_model":               "test-model",
			"context_collapse_min_bytes": 1,
		},
	})
	if got := os.Getenv("MASHURA_TEST_OR_KEY"); got != "x" {
		t.Errorf("MASHURA_TEST_OR_KEY = %q after startE2E, want %q (harness blanked it)", got, "x")
	}
	if got := os.Getenv("MASHURA_TEST_KEY"); got != "x" {
		t.Errorf("MASHURA_TEST_KEY = %q after startE2E, want %q (harness blanked it)", got, "x")
	}
	e2eReady(t, e)

	e.submit("/mashura high")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(e.screen(), "high") {
		time.Sleep(50 * time.Millisecond)
	}

	e.submit("fix the answer")
	e2eAnswerApprovals(t, e, 1)

	evs, err := e.events()
	if err != nil {
		t.Fatalf("events() read error: %v", err)
	}
	rounds, dones := 0, 0
	for _, ev := range evs {
		switch ev.Kind {
		case event.KindAsyncJobStarted:
			rounds++
			t.Logf("async_job_started label: %q", ev.Payload.(event.AsyncJobStarted).Label)
		case event.KindAsyncJobCompleted:
			dones++
			p := ev.Payload.(event.AsyncJobCompleted)
			t.Logf("async_job_completed status %q summary %q", p.Status, p.SummaryPreview)
		}
	}
	t.Logf("counsel fake hits: %d", counsel.count())
	for _, body := range counsel.requestBodies() {
		t.Logf("counsel request body: %s", body)
	}
	seen := map[string]bool{}
	struggleLines := 0
	for _, line := range strings.FieldsFunc(e.screen(), func(r rune) bool { return r == '\n' || r == '\r' }) {
		if strings.Contains(line, "struggle detected") && !seen[line] {
			seen[line] = true
			t.Logf("struggle line: %s", line)
		}
		if strings.Contains(line, "struggle detected: starting mashura debug") && !seen[line+"\x00"] {
			seen[line+"\x00"] = true
			struggleLines++
		}
	}
	if struggleLines != 1 {
		t.Errorf("de-duplicated 'struggle detected: starting mashura debug' screen lines = %d, want 1", struggleLines)
	}
	reqs := e.requests()
	var lastTurn1Body []byte
	if len(reqs) > 0 {
		lastTurn1Body = reqs[len(reqs)-1]
	}
	t.Logf("turn 1 model requests: %d", len(reqs))
	autoCountTurn1 := strings.Count(string(lastTurn1Body), "[automatic mashura round")

	e.submit("second question")
	e2eAnswerApprovals(t, e, 2)
	collapseLine := e.waitScreenLine(30*time.Second, "collapsed request 1 line", func(line string) bool {
		return strings.Contains(line, "collapsed request 1")
	})
	t.Logf("collapse line: %s", collapseLine)
	reqs2 := e.requests()
	var lastTurn2Body []byte
	if len(reqs2) > 0 {
		lastTurn2Body = reqs2[len(reqs2)-1]
	}

	if rounds != 1 {
		t.Errorf("async_job_started rounds = %d, want 1 (four identical writes are one symptom)", rounds)
	}
	if dones != 1 {
		t.Errorf("async_job_completed dones = %d, want 1", dones)
	}
	if got := counsel.count(); got != 2 {
		t.Errorf("counsel fake hits = %d, want 2 (two members, one round)", got)
	}
	if autoCountTurn1 != 1 {
		t.Errorf("[automatic mashura round count in last turn 1 request = %d, want 1", autoCountTurn1)
	}
	// The injected header must state the counts plainly in the model-facing
	// text (FACTS a from the 0.8.3 live run: the model wrote "the two
	// automatic counsel rounds" after one round with two members). One
	// round, two members, cap the default 2.
	if !strings.Contains(string(lastTurn1Body), "[automatic mashura round one — Wakil started this round automatically; you did not call it · two panel members, cap 2 per turn]") {
		t.Errorf("injected header does not state plain counts (one round, two panel members): %.200s", string(lastTurn1Body))
	}
	var chat struct {
		Messages []struct {
			Role      string `json:"role"`
			ToolCalls []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(lastTurn1Body, &chat); err != nil {
		t.Errorf("decode last turn 1 request body: %v", err)
	} else {
		for _, msg := range chat.Messages {
			if msg.Role != "assistant" {
				continue
			}
			for _, tc := range msg.ToolCalls {
				if strings.HasPrefix(tc.Function.Name, "mashura__") {
					t.Errorf("assistant message carries mashura tool call %q, want none", tc.Function.Name)
				}
			}
		}
	}
	if !strings.Contains(collapseLine, "automatic mashura rounds (started by wakil): 1") {
		t.Errorf("collapse line %q lacks %q", collapseLine, "automatic mashura rounds (started by wakil): 1")
	}
	if !strings.Contains(string(lastTurn2Body), "automatic mashura rounds (started by wakil): 1") {
		t.Errorf("last turn 2 request body lacks automatic mashura rounds count 1")
	}
	data, err := os.ReadFile(filepath.Join(e.workdir, "answer.txt"))
	if err != nil {
		t.Errorf("read answer.txt: %v", err)
	} else if string(data) != "same" {
		t.Errorf("answer.txt = %q, want %q", data, "same")
	}
}
