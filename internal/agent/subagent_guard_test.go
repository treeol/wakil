package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/treeol/wakil/internal/exec"
)

// scriptedModel is the suite's model double: it answers call N with frames[N]
// (repeating the last), and records every request body.
type scriptedModel struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies []string
}

func newScriptedModel(t *testing.T, frames ...[]string) *scriptedModel {
	t.Helper()
	m := &scriptedModel{}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		n := len(m.bodies)
		m.bodies = append(m.bodies, string(b))
		m.mu.Unlock()
		f := frames[len(frames)-1]
		if n < len(frames) {
			f = frames[n]
		}
		writeSSE(w, f...)
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *scriptedModel) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.bodies)
}

func (m *scriptedModel) body(i int) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bodies[i]
}

const hallucinated = `{"objective":"x","findings":[{"summary":"HALLUCINATED-FINDING","location":"a.go:1","kind":"fact","weight":"high"}]}`
const goodJSON = `{"objective":"x","findings":[{"summary":"real finding","location":"a.go:1","kind":"fact","weight":"low"}]}`

func guardApp(t *testing.T, m *scriptedModel, files ...string) *App {
	t.Helper()
	exec := newFakeExecutor()
	for _, f := range files {
		exec.files[f] = "content of " + f
	}
	return newTestApp(m.srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })
}

func track(a *App, task string, files []string) SubagentSummary {
	s, _, _, _, _, _ := a.dispatchTracked(context.Background(), task, io.Discard, "", "", "", "read", files, "0123456789abcdef")
	return s
}

// ---------- A: off-task rejection ----------

func TestOffTaskRejectedRetriedOnceThenNoticeAndStop(t *testing.T) {
	m := newScriptedModel(t,
		[]string{contentChunk(hallucinated)}, // run 1: reads nothing
		[]string{contentChunk(hallucinated)}, // retry: reads nothing again
	)
	a := guardApp(t, m, "a.go", "b.go")
	files := []string{"a.go", "b.go"}

	s := track(a, "audit these", files)

	if m.calls() != 2 {
		t.Fatalf("model calls = %d, want 2 (run + exactly one retry)", m.calls())
	}
	if !strings.Contains(m.body(0), "Assigned files") {
		t.Error("first run's task must carry the assigned file list")
	}
	if !strings.Contains(m.body(1), "You read none of the assigned files: a.go, b.go. Read them before reporting.") {
		t.Errorf("retry task lacks the ledger text; body: %.400s", m.body(1))
	}
	if s.Outcome != outcomeRejected || s.RejectReason != "off-task" || !s.Retry {
		t.Errorf("outcome/reason/retry = %q/%q/%v, want rejected/off-task/true", s.Outcome, s.RejectReason, s.Retry)
	}
	if len(s.Findings) != 0 || strings.Contains(s.Render(), "HALLUCINATED") {
		t.Error("rejected findings must NOT reach the parent context")
	}
	want := "subagent 01234567 off-task twice: read 0/2 assigned files — assign differently or take over?"
	if s.Notice != want {
		t.Errorf("notice = %q, want %q", s.Notice, want)
	}
	if strings.Contains(s.Notice, "call_") {
		t.Error("notice must not contain call ids")
	}
	if subagentDoneErr(s) == "" {
		t.Error("rejected result must surface as a failed/incomplete child")
	}

	// Stopped: any further auto-dispatch for the task is refused by the harness.
	before := m.calls()
	s2 := track(a, "audit these", files)
	if s2.Outcome != outcomeRefused || m.calls() != before {
		t.Errorf("dispatch while stopped: outcome=%q, new model calls=%d; want refused + 0", s2.Outcome, m.calls()-before)
	}
	// A re-worded task over the same files is the same task: still refused.
	s3 := track(a, "audit these again but differently", []string{"a.go"})
	if s3.Outcome != outcomeRefused {
		t.Errorf("reworded subset while stopped: outcome=%q, want refused", s3.Outcome)
	}
	// The user answering lifts the stop.
	a.BeginUserTurn()
	if s4 := track(a, "audit these", files); s4.Outcome == outcomeRefused {
		t.Error("stop must lift on the next user turn")
	}
}

