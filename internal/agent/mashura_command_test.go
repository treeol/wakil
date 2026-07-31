package agent

import (
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
)

// mashuraCmdApp builds an App with a real workspace (temp dir) so that
// saveRepoState → updateRepoState does NOT no-op — the persistence closures
// in mashura_command.go only execute when SessionWorkspace() returns non-empty.
func mashuraCmdApp(t *testing.T) *App {
	t.Helper()
	withRepoStateDir(t)
	ws := t.TempDir()
	return &App{
		Cfg: config.Config{
			ExecMode:             "direct",
			WorkDir:              ws,
			OracleModel:          "test-model",
			OracleMaxTokens:      4096,
			OracleTimeoutSeconds: 300,
		},
	}
}

// cmdNote executes a Cmd and extracts the text from the returned SysNoteMsg.
func cmdNote(t *testing.T, cmd Cmd) string {
	t.Helper()
	if cmd == nil {
		t.Fatal("cmd is nil")
	}
	msg := cmd()
	sn, ok := msg.(SysNoteMsg)
	if !ok {
		t.Fatalf("expected SysNoteMsg, got %T", msg)
	}
	return sn.Text
}

// --- /mashura (status) ---

func TestMashuraCmdStatusEmpty(t *testing.T) {
	app := mashuraCmdApp(t)
	handled, quit, cmd := handleMashuraCommand([]string{"/mashura"}, app)
	if !handled || quit {
		t.Fatalf("handled=%v quit=%v, want true/false", handled, quit)
	}
	text := cmdNote(t, cmd)
	if !strings.Contains(text, "Mashūra Counsel Status") {
		t.Errorf("status missing header: %q", text)
	}
	if !strings.Contains(text, "(none") {
		t.Errorf("status should show no panels: %q", text)
	}
}

func TestMashuraCmdStatusWithPanels(t *testing.T) {
	app := mashuraCmdApp(t)
	app.Cfg.MashuraPanels = map[string]config.MashuraPanelConfig{
		"alpha": {Models: []string{"anthropic:claude-opus-4-8", "openrouter:gpt-5.6"}, Mode: "panel"},
		"beta":  {Models: []string{"anthropic:claude-fable-5"}, Mode: "fallback"},
	}
	app.Cfg.MashuraToolPanels = map[string]string{"review": "alpha"}

	handled, _, cmd := handleMashuraCommand([]string{"/mashura"}, app)
	if !handled {
		t.Fatal("not handled")
	}
	text := cmdNote(t, cmd)
	// Panels are sorted alphabetically — alpha before beta.
	alphaIdx := strings.Index(text, "alpha")
	betaIdx := strings.Index(text, "beta")
	if alphaIdx < 0 || betaIdx < 0 {
		t.Fatalf("status missing panel names: %q", text)
	}
	if alphaIdx > betaIdx {
		t.Errorf("panels not sorted: alpha at %d, beta at %d", alphaIdx, betaIdx)
	}
	// Blank mode defaults to "panel" — beta has explicit "fallback".
	if !strings.Contains(text, "fallback") {
		t.Errorf("status missing panel mode: %q", text)
	}
	// Tool mapping shown — check for the arrow form, not just the panel name.
	if !strings.Contains(text, "review") || !strings.Contains(text, "→") {
		t.Errorf("status missing tool mapping arrow: %q", text)
	}
}

// --- /mashura (bare, via HandleTUICommand routing) ---

func TestMashuraCmdRoutedViaHandleTUICommand(t *testing.T) {
	app := mashuraCmdApp(t)
	handled, quit, cmd := HandleTUICommand("/mashura", app)
	if !handled || quit {
		t.Fatalf("handled=%v quit=%v, want true/false", handled, quit)
	}
	text := cmdNote(t, cmd)
	if !strings.Contains(text, "Mashūra Counsel Status") {
		t.Errorf("routed command missing status header: %q", text)
	}
}

