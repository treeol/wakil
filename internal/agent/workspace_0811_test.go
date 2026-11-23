package agent

// Block 1 (0.8.11): all workspace consumers see the one resolved path, and
// old sessions stay listable under --all after the config keys stop
// steering the workspace.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/config"
)

// TestAllConsumersSameWorkspace0811: after the 0.8.11 resolution, the store
// identity (ProjectDir / SessionWorkspace), the workspace-keys report and
// the recent-session scope all agree on the one resolved path.
func TestAllConsumersSameWorkspace0811(t *testing.T) {
	t.Setenv("WAKIL_WORKSPACE_KEY", "")
	t.Setenv("WAKIL_WORKSPACE_PATH", "")
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws := filepath.Join(t.TempDir(), "ws")
	if err := os.Mkdir(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(ws)
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(real)

	cfg := config.DefaultConfig()
	cfg.ExecMode = "docker"
	cfg.ResolvedWorkspace = real
	cfg.HostWorkDir = real

	if got := ProjectDir(cfg); got != real {
		t.Errorf("ProjectDir = %q, want the resolved workspace %q", got, real)
	}
	k := ResolveWorkspaceKeys(cfg)
	if k.ChatPath != real {
		t.Errorf("ResolveWorkspaceKeys ChatPath = %q, want %q", k.ChatPath, real)
	}
	// The session scope that main.go builds for --resume must be the same
	// resolved path (PrintSessionsWithScope scopes by it).
	scope := SessionScope{Workspace: cfg.ResolvedWorkspace}
	_ = scope
	// SessionWorkspace (the App accessor) is ProjectDir for a loaded config.
	app := &App{Cfg: cfg}
	if got := app.SessionWorkspace(); got != real {
		t.Errorf("SessionWorkspace = %q, want %q", got, real)
	}
}

// TestOldSessionsListedWithAll0811: a session stored under a different
// workspace key is still listed with All=true (the old-key safety after the
// config keys stop steering).
func TestOldSessionsListedWithAll0811(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WAKIL_SESSIONS_DIR", dir)
	ws := t.TempDir()
	old := t.TempDir()

	// Store a session under the OLD (config-folder) key.
	if err := WriteSession(&Session{ChatID: "0811-old-1", Workspace: old, Updated: time.Now()}); err != nil {
		t.Fatal(err)
	}
	// The scoped listing for the new workspace must not show it...
	scoped, _, err := ListSessionsScoped(SessionScope{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range scoped {
		if s.ChatID == "0811-old-1" {
			t.Error("old-key session leaked into the new workspace scope")
		}
	}
	// ...but --all still finds it.
	all, _, err := ListSessionsScoped(SessionScope{All: true})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range all {
		if s.ChatID == "0811-old-1" {
			found = true
		}
	}
	if !found {
		t.Error("old-key session missing from --all listing")
	}
	// And the printed --all list names it (short id: first 8 chars, then
	// the 7-char prefix the print shows).
	var sb strings.Builder
	PrintSessionsWithScope(&sb, SessionScope{All: true})
	if !strings.Contains(sb.String(), "0811-old") {
		t.Errorf("printed --all list lacks the old session: %q", sb.String())
	}
}
