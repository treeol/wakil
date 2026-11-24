package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/sessionclient"
	"github.com/treeol/wakil/internal/proxy"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// sessionclientMessage aliases the transcript message type.
type sessionclientMessage = proxy.Message

func testSessions() []sessionclient.SessionSummary {
	return []sessionclient.SessionSummary{
		{ChatID: "newest01", Updated: time.Now(), Conv: []sessionclientMessage{{Role: "user", Content: strPtr("newest task")}}},
		{ChatID: "middle02", Updated: time.Now().Add(-time.Hour), Conv: []sessionclientMessage{{Role: "user", Content: strPtr("middle task")}}},
		{ChatID: "oldest03", Updated: time.Now().Add(-2 * time.Hour), Conv: []sessionclientMessage{{Role: "user", Content: strPtr("oldest task")}}},
	}
}

func newPickerModel() tuiModel {
	f := &fakeFacade{sid: "sess_tui_test", chatID: "chat123"}
	m := tuiModel{
		facade: f,
		ta:     newTA(""),
		width:  80, height: 24, ready: true,
	}
	m = m.openResumePicker(testSessions(), sessionclient.SessionScope{Workspace: "/work"}, 0)
	return m
}

func TestOpenResumePicker_ActivatesAndClosesCompletion(t *testing.T) {
	m := tuiModel{facade: &fakeFacade{sid: "sess_tui_test", chatID: "chat123"}, ta: newTA("")}
	m.comp = completionState{active: true}
	m = m.openResumePicker(testSessions(), sessionclient.SessionScope{}, 0)
	if !m.resumePicker.active {
		t.Fatal("expected picker to be active")
	}
	if m.comp.active {
		t.Fatal("opening the resume picker should close the completion picker")
	}
	if len(m.resumePicker.sessions) != 3 {
		t.Fatalf("expected 3 sessions, got %d", len(m.resumePicker.sessions))
	}
	if m.resumePicker.sel != 0 {
		t.Fatalf("initial selection should be 0 (newest); got %d", m.resumePicker.sel)
	}
}

func TestResumePickerNavigation(t *testing.T) {
	m := newPickerModel()

	m, _, consumed := m.handleResumePickerKey(tea.KeyMsg{Type: tea.KeyDown})
	if !consumed {
		t.Fatal("down should be consumed")
	}
	if m.resumePicker.sel != 1 {
		t.Fatalf("sel after down = %d, want 1", m.resumePicker.sel)
	}

	m, _, _ = m.handleResumePickerKey(tea.KeyMsg{Type: tea.KeyUp})
	if m.resumePicker.sel != 0 {
		t.Fatalf("sel after up = %d, want 0", m.resumePicker.sel)
	}

	// Wraps at the top going up.
	m, _, _ = m.handleResumePickerKey(tea.KeyMsg{Type: tea.KeyUp})
	if m.resumePicker.sel != 2 {
		t.Fatalf("sel should wrap to last index (2); got %d", m.resumePicker.sel)
	}
}

func TestResumePickerEsc_ClosesWithoutMutating(t *testing.T) {
	m := newPickerModel()
	origSession := m.sessionID

	m, cmd, consumed := m.handleResumePickerKey(tea.KeyMsg{Type: tea.KeyEsc})
	if !consumed {
		t.Fatal("esc should be consumed")
	}
	if m.resumePicker.active {
		t.Fatal("esc should close the picker")
	}
	if cmd != nil {
		t.Fatal("esc should not produce a resume command")
	}
	if m.sessionID != origSession {
		t.Fatal("esc must not mutate conversation state")
	}
}

func TestResumePickerEnter_ResumesSelected(t *testing.T) {
	m := newPickerModel()
	// Select the middle session.
	m, _, _ = m.handleResumePickerKey(tea.KeyMsg{Type: tea.KeyDown})
	if m.resumePicker.sel != 1 {
		t.Fatalf("setup: sel = %d, want 1", m.resumePicker.sel)
	}

	m, cmd, consumed := m.handleResumePickerKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !consumed {
		t.Fatal("enter should be consumed")
	}
	if m.resumePicker.active {
		t.Fatal("enter should close the picker")
	}
	if cmd == nil {
		t.Fatal("enter should produce a rotation command")
	}
	_ = m // picker model lacks ready/width — skip render-dependent asserts
	// The rotation Cmd needs a manager; with none bound it returns a failed
	// rotationMsg — assert the message type (the wired path is covered by
	// applyRotation tests). beginRotate batches the rotation Cmd with the
	// dot-tick arm command, so unpack tea.BatchMsg.
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		if len(batch) == 0 {
			t.Fatal("empty batch from resume")
		}
		var found bool
		for _, c := range batch {
			if r, ok := c().(rotationMsg); ok {
				_ = r
				found = true
			}
		}
		if !found {
			t.Fatalf("expected rotationMsg inside batch, got %T", msg)
		}
		return
	}
	if _, ok := msg.(rotationMsg); !ok {
		t.Fatalf("expected rotationMsg from resume, got %T", msg)
	}
}

