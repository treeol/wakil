package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/memory"
	"github.com/treeol/wakil/internal/proxy"
)

// skillTestApp creates an App with a real (temp-dir) skills store for testing.
// confirm controls the Confirm gate; pass nil for an auto-approve confirmer.
func skillTestApp(t *testing.T, isSubagent bool, confirm Confirmer) *App {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "skills", "skills.db")

	store, err := memory.Open(dbPath, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	prefix := "main"
	if isSubagent {
		prefix = "sub-abc12345"
	}
	if confirm == nil {
		confirm = func(_, _, _ string, _ bool) bool { return true }
	}

	return &App{
		SkillStore:  newSkillsProfile(store),
		AgentPrefix: prefix,
		IsSubagent:  isSubagent,
		Confirm:     confirm,
	}
}

func skillTC(name, args string) proxy.ToolCall {
	return proxy.ToolCall{Function: proxy.FunctionCall{Name: name, Arguments: args}}
}

// ─── Read tools ─────────────────────────────────────────────────────────────

func TestSkillHandlers_ListEmpty(t *testing.T) {
	app := skillTestApp(t, false, nil)
	res := app.handleListSkills(context.Background(), skillTC("list_skills", `{}`))
	if !strings.Contains(res, "no skills") {
		t.Errorf("empty store should say no skills, got: %q", res)
	}
}

func TestSkillHandlers_SaveListLoad(t *testing.T) {
	app := skillTestApp(t, false, nil)
	ctx := context.Background()

	save := app.handleSaveSkill(ctx, skillTC("save_skill", `{"key":"git-flow","value":"body","description":"How we git"}`))
	if !strings.Contains(save, "saved skill: git-flow") {
		t.Fatalf("save failed: %q", save)
	}

	list := app.handleListSkills(ctx, skillTC("list_skills", `{}`))
	if !strings.Contains(list, "git-flow") || !strings.Contains(list, "How we git") {
		t.Errorf("list missing skill/description: %q", list)
	}

	load := app.handleLoadSkill(ctx, skillTC("load_skill", `{"key":"git-flow"}`))
	if !strings.Contains(load, "How we git") || !strings.Contains(load, "body") {
		t.Errorf("load missing content: %q", load)
	}
	if !strings.Contains(load, "[skill | active |") {
		t.Errorf("load missing provenance header: %q", load)
	}
}

func TestSkillHandlers_LoadMissing(t *testing.T) {
	app := skillTestApp(t, false, nil)
	res := app.handleLoadSkill(context.Background(), skillTC("load_skill", `{"key":"nope"}`))
	if !strings.Contains(res, "not found") {
		t.Errorf("missing key should say not found, got: %q", res)
	}
}

func TestSkillHandlers_Search(t *testing.T) {
	app := skillTestApp(t, false, nil)
	ctx := context.Background()
	app.handleSaveSkill(ctx, skillTC("save_skill", `{"key":"a","value":"conventional commits guide"}`))
	app.handleSaveSkill(ctx, skillTC("save_skill", `{"key":"b","value":"unrelated formatting"}`))

	res := app.handleSkillSearch(ctx, skillTC("skill_search", `{"query":"commits"}`))
	if !strings.Contains(res, "conventional commits") {
		t.Errorf("search should find skill a: %q", res)
	}
}

func TestSkillHandlers_History(t *testing.T) {
	app := skillTestApp(t, false, nil)
	ctx := context.Background()
	app.handleSaveSkill(ctx, skillTC("save_skill", `{"key":"k","value":"v1"}`))
	app.handleUpdateSkill(ctx, skillTC("update_skill", `{"key":"k","value":"v2"}`))

	res := app.handleSkillHistory(ctx, skillTC("skill_history", `{"key":"k"}`))
	if !strings.Contains(res, "2 versions") {
		t.Errorf("history should show 2 versions: %q", res)
	}
}

// ─── Write gating ───────────────────────────────────────────────────────────