// mustLoadRepoState loads repo-state for ws, failing the test on error.
func mustLoadRepoState(t *testing.T, ws string) *RepoState {
	t.Helper()
	st, err := LoadRepoState(ws)
	if err != nil {
		t.Fatalf("LoadRepoState(%q): %v", ws, err)
	}
	return st
}

// --- /mashura panel add ---

func TestMashuraCmdPanelAddDefaultMode(t *testing.T) {
	app := mashuraCmdApp(t)
	handled, _, cmd := handleMashuraCommand(
		[]string{"/mashura", "panel", "add", "mypanel", "anthropic:claude-opus-4-8,openrouter:gpt-5.6"},
		app)
	if !handled {
		t.Fatal("not handled")
	}
	text := cmdNote(t, cmd)
	if !strings.Contains(text, "mypanel") || !strings.Contains(text, "panel") {
		t.Errorf("add response missing name/mode: %q", text)
	}
	// In-memory config updated.
	p, ok := app.Cfg.MashuraPanels["mypanel"]
	if !ok {
		t.Fatal("panel not in Cfg.MashuraPanels")
	}
	if len(p.Models) != 2 {
		t.Errorf("models count = %d, want 2", len(p.Models))
	}
	if p.Mode != "panel" {
		t.Errorf("mode = %q, want panel", p.Mode)
	}
	// Persisted to repo-state.
	st := mustLoadRepoState(t, app.SessionWorkspace())
	if _, ok := st.MashuraPanels["mypanel"]; !ok {
		t.Error("panel not persisted in repo-state")
	}
}

func TestMashuraCmdPanelAddExplicitMode(t *testing.T) {
	app := mashuraCmdApp(t)
	handled, _, cmd := handleMashuraCommand(
		[]string{"/mashura", "panel", "add", "dbpanel", "anthropic:claude-opus-4-8", "--mode", "debate"},
		app)
	if !handled {
		t.Fatal("not handled")
	}
	text := cmdNote(t, cmd)
	if !strings.Contains(text, "debate") {
		t.Errorf("add response missing debate mode: %q", text)
	}
	if p := app.Cfg.MashuraPanels["dbpanel"]; p.Mode != "debate" {
		t.Errorf("mode = %q, want debate", p.Mode)
	}
}

func TestMashuraCmdPanelAddInvalidMode(t *testing.T) {
	app := mashuraCmdApp(t)
	handled, _, cmd := handleMashuraCommand(
		[]string{"/mashura", "panel", "add", "bad", "anthropic:m", "--mode", "bogus"},
		app)
	if !handled {
		t.Fatal("not handled")
	}
	text := cmdNote(t, cmd)
	if !strings.Contains(text, "invalid mode") {
		t.Errorf("expected invalid mode error: %q", text)
	}
	if _, ok := app.Cfg.MashuraPanels["bad"]; ok {
		t.Error("panel should not be created with invalid mode")
	}
}

func TestMashuraCmdPanelAddMissingArgs(t *testing.T) {
	app := mashuraCmdApp(t)
	handled, _, cmd := handleMashuraCommand(
		[]string{"/mashura", "panel", "add", "only-name"},
		app)
	if !handled {
		t.Fatal("not handled")
	}
	text := cmdNote(t, cmd)
	if !strings.Contains(text, "usage") {
		t.Errorf("expected usage hint: %q", text)
	}
}

// --- /mashura panel rm ---