func TestResumePickerCtrlC_NotConsumed(t *testing.T) {
	m := newPickerModel()
	_, _, consumed := m.handleResumePickerKey(tea.KeyMsg{Type: tea.KeyCtrlC})
	if consumed {
		t.Fatal("ctrl+c must not be consumed by the picker — quitting must still work")
	}
}

func TestResumePickerEmptyState(t *testing.T) {
	m := tuiModel{facade: &fakeFacade{sid: "sess_tui_test", chatID: "chat123"}, ta: newTA(""), width: 80, height: 24, ready: true}
	m = m.openResumePicker(nil, sessionclient.SessionScope{}, 2)
	out := plain(m.renderResumePicker())
	if !strings.Contains(out, "no sessions") {
		t.Fatalf("expected 'no sessions' hint; got %q", out)
	}
}

// ---- Block 2: search, scope toggle, refresh, modal ----

func summ(id, title, ws string, age time.Duration) sessionclient.SessionSummary {
	return sessionclient.SessionSummary{ChatID: id, Title: title, Workspace: ws, Updated: time.Now().Add(-age), TurnCount: 3}
}

func typeRunes(m tuiModel, s string) tuiModel {
	for _, r := range s {
		m, _, _ = m.handleResumePickerKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return m
}

// scopeFacade answers ListSessions per scope, recording every call.
type scopeFacade struct {
	*fakeFacade
	byScope map[bool][]sessionclient.SessionSummary
	calls   []bool // reconcile flag per call
}

func (f *scopeFacade) ListSessions(scope sessionclient.SessionScope, reconcile bool) ([]sessionclient.SessionSummary, int, error) {
	f.calls = append(f.calls, reconcile)
	return f.byScope[scope.All], 0, nil
}

func pickerWith(sessions []sessionclient.SessionSummary) tuiModel {
	m := tuiModel{facade: &fakeFacade{sid: "s", chatID: "c"}, ta: newTA(""), width: 100, height: 30, ready: true}
	return m.openResumePicker(sessions, sessionclient.SessionScope{Workspace: "/work"}, 0)
}

func TestResumePickerSearchFilters(t *testing.T) {
	m := pickerWith([]sessionclient.SessionSummary{
		summ("id-aaaa1", "Fix the Login bug", "/work/app", 0),
		summ("id-bbbb2", "add telemetry", "/work/api", time.Hour),
		summ("id-cccc3", "refactor", "/other/LOGIN-service", 2*time.Hour),
	})
	m = typeRunes(m, "login")
	if got := len(m.resumePicker.sessions); got != 2 {
		t.Fatalf("query 'login' (title and workspace, case-insensitive) matched %d, want 2", got)
	}
	m = typeRunes(m, "zzz")
	if len(m.resumePicker.sessions) != 0 {
		t.Fatal("no-match query should empty the list")
	}
	if out := plain(m.renderResumePicker()); !strings.Contains(out, "no match") {
		t.Errorf("empty filter should say 'no match': %q", out)
	}
	// Backspace widens again; ctrl+u clears.
	for i := 0; i < 3; i++ {
		m, _, _ = m.handleResumePickerKey(tea.KeyMsg{Type: tea.KeyBackspace})
	}
	if len(m.resumePicker.sessions) != 2 {
		t.Fatalf("after backspace: %d, want 2", len(m.resumePicker.sessions))
	}
	m, _, _ = m.handleResumePickerKey(tea.KeyMsg{Type: tea.KeyCtrlU})
	if m.resumePicker.query != "" || len(m.resumePicker.sessions) != 3 {
		t.Fatalf("ctrl+u: query %q, %d sessions", m.resumePicker.query, len(m.resumePicker.sessions))
	}
	// j and k are search characters now, not navigation.
	m = typeRunes(m, "k")
	if m.resumePicker.query != "k" {
		t.Errorf("k should type into the query, got %q", m.resumePicker.query)
	}
	// Label matches too, and a space is a space.
	m = pickerWith([]sessionclient.SessionSummary{{ChatID: "id-1", Label: "release prep", Title: "x"}})
	m = typeRunes(m, "release")
	m, _, _ = m.handleResumePickerKey(tea.KeyMsg{Type: tea.KeySpace})
	m = typeRunes(m, "prep")
	if len(m.resumePicker.sessions) != 1 {
		t.Errorf("label search with a space: %d matches, want 1", len(m.resumePicker.sessions))
	}
}

func TestResumePickerEnterResumesFilteredSelection(t *testing.T) {
	m := pickerWith([]sessionclient.SessionSummary{summ("id-aaaa1", "alpha", "/w", 0), summ("id-bbbb2", "beta", "/w", time.Hour)})
	m = typeRunes(m, "beta")
	rec := &recordingManager{}
	m.manager = rec
	m, cmd, _ := m.handleResumePickerKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.resumePicker.active || cmd == nil {
		t.Fatalf("enter: active=%v cmd=%v", m.resumePicker.active, cmd != nil)
	}
	// Run the rotation Cmd(s); the manager records which session was resumed.
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			if c != nil {
				c()
			}
		}
	}
	if rec.resumed != "id-bbbb2" {
		t.Errorf("resumed %q, want the filtered match id-bbbb2", rec.resumed)
	}
}

