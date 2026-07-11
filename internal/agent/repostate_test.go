package agent

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
)

// repoStateTestClient is a minimal proxy.Client for tests that only need
// Model/ConfiguredModel bookkeeping, mirroring newTestClient's shape.
func repoStateTestClient(model string) *proxy.Client {
	return &proxy.Client{Model: model, ConfiguredModel: model, ChatID: "test", HTTP: http.DefaultClient}
}

// withRepoStateDir points repoStateDir() at a fresh temp dir for the duration
// of the test, so tests never touch the real ~/.local/share/wakil/repo-state.
func withRepoStateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("WAKIL_REPO_STATE_DIR", dir)
	return dir
}

func TestRepoStateRoundTrip(t *testing.T) {
	withRepoStateDir(t)
	ws := t.TempDir()

	if st, err := LoadRepoState(ws); err != nil || st != nil {
		t.Fatalf("expected nil,nil for missing state, got %+v, %v", st, err)
	}

	if err := updateRepoState(ws, func(s *RepoState) {
		s.Model = "foo-model"
		s.Backend = "openrouter"
	}); err != nil {
		t.Fatalf("updateRepoState: %v", err)
	}

	st, err := LoadRepoState(ws)
	if err != nil || st == nil {
		t.Fatalf("expected loaded state, got %+v, %v", st, err)
	}
	if st.Model != "foo-model" || st.Backend != "openrouter" {
		t.Errorf("got model=%q backend=%q", st.Model, st.Backend)
	}
	if st.Workspace != ws {
		t.Errorf("workspace = %q, want %q", st.Workspace, ws)
	}
	if st.SchemaVersion != repoStateSchemaVersion {
		t.Errorf("schema version = %d, want %d", st.SchemaVersion, repoStateSchemaVersion)
	}
}

func TestRepoStateEmptyWorkspaceIsNoOp(t *testing.T) {
	withRepoStateDir(t)
	if err := updateRepoState("", func(s *RepoState) { s.Model = "x" }); err != nil {
		t.Fatalf("updateRepoState with empty ws should no-op, got err: %v", err)
	}
	if st, err := LoadRepoState(""); err != nil || st != nil {
		t.Fatalf("expected nil,nil for empty ws, got %+v, %v", st, err)
	}
}

// TestRepoStatePatchSemantics verifies fix #1: two sequential updateRepoState
// calls touching different fields must not clobber each other's values —
// this is the core defect the whole-snapshot approach in the original draft
// plan would have introduced.
func TestRepoStatePatchSemantics(t *testing.T) {
	withRepoStateDir(t)
	ws := t.TempDir()

	if err := updateRepoState(ws, func(s *RepoState) { s.Model = "m1" }); err != nil {
		t.Fatal(err)
	}
	if err := updateRepoState(ws, func(s *RepoState) { s.AutoApprove = true }); err != nil {
		t.Fatal(err)
	}

	st, err := LoadRepoState(ws)
	if err != nil || st == nil {
		t.Fatalf("load failed: %+v, %v", st, err)
	}
	if st.Model != "m1" {
		t.Errorf("model = %q, want m1 (should survive the later AutoApprove-only patch)", st.Model)
	}
	if !st.AutoApprove {
		t.Error("AutoApprove should be true after the second patch")
	}
}