func TestMashuraCmdPanelRmExisting(t *testing.T) {
	app := mashuraCmdApp(t)
	// Seed: add a panel and a tool mapping pointing to it.
	app.Cfg.MashuraPanels = map[string]config.MashuraPanelConfig{
		"goner": {Models: []string{"anthropic:m"}, Mode: "panel"},
	}
	app.Cfg.MashuraToolPanels = map[string]string{"review": "goner"}
	// Persist the seeded state.
	app.saveRepoState(func(s *RepoState) {
		s.MashuraPanels = map[string]config.MashuraPanelConfig{
			"goner": {Models: []string{"anthropic:m"}, Mode: "panel"},
		}
		s.MashuraToolPanels = map[string]string{"review": "goner"}
	})

	handled, _, cmd := handleMashuraCommand(
		[]string{"/mashura", "panel", "rm", "goner"}, app)
	if !handled {
		t.Fatal("not handled")
	}
	text := cmdNote(t, cmd)
	if !strings.Contains(text, "removed") {
		t.Errorf("expected removal confirmation: %q", text)
	}
	// Removed from in-memory config.
	if _, ok := app.Cfg.MashuraPanels["goner"]; ok {
		t.Error("panel still in Cfg.MashuraPanels")
	}
	// Tool mapping pointing to it also removed.
	if _, ok := app.Cfg.MashuraToolPanels["review"]; ok {
		t.Error("tool mapping pointing to removed panel should be cleaned up")
	}
	// Persisted removal.
	st := mustLoadRepoState(t, app.SessionWorkspace())
	if _, ok := st.MashuraPanels["goner"]; ok {
		t.Error("panel should be removed from persisted state")
	}
	if _, ok := st.MashuraToolPanels["review"]; ok {
		t.Error("tool mapping should be removed from persisted state")
	}
}

func TestMashuraCmdPanelRmMissing(t *testing.T) {
	app := mashuraCmdApp(t)
	handled, _, cmd := handleMashuraCommand(
		[]string{"/mashura", "panel", "rm", "nope"}, app)
	if !handled {
		t.Fatal("not handled")
	}
	text := cmdNote(t, cmd)
	if !strings.Contains(text, "not found") {
		t.Errorf("expected not found: %q", text)
	}
}

func TestMashuraCmdPanelRmMissingArgs(t *testing.T) {
	app := mashuraCmdApp(t)
	handled, _, cmd := handleMashuraCommand(
		[]string{"/mashura", "panel", "rm"}, app)
	if !handled {
		t.Fatal("not handled")
	}
	text := cmdNote(t, cmd)
	if !strings.Contains(text, "usage") {
		t.Errorf("expected usage hint: %q", text)
	}
}

// --- /mashura panel <name> (show details / set mode) ---

func TestMashuraCmdPanelShowDetails(t *testing.T) {
	app := mashuraCmdApp(t)
	app.Cfg.MashuraPanels = map[string]config.MashuraPanelConfig{
		"zoom": {Models: []string{"anthropic:claude-opus-4-8", "openrouter:gpt-5.6"}, Mode: "panel"},
	}
	handled, _, cmd := handleMashuraCommand(
		[]string{"/mashura", "panel", "zoom"}, app)
	if !handled {
		t.Fatal("not handled")
	}
	text := cmdNote(t, cmd)
	if !strings.Contains(text, "zoom") || !strings.Contains(text, "claude-opus-4-8") {
		t.Errorf("panel details missing: %q", text)
	}
}

func TestMashuraCmdPanelShowMissing(t *testing.T) {
	app := mashuraCmdApp(t)
	handled, _, cmd := handleMashuraCommand(
		[]string{"/mashura", "panel", "ghost"}, app)
	if !handled {
		t.Fatal("not handled")
	}
	text := cmdNote(t, cmd)
	if !strings.Contains(text, "not found") {
		t.Errorf("expected not found: %q", text)
	}
}

