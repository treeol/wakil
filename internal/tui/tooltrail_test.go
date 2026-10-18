package tui

import (
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/core/event"
)

// TestToolTrailInPlaceTailAndTurnEndCollapse covers the compact tool display:
// completed calls accumulate in the trail (rendered as ONE replacing line in
// the live tail), and turn end commits a single collapsed summary instead of
// one viewport line per call.
func TestToolTrailInPlaceTailAndTurnEndCollapse(t *testing.T) {
	m, f := newTestTUI(t)
	m.state = stateStreaming

	complete := func(id, name, line, status string) {
		m = step(m, evt(event.KindToolCallCompleted, event.ToolCallCompleted{
			ToolCallID: event.ToolCallID(id), Name: name, Status: status, ToolLine: line,
		}, f.sid))
	}
	complete("t1", "read_file", "· read_file README.md → 76 lines · 5.5KB", "ok")
	complete("t2", "edit_file", "· edit_file main.go → ok", "ok")
	complete("t3", "run_shell", "· run_shell pwd → ✗ nope", "error")

	if len(m.turnToolTrail) != 3 {
		t.Fatalf("trail = %d entries; want 3", len(m.turnToolTrail))
	}
	if m.pendingToolErrs != 1 {
		t.Errorf("pendingToolErrs = %d; want 1", m.pendingToolErrs)
	}
	// No per-call items were committed to the viewport during the turn —
	// the trail renders in the live tail only.
	if len(*m.items) != 0 {
		t.Errorf("per-call lines must not be committed mid-turn; got %d items", len(*m.items))
	}

	m = step(m, evt(event.KindTurnCompleted, event.TurnCompleted{TurnID: "trn_1", Outcome: "complete"}, f.sid))
	if len(*m.items) != 1 {
		t.Fatalf("turn end should commit exactly one summary item; got %d", len(*m.items))
	}
	got := lastItemText(m)
	for _, want := range []string{"3 tool calls", "read_file", "edit_file", "run_shell", "1 failed"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q missing %q", got, want)
		}
	}
	if m.turnToolTrail != nil || m.pendingToolErrs != 0 {
		t.Error("trail state must be cleared at turn end")
	}
}

// TestToolTrailSingleCallSummary verifies the singular phrasing and that a
// clean turn carries no failure suffix.
func TestToolTrailSingleCallSummary(t *testing.T) {
	m, f := newTestTUI(t)
	m.state = stateStreaming
	m = step(m, evt(event.KindToolCallCompleted, event.ToolCallCompleted{
		ToolCallID: "t1", Name: "read_file", Status: "ok", ToolLine: "· read_file x.go → ok",
	}, f.sid))
	m = step(m, evt(event.KindTurnCompleted, event.TurnCompleted{TurnID: "trn_1", Outcome: "complete"}, f.sid))
	got := lastItemText(m)
	if !strings.Contains(got, "1 tool call: read_file") {
		t.Errorf("summary = %q; want '1 tool call: read_file'", got)
	}
	if strings.Contains(got, "failed") {
		t.Errorf("clean turn must not flag failures; got %q", got)
	}
}

// TestToolTailLineRendersLatest verifies the in-place tail line in the
// actual rendered View: the latest completed call renders prefixed with the
// call count, and each completion replaces the previous line (only ONE
// tool line visible).
func TestToolTailLineRendersLatest(t *testing.T) {
	m, f := newTestTUI(t)
	m.state = stateStreaming
	m = step(m, evt(event.KindToolCallCompleted, event.ToolCallCompleted{
		ToolCallID: "t1", Name: "read_file", Status: "ok", ToolLine: "· read_file a.go → ok",
	}, f.sid))
	m = step(m, evt(event.KindToolCallCompleted, event.ToolCallCompleted{
		ToolCallID: "t2", Name: "search_files", Status: "ok", ToolLine: "· search_files q → 3 matches",
	}, f.sid))
	m.refreshViewport()
	view := plain(m.View())
	if !strings.Contains(view, "2· search_files q → 3 matches") {
		t.Errorf("tail must render '2· search_files q → 3 matches'; view = %q", view)
	}
	if strings.Contains(view, "1· read_file") {
		t.Errorf("previous tool line must be replaced, not stacked; view = %q", view)
	}
	if got := strings.Count(view, "· "); got > 3 {
		t.Errorf("unexpected line stacking (%d '· ' markers); view = %q", got, view)
	}
}

// TestToolTrailClearedOnSessionError verifies the standalone session-error
// path also commits the collapsed summary and drops the live trail.
func TestToolTrailClearedOnSessionError(t *testing.T) {
	m, f := newTestTUI(t)
	m.state = stateStreaming
	m = step(m, evt(event.KindToolCallCompleted, event.ToolCallCompleted{
		ToolCallID: "t1", Name: "run_shell", Status: "ok", ToolLine: "· run_shell ls → ok",
	}, f.sid))
	m = step(m, evt(event.KindSessionError, event.SessionError{Reason: "request_error", Err: "boom"}, f.sid))
	if m.turnToolTrail != nil {
		t.Error("session error must clear the tool trail")
	}
	found := false
	for _, it := range *m.items {
		if strings.Contains(plain(it.text), "1 tool call: run_shell") {
			found = true
		}
	}
	if !found {
		t.Error("session error must commit the collapsed tool summary")
	}
}

// TestToolTrailEmptyToolLineLegacy covers events without ToolLine (replay or
// legacy producers): the trail still counts the call; the turn-end summary
// derives from the name.
func TestToolTrailEmptyToolLineLegacy(t *testing.T) {
	m, f := newTestTUI(t)
	m.state = stateStreaming
	m = step(m, evt(event.KindToolCallCompleted, event.ToolCallCompleted{
		ToolCallID: "t1", Name: "run_shell", Status: "",
	}, f.sid))
	if len(m.turnToolTrail) != 1 {
		t.Fatal("legacy completion must still be recorded")
	}
	m = step(m, evt(event.KindTurnCompleted, event.TurnCompleted{TurnID: "trn_1", Outcome: "complete"}, f.sid))
	if !strings.Contains(lastItemText(m), "1 tool call: run_shell") {
		t.Errorf("legacy summary = %q", lastItemText(m))
	}
}

// TestMessageDeltaStripsRawANSI is the defensive guard: raw ANSI reaching the
// streaming buffer used to surface as literal "[2m…[0m" after the markdown
// commit. Ingestion now strips it.
func TestMessageDeltaStripsRawANSI(t *testing.T) {
	m, f := newTestTUI(t)
	m.state = stateStreaming
	m = step(m, evt(event.KindMessageDelta, event.MessageDelta{Text: "\x1b[2mdim note\x1b[0m"}, f.sid))
	if s := m.streaming.String(); strings.Contains(s, "\x1b") {
		t.Errorf("streaming buffer must be ANSI-free; got %q", s)
	}
	if !strings.Contains(m.streaming.String(), "dim note") {
		t.Errorf("stripped text must survive; got %q", m.streaming.String())
	}
}
