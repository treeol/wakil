package agent

// skill_suggestion_test.go — tests for the B1/B2 workflow-suggestion tracker.

import (
	"github.com/treeol/wakil/internal/config"
	"strings"
	"testing"
)

func newTracker() *skillSuggestionState {
	return &skillSuggestionState{}
}

func seq(names ...string) []string { return names }

func TestRecordSkillToolCall_B1LongWorkflowFires(t *testing.T) {
	s := newTracker()
	var hint string
	for i := 0; i < b1MinCalls; i++ {
		hint = s.recordSkillToolCall("run_shell", true)
	}
	// run_shell alone: distinct=1 — only B1 (≥8 calls, ≥1 non-read) can fire.
	if hint == "" {
		t.Fatal("expected B1 hint after 8 successful run_shell calls")
	}
	if hint != triggerLong {
		t.Errorf("B1 must report the long trigger, got %q", hint)
	}
}

func TestRecordSkillToolCall_B1RequiresNonReadTool(t *testing.T) {
	s := newTracker()
	var hint string
	for i := 0; i < b1MinCalls+2; i++ {
		hint = s.recordSkillToolCall("read_file", true)
	}
	if hint != "" {
		t.Errorf("pure-read sequence must never hint, got: %s", hint)
	}
}

func TestRecordSkillToolCall_B2RepetitionFires(t *testing.T) {
	s := newTracker()
	wf := seq("read_file", "search_files", "run_shell", "edit_file")
	// Turn 1: run the workflow (4 calls: qualifies but no history → no hint).
	for _, n := range wf {
		if h := s.recordSkillToolCall(n, true); h != "" {
			t.Fatalf("first run must not hint (no history), got: %s", h)
		}
	}
	s.commitSkillTurn()
	// Turn 2: same sequence → hint fires on the 4th call.
	var hint string
	for _, n := range wf {
		if h := s.recordSkillToolCall(n, true); h != "" {
			hint = h
		}
	}
	if hint == "" {
		t.Fatal("expected B2 hint on repeated sequence")
	}
	if hint != triggerRepeat {
		t.Errorf("B2 must report the repeat trigger, got %q", hint)
	}
}

func TestRecordSkillToolCall_NoSelfMatchWithinTurn(t *testing.T) {
	s := newTracker()
	wf := seq("read_file", "search_files", "run_shell", "edit_file", "read_file", "search_files", "run_shell", "edit_file")
	// One long turn replaying the same 4-call prefix — history is empty, so
	// nothing to match; B1 fires only at 8.
	var hinted bool
	for _, n := range wf {
		if h := s.recordSkillToolCall(n, true); strings.Contains(h, "matches an earlier turn") {
			hinted = true
		}
	}
	if hinted {
		t.Error("B2 must not fire from self-match within one turn")
	}
}

func TestRecordSkillToolCall_OneHintPerTurn(t *testing.T) {
	s := newTracker()
	count := 0
	for i := 0; i < 20; i++ {
		if h := s.recordSkillToolCall("run_shell", true); h != "" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("hints in one turn = %d, want exactly 1", count)
	}
}

func TestRecordSkillToolCall_SessionCap(t *testing.T) {
	s := newTracker()
	fired := 0
	// Three long turns → third turn's hint must be suppressed by the cap.
	for turn := 0; turn < 3; turn++ {
		for i := 0; i < b1MinCalls+1; i++ {
			if h := s.recordSkillToolCall("run_shell", true); h != "" {
				fired++
			}
		}
		s.commitSkillTurn()
	}
	if fired != skillHintMaxPerSession {
		t.Errorf("hints fired = %d, want %d (session cap)", fired, skillHintMaxPerSession)
	}
}

func TestRecordSkillToolCall_FailuresAndExcludedIgnored(t *testing.T) {
	s := newTracker()
	s.recordSkillToolCall("run_shell", false) // failure ignored
	s.recordSkillToolCall("save_skill", true) // excluded
	if len(s.current) != 0 {
		t.Errorf("failed/excluded calls must not enter the sequence, got %v", s.current)
	}
	// A subagent-only dispatch bookkeeping call doesn't count either.
	s.recordSkillToolCall("dispatch_subagent", true)
	if len(s.current) != 0 {
		t.Errorf("excluded orchestration tool entered sequence: %v", s.current)
	}
}

func TestRecordSkillToolCall_B2NeedsDistinctAndNonRead(t *testing.T) {
	s := newTracker()
	// Turn 1: 4 identical read calls — does NOT qualify, must not be committed.
	for i := 0; i < b2MinCalls; i++ {
		s.recordSkillToolCall("read_file", true)
	}
	s.commitSkillTurn()
	if len(s.history) != 0 {
		t.Fatalf("non-qualifying turn must not enter history, got %v", s.history)
	}
	// Turn 2: read,read,read,read,edit — prev is a prefix BUT prev never
	// qualified, so B2 must not fire (the matched region is pure reads).
	var hint string
	for i := 0; i < b2MinCalls; i++ {
		hint = s.recordSkillToolCall("read_file", true)
	}
	if hint != "" {
		t.Fatal("B2 fired against a non-qualifying read-only history entry")
	}
	hint = s.recordSkillToolCall("edit_file", true)
	if strings.Contains(hint, "matches an earlier turn") {
		t.Errorf("B2 must not fire when the matched prefix is pure reads, got: %s", hint)
	}
}

