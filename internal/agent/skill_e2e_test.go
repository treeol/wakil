package agent

// skill_e2e_test.go holds cross-cutting integration tests for the skill
// capability: CapOrStub routing and global-store visibility across two App
// instances (simulating two sessions).

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/memory"
)

// TestSkillCapOrStub_LoadSkillSpillsFull verifies that load_skill results are
// routed through SpillFullResult (NOT truncated to ToolResultCap) — the same
// treatment read_file_full gets. A skill larger than ToolResultCap must come
// back intact (with a spill pointer), not windowed to 8K.
func TestSkillCapOrStub_LoadSkillSpillsFull(t *testing.T) {
	app := &App{Cfg: config.DefaultConfig()} // ToolResultCap = 8000
	app.Cfg.ToolResultCap = 8000

	big := strings.Repeat("x", 20000) // > 8000
	got := app.CapOrStub(big, "load_skill", 0)

	// Must NOT be truncated to ~8000. SpillFullResult returns full content
	// plus a "[full content at: PATH]" pointer (or just content if small).
	if len(got) < 20000 {
		t.Errorf("load_skill result was capped: len=%d, want >=20000 (should bypass ToolResultCap)", len(got))
	}
	// Sanity: an unprotected tool of the same size IS capped.
	capped := app.CapOrStub(big, "run_shell", 0)
	if len(capped) >= 20000 {
		t.Errorf("run_shell should be capped to ToolResultCap, got len=%d", len(capped))
	}
}

// TestSkillGlobalVisibility verifies the core product claim: two App
// instances opened on the SAME skills.db path (simulating two sessions,
// possibly different workspaces) see each other's skills. This is what makes
// the store GLOBAL rather than workspace-scoped.
func TestSkillGlobalVisibility(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "skills", "skills.db")
	ctx := context.Background()

	// Session 1: main agent saves a skill.
	s1, err := memory.Open(dbPath, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Close()
	app1 := &App{SkillStore: newSkillsProfile(s1), AgentPrefix: "main",
		Confirm: func(_, _, _ string, _ bool) bool { return true }}
	app1.handleSaveSkill(ctx, skillTC("save_skill", `{"key":"shared-runbook","value":"deploy steps"}`))

	// Session 2: a fresh Open on the SAME path (different Store handle).
	s2, err := memory.Open(dbPath, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	app2 := &App{SkillStore: newSkillsProfile(s2), AgentPrefix: "main",
		Confirm: func(_, _, _ string, _ bool) bool { return true }}

	load := app2.handleLoadSkill(ctx, skillTC("load_skill", `{"key":"shared-runbook"}`))
	if !strings.Contains(load, "deploy steps") {
		t.Errorf("session 2 should see session 1's skill (global store): %q", load)
	}
	list := app2.handleListSkills(ctx, skillTC("list_skills", `{}`))
	if !strings.Contains(list, "shared-runbook") {
		t.Errorf("session 2 should list session 1's skill: %q", list)
	}
}
