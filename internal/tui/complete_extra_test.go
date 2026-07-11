package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	agent "github.com/treeol/wakil/internal/agent"

	"github.com/treeol/wakil/internal/config"

	tea "github.com/charmbracelet/bubbletea"
)

func TestListCandidatesRankingAndFilter(t *testing.T) {
	dir := t.TempDir()
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.Mkdir(filepath.Join(dir, "alpha"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "apple.go"), nil, 0o644))
	must(os.WriteFile(filepath.Join(dir, "banana.go"), nil, 0o644))
	must(os.WriteFile(filepath.Join(dir, ".hidden"), nil, 0o644))

	// Empty leaf: all non-dotfiles, directories first.
	all := listCandidates(dir, "")
	if len(all) != 3 {
		t.Fatalf("want 3 candidates (dotfile hidden), got %d: %+v", len(all), all)
	}
	if !all[0].isDir || all[0].name != "alpha" {
		t.Errorf("directory should sort first; got %+v", all[0])
	}

	// Leaf filters case-insensitively by substring.
	ap := listCandidates(dir, "ap")
	if len(ap) != 1 || ap[0].name != "apple.go" {
		t.Errorf("leaf 'ap' should match only apple.go; got %+v", ap)
	}

	// A dot-leaf reveals dotfiles.
	dots := listCandidates(dir, ".")
	found := false
	for _, c := range dots {
		if c.name == ".hidden" {
			found = true
		}
	}
	if !found {
		t.Error("dot-leaf should reveal dotfiles")
	}
}

func compModel(t *testing.T, cands []candidate) tuiModel {
	t.Helper()
	app := &agent.App{Cfg: config.DefaultConfig(), Client: newTestClient(""), Exec: newFakeExecutor()}
	m := NewTUIModel(app)
	m.comp = completionState{active: true, cands: cands}
	return m
}

func TestHandleCompletionKeyNavigation(t *testing.T) {
	cands := []candidate{{name: "a"}, {name: "b"}, {name: "c"}}
	m := compModel(t, cands)

	// down wraps forward.
	m, used := m.handleCompletionKey(tea.KeyMsg{Type: tea.KeyDown})
	if !used || m.comp.sel != 1 {
		t.Fatalf("down should select index 1; got sel=%d used=%v", m.comp.sel, used)
	}
	// up from 0 wraps to the last.
	m.comp.sel = 0
	m, _ = m.handleCompletionKey(tea.KeyMsg{Type: tea.KeyUp})
	if m.comp.sel != len(cands)-1 {
		t.Errorf("up from 0 should wrap to last; got %d", m.comp.sel)
	}
	// esc dismisses.
	m, used = m.handleCompletionKey(tea.KeyMsg{Type: tea.KeyEsc})
	if !used || m.comp.active {
		t.Errorf("esc should dismiss the picker")
	}

	// Inactive picker consumes nothing.
	idle := compModel(t, nil)
	idle.comp.active = false
	if _, used := idle.handleCompletionKey(tea.KeyMsg{Type: tea.KeyDown}); used {
		t.Error("inactive picker must not consume keys")
	}
}

func TestAcceptCompletionFileClosesPicker(t *testing.T) {
	m := compModel(t, []candidate{{name: "main.go", isDir: false}})
	m.comp.sel = 0
	m.comp.leafLen = 0
	m = m.acceptCompletion()
	if m.comp.active {
		t.Error("accepting a file should close the picker")
	}
	if !strings.Contains(m.ta.Value(), "main.go") {
		t.Errorf("accepted name should be inserted into the textarea; got %q", m.ta.Value())
	}
}

func TestCompletionHeightAndRender(t *testing.T) {
	// No matches → "no matches" + border height.
	empty := compModel(t, nil)
	if h := empty.completionHeight(); h != 3 {
		t.Errorf("empty picker height = %d, want 3", h)
	}
	empty.width = 40
	if !strings.Contains(plain(empty.renderCompletion()), "no matches") {
		t.Error("empty picker should render 'no matches'")
	}

	// A handful of candidates renders each name.
	m := compModel(t, []candidate{{name: "x.go"}, {name: "y", isDir: true}})
	m.width = 40
	out := plain(m.renderCompletion())
	if !strings.Contains(out, "x.go") || !strings.Contains(out, "y/") {
		t.Errorf("render should list names with dir slash; got %q", out)
	}
}