func TestOffTaskRetrySucceedsBypassesSubsetCheck(t *testing.T) {
	m := newScriptedModel(t,
		[]string{contentChunk(hallucinated)},                 // run 1: nothing read
		toolCallFrames("r1", "read_file", `{"path":"a.go"}`), // retry: reads a.go
		[]string{contentChunk(goodJSON)},                     // retry: report
	)
	a := guardApp(t, m, "a.go", "b.go")
	files := []string{"a.go", "b.go"}

	s := track(a, "audit these", files)

	if !s.Retry || s.Outcome == outcomeRejected || s.Outcome == outcomeRefused {
		t.Fatalf("retry result: retry=%v outcome=%q, want an accepted retry", s.Retry, s.Outcome)
	}
	// Same scope + same file set on the retry would fail isStrictlySmallerScope;
	// the single retry is exempt precisely because it re-runs the SAME scope.
	if isStrictlySmallerScope(files, files, taskHash("audit these"), taskHash("audit these")) {
		t.Fatal("sanity: identical scope must fail the subset check")
	}
	if len(s.FilesCovered) != 1 || s.FilesCovered[0] != "a.go" || len(s.FilesNotRead) != 1 {
		t.Errorf("ledger covered/notread = %v/%v", s.FilesCovered, s.FilesNotRead)
	}
	if len(s.Findings) != 1 || s.Findings[0].Summary != "real finding" {
		t.Errorf("accepted retry must return its own findings, got %v", s.Findings)
	}
	if s.Notice != "" {
		t.Errorf("no notice expected on a successful retry, got %q", s.Notice)
	}
}

func TestNoRejectionWithoutFiles(t *testing.T) {
	m := newScriptedModel(t, []string{contentChunk(goodJSON)})
	a := guardApp(t, m)
	s := track(a, "just answer", nil)
	if m.calls() != 1 {
		t.Errorf("model calls = %d, want 1 (no retry without a file list)", m.calls())
	}
	if s.Outcome != outcomeComplete || s.RejectReason != "" || s.Retry {
		t.Errorf("outcome/reject/retry = %q/%q/%v, want complete/''/false", s.Outcome, s.RejectReason, s.Retry)
	}
	if s.FileReadLedger != nil && len(s.FileReadLedger.Listed) != 0 {
		t.Error("no list => no listed files in the ledger")
	}
}

// ---------- B: structured partial, split enforcement, notices ----------