// recordingManager is a ConversationManager that records ResumeConversation.
type recordingManager struct {
	sessionclient.ConversationManager
	resumed string
}

func (r *recordingManager) ResumeConversation(_ context.Context, _ core.Principal, id string) (sessionclient.Facade, error) {
	r.resumed = id
	return nil, errors.New("recorded")
}

func TestResumePickerScopeToggle(t *testing.T) {
	ff := &scopeFacade{fakeFacade: &fakeFacade{sid: "s", chatID: "c"}, byScope: map[bool][]sessionclient.SessionSummary{
		false: {summ("id-aaaa1", "here", "/work", 0)},
		true:  {summ("id-aaaa1", "here", "/work", 0), summ("id-zzzz9", "elsewhere", "/other", time.Hour)},
	}}
	m := tuiModel{facade: ff, ta: newTA(""), width: 100, height: 30, ready: true}
	m = m.openResumePicker(ff.byScope[false], sessionclient.SessionScope{Workspace: "/work"}, 1)

	// "a" with an empty query toggles scope: index read at once, reconcile Cmd.
	m, cmd, consumed := m.handleResumePickerKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	if !consumed || cmd == nil {
		t.Fatalf("'a' on an empty query: consumed=%v cmd=%v", consumed, cmd != nil)
	}
	if !m.resumePicker.scope.All || len(m.resumePicker.sessions) != 2 || !m.resumePicker.loading {
		t.Fatalf("after toggle: all=%v n=%d loading=%v", m.resumePicker.scope.All, len(m.resumePicker.sessions), m.resumePicker.loading)
	}
	if len(ff.calls) != 1 || ff.calls[0] != false {
		t.Fatalf("toggle must read the index without reconciling inline: calls %v", ff.calls)
	}
	// The Cmd reconciles (reconcile=true) and returns a refresh for this gen.
	msg, ok := cmd().(resumePickerRefreshMsg)
	if !ok || msg.gen != m.resumePicker.gen || len(ff.calls) != 2 || ff.calls[1] != true {
		t.Fatalf("reconcile cmd: ok=%v gen %d/%d calls %v", ok, msg.gen, m.resumePicker.gen, ff.calls)
	}
	// Tab always toggles, even with a query; "a" is a search char once typed.
	m = typeRunes(m, "x")
	m, _, _ = m.handleResumePickerKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	if m.resumePicker.query != "xa" {
		t.Errorf("'a' after typing should extend the query, got %q", m.resumePicker.query)
	}
	m, cmd, _ = m.handleResumePickerKey(tea.KeyMsg{Type: tea.KeyTab})
	if m.resumePicker.scope.All || cmd == nil {
		t.Errorf("tab should toggle back to the workspace scope: all=%v cmd=%v", m.resumePicker.scope.All, cmd != nil)
	}
}