// TestRepoStateNeverSerializesAllowDestructive proves the structural
// guarantee: even a hand-crafted JSON file with an unknown allow_destructive
// key must never surface as anything App reads, because RepoState has no
// field it could unmarshal into (and RestoreRepoState never touches
// App.AllowDestructive at all).
func TestRepoStateNeverSerializesAllowDestructive(t *testing.T) {
	dir := withRepoStateDir(t)
	ws := t.TempDir()

	key := repoStateKey(ws)
	path := filepath.Join(dir, key+".json")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	hostile := `{"schema_version":1,"workspace":"` + strings.ReplaceAll(ws, `\`, `\\`) + `","allow_destructive":true,"auto_approve":true}`
	if err := os.WriteFile(path, []byte(hostile), 0o644); err != nil {
		t.Fatal(err)
	}

	app := &App{Cfg: config.Config{ExecMode: "direct", WorkDir: ws}}
	result := RestoreRepoState(app)
	if app.AllowDestructive {
		t.Error("AllowDestructive must never be set by RestoreRepoState")
	}
	if !app.AutoApprove {
		t.Error("AutoApprove should have been restored from the hostile-but-otherwise-valid file")
	}
	if result.Note == "" {
		t.Error("expected a restore note")
	}
}

// TestRepoStateSchemaVersionMismatchIgnored ensures a file with the wrong (or
// missing) schema version is treated as absent, never causing a startup
// crash or a garbage restore.
func TestRepoStateSchemaVersionMismatchIgnored(t *testing.T) {
	dir := withRepoStateDir(t)
	ws := t.TempDir()
	key := repoStateKey(ws)
	path := filepath.Join(dir, key+".json")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"schema_version":999,"model":"nope"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := LoadRepoState(ws)
	if err != nil || st != nil {
		t.Fatalf("expected nil,nil for mismatched schema version, got %+v, %v", st, err)
	}
}

// TestRepoStateCorruptFileIgnored ensures malformed JSON never blocks startup.
func TestRepoStateCorruptFileIgnored(t *testing.T) {
	dir := withRepoStateDir(t)
	ws := t.TempDir()
	key := repoStateKey(ws)
	path := filepath.Join(dir, key+".json")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{not valid json`), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := LoadRepoState(ws)
	if err != nil || st != nil {
		t.Fatalf("expected nil,nil for corrupt file, got %+v, %v", st, err)
	}
}

func TestRepoStateKeyEvalSymlinksFailureFallback(t *testing.T) {
	// A path that does not exist must still produce a stable, non-empty key
	// (falls back to Abs alone rather than erroring).
	k1 := repoStateKey("/definitely/does/not/exist/on/this/machine")
	k2 := repoStateKey("/definitely/does/not/exist/on/this/machine")
	if k1 == "" || k1 != k2 {
		t.Errorf("expected stable non-empty key for a nonexistent path, got %q and %q", k1, k2)
	}
}

// --- ApplyModelOverride ---

func TestApplyModelOverrideIlmProxy(t *testing.T) {
	app := &App{
		Cfg:    config.Config{ExecMode: "direct"},
		Client: repoStateTestClient("old-model"),
	}
	ApplyModelOverride(app, "new-model")
	if app.SelectedModel != "new-model" {
		t.Errorf("SelectedModel = %q, want new-model", app.SelectedModel)
	}
}

func TestApplyModelOverrideOpenAI(t *testing.T) {
	app := &App{
		Cfg: config.Config{
			ExecMode: "direct",
			Endpoint: config.EndpointConfig{Kind: config.EndpointKindOpenAI, Model: "old-model"},
		},
		Client: repoStateTestClient("old-model"),
	}
	ApplyModelOverride(app, "new-model")
	if app.SelectedModel != "" {
		t.Errorf("SelectedModel should be cleared for openai kind, got %q", app.SelectedModel)
	}
	if app.Client.Model != "new-model" || app.Client.ConfiguredModel != "new-model" {
		t.Errorf("Client model not updated: Model=%q ConfiguredModel=%q", app.Client.Model, app.Client.ConfiguredModel)
	}
	if app.Cfg.Endpoint.Model != "new-model" {
		t.Errorf("Cfg.Endpoint.Model = %q, want new-model", app.Cfg.Endpoint.Model)
	}
}

// --- RestoreRepoState guards ---

func TestRestoreRepoStateSkipsModelWhenExplicit(t *testing.T) {
	withRepoStateDir(t)
	ws := t.TempDir()
	if err := updateRepoState(ws, func(s *RepoState) { s.Model = "restored-model" }); err != nil {
		t.Fatal(err)
	}
	app := &App{
		Cfg:    config.Config{ExecMode: "direct", WorkDir: ws, ModelExplicit: true},
		Client: repoStateTestClient("cmdline-model"),
	}
	result := RestoreRepoState(app)
	if app.SelectedModel == "restored-model" {
		t.Error("model restore should have been suppressed by ModelExplicit")
	}
	if result.Model != "" {
		t.Errorf("result.Model should be empty when suppressed, got %q", result.Model)
	}
}

func TestRestoreRepoStateSkipsAutoWhenExplicit(t *testing.T) {
	withRepoStateDir(t)
	ws := t.TempDir()
	if err := updateRepoState(ws, func(s *RepoState) { s.AutoApprove = true }); err != nil {
		t.Fatal(err)
	}
	app := &App{Cfg: config.Config{ExecMode: "direct", WorkDir: ws, AutoExplicit: true}}
	RestoreRepoState(app)
	if app.AutoApprove {
		t.Error("auto restore should have been suppressed by AutoExplicit")
	}
}

func TestRestoreRepoStateStaleSubagentEndpointSkipped(t *testing.T) {
	withRepoStateDir(t)
	ws := t.TempDir()
	if err := updateRepoState(ws, func(s *RepoState) { s.SubagentEndpoint = "gone" }); err != nil {
		t.Fatal(err)
	}
	app := &App{Cfg: config.Config{ExecMode: "direct", WorkDir: ws}} // no "gone" endpoint configured
	result := RestoreRepoState(app)
	if app.SubagentEndpointOverride != "" {
		t.Errorf("stale endpoint should not have been applied, got %q", app.SubagentEndpointOverride)
	}
	_ = result // no panic, no error — startup must never hard-fail on stale state
}

func TestRestoreRepoStateEndpointMismatchSkipsModelAndBackend(t *testing.T) {
	withRepoStateDir(t)
	ws := t.TempDir()
	if err := updateRepoState(ws, func(s *RepoState) {
		s.Model = "model-a"
		s.Backend = "backend-a"
		s.EndpointName = "endpoint-a"
		s.RawTools = true
	}); err != nil {
		t.Fatal(err)
	}
	app := &App{
		Cfg: config.Config{
			ExecMode:     "direct",
			WorkDir:      ws,
			EndpointName: "endpoint-b", // different endpoint active this run
			Endpoint:     config.EndpointConfig{Kind: config.EndpointKindIlmProxy},
		},
		Client: repoStateTestClient(""),
	}
	result := RestoreRepoState(app)
	if app.SelectedModel != "" || result.Model != "" {
		t.Error("model should be skipped on endpoint mismatch")
	}
	if app.SelectedBackend != "" || result.Backend != "" {
		t.Error("backend should be skipped on endpoint mismatch")
	}
	if !app.RawTools {
		t.Error("RawTools is endpoint-independent and should still apply")
	}
}

func TestRestoreRepoStateBackendOnlyForIlmProxyKind(t *testing.T) {
	withRepoStateDir(t)
	ws := t.TempDir()
	if err := updateRepoState(ws, func(s *RepoState) { s.Backend = "openrouter" }); err != nil {
		t.Fatal(err)
	}
	app := &App{
		Cfg: config.Config{
			ExecMode: "direct",
			WorkDir:  ws,
			Endpoint: config.EndpointConfig{Kind: config.EndpointKindOpenAI, Model: "m"},
		},
		Client: repoStateTestClient("m"),
	}
	result := RestoreRepoState(app)
	if app.SelectedBackend != "" || result.Backend != "" {
		t.Error("backend restore must not apply to an openai-kind active endpoint")
	}
}

// TestRestoreRepoStateOpenAIRoundTrip proves fix #2: a /model set on an
// openai-kind endpoint (which clears SelectedModel) still round-trips
// correctly through repo-state and RestoreRepoState.
func TestRestoreRepoStateOpenAIRoundTrip(t *testing.T) {
	withRepoStateDir(t)
	ws := t.TempDir()

	app := &App{
		Cfg: config.Config{
			ExecMode:     "direct",
			WorkDir:      ws,
			EndpointName: "oai",
			Endpoint:     config.EndpointConfig{Kind: config.EndpointKindOpenAI, Model: "initial"},
		},
		Client: repoStateTestClient("initial"),
	}

	// Simulate what the /model command handler now does.
	model := "gpt-restored"
	ApplyModelOverride(app, model)
	if app.SelectedModel != "" {
		t.Fatal("sanity: openai kind should clear SelectedModel")
	}
	if err := updateRepoState(ws, func(s *RepoState) {
		s.Model = model
		s.EndpointName = app.Cfg.EndpointName
	}); err != nil {
		t.Fatal(err)
	}

	// Fresh app, same workspace + endpoint, simulating a restart.
	app2 := &App{
		Cfg: config.Config{
			ExecMode:     "direct",
			WorkDir:      ws,
			EndpointName: "oai",
			Endpoint:     config.EndpointConfig{Kind: config.EndpointKindOpenAI, Model: "initial"},
		},
		Client: repoStateTestClient("initial"),
	}
	result := RestoreRepoState(app2)
	if result.Model != model {
		t.Errorf("result.Model = %q, want %q", result.Model, model)
	}
	if app2.Client.ConfiguredModel != model || app2.Client.Model != model {
		t.Errorf("Client model not restored: ConfiguredModel=%q Model=%q", app2.Client.ConfiguredModel, app2.Client.Model)
	}
}

func TestClearRepoState(t *testing.T) {
	withRepoStateDir(t)
	ws := t.TempDir()
	if err := updateRepoState(ws, func(s *RepoState) { s.Model = "x" }); err != nil {
		t.Fatal(err)
	}
	app := &App{Cfg: config.Config{ExecMode: "direct", WorkDir: ws}}
	if err := ClearRepoState(app); err != nil {
		t.Fatalf("ClearRepoState: %v", err)
	}
	if st, err := LoadRepoState(ws); err != nil || st != nil {
		t.Fatalf("expected nil,nil after clear, got %+v, %v", st, err)
	}
	// Clearing twice must not error.
	if err := ClearRepoState(app); err != nil {
		t.Fatalf("ClearRepoState on already-clear state: %v", err)
	}
}

func TestDescribeRepoStateNoneYet(t *testing.T) {
	withRepoStateDir(t)
	app := &App{Cfg: config.Config{ExecMode: "direct", WorkDir: t.TempDir()}}
	got := DescribeRepoState(app)
	if !strings.Contains(got, "none yet") {
		t.Errorf("expected 'none yet' message, got: %s", got)
	}
}

// TestHeadlessPathNeverReferencesRepoStateAutoRestore is a structural smoke
// check for fix #3: cmd/wakil/run.go (the headless "wakil run" entry point)
// must never call RestoreRepoState, updateRepoState, or saveRepoState —
// restoring AutoApprove there would create a new unattended-approval path
// that bypasses --auto (headlessConfirmer checks RunFlags.Auto directly, not
// App.AutoApprove). This is enforced by grepping the actual source file
// rather than by a runtime check, since the constraint is "this file never
// calls these functions," not something expressible as a unit test against
// behavior.
func TestHeadlessPathNeverReferencesRepoStateAutoRestore(t *testing.T) {
	b, err := os.ReadFile("../../cmd/wakil/run.go")
	if err != nil {
		t.Skipf("cannot read cmd/wakil/run.go from this working dir: %v", err)
	}
	src := string(b)
	for _, forbidden := range []string{"RestoreRepoState", "updateRepoState", "saveRepoState"} {
		if strings.Contains(src, forbidden) {
			t.Errorf("cmd/wakil/run.go must never reference %q (headless AutoApprove restore is out of scope)", forbidden)
		}
	}
}