func TestMashuraCmdPanelSetMode(t *testing.T) {
	app := mashuraCmdApp(t)
	app.Cfg.MashuraPanels = map[string]config.MashuraPanelConfig{
		"chg": {Models: []string{"anthropic:m"}, Mode: "panel"},
	}
	handled, _, cmd := handleMashuraCommand(
		[]string{"/mashura", "panel", "chg", "--mode", "fusion"}, app)
	if !handled {
		t.Fatal("not handled")
	}
	text := cmdNote(t, cmd)
	if !strings.Contains(text, "fusion") {
		t.Errorf("expected mode set confirmation: %q", text)
	}
	if p := app.Cfg.MashuraPanels["chg"]; p.Mode != "fusion" {
		t.Errorf("mode = %q, want fusion", p.Mode)
	}
	// Persisted.
	st := mustLoadRepoState(t, app.SessionWorkspace())
	if p := st.MashuraPanels["chg"]; p.Mode != "fusion" {
		t.Errorf("persisted mode = %q, want fusion", p.Mode)
	}
}

func TestMashuraCmdPanelSetInvalidMode(t *testing.T) {
	app := mashuraCmdApp(t)
	app.Cfg.MashuraPanels = map[string]config.MashuraPanelConfig{
		"chg": {Models: []string{"anthropic:m"}, Mode: "panel"},
	}
	handled, _, cmd := handleMashuraCommand(
		[]string{"/mashura", "panel", "chg", "--mode", "bogus"}, app)
	if !handled {
		t.Fatal("not handled")
	}
	text := cmdNote(t, cmd)
	if !strings.Contains(text, "invalid mode") {
		t.Errorf("expected invalid mode error: %q", text)
	}
	// Mode unchanged.
	if p := app.Cfg.MashuraPanels["chg"]; p.Mode != "panel" {
		t.Errorf("mode should be unchanged, got %q", p.Mode)
	}
}

func TestMashuraCmdPanelBlankModeShownAsPanel(t *testing.T) {
	app := mashuraCmdApp(t)
	// Panel with blank mode — status detail should display "panel" as the mode.
	app.Cfg.MashuraPanels = map[string]config.MashuraPanelConfig{
		"blank": {Models: []string{"anthropic:m"}, Mode: ""},
	}
	handled, _, cmd := handleMashuraCommand(
		[]string{"/mashura", "panel", "blank"}, app)
	if !handled {
		t.Fatal("not handled")
	}
	text := cmdNote(t, cmd)
	// The detail format is: panel "blank" [panel]:  — check for the bracketed mode.
	if !strings.Contains(text, "[panel]") {
		t.Errorf("blank mode should display as [panel] in detail: %q", text)
	}
}

// --- /mashura map ---

func TestMashuraCmdMapSetAndShow(t *testing.T) {
	app := mashuraCmdApp(t)
	app.Cfg.MashuraPanels = map[string]config.MashuraPanelConfig{
		"rev": {Models: []string{"anthropic:m"}, Mode: "panel"},
	}
	// Set mapping.
	handled, _, cmd := handleMashuraCommand(
		[]string{"/mashura", "map", "review", "rev"}, app)
	if !handled {
		t.Fatal("not handled")
	}
	text := cmdNote(t, cmd)
	if !strings.Contains(text, "review") || !strings.Contains(text, "rev") {
		t.Errorf("map set response: %q", text)
	}
	if app.Cfg.MashuraToolPanels["review"] != "rev" {
		t.Errorf("mapping not set in Cfg")
	}
	// Persisted.
	st := mustLoadRepoState(t, app.SessionWorkspace())
	if st.MashuraToolPanels["review"] != "rev" {
		t.Error("mapping not persisted")
	}

	// Show mapping.
	handled, _, cmd = handleMashuraCommand(
		[]string{"/mashura", "map", "review"}, app)
	if !handled {
		t.Fatal("not handled")
	}
	text = cmdNote(t, cmd)
	if !strings.Contains(text, "rev") {
		t.Errorf("map show should reference panel: %q", text)
	}
}

func TestMashuraCmdMapShowDefault(t *testing.T) {
	app := mashuraCmdApp(t)
	handled, _, cmd := handleMashuraCommand(
		[]string{"/mashura", "map", "debug"}, app)
	if !handled {
		t.Fatal("not handled")
	}
	text := cmdNote(t, cmd)
	if !strings.Contains(text, "default") {
		t.Errorf("unmapped tool should show default: %q", text)
	}
}

