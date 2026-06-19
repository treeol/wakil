package tui

import (
	"strings"
	"testing"

	agent "wakil/internal/agent"

	tea "github.com/charmbracelet/bubbletea"
)

// searchModel returns a tuiModel in stateIdle with the given history entries
// loaded (most-recent-first is the internal order; the slice is used as-is).
func searchModel(t *testing.T, history ...string) tuiModel {
	t.Helper()
	m := keyModel(t)
	m.inputHistory = history
	return m
}

func TestSearchHistory(t *testing.T) {
	h := []string{"git commit", "git push", "go build", "go test"}

	tests := []struct {
		name     string
		query    string
		startIdx int
		want     int
	}{
		{"empty query matches all from 0", "", 0, 0},
		{"empty query from 2", "", 2, 2},
		{"case-insensitive match", "GIT", 0, 0},
		{"second match via startIdx", "git", 1, 1},
		{"substring match", "build", 0, 2},
		{"no match", "deploy", 0, -1},
		{"startIdx past end", "git", 4, -1},
		{"startIdx negative clamped to 0", "git", -1, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := searchHistory(h, tc.query, tc.startIdx)
			if got != tc.want {
				t.Errorf("searchHistory(%q, %d) = %d, want %d", tc.query, tc.startIdx, got, tc.want)
			}
		})
	}

	// Empty history.
	if got := searchHistory(nil, "foo", 0); got != -1 {
		t.Errorf("searchHistory(nil, ...) = %d, want -1", got)
	}
}

func TestHandleKeyCtrlR_EntersSearch(t *testing.T) {
	m := searchModel(t, "git commit", "go build")
	m.ta.SetValue("draft text")

	m2, _, consumed := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlR})
	if !consumed {
		t.Fatal("ctrl+r should be consumed")
	}
	if !m2.searchActive {
		t.Error("searchActive should be true after Ctrl+R")
	}
	if m2.searchQuery != "" {
		t.Errorf("searchQuery = %q, want empty", m2.searchQuery)
	}
	if m2.searchSaved != "draft text" {
		t.Errorf("searchSaved = %q, want %q", m2.searchSaved, "draft text")
	}
	if m2.searchIdx != 0 {
		t.Errorf("searchIdx = %d, want 0 (most recent entry)", m2.searchIdx)
	}
	if m2.searchFailed {
		t.Error("searchFailed should be false with a match")
	}
	if m2.ta.Value() != "git commit" {
		t.Errorf("textarea = %q, want %q", m2.ta.Value(), "git commit")
	}
	// Picker should be closed.
	if m2.comp.active {
		t.Error("picker should be closed when entering search")
	}
}

func TestHandleKeyCtrlR_RepeatFindsOlder(t *testing.T) {
	h := []string{"git commit", "git push", "go build"}
	m := searchModel(t, h...)

	m1, _, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlR})
	if m1.searchIdx != 0 || m1.ta.Value() != "git commit" {
		t.Fatalf("first Ctrl+R: idx=%d value=%q, want idx=0 value=%q", m1.searchIdx, m1.ta.Value(), "git commit")
	}

	m2, _, _ := m1.handleKey(tea.KeyMsg{Type: tea.KeyCtrlR})
	if m2.searchIdx != 1 || m2.ta.Value() != "git push" {
		t.Errorf("second Ctrl+R: idx=%d value=%q, want idx=1 value=%q", m2.searchIdx, m2.ta.Value(), "git push")
	}
}

func TestHandleKeyCtrlR_PastTheEnd(t *testing.T) {
	h := []string{"git commit", "git push"}
	m := searchModel(t, h...)

	m1, _, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlR})  // idx=0
	m2, _, _ := m1.handleKey(tea.KeyMsg{Type: tea.KeyCtrlR}) // idx=1
	m3, _, consumed := m2.handleKey(tea.KeyMsg{Type: tea.KeyCtrlR})
	if !consumed {
		t.Fatal("repeat Ctrl+R past end should still be consumed")
	}
	if m3.searchIdx != 1 {
		t.Errorf("searchIdx = %d, want 1 (kept last match)", m3.searchIdx)
	}
	if !m3.searchFailed {
		t.Error("searchFailed should be true when no further match")
	}
	if m3.ta.Value() != "git push" {
		t.Errorf("textarea = %q, want %q (kept last match)", m3.ta.Value(), "git push")
	}
}

func TestHandleKeyCtrlR_EscAborts(t *testing.T) {
	m := searchModel(t, "git commit")
	m.ta.SetValue("original draft")

	m1, _, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlR})
	if !m1.searchActive {
		t.Fatal("should be in search mode")
	}

	m2, _, consumed := m1.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if !consumed {
		t.Fatal("esc during search should be consumed")
	}
	if m2.searchActive {
		t.Error("searchActive should be false after esc")
	}
	if m2.ta.Value() != "original draft" {
		t.Errorf("textarea = %q, want %q (restored draft)", m2.ta.Value(), "original draft")
	}
}