// --- Slash completion: unit-level (computeSlashCompletion) ---

func TestComputeSlashCompletionCommandPicker(t *testing.T) {
	src := compSources{} // no data needed for command picker
	st := computeSlashCompletion(newTA("/"), src)
	if !st.active {
		t.Fatal("picker should activate for /")
	}
	if st.kind != compKindCommand {
		t.Fatalf("kind = %v, want compKindCommand", st.kind)
	}
	// "/" matches all commands; verify a known subset is present.
	names := map[string]bool{}
	for _, c := range st.cands {
		names[c.name] = true
	}
	for _, want := range []string{"/backend", "/new", "/model", "/resume", "/help"} {
		if !names[want] {
			t.Errorf("command picker missing %q; got %v", want, names)
		}
	}
}

func TestComputeSlashCompletionCommandNarrowing(t *testing.T) {
	src := compSources{}
	st := computeSlashCompletion(newTA("/ba"), src)
	if !st.active {
		t.Fatal("picker should activate for /ba")
	}
	// Only /backend starts with /ba.
	if len(st.cands) != 1 || st.cands[0].name != "/backend" {
		t.Errorf("expected only /backend; got %+v", st.cands)
	}
	// leafLen covers "/ba" (3 runes) so accepting replaces the whole prefix.
	if st.leafLen != 3 {
		t.Errorf("leafLen = %d, want 3", st.leafLen)
	}
}

func TestComputeSlashCompletionBackendArg(t *testing.T) {
	src := compSources{backends: []string{"llama", "openrouter", "together"}}
	st := computeSlashCompletion(newTA("/backend op"), src)
	if !st.active {
		t.Fatal("picker should activate after /backend ")
	}
	if len(st.cands) != 1 || st.cands[0].name != "openrouter" {
		t.Errorf("expected openrouter; got %+v", st.cands)
	}
	if st.leafLen != 2 {
		t.Errorf("leafLen = %d (length of 'op'), want 2", st.leafLen)
	}
}

func TestComputeSlashCompletionModelArg(t *testing.T) {
	src := compSources{models: []string{"claude-sonnet-4-6", "claude-opus-4-8", "gpt-4o"}}
	st := computeSlashCompletion(newTA("/model claude"), src)
	if !st.active {
		t.Fatal("picker should activate after /model ")
	}
	// Both claude-* models match "claude" (substring).
	if len(st.cands) != 2 {
		t.Errorf("expected 2 claude-* models; got %+v", st.cands)
	}
}

func TestComputeSlashCompletionResumeArg(t *testing.T) {
	src := compSources{sessions: []string{"abc12345", "abc99999", "def00000"}}
	st := computeSlashCompletion(newTA("/resume abc"), src)
	if !st.active {
		t.Fatal("picker should activate after /resume ")
	}
	if len(st.cands) != 2 {
		t.Errorf("expected 2 abc* sessions; got %+v", st.cands)
	}
}

func TestComputeSlashCompletionSubagentNarrowing(t *testing.T) {
	src := compSources{}
	st := computeSlashCompletion(newTA("/su"), src)
	if !st.active {
		t.Fatal("picker should activate for /su")
	}
	// Both /subagent and /submodel start with /su.
	names := map[string]bool{}
	for _, c := range st.cands {
		names[c.name] = true
	}
	if !names["/subagent"] || !names["/submodel"] {
		t.Errorf("expected /subagent and /submodel; got %v", names)
	}
	// /sub narrows further to only /subagent.
	st = computeSlashCompletion(newTA("/suba"), src)
	if len(st.cands) != 1 || st.cands[0].name != "/subagent" {
		t.Errorf("expected only /subagent for /suba; got %+v", st.cands)
	}
}

func TestComputeSlashCompletionSubmodelNarrowing(t *testing.T) {
	src := compSources{}
	st := computeSlashCompletion(newTA("/subm"), src)
	if !st.active {
		t.Fatal("picker should activate for /subm")
	}
	if len(st.cands) != 1 || st.cands[0].name != "/submodel" {
		t.Errorf("expected only /submodel; got %+v", st.cands)
	}
}

func TestComputeSlashCompletionSubmodelArg(t *testing.T) {
	src := compSources{models: []string{"claude-sonnet-4-6", "claude-opus-4-8", "qwen3-8b"}}
	st := computeSlashCompletion(newTA("/submodel claude"), src)
	if !st.active {
		t.Fatal("picker should activate after /submodel ")
	}
	// Both claude-* models match "claude" (substring).
	if len(st.cands) != 2 {
		t.Errorf("expected 2 claude-* models; got %+v", st.cands)
	}
}

