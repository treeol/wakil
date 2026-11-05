package agent

// End-to-end 0.4.2 dispatch tests. REAL: DirectExecutor on a temp repo, the
// real read_file tool path (ExecuteToolCall → ledger recorder), the real
// dispatch_subagent handler, prepareSubagentBlock, the async worker, the
// harness guard, notices and the async envelope. SCRIPTED: only the model, via
// the suite's existing httptest SSE double (no live model is called).
// The exhaustion budget is forced through the real config key
// subagent_max_tool_iterations (the class byte budget has a 50 KB floor, so
// three small temp files cannot reach it).

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
	"sync/atomic"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/exec"
	"github.com/treeol/wakil/internal/proxy"
)

type e2eModel struct {
	srv       *httptest.Server
	mu        sync.Mutex
	subCalls  map[string]int
	parentNxt atomic.Pointer[[]string] // frames for the NEXT parent call; later parent calls answer "ok"
}

func newE2EModel(t *testing.T) *e2eModel {
	t.Helper()
	m := &e2eModel{subCalls: map[string]int{}}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !isSubagentRequest(body) {
			if fr := m.parentNxt.Swap(nil); fr != nil {
				writeSSE(w, *fr...)
				return
			}
			writeSSE(w, contentChunk("ok"))
			return
		}
		b := string(body)
		marker := ""
		for _, mk := range []string{"AUDIT-ALL", "LEFTOVER-C", "NOOPEN"} {
			if strings.Contains(b, mk) {
				marker = mk
			}
		}
		m.mu.Lock()
		m.subCalls[marker]++
		n := m.subCalls[marker]
		m.mu.Unlock()
		switch marker {
		case "AUDIT-ALL": // reads a.go, b.go, then the wall (iteration cap 2)
			switch n {
			case 1:
				writeSSE(w, toolCallFrames("call_sub_1", "read_file", `{"path":"a.go"}`)...)
			case 2:
				writeSSE(w, toolCallFrames("call_sub_2", "read_file", `{"path":"b.go"}`)...)
			default:
				writeSSE(w, contentChunk(`{"objective":"audit","findings":[{"summary":"a.go and b.go reviewed","location":"a.go:1","kind":"fact","weight":"low"}]}`))
			}
		case "LEFTOVER-C":
			if n == 1 {
				writeSSE(w, toolCallFrames("call_sub_3", "read_file", `{"path":"c.go"}`)...)
				return
			}
			writeSSE(w, contentChunk(`{"objective":"leftover","findings":[{"summary":"c.go reviewed","location":"c.go:1","kind":"fact","weight":"low"}]}`))
		default: // NOOPEN: answers without ever opening a file
			writeSSE(w, contentChunk(`{"objective":"noopen","findings":[{"summary":"HALLUCINATED-FINDING","location":"x:1","kind":"fact","weight":"high"}]}`))
		}
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *e2eModel) calls(marker string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.subCalls[marker]
}