func TestHandleKeyCtrlR_CtrlCAborts(t *testing.T) {
	m := searchModel(t, "git commit")
	m.ta.SetValue("original draft")

	m1, _, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlR})
	if !m1.searchActive {
		t.Fatal("should be in search mode")
	}

	m2, cmds, consumed := m1.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC})
	if !consumed {
		t.Fatal("ctrl+c during search should be consumed")
	}
	if len(cmds) != 0 {
		t.Errorf("ctrl+c during search should not quit; got %d cmds", len(cmds))
	}
	if m2.searchActive {
		t.Error("searchActive should be false after ctrl+c")
	}
	if m2.ta.Value() != "original draft" {
		t.Errorf("textarea = %q, want %q (restored draft)", m2.ta.Value(), "original draft")
	}
}

func TestHandleKeyCtrlR_CtrlGAborts(t *testing.T) {
	m := searchModel(t, "git commit")
	m.ta.SetValue("original draft")

	m1, _, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlR})
	m2, _, consumed := m1.handleKey(tea.KeyMsg{Type: tea.KeyCtrlG})
	if !consumed {
		t.Fatal("ctrl+g during search should be consumed")
	}
	if m2.searchActive {
		t.Error("searchActive should be false after ctrl+g")
	}
	if m2.ta.Value() != "original draft" {
		t.Errorf("textarea = %q, want %q (restored draft)", m2.ta.Value(), "original draft")
	}
}

func TestHandleKeyCtrlR_EnterExecutesMatch(t *testing.T) {
	m := searchModel(t, "git commit", "go build")
	m1, _, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlR})
	if m1.ta.Value() != "git commit" {
		t.Fatalf("expected match in textarea, got %q", m1.ta.Value())
	}

	m2, _, consumed := m1.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !consumed {
		t.Fatal("enter should be consumed")
	}
	if m2.searchActive {
		t.Error("search should be exited after enter")
	}
	// The matched entry should be in history (moved to front).
	if len(m2.inputHistory) == 0 || m2.inputHistory[0] != "git commit" {
		t.Errorf("inputHistory[0] = %q, want %q", safeIdx(m2.inputHistory, 0), "git commit")
	}
}

func TestHandleKeyCtrlR_TypeSearches(t *testing.T) {
	h := []string{"git commit", "git push", "go build"}
	m := searchModel(t, h...)

	m1, _, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlR})
	m2, _, _ := m1.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")})
	if m2.searchQuery != "g" {
		t.Errorf("searchQuery = %q, want %q", m2.searchQuery, "g")
	}
	if m2.searchIdx != 0 {
		t.Errorf("searchIdx = %d, want 0 (first match: git commit)", m2.searchIdx)
	}
	if m2.ta.Value() != "git commit" {
		t.Errorf("textarea = %q, want %q", m2.ta.Value(), "git commit")
	}

	// Type "i" → query "gi" → still matches "git commit" at index 0.
	m3, _, _ := m2.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("i")})
	if m3.searchQuery != "gi" {
		t.Errorf("searchQuery = %q, want %q", m3.searchQuery, "gi")
	}
	if m3.ta.Value() != "git commit" {
		t.Errorf("textarea = %q, want %q", m3.ta.Value(), "git commit")
	}
}

func TestHandleKeyCtrlR_SpaceInQuery(t *testing.T) {
	h := []string{"git push origin", "git commit", "go build"}
	m := searchModel(t, h...)

	m1, _, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlR})
	m2, _, _ := m1.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")})
	m3, _, _ := m2.handleKey(tea.KeyMsg{Type: tea.KeySpace})
	if m3.searchQuery != "g " {
		t.Errorf("searchQuery = %q, want %q", m3.searchQuery, "g ")
	}
	m4, _, _ := m3.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	if m4.searchQuery != "g p" {
		t.Errorf("searchQuery = %q, want %q", m4.searchQuery, "g p")
	}
	// "g p" matches "git push origin" at index 0.
	if m4.searchIdx != 0 || m4.ta.Value() != "git push origin" {
		t.Errorf("match: idx=%d value=%q, want idx=0 value=%q", m4.searchIdx, m4.ta.Value(), "git push origin")
	}
}

func TestHandleKeyCtrlR_AltKeyIgnored(t *testing.T) {
	m := searchModel(t, "git commit")
	m1, _, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlR})

	// Alt+a should not insert 'a' into the query; it exits search and forwards.
	m2, _, consumed := m1.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a"), Alt: true})
	if consumed {
		t.Error("alt+rune should not be consumed (exits search, forwards to textarea)")
	}
	if m2.searchActive {
		t.Error("search should be exited after alt+rune")
	}
	if m2.searchQuery != "" {
		t.Errorf("searchQuery = %q, want empty (alt not inserted)", m2.searchQuery)
	}
}