func TestSkillHandlers_SaveDuplicateRejected(t *testing.T) {
	app := skillTestApp(t, false, nil)
	ctx := context.Background()
	app.handleSaveSkill(ctx, skillTC("save_skill", `{"key":"dup","value":"v1"}`))
	res := app.handleSaveSkill(ctx, skillTC("save_skill", `{"key":"dup","value":"v2"}`))
	if !strings.Contains(res, "already exists") {
		t.Errorf("duplicate save should say already exists: %q", res)
	}
}

func TestSkillHandlers_UpdateMissingRejected(t *testing.T) {
	app := skillTestApp(t, false, nil)
	res := app.handleUpdateSkill(context.Background(), skillTC("update_skill", `{"key":"ghost","value":"v"}`))
	if !strings.Contains(res, "not found") {
		t.Errorf("update of missing key should say not found: %q", res)
	}
}

func TestSkillHandlers_UpdatePreservesHistory(t *testing.T) {
	app := skillTestApp(t, false, nil)
	ctx := context.Background()
	app.handleSaveSkill(ctx, skillTC("save_skill", `{"key":"k","value":"v1"}`))
	app.handleUpdateSkill(ctx, skillTC("update_skill", `{"key":"k","value":"v2"}`))

	load := app.handleLoadSkill(ctx, skillTC("load_skill", `{"key":"k"}`))
	if !strings.Contains(load, "v2") {
		t.Errorf("active should be v2: %q", load)
	}
	hist := app.handleSkillHistory(ctx, skillTC("skill_history", `{"key":"k"}`))
	if !strings.Contains(hist, "2 versions") {
		t.Errorf("history should have 2 versions after update: %q", hist)
	}
}

func TestSkillHandlers_ForgetTombstones(t *testing.T) {
	app := skillTestApp(t, false, nil)
	ctx := context.Background()
	app.handleSaveSkill(ctx, skillTC("save_skill", `{"key":"gone","value":"v"}`))

	res := app.handleForgetSkill(ctx, skillTC("forget_skill", `{"key":"gone"}`))
	if !strings.Contains(res, "forgotten skill") {
		t.Fatalf("forget failed: %q", res)
	}

	// Gone from list/load/search.
	if list := app.handleListSkills(ctx, skillTC("list_skills", `{}`)); strings.Contains(list, "gone") {
		t.Errorf("forgotten skill still in list: %q", list)
	}
	if load := app.handleLoadSkill(ctx, skillTC("load_skill", `{"key":"gone"}`)); !strings.Contains(load, "not found") {
		t.Errorf("forgotten skill still loadable: %q", load)
	}
	// Still in history.
	if hist := app.handleSkillHistory(ctx, skillTC("skill_history", `{"key":"gone"}`)); !strings.Contains(hist, "gone") {
		t.Errorf("forgotten skill missing from history: %q", hist)
	}
}

func TestSkillHandlers_ConfirmDeclined(t *testing.T) {
	decline := func(_, _, _ string, _ bool) bool { return false }
	app := skillTestApp(t, false, decline)
	res := app.handleSaveSkill(context.Background(), skillTC("save_skill", `{"key":"k","value":"v"}`))
	if !strings.Contains(res, "declined") {
		t.Errorf("declined save should say declined: %q", res)
	}
	// Not saved.
	if list := app.handleListSkills(context.Background(), skillTC("list_skills", `{}`)); strings.Contains(list, "k —") {
		t.Errorf("declined skill should not be saved: %q", list)
	}
}

// TestSkillHandlers_ForgetDoomedNoPrompt verifies the forget ordering fix:
// forgetting a nonexistent (or invalid) key must fail BEFORE Confirm — a
// doomed forget never prompts. Mashūra flagged the original confirm-first bug.
func TestSkillHandlers_ForgetDoomedNoPrompt(t *testing.T) {
	confirmCalled := false
	confirm := func(_, _, _ string, _ bool) bool { confirmCalled = true; return true }
	app := skillTestApp(t, false, confirm)

	// Nonexistent key.
	res := app.handleForgetSkill(context.Background(), skillTC("forget_skill", `{"key":"ghost"}`))
	if !strings.Contains(res, "not found") {
		t.Errorf("forget ghost should say not found: %q", res)
	}
	if confirmCalled {
		t.Error("Confirm was called for a doomed forget — must validate/exist-check first")
	}

	// Invalid key.
	confirmCalled = false
	res = app.handleForgetSkill(context.Background(), skillTC("forget_skill", `{"key":"a/b"}`))
	if !strings.Contains(res, "ERROR") {
		t.Errorf("forget invalid key should error: %q", res)
	}
	if confirmCalled {
		t.Error("Confirm was called for an invalid key")
	}
}