func (m *e2eModel) parentDispatches(callID string, dispatches ...map[string]any) {
	var frames []string
	for i, d := range dispatches {
		args, _ := json.Marshal(d)
		argStr, _ := json.Marshal(string(args))
		frames = append(frames, fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":%d,"id":"%s_%d","type":"function","function":{"name":"dispatch_subagent","arguments":%s}}]},"finish_reason":null}]}`, i, callID, i, argStr))
	}
	frames = append(frames, `{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
	m.parentNxt.Store(&frames)
}

func lastEnvelope(app *App) string {
	if env := app.drainAsyncInbox(); env != "" {
		return env
	}
	app.convMu.RLock()
	defer app.convMu.RUnlock()
	for i := len(app.Conv) - 1; i >= 0; i-- {
		if app.Conv[i].Role == "user" {
			if s := DerefStr(app.Conv[i].Content); strings.Contains(s, asyncBlockHeader) {
				return s
			}
		}
	}
	return ""
}

func e2eApp(t *testing.T, m *e2eModel) (*App, *strings.Builder) {
	t.Helper()
	dir := t.TempDir()
	for _, f := range []string{"a.go", "b.go", "c.go"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("package x // "+f+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	exe, err := exec.NewDirectExecutor(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	app := newTestApp(m.srv.URL, exe, func(_, _, _ string, _ bool) bool { return true })
	app.Out = &out
	app.Cfg.SubagentMaxToolIter = 2
	return app, &out
}

func sendAndSettle(t *testing.T, app *App, text string) string {
	t.Helper()
	done := make(chan error, 1)
	go func() { _, err := app.Send(t.Context(), text); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Send did not finish")
	}
	waitAsyncOps(t, app)
	return lastEnvelope(app)
}

func TestE2EBudgetExhaustionPartialLedgerAndSplit(t *testing.T) {
	m := newE2EModel(t)
	app, out := e2eApp(t, m)
	files := []string{"a.go", "b.go", "c.go"}

	m.parentDispatches("call_parent_1", map[string]any{"task": "AUDIT-ALL three files", "class": "read", "files": files})
	env := sendAndSettle(t, app, "audit the repo")

	// Real tool path + real exhaustion: partial, ledger from the actual reads.
	for _, want := range []string{`"outcome":"partial"`, `"files_covered":["a.go","b.go"]`, `"files_not_read":["c.go"]`, `"files_listed":3`, `"remaining_scope"`, `"class":"read"`} {
		if !strings.Contains(env, want) {
			t.Errorf("envelope lacks %s\n%s", want, env)
		}
	}
	if got := m.calls("AUDIT-ALL"); got != 3 {
		t.Errorf("child model calls = %d, want 3 (read, read, forced final)", got)
	}
	if strings.Contains(out.String(), "partial:") {
		t.Errorf("a first partial with a file left to split must not stop the task; out=%q", out.String())
	}

	// Follow-up #1: identical scope → refused by the harness; the child never runs.
	m.parentDispatches("call_parent_2", map[string]any{"task": "AUDIT-ALL three files", "class": "read", "files": files})
	env2 := sendAndSettle(t, app, "again")
	if !strings.Contains(env2, `"outcome":"refused"`) || m.calls("AUDIT-ALL") != 3 {
		t.Errorf("identical re-dispatch must be refused without a model call (calls=%d)\n%s", m.calls("AUDIT-ALL"), env2)
	}

	// Follow-up #2: strictly smaller scope (the unread file) + different text → allowed, completes.
	m.parentDispatches("call_parent_3", map[string]any{"task": "LEFTOVER-C only", "class": "read", "files": []string{"c.go"}})
	env3 := sendAndSettle(t, app, "finish it")
	if !strings.Contains(env3, `"files_covered":["c.go"]`) || strings.Contains(env3, `"outcome":"refused"`) {
		t.Errorf("strictly smaller follow-up must run and cover c.go\n%s", env3)
	}
	if m.calls("LEFTOVER-C") != 2 {
		t.Errorf("follow-up child calls = %d, want 2", m.calls("LEFTOVER-C"))
	}

	// No call_ ids in any user-visible notice or in the collapse line.
	if strings.Contains(out.String(), "call_") {
		t.Errorf("user-visible output leaks a call id: %q", out.String())
	}
	app.convMu.RLock()
	var tools []proxy.Message
	for _, msg := range app.Conv {
		if msg.Role == "tool" {
			tools = append(tools, msg)
		}
	}
	app.convMu.RUnlock()
	if len(tools) == 0 {
		t.Fatal("expected tool messages in the parent conversation")
	}
	notice := formatCollapseNotice(1, 40000, 10000, tools, 0, "")
	if strings.Contains(notice, "call_") || !strings.Contains(notice, "dispatch_subagent ×") {
		t.Errorf("collapse notice = %q (want tool aggregation, no call_ ids)", notice)
	}
}

func TestE2EOffTaskRejectedRetriedOnceThenNoticeAndStop(t *testing.T) {
	m := newE2EModel(t)
	app, out := e2eApp(t, m)

	task := "NOOPEN: do not open any files, just answer"
	m.parentDispatches("call_parent_1", map[string]any{"task": task, "class": "read", "files": []string{"a.go", "b.go"}})
	env := sendAndSettle(t, app, "go")

	if got := m.calls("NOOPEN"); got != 2 {
		t.Errorf("child model calls = %d, want exactly 2 (run + one harness retry)", got)
	}
	if strings.Contains(env, "HALLUCINATED") {
		t.Errorf("rejected findings leaked into the parent context:\n%s", env)
	}
	if !strings.Contains(env, `"reject_reason":"off-task"`) || !strings.Contains(env, `"retry":true`) {
		t.Errorf("envelope must mark a rejected off-task retry:\n%s", env)
	}
	want := "off-task twice: read 0/2 assigned files — assign differently or take over?"
	if !strings.Contains(out.String(), want) {
		t.Errorf("user notice missing %q; out=%q", want, out.String())
	}
	if strings.Contains(out.String(), "call_") {
		t.Errorf("notice leaks a call id: %q", out.String())
	}

	// Stopped: the parent trying again is refused by the harness, no model call.
	m.parentDispatches("call_parent_2", map[string]any{"task": task, "class": "read", "files": []string{"a.go", "b.go"}})
	env2 := sendAndSettle(t, app, "try again")
	if !strings.Contains(env2, `"outcome":"refused"`) || m.calls("NOOPEN") != 2 {
		t.Errorf("dispatch while stopped must be refused with no model call (calls=%d)\n%s", m.calls("NOOPEN"), env2)
	}

	// The USER answering lifts the stop.
	app.BeginUserTurn()
	m.parentDispatches("call_parent_3", map[string]any{"task": "LEFTOVER-C assigned differently", "class": "read", "files": []string{"c.go"}})
	env3 := sendAndSettle(t, app, "assign differently")
	if strings.Contains(env3, `"outcome":"refused"`) {
		t.Errorf("after the user's turn dispatch must be allowed again:\n%s", env3)
	}
}