func TestHandleKeyCtrlR_BackspaceRemovesQuery(t *testing.T) {
	h := []string{"git commit", "go build"}
	m := searchModel(t, h...)

	m1, _, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlR})
	m2, _, _ := m1.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")})
	m3, _, _ := m2.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("i")})
	if m3.searchQuery != "gi" {
		t.Fatalf("setup: searchQuery = %q, want %q", m3.searchQuery, "gi")
	}

	m4, _, _ := m3.handleKey(tea.KeyMsg{Type: tea.KeyBackspace})
	if m4.searchQuery != "g" {
		t.Errorf("searchQuery = %q, want %q", m4.searchQuery, "g")
	}
	// "g" matches both; first match is index 0.
	if m4.searchIdx != 0 {
		t.Errorf("searchIdx = %d, want 0", m4.searchIdx)
	}
}

func TestHandleKeyCtrlR_BackspaceVariant(t *testing.T) {
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyBackspace},
		{Type: tea.KeyCtrlH},
	} {
		t.Run(key.String(), func(t *testing.T) {
			m := searchModel(t, "git commit")
			m1, _, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlR})
			m2, _, _ := m1.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")})
			if m2.searchQuery != "g" {
				t.Fatalf("setup: searchQuery = %q, want %q", m2.searchQuery, "g")
			}
			m3, _, consumed := m2.handleKey(key)
			if !consumed {
				t.Fatal("backspace variant should be consumed during search")
			}
			if m3.searchQuery != "" {
				t.Errorf("searchQuery = %q, want empty", m3.searchQuery)
			}
		})
	}
}

func TestHandleKeyCtrlR_NotDuringStreaming(t *testing.T) {
	m := searchModel(t, "git commit")
	m.state = stateStreaming

	m2, _, consumed := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlR})
	// Ctrl+R during streaming is consumed (swallowed) so it doesn't insert 'r'
	// into the textarea, but search does NOT activate.
	if !consumed {
		t.Fatal("ctrl+r during streaming should be consumed (not insert 'r')")
	}
	if m2.searchActive {
		t.Error("search should not activate during streaming")
	}
}

func TestHandleKeyCtrlR_NotDuringConfirm(t *testing.T) {
	m := searchModel(t, "git commit")
	m.state = stateConfirm
	m.pendConf = &agent.ConfirmReqMsg{
		RespCh: make(chan agent.ConfirmChoice, 1),
	}

	m2, _, consumed := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlR})
	// Confirm gate consumes ALL keys before search logic.
	if !consumed {
		t.Fatal("confirm gate should consume ctrl+r")
	}
	if m2.searchActive {
		t.Error("search should not activate during confirm gate")
	}
}

func TestHandleKeyCtrlR_PickerOpenCloses(t *testing.T) {
	m := searchModel(t, "git commit")
	m.comp = completionState{active: true}

	m2, _, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlR})
	if m2.comp.active {
		t.Error("picker should be closed when entering search")
	}
	if !m2.searchActive {
		t.Error("search should be active")
	}
}

func TestHandleKeyCtrlR_ArrowsExitSearch(t *testing.T) {
	h := []string{"git commit", "git push", "go build"}
	m := searchModel(t, h...)

	// Enter search, type "git" → matches index 0 (git commit).
	m1, _, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlR})
	m2, _, _ := m1.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")})
	m3, _, _ := m2.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("i")})
	m4, _, _ := m3.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	if m4.searchIdx != 0 {
		t.Fatalf("setup: searchIdx = %d, want 0", m4.searchIdx)
	}

	// UP exits search, reconciles histIdx to searchIdx, then increments.
	m5, _, consumed := m4.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	if !consumed {
		t.Fatal("up after search should be consumed")
	}
	if m5.searchActive {
		t.Error("search should be exited after up")
	}
	// histIdx was set to searchIdx (0), then UP incremented to 1.
	if m5.histIdx != 1 {
		t.Errorf("histIdx = %d, want 1 (reconciled to searchIdx then incremented)", m5.histIdx)
	}
	if m5.ta.Value() != "git push" {
		t.Errorf("textarea = %q, want %q", m5.ta.Value(), "git push")
	}
}

