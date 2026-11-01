package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/memory"
	"github.com/treeol/wakil/internal/proxy"
)

const scPlanJSON = `{"reusable":true,"key":"shop-swarm","description":"Swarm stack + deploy script",
"guidance":"Use for Swarm shops. Run bash -n deploy.sh first.",
"params":[{"name":"STACK_NAME","value":"shop-prod","pattern":"[a-z0-9-]+"}],
"templates":[{"name":"stack","description":"stack file","paths":["stack.yml"]},
             {"name":"deploy-script","description":"deploy","requires":["stack"],"verify":["bash -n deploy.sh"],"paths":["deploy.sh"]}]}`

func scApp(t *testing.T) (*App, string) {
	app, ws, _ := templateTestApp(t, true)
	writeWS(t, ws, "ops/deploy.sh", deployBody, 0o755)
	writeWS(t, ws, "ops/stack.yml", "services:\n  shop-prod:\n    image: nginx\n", 0o644)
	app.noteSkillFileWritten(ws + "/ops/deploy.sh")
	app.noteSkillFileWritten(ws + "/ops/stack.yml")
	return app, ws
}

func scHooks(app *App, plan string, reviews ...string) *int32 {
	var calls int32
	app.skillCreate.hooks = skillCreationHooks{
		plan: func(context.Context, string) (string, error) { return plan, nil },
		review: func(_ context.Context, _, briefing string) ([]skillReviewResult, string, error) {
			i := int(atomic.AddInt32(&calls, 1)) - 1
			if i >= len(reviews) {
				return nil, "", errors.New("unexpected extra review")
			}
			return []skillReviewResult{{Model: "m1", Answer: reviews[i]}}, "panel", nil
		},
	}
	return &calls
}

func scRun(app *App, mode string) skillCreationOutcome {
	return app.runSkillCreation(context.Background(), skillCreationRequest{Mode: mode})
}

func TestSkillPipeline_AutoModeSavesDraft(t *testing.T) {
	app, _ := scApp(t)
	scHooks(app, scPlanJSON)
	out := scRun(app, config.SkillCreationAuto)
	if out.Kind != "draft" || out.Key != "shop-swarm" {
		t.Fatalf("outcome = %+v", out)
	}
	if _, err := app.getSkillStore().getActiveSkill(context.Background(), "shop-swarm"); err == nil {
		t.Fatal("auto mode must not activate")
	}
	drafts, _ := app.getSkillStore().listSkillDrafts(context.Background())
	if len(drafts) != 1 {
		t.Fatalf("drafts = %d", len(drafts))
	}
	v := drafts[0].Value
	if !strings.Contains(v, "@@STACK_NAME@@") || strings.Contains(v, "shop-prod") {
		t.Errorf("param not applied:\n%s", v)
	}
	if err := validateSkillTemplateValue(v); err != nil {
		t.Errorf("draft invalid: %v", err)
	}
	// {{ }} style content and shebang bytes must pass through exactly.
	if !strings.Contains(v, "docker service ls --format '{{.Name}}'") {
		t.Errorf("body not preserved:\n%s", v)
	}
}

func TestSkillPipeline_MashuraApproveActivatesUnconfirmed(t *testing.T) {
	app, _ := scApp(t)
	calls := scHooks(app, scPlanJSON, `{"verdict":"approve","reasons":["ok"]}`)
	out := scRun(app, config.SkillCreationAutoMashura)
	if out.Kind != "active" {
		t.Fatalf("outcome = %+v", out)
	}
	if *calls != 1 {
		t.Errorf("review calls = %d", *calls)
	}
	e, err := app.getSkillStore().getActiveSkill(context.Background(), "shop-swarm")
	if err != nil {
		t.Fatal(err)
	}
	if !isUnconfirmedAutoSkill(e) || e.Writer != skillWriterAutoMashura || !strings.Contains(e.Note, "digest="+skillDigest(e.Value)) {
		t.Errorf("entry meta wrong: writer=%q note=%q", e.Writer, e.Note)
	}
}