func TestMashuraCmdMapInvalidTool(t *testing.T) {
	app := mashuraCmdApp(t)
	handled, _, cmd := handleMashuraCommand(
		[]string{"/mashura", "map", "bogus"}, app)
	if !handled {
		t.Fatal("not handled")
	}
	text := cmdNote(t, cmd)
	if !strings.Contains(text, "unknown tool") {
		t.Errorf("expected unknown tool error: %q", text)
	}
}

func TestMashuraCmdMapNonexistentPanel(t *testing.T) {
	app := mashuraCmdApp(t)
	handled, _, cmd := handleMashuraCommand(
		[]string{"/mashura", "map", "review", "ghost"}, app)
	if !handled {
		t.Fatal("not handled")
	}
	text := cmdNote(t, cmd)
	if !strings.Contains(text, "not found") {
		t.Errorf("expected not found error: %q", text)
	}
}

func TestMashuraCmdMapMissingArgs(t *testing.T) {
	app := mashuraCmdApp(t)
	handled, _, cmd := handleMashuraCommand(
		[]string{"/mashura", "map"}, app)
	if !handled {
		t.Fatal("not handled")
	}
	text := cmdNote(t, cmd)
	if !strings.Contains(text, "usage") {
		t.Errorf("expected usage hint: %q", text)
	}
}

// --- /mashura model ---

func TestMashuraCmdModelSetAndQuery(t *testing.T) {
	app := mashuraCmdApp(t)
	// Query default.
	handled, _, cmd := handleMashuraCommand(
		[]string{"/mashura", "model"}, app)
	if !handled {
		t.Fatal("not handled")
	}
	text := cmdNote(t, cmd)
	if !strings.Contains(text, "test-model") {
		t.Errorf("query should show current model: %q", text)
	}

	// Set model.
	handled, _, cmd = handleMashuraCommand(
		[]string{"/mashura", "model", "claude-opus-4-8"}, app)
	if !handled {
		t.Fatal("not handled")
	}
	text = cmdNote(t, cmd)
	if !strings.Contains(text, "claude-opus-4-8") {
		t.Errorf("set response should show model: %q", text)
	}
	if app.Cfg.OracleModel != "claude-opus-4-8" {
		t.Errorf("OracleModel = %q, want claude-opus-4-8", app.Cfg.OracleModel)
	}
	st := mustLoadRepoState(t, app.SessionWorkspace())
	if st.MashuraDefaultModel != "claude-opus-4-8" {
		t.Error("model not persisted")
	}
}

// --- /mashura maxtokens ---

func TestMashuraCmdMaxtokensSetAndQuery(t *testing.T) {
	app := mashuraCmdApp(t)
	// Query.
	handled, _, cmd := handleMashuraCommand(
		[]string{"/mashura", "maxtokens"}, app)
	if !handled {
		t.Fatal("not handled")
	}
	text := cmdNote(t, cmd)
	if !strings.Contains(text, "4096") {
		t.Errorf("query should show current maxtokens: %q", text)
	}

	// Set valid.
	handled, _, cmd = handleMashuraCommand(
		[]string{"/mashura", "maxtokens", "8192"}, app)
	if !handled {
		t.Fatal("not handled")
	}
	text = cmdNote(t, cmd)
	if !strings.Contains(text, "8192") {
		t.Errorf("set response should show value: %q", text)
	}
	if app.Cfg.OracleMaxTokens != 8192 {
		t.Errorf("OracleMaxTokens = %d, want 8192", app.Cfg.OracleMaxTokens)
	}
	st := mustLoadRepoState(t, app.SessionWorkspace())
	if st.MashuraMaxTokens != 8192 {
		t.Error("maxtokens not persisted")
	}
}

