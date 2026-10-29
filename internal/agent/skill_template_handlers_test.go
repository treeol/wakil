package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/treeol/wakil/internal/exec"
	"github.com/treeol/wakil/internal/workflow"
)

// templateTestApp builds a main-agent App with a real skills store and a real
// DirectExecutor over a temp workspace. confirmCount counts Confirm calls.
func templateTestApp(t *testing.T, approve bool) (*App, string, *int) {
	t.Helper()
	app := skillTestApp(t, false, nil)
	ws := t.TempDir()
	ws, _ = filepath.EvalSymlinks(ws)
	ex, err := exec.NewDirectExecutor(ws)
	if err != nil {
		t.Fatal(err)
	}
	app.Exec = ex
	n := 0
	app.Confirm = func(_, _, _ string, _ bool) bool { n++; return approve }
	return app, ws, &n
}

func writeWS(t *testing.T, ws, rel, content string, mode os.FileMode) {
	t.Helper()
	p := filepath.Join(ws, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func readWS(t *testing.T, ws, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(ws, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const deployBody = "#!/bin/bash\nset -euo pipefail\nSTACK=shop-prod\ndocker service ls --format '{{.Name}}' | grep shop-prod\n"

// captureDeploy captures ws/src/deploy.sh + stack.yml as skill "swarm".
func captureDeploy(t *testing.T, app *App, ws string) string {
	t.Helper()
	writeWS(t, ws, "src/deploy.sh", deployBody, 0o755)
	writeWS(t, ws, "src/stack.yml", "services:\n  shop-prod:\n    image: nginx\n", 0o644)
	args := `{"key":"swarm","paths":["deploy.sh","stack.yml"],"base_dir":"src","description":"Swarm deploy","params":[{"name":"STACK_NAME","value":"shop-prod","pattern":"[a-z0-9-]+"}],"verify":["bash -n deploy.sh"]}`
	return app.handleCaptureSkillTemplate(context.Background(), skillTC("capture_skill_template", args))
}

func TestCaptureThenApplyRoundTrip(t *testing.T) {
	app, ws, confirms := templateTestApp(t, true)
	ctx := context.Background()

	res := captureDeploy(t, app, ws)
	if !strings.Contains(res, "saved template skill: swarm") {
		t.Fatalf("capture: %s", res)
	}
	e, err := app.SkillStore.getActiveSkill(ctx, "swarm")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(e.Value, "shop-prod") {
		t.Error("captured value must not be stored")
	}
	if !strings.Contains(e.Value, "{{.Name}}") {
		t.Error("{{.Name}} must survive capture verbatim")
	}

	// load_skill hides bodies; include_files shows them.
	load := app.handleLoadSkill(ctx, skillTC("load_skill", `{"key":"swarm"}`))
	if strings.Contains(load, "set -euo pipefail") || !strings.Contains(load, "STACK_NAME (required") {
		t.Errorf("load_skill summary wrong:\n%s", load)
	}
	full := app.handleLoadSkill(ctx, skillTC("load_skill", `{"key":"swarm","include_files":true}`))
	if !strings.Contains(full, "set -euo pipefail") {
		t.Error("include_files=true must return bodies")
	}

	// Apply with a new stack name into a fresh dir.
	*confirms = 0
	res = app.handleApplySkillTemplate(ctx, skillTC("apply_skill_template", `{"key":"swarm","params":{"STACK_NAME":"blog"},"target_dir":"out"}`))
	if !strings.Contains(res, "applied template skill swarm") || *confirms != 1 {
		t.Fatalf("apply (confirms=%d): %s", *confirms, res)
	}
	if strings.Contains(res, "set -euo") {
		t.Error("apply result must not echo bodies")
	}
	want := strings.ReplaceAll(deployBody, "shop-prod", "blog")
	if got := readWS(t, ws, "out/deploy.sh"); got != want {
		t.Errorf("deploy.sh = %q, want %q", got, want)
	}
	if info, _ := os.Stat(filepath.Join(ws, "out/deploy.sh")); info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %o, want 755", info.Mode().Perm())
	}
	if !strings.Contains(res, "bash -n deploy.sh") || !strings.Contains(res, "NOT run") {
		t.Error("verify steps must be surfaced as not run")
	}

	// Provenance recorded, workspace-relative.
	var pf provenanceFile
	if err := json.Unmarshal([]byte(readWS(t, ws, provenancePath)), &pf); err != nil {
		t.Fatal(err)
	}
	if len(pf.Files) != 2 || pf.Files[0].Path != "out/deploy.sh" || pf.Files[0].SkillEntryID != e.ID || pf.Files[0].Params["STACK_NAME"] != "blog" {
		t.Errorf("provenance = %+v", pf)
	}

	// Re-apply: no-op, no prompt.
	*confirms = 0
	res = app.handleApplySkillTemplate(ctx, skillTC("apply_skill_template", `{"key":"swarm","params":{"STACK_NAME":"blog"},"target_dir":"out"}`))
	if !strings.HasPrefix(res, "no-op") || *confirms != 0 {
		t.Errorf("re-apply (confirms=%d): %s", *confirms, res)
	}

	// check: unchanged, then modified.
	chk := app.handleCheckSkillTemplate(ctx, skillTC("check_skill_template", `{}`))
	if strings.Count(chk, "unchanged since apply") != 2 || !strings.Contains(chk, "current (no newer version)") {
		t.Errorf("check:\n%s", chk)
	}
	writeWS(t, ws, "out/stack.yml", "edited\n", 0o644)
	chk = app.handleCheckSkillTemplate(ctx, skillTC("check_skill_template", `{"path":"out/stack.yml"}`))
	if !strings.Contains(chk, "locally modified") || strings.Contains(chk, "deploy.sh") {
		t.Errorf("check after edit:\n%s", chk)
	}

	// Overwrite refused without flag, nothing prompted or written.
	*confirms = 0
	res = app.handleApplySkillTemplate(ctx, skillTC("apply_skill_template", `{"key":"swarm","params":{"STACK_NAME":"blog"},"target_dir":"out"}`))
	if !strings.Contains(res, "overwrite=true") || *confirms != 0 || readWS(t, ws, "out/stack.yml") != "edited\n" {
		t.Errorf("overwrite refusal: %s", res)
	}
	res = app.handleApplySkillTemplate(ctx, skillTC("apply_skill_template", `{"key":"swarm","params":{"STACK_NAME":"blog"},"target_dir":"out","overwrite":true}`))
	if !strings.Contains(res, "overwritten    stack.yml") || readWS(t, ws, "out/stack.yml") == "edited\n" {
		t.Errorf("overwrite: %s", res)
	}

	// Newer skill version → check reports drift.
	upd := app.handleUpdateSkill(ctx, skillTC("update_skill", `{"key":"swarm","value":"Swarm deploy\n\nnew prose"}`))
	if !strings.Contains(upd, "preserved") {
		t.Fatalf("update: %s", upd)
	}
	chk = app.handleCheckSkillTemplate(ctx, skillTC("check_skill_template", `{"key":"swarm"}`))
	if !strings.Contains(chk, "newer skill version exists") {
		t.Errorf("check drift:\n%s", chk)
	}
}

func TestApplySkillTemplateModeOnlyChange(t *testing.T) {
	app, ws, _ := templateTestApp(t, true)
	ctx := context.Background()
	captureDeploy(t, app, ws)
	apply := `{"key":"swarm","params":{"STACK_NAME":"x-1"},"target_dir":"m"}`
	app.handleApplySkillTemplate(ctx, skillTC("apply_skill_template", apply))
	if err := os.Chmod(filepath.Join(ws, "m/deploy.sh"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := app.handleApplySkillTemplate(ctx, skillTC("apply_skill_template", apply))
	if !strings.Contains(res, "overwrite=true") {
		t.Fatalf("mode-only drift must need overwrite: %s", res)
	}
	res = app.handleApplySkillTemplate(ctx, skillTC("apply_skill_template", `{"key":"swarm","params":{"STACK_NAME":"x-1"},"target_dir":"m","overwrite":true}`))
	if !strings.Contains(res, "mode changed") {
		t.Errorf("mode-only apply result: %s", res)
	}
	if info, _ := os.Stat(filepath.Join(ws, "m/deploy.sh")); info.Mode().Perm() != 0o755 {
		t.Errorf("mode not restored: %o", info.Mode().Perm())
	}
}

func TestApplySkillTemplateDeclinedAndDenied(t *testing.T) {
	app, ws, _ := templateTestApp(t, true)
	ctx := context.Background()
	captureDeploy(t, app, ws)

	app.Confirm = func(_, _, _ string, _ bool) bool { return false }
	res := app.handleApplySkillTemplate(ctx, skillTC("apply_skill_template", `{"key":"swarm","params":{"STACK_NAME":"abc"},"target_dir":"d"}`))
	if res != "[declined by user]" {
		t.Errorf("declined: %s", res)
	}
	if _, err := os.Stat(filepath.Join(ws, "d")); !os.IsNotExist(err) {
		t.Error("declined apply must write nothing")
	}

	app.IsSubagent = true
	called := false
	app.Confirm = func(_, _, _ string, _ bool) bool { called = true; return true }
	for _, name := range []string{"apply_skill_template", "capture_skill_template"} {
		var got string
		if name == "apply_skill_template" {
			got = app.handleApplySkillTemplate(ctx, skillTC(name, `{"key":"swarm"}`))
		} else {
			got = app.handleCaptureSkillTemplate(ctx, skillTC(name, `{"key":"x","paths":["a"]}`))
		}
		if got != skillTemplateMainOnlyDenied {
			t.Errorf("%s as subagent: %s", name, got)
		}
	}
	if called {
		t.Error("subagent denial must happen before Confirm")
	}
}

func TestApplySkillTemplateErrors(t *testing.T) {
	app, ws, confirms := templateTestApp(t, true)
	ctx := context.Background()
	captureDeploy(t, app, ws)
	*confirms = 0
	cases := map[string]string{
		"missing param":   `{"key":"swarm","target_dir":"e"}`,
		"pattern":         `{"key":"swarm","params":{"STACK_NAME":"Bad Name"},"target_dir":"e"}`,
		"outside ws":      `{"key":"swarm","params":{"STACK_NAME":"abc"},"target_dir":"../.."}`,
		"no such skill":   `{"key":"nope","params":{}}`,
		"plain skill":     `{"key":"plain"}`,
		"unknown param":   `{"key":"swarm","params":{"STACK_NAME":"abc","EXTRA":"1"}}`,
		"reserved target": `{"key":"swarm","params":{"STACK_NAME":"abc"},"target_dir":".wakil/../.."}`,
	}
	app.handleSaveSkill(ctx, skillTC("save_skill", `{"key":"plain","value":"just prose"}`))
	*confirms = 0
	for name, args := range cases {
		if res := app.handleApplySkillTemplate(ctx, skillTC("apply_skill_template", args)); !strings.HasPrefix(res, "ERROR") {
			t.Errorf("%s: expected ERROR, got %s", name, res)
		}
	}
	if *confirms != 0 {
		t.Errorf("failed applies must not prompt (confirms=%d)", *confirms)
	}
}

func TestApplySkillTemplateSymlinkEscape(t *testing.T) {
	app, ws, _ := templateTestApp(t, true)
	ctx := context.Background()
	captureDeploy(t, app, ws)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(ws, "link")); err != nil {
		t.Fatal(err)
	}
	res := app.handleApplySkillTemplate(ctx, skillTC("apply_skill_template", `{"key":"swarm","params":{"STACK_NAME":"abc"},"target_dir":"link/sub"}`))
	if !strings.HasPrefix(res, "ERROR") {
		t.Fatalf("symlinked target must be rejected: %s", res)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Error("nothing may be written outside the workspace")
	}
}

func TestApplySkillTemplateChangedDuringConfirm(t *testing.T) {
	app, ws, _ := templateTestApp(t, true)
	ctx := context.Background()
	captureDeploy(t, app, ws)
	app.Confirm = func(_, _, _ string, _ bool) bool {
		writeWS(t, ws, "c/stack.yml", "sneaky\n", 0o644)
		return true
	}
	res := app.handleApplySkillTemplate(ctx, skillTC("apply_skill_template", `{"key":"swarm","params":{"STACK_NAME":"abc"},"target_dir":"c"}`))
	if !strings.Contains(res, "changed while the confirm was open") {
		t.Fatalf("expected change detection: %s", res)
	}
	if _, err := os.Stat(filepath.Join(ws, "c/deploy.sh")); !os.IsNotExist(err) {
		t.Error("nothing may be written after change detection")
	}
}

func TestApplySkillTemplateCorruptProvenance(t *testing.T) {
	app, ws, _ := templateTestApp(t, true)
	ctx := context.Background()
	captureDeploy(t, app, ws)
	writeWS(t, ws, provenancePath, "{broken", 0o644)
	res := app.handleApplySkillTemplate(ctx, skillTC("apply_skill_template", `{"key":"swarm","params":{"STACK_NAME":"abc"},"target_dir":"p"}`))
	if !strings.Contains(res, "not valid JSON") || readWS(t, ws, provenancePath) != "{broken" {
		t.Errorf("corrupt provenance must block and be kept: %s", res)
	}
	if chk := app.handleCheckSkillTemplate(ctx, skillTC("check_skill_template", `{}`)); !strings.HasPrefix(chk, "ERROR") {
		t.Errorf("check on corrupt provenance: %s", chk)
	}
}

func TestCheckSkillTemplateRejectsTraversalRecord(t *testing.T) {
	app, ws, _ := templateTestApp(t, true)
	writeWS(t, ws, provenancePath, `{"version":1,"files":[{"path":"../../etc/passwd","skill":"x","skill_entry_id":1,"sha256":"00","mode":"0644"}]}`, 0o644)
	chk := app.handleCheckSkillTemplate(context.Background(), skillTC("check_skill_template", `{}`))
	if !strings.Contains(chk, "INVALID record") {
		t.Errorf("traversal record must be flagged: %s", chk)
	}
}

func TestCaptureSkillTemplateErrors(t *testing.T) {
	app, ws, confirms := templateTestApp(t, true)
	ctx := context.Background()
	writeWS(t, ws, "a.txt", "hello world\n", 0o644)
	writeWS(t, ws, "tok.txt", "x @@ALREADY@@ y\n", 0o644)
	writeWS(t, ws, "big.txt", strings.Repeat("x", captureMaxFileBytes+1), 0o644)
	writeWS(t, ws, "sec.txt", "password = hunter2hunter2\n", 0o644)
	cases := map[string]string{
		"short value":     `{"key":"k","paths":["a.txt"],"description":"d","params":[{"name":"P","value":"he"}]}`,
		"unused value":    `{"key":"k","paths":["a.txt"],"description":"d","params":[{"name":"P","value":"absent"}]}`,
		"existing tokens": `{"key":"k","paths":["tok.txt"],"description":"d"}`,
		"too big":         `{"key":"k","paths":["big.txt"],"description":"d"}`,
		"no description":  `{"key":"k","paths":["a.txt"]}`,
		"outside":         `{"key":"k","paths":["../x"],"description":"d"}`,
		"secret":          `{"key":"k","paths":["sec.txt"],"description":"d"}`,
		"dup values":      `{"key":"k","paths":["a.txt"],"description":"d","params":[{"name":"P","value":"hello"},{"name":"Q","value":"hello"}]}`,
	}
	for name, args := range cases {
		if res := app.handleCaptureSkillTemplate(ctx, skillTC("capture_skill_template", args)); !strings.HasPrefix(res, "ERROR") {
			t.Errorf("%s: expected ERROR, got %s", name, res)
		}
	}
	if *confirms != 0 {
		t.Errorf("failed captures must not prompt (confirms=%d)", *confirms)
	}
}

func TestCaptureSkillTemplateUpdateKeepsProse(t *testing.T) {
	app, ws, _ := templateTestApp(t, true)
	ctx := context.Background()
	app.handleSaveSkill(ctx, skillTC("save_skill", `{"key":"swarm","value":"Swarm conventions\n\nWhy: pitfalls here."}`))
	res := captureDeploy(t, app, ws)
	if !strings.Contains(res, "updated template skill") {
		t.Fatalf("capture update: %s", res)
	}
	e, _ := app.SkillStore.getActiveSkill(ctx, "swarm")
	if !strings.HasPrefix(e.Value, "Swarm conventions\n\nWhy: pitfalls here.\n\n"+tmplSectionMarker) {
		t.Errorf("prose not kept:\n%s", e.Value)
	}
}

func TestUpdateSkillTemplatePreservation(t *testing.T) {
	app, ws, _ := templateTestApp(t, true)
	ctx := context.Background()
	captureDeploy(t, app, ws)
	orig, _ := app.SkillStore.getActiveSkill(ctx, "swarm")
	_, section, _ := splitSkillTemplate(orig.Value)

	res := app.handleUpdateSkill(ctx, skillTC("update_skill", `{"key":"swarm","value":"Better prose"}`))
	if !strings.Contains(res, "preserved") {
		t.Fatalf("update: %s", res)
	}
	e, _ := app.SkillStore.getActiveSkill(ctx, "swarm")
	if _, got, _ := splitSkillTemplate(e.Value); got != section {
		t.Error("section must be preserved byte-for-byte")
	}
	// Same prose again → no-op (no spurious history version).
	if res := app.handleUpdateSkill(ctx, skillTC("update_skill", `{"key":"swarm","value":"Better prose"}`)); !strings.HasPrefix(res, "no-op") {
		t.Errorf("repeat update should be a no-op: %s", res)
	}
	// Explicit drop.
	res = app.handleUpdateSkill(ctx, skillTC("update_skill", `{"key":"swarm","value":"Better prose","drop_template":true}`))
	if !strings.Contains(res, "REMOVED") {
		t.Fatalf("drop: %s", res)
	}
	e, _ = app.SkillStore.getActiveSkill(ctx, "swarm")
	if _, _, has := splitSkillTemplate(e.Value); has {
		t.Error("drop_template must remove the section")
	}
	// Invalid section rejected on save.
	bad := app.handleSaveSkill(ctx, skillTC("save_skill", `{"key":"bad","value":"p\n\n`+tmplSectionMarker+`\n{oops}\n"}`))
	if !strings.Contains(bad, "invalid template section") {
		t.Errorf("save with broken section: %s", bad)
	}
}

// TestSkillTemplateGating: apply/capture are never auto-approved, check is
// never suspended, and apply is blocked before plan approval in workflows.
func TestSkillTemplateGating(t *testing.T) {
	app := &App{}
	app.SetConsent(ConsentSnapshot{AutoApprove: true, AllowDestructive: true})
	for _, n := range []string{"apply_skill_template", "capture_skill_template"} {
		if SuspendAuto(n, app, "detail") == "" {
			t.Errorf("SuspendAuto(%q) must carve out /auto", n)
		}
	}
	if SuspendAuto("check_skill_template", app, "detail") != "" {
		t.Error("check_skill_template must not be suspended")
	}

	app.Workflow = &workflow.WorkflowState{Phase: workflow.WFGather}
	if msg := app.wfPhaseBlock("apply_skill_template", `{"key":"x"}`); msg == "" {
		t.Error("apply_skill_template must be blocked in pre-implementation phases")
	}
	if msg := app.wfPhaseBlock("check_skill_template", `{}`); msg != "" {
		t.Errorf("check_skill_template must not be phase-blocked: %s", msg)
	}
}

func TestApplySkillTemplateReservedTargetDir(t *testing.T) {
	app, ws, confirms := templateTestApp(t, true)
	ctx := context.Background()
	captureDeploy(t, app, ws)
	if err := os.MkdirAll(filepath.Join(ws, ".wakil"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(ws, ".wakil"), filepath.Join(ws, "alias")); err != nil {
		t.Fatal(err)
	}
	*confirms = 0
	for _, dir := range []string{".wakil", ".wakil/sub", "alias"} {
		res := app.handleApplySkillTemplate(ctx, skillTC("apply_skill_template", `{"key":"swarm","params":{"STACK_NAME":"abc"},"target_dir":"`+dir+`"}`))
		if !strings.Contains(res, "reserved") {
			t.Errorf("target_dir %q must be rejected as reserved: %s", dir, res)
		}
	}
	if *confirms != 0 {
		t.Error("reserved targets must be rejected before confirm")
	}
}

func TestApplySkillTemplateRetargetedSymlinkDuringConfirm(t *testing.T) {
	app, ws, _ := templateTestApp(t, true)
	ctx := context.Background()
	captureDeploy(t, app, ws)
	for _, d := range []string{"A", "B"} {
		if err := os.MkdirAll(filepath.Join(ws, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(ws, "cur")
	if err := os.Symlink(filepath.Join(ws, "A"), link); err != nil {
		t.Fatal(err)
	}
	app.Confirm = func(_, _, _ string, _ bool) bool {
		_ = os.Remove(link)
		_ = os.Symlink(filepath.Join(ws, "B"), link)
		return true
	}
	res := app.handleApplySkillTemplate(ctx, skillTC("apply_skill_template", `{"key":"swarm","params":{"STACK_NAME":"abc"},"target_dir":"cur"}`))
	if !strings.Contains(res, "changed while the confirm was open") {
		t.Fatalf("retarget must be detected: %s", res)
	}
	for _, d := range []string{"A", "B"} {
		if entries, _ := os.ReadDir(filepath.Join(ws, d)); len(entries) != 0 {
			t.Errorf("%s must stay empty", d)
		}
	}
}

func TestApplySkillTemplateRejectsNonRegularTarget(t *testing.T) {
	app, ws, _ := templateTestApp(t, true)
	ctx := context.Background()
	captureDeploy(t, app, ws)
	if err := os.MkdirAll(filepath.Join(ws, "d/deploy.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	res := app.handleApplySkillTemplate(ctx, skillTC("apply_skill_template", `{"key":"swarm","params":{"STACK_NAME":"abc"},"target_dir":"d","overwrite":true}`))
	if !strings.Contains(res, "not a regular file") {
		t.Errorf("directory target: %s", res)
	}
	if err := syscall.Mkfifo(filepath.Join(ws, "f-deploy"), 0o644); err != nil {
		t.Skip("mkfifo unavailable")
	}
	if err := os.MkdirAll(filepath.Join(ws, "f"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(ws, "f-deploy"), filepath.Join(ws, "f/deploy.sh")); err != nil {
		t.Fatal(err)
	}
	res = app.handleApplySkillTemplate(ctx, skillTC("apply_skill_template", `{"key":"swarm","params":{"STACK_NAME":"abc"},"target_dir":"f","overwrite":true}`))
	if !strings.Contains(res, "not a regular file") {
		t.Errorf("FIFO target must be rejected without blocking: %s", res)
	}
}

func TestCaptureSkillTemplateKeepsProseBytes(t *testing.T) {
	app, ws, _ := templateTestApp(t, true)
	ctx := context.Background()
	prose := "Swarm conventions\n\nindented   \n  trailing spaces  \n\n\n"
	b, _ := json.Marshal(map[string]string{"key": "swarm", "value": prose})
	app.handleSaveSkill(ctx, skillTC("save_skill", string(b)))
	captureDeploy(t, app, ws)
	e, _ := app.SkillStore.getActiveSkill(ctx, "swarm")
	if !strings.HasPrefix(e.Value, prose+tmplSectionMarker) {
		t.Errorf("prose must be kept byte-for-byte:\n%q", e.Value)
	}
	// Recapture: bytes before the section unchanged.
	captureDeploy(t, app, ws)
	e2, _ := app.SkillStore.getActiveSkill(ctx, "swarm")
	if e2.Value != e.Value {
		t.Error("identical recapture must be a byte-identical no-op")
	}
}

// TestConfineForCreateNoEscape: the missing-ancestor fallback must never
// produce a path outside the workspace.
func TestConfineForCreateNoEscape(t *testing.T) {
	app, ws, _ := templateTestApp(t, true)
	ctx := context.Background()
	outside := t.TempDir()
	if err := os.Symlink(filepath.Join(outside, "gone"), filepath.Join(ws, "dangling")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"..", "../x", ".wakil/../..", "a/../../b", "/", outside + "/x", "dangling/x/y", "dangling"} {
		if got, err := app.confineForCreate(ctx, p); err == nil && !isWithin(got, ws) {
			t.Errorf("confineForCreate(%q) = %q escapes workspace %q", p, got, ws)
		} else if err == nil && p != "/" {
			t.Errorf("confineForCreate(%q) = %q, want an error", p, got)
		}
	}
	if got, err := app.confineForCreate(ctx, "new/deep/file"); err != nil || got != filepath.Join(ws, "new/deep/file") {
		t.Errorf("confineForCreate(new/deep/file) = %q, %v", got, err)
	}
}

func TestApplySkillTemplateCanonicalCollision(t *testing.T) {
	app, ws, confirms := templateTestApp(t, true)
	ctx := context.Background()
	// Template with two files that collide only after symlink resolution.
	tmpl := &skillTemplate{Files: []templateFile{{Path: "real/x", Body: "one\n"}, {Path: "alias/x", Body: "two\n"}}}
	value, err := joinSkillTemplate("collide", tmpl)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]string{"key": "collide", "value": value})
	app.handleSaveSkill(ctx, skillTC("save_skill", string(b)))
	if err := os.MkdirAll(filepath.Join(ws, "out/real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(ws, "out/real"), filepath.Join(ws, "out/alias")); err != nil {
		t.Fatal(err)
	}
	*confirms = 0
	res := app.handleApplySkillTemplate(ctx, skillTC("apply_skill_template", `{"key":"collide","target_dir":"out"}`))
	if !strings.Contains(res, "resolve to the same file") || *confirms != 0 {
		t.Errorf("canonical collision must be rejected before confirm: %s", res)
	}
}

func TestApplySkillTemplateSymlinkedProvenanceRefused(t *testing.T) {
	app, ws, _ := templateTestApp(t, true)
	ctx := context.Background()
	captureDeploy(t, app, ws)
	writeWS(t, ws, "decoy.json", `{"version":1,"files":[]}`, 0o644)
	if err := os.MkdirAll(filepath.Join(ws, ".wakil"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(ws, "decoy.json"), filepath.Join(ws, provenancePath)); err != nil {
		t.Fatal(err)
	}
	res := app.handleApplySkillTemplate(ctx, skillTC("apply_skill_template", `{"key":"swarm","params":{"STACK_NAME":"abc"},"target_dir":"p"}`))
	if !strings.Contains(res, "symlinked") {
		t.Errorf("symlinked provenance must be refused: %s", res)
	}
}

func TestCheckSkillTemplateFIFONoHang(t *testing.T) {
	app, ws, _ := templateTestApp(t, true)
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Join(ws, ".wakil"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(ws, "pipe"), 0o644); err != nil {
		t.Skip("mkfifo unavailable")
	}
	writeWS(t, ws, provenancePath, `{"version":1,"files":[{"path":"pipe","skill":"x","skill_entry_id":1,"sha256":"00"}]}`, 0o644)
	if chk := app.handleCheckSkillTemplate(ctx, skillTC("check_skill_template", `{}`)); !strings.Contains(chk, "not a regular file") {
		t.Errorf("FIFO record must be reported without reading: %s", chk)
	}
	if err := os.Remove(filepath.Join(ws, provenancePath)); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(ws, provenancePath), 0o644); err != nil {
		t.Fatal(err)
	}
	if chk := app.handleCheckSkillTemplate(ctx, skillTC("check_skill_template", `{}`)); !strings.Contains(chk, "not a regular file") {
		t.Errorf("FIFO provenance must be rejected without reading: %s", chk)
	}
}

func TestCaptureSkillTemplatePatternMismatch(t *testing.T) {
	app, ws, confirms := templateTestApp(t, true)
	writeWS(t, ws, "a.txt", "name: Shop_Prod\n", 0o644)
	res := app.handleCaptureSkillTemplate(context.Background(), skillTC("capture_skill_template",
		`{"key":"k","paths":["a.txt"],"description":"d","params":[{"name":"N","value":"Shop_Prod","pattern":"[a-z-]+"}]}`))
	if !strings.Contains(res, "does not fully match") || *confirms != 0 {
		t.Errorf("captured value violating its pattern: %s", res)
	}
}

func TestSplitSkillTemplateCRLF(t *testing.T) {
	v := "prose\r\n\r\n" + tmplSectionMarker + "\r\n{}\r\n"
	prose, section, found := splitSkillTemplate(v)
	if !found || prose != "prose\r\n\r\n" || !strings.HasPrefix(section, tmplSectionMarker) {
		t.Errorf("CRLF marker not recognized: found=%v prose=%q", found, prose)
	}
	if err := validateSkillTemplateValue(v); err == nil {
		t.Error("a CRLF section with an invalid manifest must be rejected, not stored as prose")
	}
}

// noModeExec hides the FileModer capability of a real executor.
type noModeExec struct{ exec.Executor }

func TestApplySkillTemplateWithoutFileModer(t *testing.T) {
	app, ws, _ := templateTestApp(t, true)
	ctx := context.Background()
	captureDeploy(t, app, ws)
	app.Exec = noModeExec{app.Exec}
	res := app.handleApplySkillTemplate(ctx, skillTC("apply_skill_template", `{"key":"swarm","params":{"STACK_NAME":"abc"},"target_dir":"n"}`))
	if !strings.Contains(res, "created") || !strings.Contains(res, "modes were not applied") {
		t.Fatalf("apply without FileModer: %s", res)
	}
	var pf provenanceFile
	if err := json.Unmarshal([]byte(readWS(t, ws, provenancePath)), &pf); err != nil {
		t.Fatal(err)
	}
	for _, r := range pf.Files {
		if r.Mode != "" {
			t.Errorf("mode must not be recorded when unmanaged: %+v", r)
		}
	}
	res = app.handleApplySkillTemplate(ctx, skillTC("apply_skill_template", `{"key":"swarm","params":{"STACK_NAME":"abc"},"target_dir":"n"}`))
	if !strings.HasPrefix(res, "no-op") {
		t.Errorf("re-apply should compare bytes only: %s", res)
	}
}