// ─── Subagent rejection (main-agent-only) ───────────────────────────────────

func TestSkillHandlers_SubagentWriteRejected(t *testing.T) {
	app := skillTestApp(t, true, nil) // subagent
	ctx := context.Background()

	for _, tc := range []proxy.ToolCall{
		skillTC("save_skill", `{"key":"k","value":"v"}`),
		skillTC("update_skill", `{"key":"k","value":"v"}`),
		skillTC("forget_skill", `{"key":"k"}`),
	} {
		var res string
		switch tc.Function.Name {
		case "save_skill":
			res = app.handleSaveSkill(ctx, tc)
		case "update_skill":
			res = app.handleUpdateSkill(ctx, tc)
		case "forget_skill":
			res = app.handleForgetSkill(ctx, tc)
		}
		if !strings.Contains(res, "main-agent only") {
			t.Errorf("subagent %s should be denied, got: %q", tc.Function.Name, res)
		}
	}
}

// TestSkillHandlers_SubagentWriteRejectedBeforeConfirm verifies the IsSubagent
// check runs BEFORE Confirm — a toolsConfirmer-style auto-approve (returns true
// unconditionally) must NOT allow a subagent write. This is the ordering bug
// Mashūra flagged: if Confirm ran first, a tools-tier subagent would pass.
func TestSkillHandlers_SubagentWriteRejectedBeforeConfirm(t *testing.T) {
	confirmCalled := false
	autoApprove := func(_, _, _ string, _ bool) bool { confirmCalled = true; return true }
	app := skillTestApp(t, true, autoApprove) // subagent with auto-approve confirmer

	res := app.handleSaveSkill(context.Background(), skillTC("save_skill", `{"key":"k","value":"v"}`))
	if !strings.Contains(res, "main-agent only") {
		t.Errorf("subagent save should be denied even with auto-approve: %q", res)
	}
	if confirmCalled {
		t.Error("Confirm was called for a subagent — IsSubagent check must run first")
	}
}

func TestSkillHandlers_SubagentReadAllowed(t *testing.T) {
	// Seed a skill as main, then confirm a subagent can read it.
	main := skillTestApp(t, false, nil)
	ctx := context.Background()
	// Both apps share the same underlying store via the profile — but
	// skillTestApp opens separate DBs. Save via main's store, read via a
	// subagent App pointing at the SAME store.
	store := main.SkillStore.store
	sub := &App{SkillStore: newSkillsProfile(store), AgentPrefix: "sub-x", IsSubagent: true,
		Confirm: func(_, _, _ string, _ bool) bool { return true }}

	main.handleSaveSkill(ctx, skillTC("save_skill", `{"key":"shared","value":"v"}`))

	if res := sub.handleListSkills(ctx, skillTC("list_skills", `{}`)); !strings.Contains(res, "shared") {
		t.Errorf("subagent should list skills: %q", res)
	}
	if res := sub.handleLoadSkill(ctx, skillTC("load_skill", `{"key":"shared"}`)); !strings.Contains(res, "shared") {
		t.Errorf("subagent should load skills: %q", res)
	}
}

// ─── Store unavailable ──────────────────────────────────────────────────────

func TestSkillHandlers_StoreUnavailable(t *testing.T) {
	app := &App{SkillStore: nil, Confirm: func(_, _, _ string, _ bool) bool { return true }}
	res := app.handleListSkills(context.Background(), skillTC("list_skills", `{}`))
	if res != skillsUnavailable {
		t.Errorf("nil store should return unavailable, got: %q", res)
	}
}