func TestMashuraCmdMaxtokensNonNumeric(t *testing.T) {
	app := mashuraCmdApp(t)
	handled, _, cmd := handleMashuraCommand(
		[]string{"/mashura", "maxtokens", "abc"}, app)
	if !handled {
		t.Fatal("not handled")
	}
	text := cmdNote(t, cmd)
	if !strings.Contains(text, "usage") {
		t.Errorf("expected usage error: %q", text)
	}
}

func TestMashuraCmdMaxtokensZero(t *testing.T) {
	app := mashuraCmdApp(t)
	handled, _, cmd := handleMashuraCommand(
		[]string{"/mashura", "maxtokens", "0"}, app)
	if !handled {
		t.Fatal("not handled")
	}
	text := cmdNote(t, cmd)
	if !strings.Contains(text, "usage") {
		t.Errorf("expected usage error for zero: %q", text)
	}
}

// --- /mashura timeout ---

func TestMashuraCmdTimeoutSetAndQuery(t *testing.T) {
	app := mashuraCmdApp(t)
	// Query.
	handled, _, cmd := handleMashuraCommand(
		[]string{"/mashura", "timeout"}, app)
	if !handled {
		t.Fatal("not handled")
	}
	text := cmdNote(t, cmd)
	if !strings.Contains(text, "300") {
		t.Errorf("query should show current timeout: %q", text)
	}

	// Set valid.
	handled, _, cmd = handleMashuraCommand(
		[]string{"/mashura", "timeout", "120"}, app)
	if !handled {
		t.Fatal("not handled")
	}
	text = cmdNote(t, cmd)
	if !strings.Contains(text, "120") {
		t.Errorf("set response should show value: %q", text)
	}
	if app.Cfg.OracleTimeoutSeconds != 120 {
		t.Errorf("OracleTimeoutSeconds = %d, want 120", app.Cfg.OracleTimeoutSeconds)
	}
	st := mustLoadRepoState(t, app.SessionWorkspace())
	if st.MashuraTimeoutSeconds != 120 {
		t.Error("timeout not persisted")
	}
}

func TestMashuraCmdTimeoutNonNumeric(t *testing.T) {
	app := mashuraCmdApp(t)
	handled, _, cmd := handleMashuraCommand(
		[]string{"/mashura", "timeout", "abc"}, app)
	if !handled {
		t.Fatal("not handled")
	}
	text := cmdNote(t, cmd)
	if !strings.Contains(text, "usage") {
		t.Errorf("expected usage error: %q", text)
	}
}

func TestMashuraCmdTimeoutNegative(t *testing.T) {
	app := mashuraCmdApp(t)
	handled, _, cmd := handleMashuraCommand(
		[]string{"/mashura", "timeout", "-5"}, app)
	if !handled {
		t.Fatal("not handled")
	}
	text := cmdNote(t, cmd)
	if !strings.Contains(text, "usage") {
		t.Errorf("expected usage error for negative: %q", text)
	}
}

// --- /mashura unknown subcommand → help ---

func TestMashuraCmdUnknownSubcommandShowsHelp(t *testing.T) {
	app := mashuraCmdApp(t)
	handled, _, cmd := handleMashuraCommand(
		[]string{"/mashura", "bogus"}, app)
	if !handled {
		t.Fatal("not handled")
	}
	text := cmdNote(t, cmd)
	if !strings.Contains(text, "/mashura") || !strings.Contains(text, "panel add") {
		t.Errorf("help text should show commands: %q", text)
	}
}

// --- /mashura panel (bare, missing args) ---

func TestMashuraCmdPanelBareShowsUsage(t *testing.T) {
	app := mashuraCmdApp(t)
	handled, _, cmd := handleMashuraCommand(
		[]string{"/mashura", "panel"}, app)
	if !handled {
		t.Fatal("not handled")
	}
	text := cmdNote(t, cmd)
	if !strings.Contains(text, "usage") {
		t.Errorf("expected usage hint: %q", text)
	}
}