func TestComputeSlashCompletionSubagentArg(t *testing.T) {
	src := compSources{endpoints: []string{"inherit", "openai-a", "proxy-b"}}
	st := computeSlashCompletion(newTA("/subagent proxy"), src)
	if !st.active {
		t.Fatal("picker should activate after /subagent ")
	}
	if len(st.cands) != 1 || st.cands[0].name != "proxy-b" {
		t.Errorf("expected proxy-b; got %+v", st.cands)
	}
}

func TestComputeSlashCompletionNoArgForOtherCommands(t *testing.T) {
	src := compSources{}
	// /compact takes no argument — picker should be inactive after the space.
	st := computeSlashCompletion(newTA("/compact "), src)
	if st.active {
		t.Error("picker should not activate for commands without argument completion")
	}
}

// --- Slash completion: live Update-loop tests ---

// slashModel builds a sized model with optional backend and model data.
func slashModel(t *testing.T, backends []string, models []string) tuiModel {
	t.Helper()
	app := &agent.App{Cfg: config.DefaultConfig(), Client: newTestClient(""), Exec: newFakeExecutor()}
	for _, name := range backends {
		app.BackendList = append(app.BackendList, agent.BackendInfo{Name: name})
	}
	app.ModelList = models
	m := NewTUIModel(app)
	return step(m, tea.WindowSizeMsg{Width: 100, Height: 40})
}

// typeString feeds each rune as a separate KeyMsg through the Update loop.
func typeString(m tuiModel, s string) tuiModel {
	for _, r := range s {
		m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return m
}

func TestSlashPickerLiveCommandFires(t *testing.T) {
	m := slashModel(t, nil, nil)
	m = typeString(m, "/")

	if !m.comp.active {
		t.Fatal("picker should be active after typing /")
	}
	if m.comp.kind != compKindCommand {
		t.Fatalf("kind = %v, want compKindCommand", m.comp.kind)
	}
	if len(m.comp.cands) == 0 {
		t.Fatal("command picker should have candidates")
	}
	// Candidates list contains the full command set.
	names := map[string]bool{}
	for _, c := range m.comp.cands {
		names[c.name] = true
	}
	for _, want := range []string{"/backend", "/new", "/model", "/resume", "/help"} {
		if !names[want] {
			t.Errorf("command picker missing %q; got %v", want, names)
		}
	}
	// The rendered picker box must contain the first visible candidates (only the
	// first compMaxVisible=8 are shown; /new is beyond that window).
	rc := plain(m.renderCompletion())
	if !strings.Contains(rc, "/backend") {
		snip := rc
		if len(snip) > 200 {
			snip = snip[:200]
		}
		t.Errorf("renderCompletion missing /backend; got: %q", snip)
	}
	// View includes the picker.
	if !strings.Contains(plain(m.View()), "/backend") {
		t.Errorf("View() missing completion picker content")
	}
}

func TestSlashPickerLiveCommandNarrows(t *testing.T) {
	m := slashModel(t, nil, nil)
	m = typeString(m, "/b")

	if !m.comp.active {
		t.Fatal("picker should be active after /b")
	}
	for _, c := range m.comp.cands {
		if !strings.HasPrefix(c.name, "/b") {
			t.Errorf("unexpected candidate %q when leaf is '/b'", c.name)
		}
	}
	// Rendered view must show the narrowed candidates.
	if !strings.Contains(plain(m.View()), "/backend") {
		t.Errorf("View() missing /backend after narrowing to /b")
	}
}

func TestSlashPickerLiveBackendFires(t *testing.T) {
	m := slashModel(t, []string{"llama", "openrouter"}, nil)
	m = typeString(m, "/backend ")

	if !m.comp.active {
		t.Fatalf("picker should be active after /backend ; got active=%v cands=%d",
			m.comp.active, len(m.comp.cands))
	}
	names := map[string]bool{}
	for _, c := range m.comp.cands {
		names[c.name] = true
	}
	if !names["llama"] || !names["openrouter"] {
		t.Errorf("backend picker missing expected names; got %v", names)
	}
	view := plain(m.View())
	if !strings.Contains(view, "llama") || !strings.Contains(view, "openrouter") {
		t.Errorf("rendered view missing backend names")
	}
}

func TestSlashPickerLiveModelFires(t *testing.T) {
	m := slashModel(t, nil, []string{"claude-sonnet-4-6", "claude-opus-4-8"})
	m = typeString(m, "/model ")

	if !m.comp.active {
		t.Fatalf("picker should be active after /model ; active=%v cands=%d",
			m.comp.active, len(m.comp.cands))
	}
	view := plain(m.View())
	if !strings.Contains(view, "claude-sonnet-4-6") {
		t.Errorf("rendered view missing model name; got snippet: %q",
			view[max(0, len(view)-400):])
	}
}

func TestSlashPickerLiveSubagentFires(t *testing.T) {
	app := &agent.App{Cfg: config.DefaultConfig(), Client: newTestClient(""), Exec: newFakeExecutor()}
	app.Cfg.Endpoints = map[string]config.EndpointConfig{
		"openai-a": {Kind: config.EndpointKindOpenAI, BaseURL: "http://x", Model: "m"},
	}
	m := NewTUIModel(app)
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 40})
	m = typeString(m, "/subagent ")

	if !m.comp.active {
		t.Fatalf("picker should be active after /subagent ; active=%v cands=%d",
			m.comp.active, len(m.comp.cands))
	}
	names := map[string]bool{}
	for _, c := range m.comp.cands {
		names[c.name] = true
	}
	if !names["inherit"] || !names["openai-a"] {
		t.Errorf("subagent arg picker missing expected names; got %v", names)
	}
	view := plain(m.View())
	if !strings.Contains(view, "openai-a") {
		t.Errorf("rendered view missing endpoint name")
	}
}