func TestSkillPipeline_ChangesThenApprove(t *testing.T) {
	app, _ := scApp(t)
	calls := scHooks(app, scPlanJSON,
		`{"verdict":"changes","reasons":["better description"],"changes":{"description":"Better description line"}}`,
		`{"verdict":"approve","reasons":[]}`)
	out := scRun(app, config.SkillCreationAutoMashura)
	if out.Kind != "active" || *calls != 2 {
		t.Fatalf("outcome=%+v calls=%d", out, *calls)
	}
	e, _ := app.getSkillStore().getActiveSkill(context.Background(), "shop-swarm")
	if !strings.HasPrefix(e.Value, "Better description line\n") {
		t.Errorf("change not applied: %q", firstLine(e.Value))
	}
}

func TestSkillPipeline_ChangesThenNotApprovedFallsBackToDraft(t *testing.T) {
	app, _ := scApp(t)
	scHooks(app, scPlanJSON,
		`{"verdict":"changes","changes":{"guidance":"New guidance"}}`,
		`{"verdict":"changes","changes":{"guidance":"Again"}}`)
	out := scRun(app, config.SkillCreationAutoMashura)
	if out.Kind != "draft" {
		t.Fatalf("outcome = %+v", out)
	}
	if _, err := app.getSkillStore().getActiveSkill(context.Background(), "shop-swarm"); err == nil {
		t.Fatal("must not be active")
	}
}

func TestSkillPipeline_RejectAndReviewFailureFallBack(t *testing.T) {
	for name, review := range map[string]string{
		"reject":      `{"verdict":"reject","reasons":["one-off"]}`,
		"unparseable": `looks fine to me`,
	} {
		t.Run(name, func(t *testing.T) {
			app, _ := scApp(t)
			scHooks(app, scPlanJSON, review)
			if out := scRun(app, config.SkillCreationAutoMashura); out.Kind != "draft" {
				t.Fatalf("outcome = %+v", out)
			}
		})
	}
	t.Run("review error", func(t *testing.T) {
		app, _ := scApp(t)
		app.skillCreate.hooks = skillCreationHooks{
			plan: func(context.Context, string) (string, error) { return scPlanJSON, nil },
			review: func(context.Context, string, string) ([]skillReviewResult, string, error) {
				return nil, "", errors.New("boom")
			},
		}
		if out := scRun(app, config.SkillCreationAutoMashura); out.Kind != "draft" {
			t.Fatalf("outcome = %+v", out)
		}
	})
}

func TestSkillPipeline_ExistingKeyBecomesDraftWithoutReview(t *testing.T) {
	app, _ := scApp(t)
	if _, err := app.getSkillStore().putActiveSkill(context.Background(), "shop-swarm", "mine", "main", "s", memory.TaintUnknown, false, ""); err != nil {
		t.Fatal(err)
	}
	calls := scHooks(app, scPlanJSON, `{"verdict":"approve"}`)
	out := scRun(app, config.SkillCreationAutoMashura)
	if out.Kind != "draft" || *calls != 0 {
		t.Fatalf("outcome=%+v calls=%d", out, *calls)
	}
	if e, _ := app.getSkillStore().getActiveSkill(context.Background(), "shop-swarm"); e.Value != "mine" {
		t.Fatal("existing skill modified")
	}
}