func TestRecordSkillToolCall_B2PrecedenceOverB1(t *testing.T) {
	s := newTracker()
	long := seq("read_file", "search_files", "run_shell", "edit_file", "read_file", "search_files", "run_shell", "edit_file", "read_file", "search_files")
	for _, n := range long {
		s.recordSkillToolCall(n, true)
	}
	s.commitSkillTurn()
	// Turn 2 replays the same 10-call sequence: at call 8 both B2 and B1 are
	// eligible — but B2 already fired at call 4, and one hint per turn means
	// B1 must not also fire.
	var hints []string
	for _, n := range long {
		if h := s.recordSkillToolCall(n, true); h != "" {
			hints = append(hints, h)
		}
	}
	if len(hints) != 1 {
		t.Fatalf("expected exactly 1 hint (B2), got %d", len(hints))
	}
	if hints[0] != triggerRepeat {
		t.Errorf("the fired trigger should be B2 (repetition), got %q", hints[0])
	}
}

func TestRecordSkillToolCall_LoadSkillSuppressesTurn(t *testing.T) {
	s := newTracker()
	wf := seq("read_file", "search_files", "run_shell", "edit_file")
	for _, n := range wf {
		s.recordSkillToolCall(n, true)
	}
	s.commitSkillTurn()
	// Turn 2 replays the workflow AND loads a skill — the loaded-skill signal
	// means the procedure already IS a skill; hint suppressed.
	s.recordSkillToolCall("read_file", true)
	s.recordSkillToolCall("search_files", true)
	s.recordSkillToolCall("run_shell", true)
	if h := s.recordSkillToolCall("load_skill", true); h != "" {
		t.Fatalf("load_skill is excluded, got hint: %s", h)
	}
	if h := s.recordSkillToolCall("edit_file", true); h != "" {
		t.Errorf("hint must be suppressed for the turn after load_skill, got: %s", h)
	}
}

func TestCommitSkillTurn_HistoryBounded(t *testing.T) {
	s := newTracker()
	for turn := 0; turn < skillHistoryMax+5; turn++ {
		s.recordSkillToolCall("read_file", true)
		s.commitSkillTurn()
	}
	if len(s.history) > skillHistoryMax {
		t.Errorf("history = %d, want ≤ %d", len(s.history), skillHistoryMax)
	}
}

func TestAppendSkillHint_SubagentAndNilStoreGating(t *testing.T) {
	// Subagent gate: real store present, IsSubagent set — no hint.
	appSub := skillTestApp(t, true, nil)
	outSub := appSub.appendSkillHint("run_shell", true, "result")
	if outSub != "result" {
		t.Error("subagents must never get hints even with a store")
	}
	// Store gate: main agent, nil store — no hint.
	app2 := &App{SkillStore: nil}
	out2 := app2.appendSkillHint("run_shell", true, "result")
	if out2 != "result" {
		t.Error("nil skill store must disable hints")
	}
	// Main agent with a store: hint machinery live.
	appMain := skillTestApp(t, false, nil)
	out3 := appMain.appendSkillHint("run_shell", true, "result")
	if out3 != "result" {
		t.Error("no hint expected below threshold")
	}
}

func TestAppendSkillHint_FiresAndPersistsAcrossCalls(t *testing.T) {
	app := skillTestApp(t, false, nil)
	var last string
	for i := 0; i < b1MinCalls; i++ {
		last = app.appendSkillHint("run_shell", true, "ok")
	}
	// 0.4.4 (D2): the suggestion is no longer injected into the tool result.
	if last != "ok" || strings.Contains(last, "app note") {
		t.Fatalf("tool result must stay unchanged, got: %q", last)
	}
	// The tracker still fired (hint counted) and the signal is pending for the
	// end-of-turn notice.
	if app.takeSkillSignal() == "" {
		t.Error("the detector signal must still reach the end-of-turn notice")
	}
	// State persisted on the App.
	if app.skillSuggest == nil || app.skillSuggest.hintsUsed != 1 {
		t.Errorf("App-level tracker state missing or wrong: %+v", app.skillSuggest)
	}
}

func TestFormatSkillSequence(t *testing.T) {
	if got := formatSkillSequence(seq("a", "b")); got != "a → b" {
		t.Errorf("formatSkillSequence = %q", got)
	}
}

// D2: a fresh config defaults to skill auto-creation "off".
func TestSkillCreationDefaultIsOff(t *testing.T) {
	var c config.SkillCreationConfig
	if got := c.ModeOrDefault(); got != config.SkillCreationOff {
		t.Errorf("default mode = %q, want off", got)
	}
	c.Mode = config.SkillCreationOn
	if got := c.ModeOrDefault(); got != config.SkillCreationOn {
		t.Errorf("an explicit mode must win: %q", got)
	}
}

// D2: in mode "on" the suggestion reaches the user as a status notice at the
// end of the turn (SysNoteMsg through the event sink), and nothing enters a
// tool result. Uses the real end-of-turn hook.
func TestSkillSuggestionNoticeInOnMode(t *testing.T) {
	app, _ := scApp(t) // a store, a workspace with files written this session
	app.setSkillCreationMode("on", false)
	var notes []string
	app.EventSink = func(msg interface{}) {
		if n, ok := msg.(SysNoteMsg); ok {
			notes = append(notes, n.Text)
		}
	}
	app.setSkillSignal(triggerLong) // what the detector leaves behind
	app.onTurnEndSkillCreation()
	if len(notes) != 1 || !strings.Contains(notes[0], "/skill-creation now") {
		t.Fatalf("on: want one status notice naming /skill-creation now, got %q", notes)
	}

	// off: the signal is consumed silently.
	app2, _ := scApp(t)
	app2.setSkillCreationMode("off", false)
	var notes2 []string
	app2.EventSink = func(msg interface{}) {
		if n, ok := msg.(SysNoteMsg); ok {
			notes2 = append(notes2, n.Text)
		}
	}
	app2.setSkillSignal(triggerLong)
	app2.onTurnEndSkillCreation()
	if len(notes2) != 0 {
		t.Errorf("off must not notify, got %q", notes2)
	}
}