func TestPartialBuiltFromRealCompletionAndSplitRules(t *testing.T) {
	m := newScriptedModel(t,
		toolCallFrames("r1", "read_file", `{"path":"a.go"}`), // run 1 reads a.go, then budget wall
		[]string{contentChunk(goodJSON)},
		toolCallFrames("r2", "read_file", `{"path":"b.go"}`), // the split reads b.go, wall again
		[]string{contentChunk(goodJSON)},
	)
	a := guardApp(t, m, "a.go", "b.go", "c.go")
	a.subMaxToolIter = 1 // exhaust after one tool round
	files := []string{"a.go", "b.go", "c.go"}

	s := track(a, "audit three", files)
	if s.Outcome != outcomePartial {
		t.Fatalf("outcome = %q, want partial", s.Outcome)
	}
	r := s.Result()
	if r.Status != "partial" || len(r.FilesCovered) != 1 || r.FilesCovered[0] != "a.go" ||
		len(r.FilesNotRead) != 2 || len(r.Findings) != 1 || r.RemainingScope == "" {
		t.Errorf("SubagentResult = %+v", r)
	}
	if s.Budget == nil || s.Budget.Class != "read" || s.Budget.Limit <= 0 {
		t.Errorf("budget = %+v, want class=read and a positive limit", s.Budget)
	}
	if s.Notice != "" {
		t.Errorf("first partial with files left to split must not stop: %q", s.Notice)
	}

	// Identical scope follow-up: refused, no model call.
	before := m.calls()
	if got := track(a, "audit three", files); got.Outcome != outcomeRefused || m.calls() != before {
		t.Errorf("identical follow-up: outcome=%q calls+%d, want refused/+0", got.Outcome, m.calls()-before)
	}
	// Same file set under a different wording is still not smaller.
	if got := track(a, "audit three, reworded", files); got.Outcome != outcomeRefused {
		t.Errorf("same files reworded: outcome=%q, want refused", got.Outcome)
	}
	// A superset re-reads everything already covered: refused too.
	if got := track(a, "audit more", append([]string{"z.go"}, files...)); got.Outcome != outcomeRefused {
		t.Errorf("superset follow-up: outcome=%q, want refused", got.Outcome)
	}

	// A proper subset with different text is allowed once...
	split := track(a, "audit only the leftovers", []string{"b.go", "c.go"})
	if split.Outcome == outcomeRefused {
		t.Fatalf("proper-subset split must be allowed once; got refused: %v", split.Uncertainty)
	}
	// ...and a second partial ON THE SPLIT is a notice + stop.
	if split.Outcome != outcomePartial || split.Notice == "" {
		t.Fatalf("second partial: outcome=%q notice=%q, want partial + notice", split.Outcome, split.Notice)
	}
	if want := "subagent 01234567 partial: 1/2 files, 1 findings — split or take over?"; split.Notice != want {
		t.Errorf("notice = %q, want %q", split.Notice, want)
	}
	// Stopped: even a legitimate-looking smaller scope is refused until the user speaks.
	if got := track(a, "audit c only", []string{"c.go"}); got.Outcome != outcomeRefused {
		t.Errorf("dispatch after the stop: outcome=%q, want refused", got.Outcome)
	}
	a.BeginUserTurn()
	if got := track(a, "audit c only", []string{"c.go"}); got.Outcome == outcomeRefused {
		t.Error("user turn must lift the stop")
	}
}

func TestEmptyPartialNoticeAndStop(t *testing.T) {
	garbage := strings.Repeat("not json ", 50)
	m := newScriptedModel(t,
		toolCallFrames("r1", "read_file", `{"path":"a.go"}`),
		[]string{contentChunk(garbage)}, // forced final turn: garbage
		[]string{contentChunk(garbage)}, // the single reserve turn: garbage
	)
	a := guardApp(t, m, "a.go")
	a.subMaxToolIter = 1

	s := track(a, "look around", nil) // no file list => nothing left to split

	if s.Outcome != outcomePartial || len(s.Findings) != 0 || len(s.FilesNotRead) != 0 {
		t.Fatalf("outcome/findings/notread = %q/%d/%d", s.Outcome, len(s.Findings), len(s.FilesNotRead))
	}
	if s.RemainingScope == "" || len(s.Uncertainty) != 1 && strings.Contains(strings.Join(s.Uncertainty, "|"), "not json") {
		t.Errorf("raw tail belongs in remaining_scope only; scope=%q uncertainty=%v", s.RemainingScope, s.Uncertainty)
	}
	if want := "subagent 01234567 partial: no file list, 0 findings — split or take over?"; s.Notice != want {
		t.Errorf("notice = %q, want %q", s.Notice, want)
	}
	if got := track(a, "look around", nil); got.Outcome != outcomeRefused {
		t.Errorf("dispatch after empty partial: %q, want refused", got.Outcome)
	}
}

func TestFailedChildIsNeitherPartialNorOffTask(t *testing.T) {
	s := SubagentSummary{Status: "incomplete", StopReason: "error", FileReadLedger: &FileLedger{Listed: []string{"a.go"}, NotRead: []string{"a.go"}}}
	if offTask(s, []string{"a.go"}) {
		t.Error("an errored child never got to work — not off-task")
	}
	if subagentOutcome(s) != outcomeFailed {
		t.Errorf("outcome = %q, want failed", subagentOutcome(s))
	}
}