func TestSkillPipeline_SecretAbortsBeforeAnyExternalCall(t *testing.T) {
	app, ws := scApp(t)
	writeWS(t, ws, "ops/extra.sh", "#!/bin/sh\nexport api_key=abcdef123456\n", 0o755)
	app.noteSkillFileWritten(ws + "/ops/extra.sh")
	var planCalls, reviewCalls int32
	app.skillCreate.hooks = skillCreationHooks{
		plan: func(context.Context, string) (string, error) { atomic.AddInt32(&planCalls, 1); return scPlanJSON, nil },
		review: func(context.Context, string, string) ([]skillReviewResult, string, error) {
			atomic.AddInt32(&reviewCalls, 1)
			return nil, "", nil
		},
	}
	out := scRun(app, config.SkillCreationAutoMashura)
	if out.Kind != "aborted" || !strings.Contains(out.Text, "secret") {
		t.Fatalf("outcome = %+v", out)
	}
	if planCalls != 0 || reviewCalls != 0 {
		t.Fatalf("external calls made: plan=%d review=%d", planCalls, reviewCalls)
	}
	if drafts, _ := app.getSkillStore().listSkillDrafts(context.Background()); len(drafts) != 0 {
		t.Fatal("nothing may be persisted")
	}
}

func TestSkillPipeline_LeftoverIPAborts(t *testing.T) {
	app, ws := scApp(t)
	writeWS(t, ws, "ops/deploy.sh", "#!/bin/sh\nssh deploy@203.0.113.7 true\n", 0o755)
	// Plan declares no param for the IP → leftover scan must stop the run, even
	// the deterministic fallback.
	var reviewCalls int32
	app.skillCreate.hooks = skillCreationHooks{
		plan: func(context.Context, string) (string, error) { return scPlanJSON, nil },
		review: func(context.Context, string, string) ([]skillReviewResult, string, error) {
			atomic.AddInt32(&reviewCalls, 1)
			return nil, "", nil
		},
	}
	out := scRun(app, config.SkillCreationAutoMashura)
	if out.Kind != "aborted" || !strings.Contains(out.Text, "IPv4") || reviewCalls != 0 {
		t.Fatalf("outcome=%+v reviewCalls=%d", out, reviewCalls)
	}
}

func TestSkillPipeline_IPBecomesParam(t *testing.T) {
	app, ws := scApp(t)
	writeWS(t, ws, "ops/deploy.sh", "#!/bin/sh\nssh deploy@203.0.113.7 true\n", 0o755)
	plan := `{"reusable":true,"key":"ssh-deploy","description":"d","params":[{"name":"REMOTE_IP","value":"203.0.113.7"}],"templates":[]}`
	scHooks(app, plan)
	out := scRun(app, config.SkillCreationAuto)
	if out.Kind != "draft" {
		t.Fatalf("outcome = %+v", out)
	}
	drafts, _ := app.getSkillStore().listSkillDrafts(context.Background())
	if strings.Contains(drafts[0].Value, "203.0.113.7") || !strings.Contains(drafts[0].Value, "@@REMOTE_IP@@") {
		t.Errorf("IP leaked:\n%s", drafts[0].Value)
	}
}

func TestSkillPipeline_BadPlanFallsBackDeterministic(t *testing.T) {
	app, _ := scApp(t)
	scHooks(app, "not json at all")
	out := scRun(app, config.SkillCreationAuto)
	if out.Kind != "draft" {
		t.Fatalf("outcome = %+v", out)
	}
	drafts, _ := app.getSkillStore().listSkillDrafts(context.Background())
	if err := validateSkillTemplateValue(drafts[0].Value); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(drafts[0].Key, "captured-") {
		t.Errorf("fallback key = %q", drafts[0].Key)
	}
}

func TestSkillPipeline_OneOffPlanCreatesNothing(t *testing.T) {
	app, _ := scApp(t)
	scHooks(app, `{"reusable":false}`)
	out := scRun(app, config.SkillCreationAuto)
	if out.Kind != "aborted" {
		t.Fatalf("outcome = %+v", out)
	}
	// ...but a manual run still builds one, and the daily budget was released.
	app.skillCreate.hooks.plan = func(context.Context, string) (string, error) { return `{"reusable":false}`, nil }
	if out := app.runSkillCreation(context.Background(), skillCreationRequest{Mode: config.SkillCreationAuto, Manual: true}); out.Kind != "draft" {
		t.Fatalf("manual outcome = %+v", out)
	}
}