func TestHandleKeyCtrlR_EmptyQueryRepeat(t *testing.T) {
	h := []string{"git commit", "git push", "go build"}
	m := searchModel(t, h...)

	m1, _, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlR}) // idx=0
	if m1.ta.Value() != "git commit" {
		t.Fatalf("first Ctrl+R: %q, want %q", m1.ta.Value(), "git commit")
	}

	m2, _, _ := m1.handleKey(tea.KeyMsg{Type: tea.KeyCtrlR}) // idx=1
	if m2.searchIdx != 1 || m2.ta.Value() != "git push" {
		t.Errorf("second Ctrl+R: idx=%d value=%q, want idx=1 value=%q", m2.searchIdx, m2.ta.Value(), "git push")
	}

	m3, _, _ := m2.handleKey(tea.KeyMsg{Type: tea.KeyCtrlR}) // idx=2
	if m3.searchIdx != 2 || m3.ta.Value() != "go build" {
		t.Errorf("third Ctrl+R: idx=%d value=%q, want idx=2 value=%q", m3.searchIdx, m3.ta.Value(), "go build")
	}
}

func TestHandleKeyCtrlR_EmptyHistory(t *testing.T) {
	m := searchModel(t) // no history
	m.ta.SetValue("draft")

	m2, _, consumed := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlR})
	if !consumed {
		t.Fatal("ctrl+r with empty history should be consumed")
	}
	if !m2.searchActive {
		t.Error("search should be active even with empty history")
	}
	if !m2.searchFailed {
		t.Error("searchFailed should be true with empty history")
	}
	if m2.searchIdx != -1 {
		t.Errorf("searchIdx = %d, want -1", m2.searchIdx)
	}

	// Status line should show failed prompt.
	prompt := m2.searchPrompt()
	if !strings.Contains(prompt, "failed reverse-i-search") {
		t.Errorf("searchPrompt = %q, should contain 'failed reverse-i-search'", prompt)
	}
}

func TestHandleKeyCtrlR_NoMatchSetsFailed(t *testing.T) {
	h := []string{"git commit", "go build"}
	m := searchModel(t, h...)

	m1, _, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlR})
	m2, _, _ := m1.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("z")})
	if !m2.searchFailed {
		t.Error("searchFailed should be true when query has no match")
	}
	if m2.searchIdx != 0 {
		t.Errorf("searchIdx = %d, want 0 (kept previous match)", m2.searchIdx)
	}
	// Previous match should remain in textarea.
	if m2.ta.Value() != "git commit" {
		t.Errorf("textarea = %q, want %q (kept previous match)", m2.ta.Value(), "git commit")
	}

	// Status prompt should show "failed".
	prompt := m2.searchPrompt()
	if !strings.Contains(prompt, "failed reverse-i-search") {
		t.Errorf("searchPrompt = %q, should contain 'failed'", prompt)
	}
}

func TestHandleKeyCtrlR_SearchPromptShows(t *testing.T) {
	m := searchModel(t, "git commit")
	m1, _, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlR})

	prompt := m1.searchPrompt()
	if !strings.Contains(prompt, "reverse-i-search") {
		t.Errorf("searchPrompt = %q, should contain 'reverse-i-search'", prompt)
	}
	if !strings.Contains(prompt, "git commit") {
		t.Errorf("searchPrompt = %q, should contain the matched entry", prompt)
	}

	// Full status line should contain the prompt.
	status := m1.statusLine()
	if !strings.Contains(status, "reverse-i-search") {
		t.Errorf("statusLine = %q, should contain search prompt", status)
	}
}

func TestHandleKeyCtrlR_SearchPromptTruncation(t *testing.T) {
	m := keyModel(t) // width=100
	longEntry := strings.Repeat("a", 200)
	m.inputHistory = []string{longEntry}

	m1, _, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlR})
	prompt := m1.searchPrompt()
	// The prompt should be truncated — definitely shorter than 200 chars.
	if len(prompt) >= 200 {
		t.Errorf("prompt should be truncated; len=%d", len(prompt))
	}
	if !strings.Contains(prompt, "…") {
		t.Errorf("prompt = %q, should contain ellipsis", prompt)
	}
}

func TestTruncateForDisplay(t *testing.T) {
	tests := []struct {
		s        string
		maxRunes int
		want     string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello", 3, "he…"},
		{"hello", 1, "…"},
		{"hello", 0, ""},
		{"", 5, ""},
	}
	for _, tc := range tests {
		got := truncateForDisplay(tc.s, tc.maxRunes)
		if got != tc.want {
			t.Errorf("truncateForDisplay(%q, %d) = %q, want %q", tc.s, tc.maxRunes, got, tc.want)
		}
	}

	// Multi-byte safety.
	got := truncateForDisplay("héllo", 3)
	if got != "hé…" {
		t.Errorf("truncateForDisplay multi-byte = %q, want %q", got, "hé…")
	}
}

// safeIdx returns the element at i, or "<oob>" if i is out of range.
func safeIdx(s []string, i int) string {
	if i < 0 || i >= len(s) {
		return "<oob>"
	}
	return s[i]
}