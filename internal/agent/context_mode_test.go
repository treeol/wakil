package agent

import (
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
)

// --- /mode context mode command ---

func TestModeCommandBareShowsExecAndContext(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })

	_, _, cmd := HandleTUICommand("/mode", app)
	msgs := runCmd(cmd)
	if len(msgs) != 1 {
		t.Fatalf("want 1 msg, got %d", len(msgs))
	}
	sn, ok := msgs[0].(SysNoteMsg)
	if !ok {
		t.Fatalf("want SysNoteMsg, got %T", msgs[0])
	}
	if !strings.Contains(sn.Text, "exec:") {
		t.Errorf("bare /mode should show exec backend, got: %q", sn.Text)
	}
	if !strings.Contains(sn.Text, "context:") {
		t.Errorf("bare /mode should show context mode, got: %q", sn.Text)
	}
	if !strings.Contains(sn.Text, config.DefaultContextMode) {
		t.Errorf("bare /mode should show the default context mode %q, got: %q", config.DefaultContextMode, sn.Text)
	}
}

func TestModeCommandSetStandard(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })

	_, _, cmd := HandleTUICommand("/mode standard", app)
	msgs := runCmd(cmd)
	if len(msgs) != 1 {
		t.Fatalf("want 1 msg, got %d", len(msgs))
	}
	sn, ok := msgs[0].(SysNoteMsg)
	if !ok {
		t.Fatalf("want SysNoteMsg, got %T", msgs[0])
	}
	if !strings.Contains(sn.Text, "standard") {
		t.Errorf("should mention standard mode, got: %q", sn.Text)
	}
	if app.ContextMode() != "standard" {
		t.Errorf("ContextMode() = %q, want standard", app.ContextMode())
	}
}

func TestModeCommandSetLongSession(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })

	_, _, cmd := HandleTUICommand("/mode long-session", app)
	msgs := runCmd(cmd)
	if len(msgs) != 1 {
		t.Fatalf("want 1 msg, got %d", len(msgs))
	}
	sn, ok := msgs[0].(SysNoteMsg)
	if !ok {
		t.Fatalf("want SysNoteMsg, got %T", msgs[0])
	}
	if !strings.Contains(sn.Text, "long-session") {
		t.Errorf("should mention long-session mode, got: %q", sn.Text)
	}
	if app.ContextMode() != "long-session" {
		t.Errorf("ContextMode() = %q, want long-session", app.ContextMode())
	}
}

func TestModeCommandSetLongAlias(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })

	_, _, cmd := HandleTUICommand("/mode long", app)
	runCmd(cmd)
	if app.ContextMode() != "long-session" {
		t.Errorf("ContextMode() = %q, want long-session (alias 'long' should normalise)", app.ContextMode())
	}
}

func TestModeCommandStatus(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")

	_, _, cmd := HandleTUICommand("/mode status", app)
	msgs := runCmd(cmd)
	if len(msgs) != 1 {
		t.Fatalf("want 1 msg, got %d", len(msgs))
	}
	sn, ok := msgs[0].(SysNoteMsg)
	if !ok {
		t.Fatalf("want SysNoteMsg, got %T", msgs[0])
	}
	if !strings.Contains(sn.Text, "long-session") {
		t.Errorf("status should show current mode, got: %q", sn.Text)
	}
}

func TestModeCommandInvalidArg(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })

	_, _, cmd := HandleTUICommand("/mode bogus", app)
	msgs := runCmd(cmd)
	if len(msgs) != 1 {
		t.Fatalf("want 1 msg, got %d", len(msgs))
	}
	sn, ok := msgs[0].(SysNoteMsg)
	if !ok {
		t.Fatalf("want SysNoteMsg, got %T", msgs[0])
	}
	if !strings.Contains(sn.Text, "usage:") {
		t.Errorf("invalid argument should show usage, got: %q", sn.Text)
	}
}

func TestIsLongSessionNotSubagent(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")

	if !app.IsLongSession() {
		t.Error("IsLongSession() should be true for non-subagent with long-session mode")
	}

	app.IsSubagent = true
	if app.IsLongSession() {
		t.Error("IsLongSession() should be false for subagent even with long-session mode")
	}
}

func TestSetContextModeInvalid(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	if err := app.SetContextMode("bogus"); err == nil {
		t.Error("SetContextMode should reject invalid mode")
	}
	// Mode should be unchanged (the default).
	if app.ContextMode() != config.DefaultContextMode {
		t.Errorf("ContextMode() = %q, want %q after failed set", app.ContextMode(), config.DefaultContextMode)
	}
}

// TestContextModeDefault: the default (no explicit selection anywhere) is
// long-session. Explicit "standard" still opts out.
func TestContextModeDefault(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	if app.ContextMode() != "long-session" {
		t.Errorf("ContextMode() = %q, want long-session (default)", app.ContextMode())
	}
	// NormalizeContextMode: "" resolves to the default too.
	if m, ok := config.NormalizeContextMode(""); !ok || m != config.DefaultContextMode {
		t.Errorf("NormalizeContextMode(\"\") = %q, %v; want %q, true", m, ok, config.DefaultContextMode)
	}
	// Explicit standard is still honored.
	if m, ok := config.NormalizeContextMode("standard"); !ok || m != "standard" {
		t.Errorf("NormalizeContextMode(\"standard\") = %q, %v; want standard, true", m, ok)
	}
}

// --- RepoState round-trip for ContextMode ---

func TestRepoStateContextModeRoundTrip(t *testing.T) {
	withRepoStateDir(t)
	ws := t.TempDir()

	if err := updateRepoState(ws, func(s *RepoState) {
		s.ContextMode = "long-session"
	}); err != nil {
		t.Fatalf("updateRepoState: %v", err)
	}

	st, err := LoadRepoState(ws)
	if err != nil || st == nil {
		t.Fatalf("LoadRepoState: %+v, %v", st, err)
	}
	if st.ContextMode != "long-session" {
		t.Errorf("ContextMode = %q, want long-session", st.ContextMode)
	}
}

func TestRestoreRepoStateContextModeApplies(t *testing.T) {
	withRepoStateDir(t)
	ws := t.TempDir()

	if err := updateRepoState(ws, func(s *RepoState) {
		s.ContextMode = "long-session"
	}); err != nil {
		t.Fatalf("updateRepoState: %v", err)
	}

	app := &App{
		Cfg:    config.Config{ExecMode: "direct", WorkDir: ws},
		Client: repoStateTestClient("m"),
	}
	RestoreRepoState(app)
	if app.ContextMode() != "long-session" {
		t.Errorf("ContextMode() = %q, want long-session after restore", app.ContextMode())
	}
}

func TestRestoreRepoStateContextModeSkippedWhenExplicit(t *testing.T) {
	withRepoStateDir(t)
	ws := t.TempDir()

	if err := updateRepoState(ws, func(s *RepoState) {
		s.ContextMode = "long-session"
	}); err != nil {
		t.Fatalf("updateRepoState: %v", err)
	}

	app := &App{
		Cfg:    config.Config{ExecMode: "direct", WorkDir: ws, ContextModeExplicit: true, ContextMode: "standard"},
		Client: repoStateTestClient("m"),
	}
	app.SetContextMode("standard") // simulate bootstrap init from Cfg
	RestoreRepoState(app)
	if app.ContextMode() != "standard" {
		t.Errorf("ContextMode() = %q, want standard (explicit flag should suppress restore)", app.ContextMode())
	}
}