func TestSkillPipeline_Limits(t *testing.T) {
	app, _ := scApp(t)
	app.Cfg.SkillCreation.MaxAutoPerDay = 1
	scHooks(app, scPlanJSON)
	if out := scRun(app, config.SkillCreationAuto); out.Kind != "draft" {
		t.Fatalf("first = %+v", out)
	}
	if out := scRun(app, config.SkillCreationAuto); out.Kind != "aborted" || !strings.Contains(out.Text, "daily limit") {
		t.Fatalf("second = %+v", out)
	}
	app.Cfg.SkillCreation.MaxAutoPerDay = 5
	app.Cfg.SkillCreation.MaxDrafts = 1
	if out := scRun(app, config.SkillCreationAuto); out.Kind != "aborted" || !strings.Contains(out.Text, "pending drafts") {
		t.Fatalf("draft cap = %+v", out)
	}
}

func TestSkillPipeline_DenylistAndAllowlist(t *testing.T) {
	cfg := config.SkillCreationConfig{}
	for _, c := range []struct {
		name string
		ok   bool
	}{{"deploy.sh", true}, {"stack.yml", true}, {"Dockerfile", true}, {"Dockerfile.prod", true}, {".env", false}, {"prod.env", false},
		{"server.pem", false}, {"id_rsa", false}, {"secrets.yml", false}, {"main.go", false}, {"token.sh", false}} {
		if got := skillFileAllowed(cfg, c.name); got != c.ok {
			t.Errorf("skillFileAllowed(%q) = %v, want %v", c.name, got, c.ok)
		}
	}
	cfg.AllowedFiles = []string{"*.go"}
	if !skillFileAllowed(cfg, "main.go") || skillFileAllowed(cfg, "deploy.sh") {
		t.Error("custom allowlist not honored")
	}
}

