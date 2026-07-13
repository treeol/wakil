package agent

import (
	"testing"

	"github.com/treeol/wakil/internal/config"
)

// TestMaxParallelSubagentsConfigRoundTrip verifies that a large MaxParallelSubagents
// value is stored and readable on the App — the clamp in runSubagentJobs is the
// runtime safety net, this test documents that the config value itself is accepted.
func TestMaxParallelSubagentsConfigRoundTrip(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.MaxParallelSubagents = 64
	if app.Cfg.MaxParallelSubagents != 64 {
		t.Fatalf("config not set: got %d, want 64", app.Cfg.MaxParallelSubagents)
	}
}

// TestRepoStateMaxParallelSubagentsRoundTrip verifies that updateRepoState +
// LoadRepoState correctly persist and restore MaxParallelSubagents.
func TestRepoStateMaxParallelSubagentsRoundTrip(t *testing.T) {
	ws := "/tmp/test-wakil-maxpar-rt"
	if err := updateRepoState(ws, func(s *RepoState) {
		s.MaxParallelSubagents = 4
	}); err != nil {
		t.Fatalf("updateRepoState: %v", err)
	}
	loaded, err := LoadRepoState(ws)
	if err != nil {
		t.Fatalf("LoadRepoState: %v", err)
	}
	if loaded == nil {
		t.Fatal("LoadRepoState returned nil")
	}
	if loaded.MaxParallelSubagents != 4 {
		t.Errorf("loaded maxpar = %d, want 4", loaded.MaxParallelSubagents)
	}
}

// TestRepoStateMaxParallelSubagentsZeroOmitted verifies that a zero value
// is omitted (omitempty) and treated as "not set" on reload.
func TestRepoStateMaxParallelSubagentsZeroOmitted(t *testing.T) {
	ws := "/tmp/test-wakil-maxpar-zero"
	_ = updateRepoState(ws, func(s *RepoState) {
		s.MaxParallelSubagents = 0
	})
	loaded, _ := LoadRepoState(ws)
	if loaded != nil && loaded.MaxParallelSubagents != 0 {
		t.Errorf("expected 0 (not set), got %d", loaded.MaxParallelSubagents)
	}
}

// TestDescribeRepoStateIncludesMaxPar verifies that /repostate output
// includes the max parallel subagents line when set.
func TestDescribeRepoStateIncludesMaxPar(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.HostWorkDir = "/tmp/test-wakil-maxpar-describe"
	app := &App{
		Cfg:     cfg,
		Session: nil,
	}
	_ = updateRepoState(cfg.HostWorkDir, func(s *RepoState) {
		s.MaxParallelSubagents = 8
	})
	desc := DescribeRepoState(app)
	if !contains(desc, "max parallel subagents: 8") {
		t.Errorf("describe output missing maxpar line:\n%s", desc)
	}
}

// TestRestoreRepoStateMaxParallelSubagents verifies that RestoreRepoState
// applies the persisted maxpar value to the App config.
func TestRestoreRepoStateMaxParallelSubagents(t *testing.T) {
	ws := "/tmp/test-wakil-maxpar-restore"
	_ = updateRepoState(ws, func(s *RepoState) {
		s.MaxParallelSubagents = 6
	})

	cfg := config.DefaultConfig() // default maxpar = 2
	cfg.HostWorkDir = ws
	app := &App{
		Cfg: cfg,
	}
	// Set the workspace so SessionWorkspace resolves to ws.
	// In direct mode, SessionWorkspace returns Cfg.WorkDir.
	cfg.ExecMode = "direct"
	cfg.WorkDir = ws

	result := RestoreRepoState(app)
	if app.Cfg.MaxParallelSubagents != 6 {
		t.Errorf("after restore: maxpar = %d, want 6", app.Cfg.MaxParallelSubagents)
	}
	if !contains(result.Note, "maxpar=6") {
		t.Errorf("restore note should mention maxpar=6: %q", result.Note)
	}
}

// TestRestoreRepoStateMaxParallelZeroKeepsDefault verifies that a zero/unset
// MaxParallelSubagents in repo-state does not override the config default.
func TestRestoreRepoStateMaxParallelZeroKeepsDefault(t *testing.T) {
	ws := "/tmp/test-wakil-maxpar-keep-default"
	// Write repo-state WITHOUT maxpar (it stays 0/omitted).
	_ = updateRepoState(ws, func(s *RepoState) {
		s.Model = "something" // write something so the file exists
	})

	cfg := config.DefaultConfig() // maxpar = 2
	cfg.ExecMode = "direct"
	cfg.WorkDir = ws
	app := &App{Cfg: cfg}

	_ = RestoreRepoState(app)
	if app.Cfg.MaxParallelSubagents != 2 {
		t.Errorf("maxpar should stay at config default 2, got %d", app.Cfg.MaxParallelSubagents)
	}
}

// contains is a local helper to avoid importing strings just for one call.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && findSubstring(s, substr)
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