func TestResumePickerRefreshKeepsSelectionAndDropsStale(t *testing.T) {
	m := pickerWith([]sessionclient.SessionSummary{summ("id-aaaa1", "a", "/w", 0), summ("id-bbbb2", "b", "/w", time.Hour)})
	m.resumePicker.loading = true
	m, _, _ = m.handleResumePickerKey(tea.KeyMsg{Type: tea.KeyDown}) // select id-bbbb2
	gen := m.resumePicker.gen

	// A refresh with a NEW session at the top keeps the selection on id-bbbb2.
	fresh := []sessionclient.SessionSummary{summ("id-newer0", "new", "/w", 0), summ("id-aaaa1", "a", "/w", time.Minute), summ("id-bbbb2", "b", "/w", time.Hour)}
	m = m.applyResumeRefresh(resumePickerRefreshMsg{gen: gen, sessions: fresh})
	if got := m.resumePicker.sessions[m.resumePicker.sel].ChatID; got != "id-bbbb2" || len(m.resumePicker.sessions) != 3 || m.resumePicker.loading {
		t.Fatalf("refresh: selected %q, n=%d, loading=%v", got, len(m.resumePicker.sessions), m.resumePicker.loading)
	}
	// A refresh for an older gen (scope toggled meanwhile) is dropped.
	before := len(m.resumePicker.all)
	m = m.applyResumeRefresh(resumePickerRefreshMsg{gen: gen - 1, sessions: nil})
	if len(m.resumePicker.all) != before {
		t.Error("stale refresh must be dropped")
	}
	// A refresh that arrives after the picker closed is dropped and does not
	// reopen it.
	m = m.closeResumePicker()
	m = m.applyResumeRefresh(resumePickerRefreshMsg{gen: gen, sessions: fresh})
	if m.resumePicker.active {
		t.Error("refresh must not reopen a closed picker")
	}
	// ... nor one that targets a reopened picker's previous life.
	m = m.openResumePicker(fresh, sessionclient.SessionScope{}, 0)
	m = m.applyResumeRefresh(resumePickerRefreshMsg{gen: gen, sessions: nil})
	if len(m.resumePicker.all) != 3 {
		t.Error("a refresh from an earlier picker must not touch a reopened one")
	}
	// An error keeps the index list.
	m = m.applyResumeRefresh(resumePickerRefreshMsg{gen: m.resumePicker.gen, err: errors.New("boom")})
	if len(m.resumePicker.all) != 3 {
		t.Error("a failed reconcile must keep the list on screen")
	}
}

// The picker is a modal over the conversation pane: no layout rows, and it
// renders inside narrow terminals.
func TestResumePickerModalTakesNoLayoutRows(t *testing.T) {
	m := pickerWith(testSessions())
	if h := m.completionHeight(); h != 0 {
		t.Fatalf("completionHeight = %d", h)
	}
	_, vpH, inputH := m.sizesWithStatusHeight(0)
	open := vpH + inputH
	m = m.closeResumePicker()
	_, vpH2, inputH2 := m.sizesWithStatusHeight(0)
	if open != vpH2+inputH2 || vpH != vpH2 {
		t.Errorf("layout changed with the picker open: %d/%d vs %d/%d", vpH, inputH, vpH2, inputH2)
	}
	for _, size := range [][2]int{{40, 12}, {30, 10}, {120, 40}} {
		m := pickerWith(testSessions())
		m.width, m.height = size[0], size[1]
		out := plain(m.renderResumePicker())
		w := 0
		for _, ln := range strings.Split(out, "\n") {
			if n := lipgloss.Width(ln); n > w {
				w = n
			}
		}
		if w > size[0] {
			t.Errorf("%dx%d: modal is %d columns wide", size[0], size[1], w)
		}
		if !strings.Contains(out, "resume") || !strings.Contains(out, "newest01") {
			t.Errorf("%dx%d: modal missing header or rows:\n%s", size[0], size[1], out)
		}
	}
}

func TestResumePickerHeaderAndLoadingHint(t *testing.T) {
	m := pickerWith(testSessions())
	m.resumePicker.loading = true
	m.resumePicker.hidden = 4
	out := plain(m.renderResumePicker())
	for _, want := range []string{"this workspace", "(4 hidden)", "indexing…", "tab scope", "esc close"} {
		if !strings.Contains(out, want) {
			t.Errorf("modal missing %q:\n%s", want, out)
		}
	}
}

// ctrl+p / ctrl+n move like up/down.
func TestResumePickerCtrlNavigation(t *testing.T) {
	m := newPickerModel()
	m, _, _ = m.handleResumePickerKey(tea.KeyMsg{Type: tea.KeyCtrlN})
	m, _, _ = m.handleResumePickerKey(tea.KeyMsg{Type: tea.KeyCtrlN})
	if m.resumePicker.sel != 2 {
		t.Fatalf("ctrl+n twice: sel = %d, want 2", m.resumePicker.sel)
	}
	m, _, _ = m.handleResumePickerKey(tea.KeyMsg{Type: tea.KeyCtrlP})
	if m.resumePicker.sel != 1 {
		t.Fatalf("ctrl+p: sel = %d, want 1", m.resumePicker.sel)
	}
}

// The list rows show the typed title, falling back to the first message.
func TestResumePickerRowTitle(t *testing.T) {
	m := pickerWith([]sessionclient.SessionSummary{
		{ChatID: "id-title1", Title: "typed title", FirstMessage: "ignored", TurnCount: 2, Updated: time.Now()},
		{ChatID: "id-fallb2", FirstMessage: "fallback first line\nsecond", TurnCount: 1, Updated: time.Now()},
	})
	out := plain(m.renderResumePicker())
	if !strings.Contains(out, "typed title") || strings.Contains(out, "ignored") {
		t.Errorf("typed title row wrong:\n%s", out)
	}
	if !strings.Contains(out, "fallback first line") || strings.Contains(out, "second") {
		t.Errorf("fallback row wrong:\n%s", out)
	}
}