func TestSkillSensitiveLeftover(t *testing.T) {
	for in, want := range map[string]string{
		"host 10.1.2.3":     "an IPv4 address",
		"bind 0.0.0.0:80":   "",
		"lo 127.0.0.1":      "",
		"version 1.2.3":     "",
		"ver 300.1.1.1":     "",
		"mail a@example.io": "an e-mail address",
	} {
		if got := skillSensitiveLeftover(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}

func TestSkillDetector_SignalAndModeSuppression(t *testing.T) {
	app, _ := scApp(t)
	run := func() string {
		out := ""
		for i := 0; i < 8; i++ {
			out = app.appendSkillHint("run_shell", true, "r")
		}
		return out
	}
	app.setSkillCreationMode("off", false)
	if h := run(); strings.Contains(h, "app note") {
		t.Error("off must suppress the model hint")
	}
	if app.takeSkillSignal() == "" {
		t.Error("detector signal should still be recorded")
	}
	app.skillSuggest = nil
	app.setSkillCreationMode("on", false)
	if h := run(); !strings.Contains(h, "app note") {
		t.Error("on must keep the model hint")
	}
	app.skillSuggest = nil
	app.setSkillCreationMode("auto", false)
	if h := run(); strings.Contains(h, "app note") {
		t.Error("auto must suppress the model hint")
	}
}

func TestSkillCreationNow_RequiresFilesAndSubagentDenied(t *testing.T) {
	app, _, _ := templateTestApp(t, true)
	if out := skillCreationNowCmd(app, nil)().(SysNoteMsg).Text; !strings.Contains(out, "no files") {
		t.Errorf("no files: %q", out)
	}
	sub := skillTestApp(t, true, nil)
	if out := skillCreationNowCmd(sub, nil)().(SysNoteMsg).Text; !strings.Contains(out, "subagents") {
		t.Errorf("subagent: %q", out)
	}
}

func TestSkillCreation_BusyFlagSingleRun(t *testing.T) {
	app, _ := scApp(t)
	app.skillCreate.busy.Store(true)
	if msg := app.startSkillCreation(skillCreationRequest{Mode: "auto"}); !strings.Contains(msg, "already in progress") {
		t.Errorf("msg = %q", msg)
	}
}

func TestSkillPipeline_TimeoutStillSavesDraft(t *testing.T) {
	app, _ := scApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	app.skillCreate.hooks = skillCreationHooks{
		plan: func(context.Context, string) (string, error) { return scPlanJSON, nil },
		review: func(context.Context, string, string) ([]skillReviewResult, string, error) {
			cancel() // the run deadline expires during the review call
			return nil, "", context.Canceled
		},
	}
	out := app.runSkillCreation(ctx, skillCreationRequest{Mode: config.SkillCreationAutoMashura})
	if out.Kind != "draft" {
		t.Fatalf("a checked draft must survive a run timeout, got %+v", out)
	}
}

func TestSkillPipeline_DegradedPlanNeverActivates(t *testing.T) {
	app, _ := scApp(t)
	var reviewCalls int32
	app.skillCreate.hooks = skillCreationHooks{
		plan: func(context.Context, string) (string, error) { return "garbage", nil },
		review: func(context.Context, string, string) ([]skillReviewResult, string, error) {
			atomic.AddInt32(&reviewCalls, 1)
			return []skillReviewResult{{Model: "m", Answer: `{"verdict":"approve"}`}}, "panel", nil
		},
	}
	out := scRun(app, config.SkillCreationAutoMashura)
	if out.Kind != "draft" || reviewCalls != 0 {
		t.Fatalf("outcome=%+v reviewCalls=%d", out, reviewCalls)
	}
}

func TestSkillPipeline_RequestSecretBlocksPlanCall(t *testing.T) {
	app, _ := scApp(t)
	app.Conv = append(app.Conv, proxy.Message{Role: "user", Content: StrPtr("deploy it, password: hunter2hunter2")})
	var planCalls int32
	app.skillCreate.hooks = skillCreationHooks{
		plan: func(context.Context, string) (string, error) { atomic.AddInt32(&planCalls, 1); return scPlanJSON, nil },
	}
	out := scRun(app, config.SkillCreationAuto)
	if out.Kind != "aborted" || planCalls != 0 {
		t.Fatalf("outcome=%+v planCalls=%d", out, planCalls)
	}
}

func TestSkillPipeline_PlanReservationKeptAfterPaidCall(t *testing.T) {
	app, _ := scApp(t)
	app.Cfg.SkillCreation.MaxAutoPerDay = 1
	scHooks(app, `{"reusable":false}`)
	if out := scRun(app, config.SkillCreationAuto); out.Kind != "aborted" {
		t.Fatalf("first = %+v", out)
	}
	// The one-off verdict consumed a paid plan call → the budget stays spent.
	if out := scRun(app, config.SkillCreationAuto); !strings.Contains(out.Text, "daily limit") {
		t.Fatalf("second = %+v", out)
	}
}

func TestApplySkillChanges(t *testing.T) {
	cfg := config.SkillCreationConfig{}
	files := []skillSrcFile{{Rel: "a.sh", Mode: "0755", Body: "echo a\n"}, {Rel: "b.sh", Mode: "0755", Body: "echo b\n"}}
	d, _, err := buildSkillDraft(cfg, files, nil, "two-files")
	if err != nil {
		t.Fatal(err)
	}
	mk := func(desc, guid string, drop ...string) *skillVerdict {
		v := &skillVerdict{}
		v.Changes.Description, v.Changes.Guidance, v.Changes.DropPaths = desc, guid, drop
		return v
	}
	if _, err := applySkillChanges(cfg, d, mk("", "")); err == nil {
		t.Error("empty change set must be refused")
	}
	if _, err := applySkillChanges(cfg, d, mk("", "", "a.sh", "nope.sh")); err == nil {
		t.Error("drop of unknown path must be refused")
	}
	if _, err := applySkillChanges(cfg, d, mk("", "", "a.sh", "b.sh")); err == nil {
		t.Error("dropping every file must be refused")
	}
	nd, err := applySkillChanges(cfg, d, mk("New desc", "", "b.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if len(nd.Tmpl.Files) != 1 || len(d.Tmpl.Files) != 2 || !strings.HasPrefix(nd.Value, "New desc\n") {
		t.Errorf("apply result wrong: files=%d orig=%d", len(nd.Tmpl.Files), len(d.Tmpl.Files))
	}
}

func TestRunSkillReview_Combination(t *testing.T) {
	app, _ := scApp(t)
	d := &autoSkillDraft{Value: "x"}
	run := func(mode string, rs ...skillReviewResult) (string, error) {
		h := skillCreationHooks{review: func(context.Context, string, string) ([]skillReviewResult, string, error) { return rs, mode, nil }}
		v, _, _, _, err := app.runSkillReview(context.Background(), h, d)
		return v, err
	}
	ok := skillReviewResult{Model: "a", Answer: `{"verdict":"approve"}`}
	ch := skillReviewResult{Model: "b", Answer: `{"verdict":"changes","changes":{"description":"x"}}`}
	rej := skillReviewResult{Model: "c", Answer: `{"verdict":"reject"}`}
	bad := skillReviewResult{Model: "d", Err: errors.New("down")}
	if v, _ := run("panel", ok, ok); v != "approve" {
		t.Errorf("all approve = %q", v)
	}
	if v, _ := run("panel", ok, ch); v != "changes" {
		t.Errorf("approve+changes = %q", v)
	}
	if v, _ := run("panel", ch, rej); v != "reject" {
		t.Errorf("reject wins = %q", v)
	}
	if _, err := run("panel", ok, bad); err == nil {
		t.Error("a failed reviewer in panel mode must fail the round")
	}
	if v, err := run("fallback", bad, ok); err != nil || v != "approve" {
		t.Errorf("fallback skips failed member: %q %v", v, err)
	}
}

func TestWrittenPathGenerations(t *testing.T) {
	app, _, _ := templateTestApp(t, true)
	app.noteSkillFileWritten("/w/a")
	app.noteSkillFileWritten("/w/b")
	_, snap := app.writtenSkillSnapshot()
	app.noteSkillFileWritten("/w/b") // rewritten during the run
	app.noteSkillFileWritten("/w/c") // new during the run
	app.consumeWritten(snap)
	got := app.writtenSkillPaths()
	if len(got) != 2 || got[0] != "/w/b" || got[1] != "/w/c" {
		t.Fatalf("remaining = %v, want [/w/b /w/c]", got)
	}
}

func TestRetrievalFilterExcludesUnconfirmedAutoSkills(t *testing.T) {
	app, _, _ := templateTestApp(t, true)
	ctx := context.Background()
	sp := app.getSkillStore()
	if _, err := sp.putActiveSkill(ctx, "pgdeploy", "postgres deploy recipe unique-zebra-token", "auto-mashura", "s", memory.TaintUnknown, false, autoSkillNoteUnconfirmed+"; digest=x"); err != nil {
		t.Fatal(err)
	}
	if got := app.retrieveMemoryContext(ctx, "postgres deploy recipe unique-zebra-token"); strings.Contains(got, "pgdeploy") || strings.Contains(got, "zebra") {
		t.Fatalf("unconfirmed auto skill was injected:\n%s", got)
	}
	e, _ := sp.getActiveSkill(ctx, "pgdeploy")
	if err := sp.setSkillNote(ctx, e.ID, autoSkillNoteConfirmed); err != nil {
		t.Fatal(err)
	}
	if got := app.retrieveMemoryContext(ctx, "postgres deploy recipe unique-zebra-token"); !strings.Contains(got, "zebra") {
		t.Fatalf("confirmed skill should be injected:\n%s", got)
	}
}

// ─── audit fixes ────────────────────────────────────────────────────────────

func TestSkillPipeline_AutoMashuraNeedsExplicitReusable(t *testing.T) {
	app, _ := scApp(t)
	plan := strings.Replace(scPlanJSON, `"reusable":true,`, ``, 1)
	calls := scHooks(app, plan, `{"verdict":"approve"}`)
	out := scRun(app, config.SkillCreationAutoMashura)
	if out.Kind != "draft" || !strings.Contains(out.Text, "reusable") {
		t.Fatalf("missing reusable must stay a draft, got %+v", out)
	}
	if *calls != 0 {
		t.Fatalf("no Mashūra call expected before the reusable gate, got %d", *calls)
	}
	if _, err := app.getSkillStore().getActiveSkill(context.Background(), "shop-swarm"); err == nil {
		t.Fatal("must not activate")
	}
}

func TestSkillPipeline_DraftNotesReplacement(t *testing.T) {
	app, _ := scApp(t)
	ctx := context.Background()
	if _, err := app.getSkillStore().putActiveSkill(ctx, "shop-swarm", "old", "main", "s", memory.TaintUnknown, false, ""); err != nil {
		t.Fatal(err)
	}
	scHooks(app, scPlanJSON)
	if out := scRun(app, config.SkillCreationAuto); out.Kind != "draft" {
		t.Fatalf("outcome = %+v", out)
	}
	drafts, _ := app.getSkillStore().listSkillDrafts(ctx)
	if len(drafts) != 1 || !strings.Contains(drafts[0].Note, "REPLACES active skill") {
		t.Fatalf("draft note must flag the replacement: %+v", drafts)
	}
}

func TestSkillPipeline_TerminalOutcomesAreMarked(t *testing.T) {
	app, ws := scApp(t)
	scHooks(app, `{"reusable":false}`)
	if out := scRun(app, config.SkillCreationAuto); !out.Terminal {
		t.Fatalf("one-off must be terminal: %+v", out)
	}
	writeWS(t, ws, "ops/leak.sh", "export api_key=abcdef123456\n", 0o644)
	if out := app.runSkillCreation(context.Background(), skillCreationRequest{Mode: "auto", Manual: true, Paths: []string{"ops/leak.sh"}}); out.Kind != "aborted" || !out.Terminal {
		t.Fatalf("secret must be terminal: %+v", out)
	}
}

// A failing automatic run consumes its snapshot (no per-turn retry loop) and a
// rewrite re-arms it; identical automatic notes are shown once.
func TestStartSkillCreation_TerminalAbortConsumesSnapshotAndDedupes(t *testing.T) {
	app, _ := scApp(t)
	scHooks(app, `{"reusable":false}`)
	var notes []string
	var mu sync.Mutex
	app.EventSink = func(msg any) {
		if n, ok := msg.(SysNoteMsg); ok {
			mu.Lock()
			notes = append(notes, n.Text)
			mu.Unlock()
		}
	}
	wait := func() {
		for i := 0; i < 400 && app.skillCreate.busy.Load(); i++ {
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond)
	}
	app.startSkillCreation(skillCreationRequest{Mode: "auto"})
	wait()
	if got := app.writtenSkillPaths(); len(got) != 0 {
		t.Fatalf("snapshot must be consumed after a terminal abort, left %v", got)
	}
	if msg := app.startSkillCreation(skillCreationRequest{Mode: "auto"}); msg != "" {
		t.Fatalf("nothing tracked → no run and no note, got %q", msg)
	}
	// Rewriting re-arms; the identical abort note is not repeated.
	app.skillCreate.hooks.plan = func(context.Context, string) (string, error) { return `{"reusable":false}`, nil }
	app.Cfg.SkillCreation.MaxAutoPerDay = 5
	app.noteSkillFileWritten(app.Exec.WorkspaceRoot() + "/ops/deploy.sh")
	app.startSkillCreation(skillCreationRequest{Mode: "auto"})
	wait()
	mu.Lock()
	defer mu.Unlock()
	aborts := 0
	for _, n := range notes {
		if strings.Contains(n, "judged one-off") {
			aborts++
		}
	}
	if aborts != 1 {
		t.Fatalf("identical automatic abort notes must be shown once, got %d: %v", aborts, notes)
	}
}

func TestStartSkillCreation_BusyMessageNotRepeatedByHook(t *testing.T) {
	app, _ := scApp(t)
	app.skillCreate.busy.Store(true)
	var sent int
	app.EventSink = func(any) { sent++ }
	app.setSkillSignal("repeat")
	app.setSkillCreationMode("auto", false)
	app.onTurnEndSkillCreation()
	if sent != 0 {
		t.Fatalf("busy note must not be emitted by the turn-end hook, got %d events", sent)
	}
}

// The end-of-turn hook fires once per finished turn: not for a suspended
// outcome, then once when Resume ends the turn.
func TestSkillTurnEndHook_FiresOnceAfterSuspendResume(t *testing.T) {
	release := make(chan struct{})
	var parentCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if isSubagentRequest(body) {
			<-release
			writeSSE(w, contentChunk(summaryFor(taskFromBody(body))))
			return
		}
		switch parentCalls.Add(1) {
		case 1:
			writeSSE(w, toolCallFrames("d1", "dispatch_subagent", `{"task":"TASK-A"}`)...)
		case 2:
			writeSSE(w, contentChunk("waiting"))
		default:
			writeSSE(w, contentChunk("all done"))
		}
	}))
	defer srv.Close()

	app := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	out, err := app.SendOutcome(context.Background(), "go")
	if err != nil || out.Kind != TurnSuspended {
		t.Fatalf("expected suspended, got %v err=%v", out.Kind, err)
	}
	if n := app.skillCreate.turnEnds.Load(); n != 0 {
		t.Fatalf("hook must not fire for a suspended turn, fired %d", n)
	}
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for out.Kind == TurnSuspended {
		if ok, werr := app.WaitForAsyncCompletion(ctx); werr != nil || !ok {
			t.Fatalf("wait: ok=%v err=%v", ok, werr)
		}
		if out, err = app.Resume(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if out.Kind != TurnFinal {
		t.Fatalf("final outcome = %v", out.Kind)
	}
	if n := app.skillCreate.turnEnds.Load(); n != 1 {
		t.Fatalf("hook must fire exactly once after the resumed turn ends, fired %d", n)
	}
}

func TestSkillTurnEndHook_NotFiredOnAbortedOutcome(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadRequest)
	}))
	defer srv.Close()
	app := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	out, _ := app.SendOutcome(context.Background(), "hi")
	if out.Kind == TurnFinal {
		t.Skip("model error did not abort in this harness")
	}
	if n := app.skillCreate.turnEnds.Load(); n != 0 {
		t.Fatalf("hook must not fire for %v, fired %d", out.Kind, n)
	}
}

func TestSkillPipeline_PlannerDownThenCheckFailureIsNotTerminal(t *testing.T) {
	app, ws := scApp(t)
	// An IP the planner would have turned into a param; with the planner down
	// the deterministic fallback cannot, so the draft check fails.
	writeWS(t, ws, "ops/ip.sh", "#!/bin/sh\nssh deploy@203.0.113.7 uptime\n", 0o755)
	app.noteSkillFileWritten(ws + "/ops/ip.sh")
	app.skillCreate.hooks = skillCreationHooks{
		plan: func(context.Context, string) (string, error) { return "", errors.New("planner timeout") },
	}
	out := scRun(app, config.SkillCreationAuto)
	if out.Kind != "aborted" || out.Terminal {
		t.Fatalf("planner outage must not consume the input: %+v", out)
	}
}