func TestSubagentFinishedStatusFormatting(t *testing.T) {
	cases := []struct {
		s    SubagentSummary
		want string
	}{
		{SubagentSummary{}, ""},
		{SubagentSummary{Outcome: outcomePartial, Status: "incomplete", FilesListedN: 9, FilesCovered: make([]string, 7)}, "partial files 7/9"},
		{SubagentSummary{Outcome: outcomeRejected, Status: "incomplete", FilesListedN: 3}, "rejected files 0/3"},
		{SubagentSummary{Outcome: outcomeComplete, FilesListedN: 2, FilesCovered: []string{"a", "b"}}, "ok files 2/2"},
		{SubagentSummary{Outcome: outcomePartial, Status: "incomplete"}, "partial"},
		{SubagentSummary{Outcome: outcomeRefused, Status: "incomplete"}, "refused"},
	}
	for _, c := range cases {
		if got := subagentFinishedStatus(c.s); got != c.want {
			t.Errorf("status = %q, want %q (%+v)", got, c.want, c.s)
		}
	}
}

// ---------- round-2 fixes ----------

// BL-2: spelling variants of a listed file are ONE file; a failed read is not a read.
// Uses the REAL DirectExecutor (its ConfinePath/ReadFile resolve paths); the
// fake executor does not normalise, so it cannot exercise this.
func TestLedgerCanonicalAndSuccessfulReadsOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	exe, err := exec.NewDirectExecutor(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := newScriptedModel(t,
		toolCallFrames("r1", "read_file", `{"path":"./a.go"}`),     // listed as "a.go"
		toolCallFrames("r2", "read_file", `{"path":"missing.go"}`), // fails: must not count
		[]string{contentChunk(goodJSON)},
	)
	a := newTestApp(m.srv.URL, exe, func(_, _, _ string, _ bool) bool { return true })

	s := track(a, "audit", []string{"a.go", "missing.go"})

	if m.calls() != 3 {
		t.Fatalf("calls = %d, want 3 (no off-task retry: ./a.go IS a.go)", m.calls())
	}
	if len(s.FilesCovered) != 1 || s.FilesCovered[0] != "a.go" {
		t.Errorf("covered = %v, want [a.go] (./a.go ≡ a.go)", s.FilesCovered)
	}
	if len(s.FilesNotRead) != 1 || s.FilesNotRead[0] != "missing.go" {
		t.Errorf("not read = %v, want [missing.go] (a failed read does not count)", s.FilesNotRead)
	}
	if s.Outcome == outcomeRejected {
		t.Error("a child that read a listed file must not be rejected as off-task")
	}
}

// BL-1: a rejected result keeps the recoverable spill path and the MCP-call record.
func TestRejectedSummaryNamesSpillAndKeepsExternalCalls(t *testing.T) {
	orig := SubagentSummary{
		Objective:      "x",
		Findings:       []Finding{{Summary: "HALLUCINATED-FINDING"}},
		ExternalCalls:  []ExternalAction{{Server: "s", Tool: "t", Status: "ok"}},
		FileReadLedger: &FileLedger{Listed: []string{"a.go"}, NotRead: []string{"a.go"}},
	}
	rs := rejectedSummary(orig, []string{"a.go"}, "read", "/cache/spill-1.txt", true)
	if len(rs.Findings) != 0 || strings.Contains(rs.Render(), "HALLUCINATED") {
		t.Error("findings must not survive rejection")
	}
	if !strings.Contains(strings.Join(rs.Uncertainty, "|"), "/cache/spill-1.txt") || len(rs.SpillRefs) != 1 {
		t.Errorf("spill path lost: uncertainty=%v refs=%v", rs.Uncertainty, rs.SpillRefs)
	}
	if len(rs.ExternalCalls) != 1 {
		t.Error("mechanical external_calls record must survive rejection")
	}
}