func TestSlashPickerLiveResumeFires(t *testing.T) {
	m := slashModel(t, nil, nil)
	m = typeString(m, "/resume ")

	// With no sessions on disk the picker shows "no matches" but IS active.
	if !m.comp.active {
		t.Fatal("picker should be active after /resume  (even with no sessions)")
	}
	if m.comp.kind != compKindCommand {
		t.Fatalf("kind = %v, want compKindCommand", m.comp.kind)
	}
}

func TestSlashPickerTabAcceptsCommandAndOpensArgPicker(t *testing.T) {
	m := slashModel(t, []string{"llama", "openrouter"}, nil)

	// Type "/b" so only "/backend" matches, then Tab to accept.
	m = typeString(m, "/b")
	if !m.comp.active {
		t.Fatal("picker should be active after /b")
	}
	m = step(m, tea.KeyMsg{Type: tea.KeyTab})

	// After accepting "/backend", the textarea should contain "/backend "
	// and the backend argument picker should be open.
	if got := m.ta.Value(); got != "/backend " {
		t.Errorf("textarea after Tab = %q, want %q", got, "/backend ")
	}
	if !m.comp.active {
		t.Fatal("backend arg picker should re-open after accepting /backend")
	}
	names := map[string]bool{}
	for _, c := range m.comp.cands {
		names[c.name] = true
	}
	if !names["llama"] || !names["openrouter"] {
		t.Errorf("backend arg picker missing names; got %v", names)
	}
}

func TestSlashPickerEnterExecutesCommandNotAccepts(t *testing.T) {
	m := slashModel(t, nil, nil)
	m = typeString(m, "/new")

	if !m.comp.active {
		t.Skip("picker not active; test not applicable")
	}
	// Enter should execute /new (clear the conversation), not just accept from picker.
	m = step(m, tea.KeyMsg{Type: tea.KeyEnter})

	// The picker should be closed and the textarea cleared (command was sent).
	if m.comp.active {
		t.Error("picker should close after Enter executes the command")
	}
	if m.ta.Value() != "" {
		t.Errorf("textarea should be empty after executing /new; got %q", m.ta.Value())
	}
}

func TestSlashPickerHeightInvariantOnOpen(t *testing.T) {
	m := slashModel(t, nil, nil)
	if total, want := heightInvariant(m); total != want {
		t.Fatalf("pre-slash invariant broken: rendered=%d terminal=%d", total, want)
	}

	m = typeString(m, "/")

	if !m.comp.active {
		t.Fatal("picker should be active")
	}
	if total, want := heightInvariant(m); total != want {
		t.Errorf("picker-open invariant broken: rendered=%d terminal=%d (overflow=%d)",
			total, want, total-want)
	}
}