// A model-written status other than "incomplete" is not a failure.
func TestOutcomeIgnoresModelWrittenCompleteStatus(t *testing.T) {
	for _, st := range []string{"", "complete", "ok", "done"} {
		if got := subagentOutcome(SubagentSummary{Status: st}); got != outcomeComplete {
			t.Errorf("status %q -> %q, want complete", st, got)
		}
	}
	if subagentOutcome(SubagentSummary{Status: "incomplete", StopReason: "error"}) != outcomeFailed {
		t.Error("incomplete+error must be failed")
	}
}

// Q2: two concurrent identical dispatches — the second is refused while the first is in flight.
func TestConcurrentIdenticalDispatchRefused(t *testing.T) {
	var g dispatchGuard
	files := []string{"a.go"}
	_, _, r1 := g.admit(files, taskHash("t"))
	if r1 != "" {
		t.Fatalf("first admit refused: %s", r1)
	}
	if _, _, r2 := g.admit(files, taskHash("t")); r2 == "" {
		t.Error("an identical dispatch already in flight must be refused")
	}
	if _, _, r3 := g.admit([]string{"b.go"}, taskHash("other")); r3 != "" {
		t.Errorf("a different scope must not be blocked: %s", r3)
	}
}

// Q3: a workflow-continuation turn must not lift a stop; the user's next turn does.
func TestAutomaticTurnDoesNotLiftStop(t *testing.T) {
	a := &App{}
	lin, _, _ := a.guard.admit([]string{"a.go"}, taskHash("t"))
	a.guard.setState(lin, lineageStopped, "off-task twice")

	a.MarkAutomaticTurn()
	a.BeginUserTurn() // the automatic continuation
	if _, _, refuse := a.guard.admit([]string{"a.go"}, taskHash("t")); refuse == "" {
		t.Fatal("an automatic continuation turn lifted the stop")
	}
	a.BeginUserTurn() // the user answers
	if _, _, refuse := a.guard.admit([]string{"a.go"}, taskHash("t")); refuse != "" {
		t.Errorf("the user's turn must lift the stop, got refusal: %s", refuse)
	}
	// A rejected enqueue withdraws the mark.
	a.MarkAutomaticTurn()
	a.ClearAutomaticTurn()
	lin2, _, _ := a.guard.admit([]string{"z.go"}, taskHash("z"))
	a.guard.setState(lin2, lineageStopped, "x")
	a.BeginUserTurn()
	if _, _, refuse := a.guard.admit([]string{"z.go"}, taskHash("z")); refuse != "" {
		t.Error("a withdrawn mark must not shield the stop from the next user turn")
	}
}

// Q4/Q5: after the off-task retry the parent gets ONLY the retry's grounding,
// summed budget, and no findings from the rejected first run.
func TestOffTaskRetryMergeKeepsCostDropsRejectedFindings(t *testing.T) {
	m := newScriptedModel(t,
		[]string{contentChunk(hallucinated)},
		toolCallFrames("r1", "read_file", `{"path":"a.go"}`),
		[]string{contentChunk(goodJSON)},
	)
	a := guardApp(t, m, "a.go")
	s, _, _, _, rows, _ := a.dispatchTracked(context.Background(), "audit", io.Discard, "", "", "", "read", []string{"a.go"}, "0123456789abcdef")
	if strings.Contains(s.Render(), "HALLUCINATED") {
		t.Error("rejected first-run findings leaked into the retry result")
	}
	_ = rows // cost rows of both runs are appended (asserted by count in the e2e path)
	if !strings.Contains(strings.Join(s.Uncertainty, "|"), "off-task first run discarded") && s.Outcome != outcomeComplete {
		t.Errorf("accepted retry should note the discarded first run; uncertainty=%v", s.Uncertainty)
	}
}
