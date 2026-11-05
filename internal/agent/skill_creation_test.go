package agent

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/memory"
	"github.com/treeol/wakil/internal/proxy"
)

func TestSkillCreationMode_Precedence(t *testing.T) {
	app := skillTestApp(t, false, nil)
	// 0.4.4 (D2): the default is off.
	if m, src := app.SkillCreationMode(); m != "off" || src != "config" {
		t.Fatalf("default = %q/%q, want off/config", m, src)
	}
	app.Cfg.SkillCreation.Mode = "off"
	if m, src := app.SkillCreationMode(); m != "off" || src != "config" {
		t.Fatalf("config = %q/%q", m, src)
	}
	app.setSkillCreationMode("auto", false)
	if m, src := app.SkillCreationMode(); m != "auto" || src != "repo" {
		t.Fatalf("repo = %q/%q", m, src)
	}
	app.setSkillCreationMode("auto-mashura", true)
	if m, src := app.SkillCreationMode(); m != "auto-mashura" || src != "session" {
		t.Fatalf("session = %q/%q", m, src)
	}
	// Repo-level change clears the session override.
	app.setSkillCreationMode("on", false)
	if m, src := app.SkillCreationMode(); m != "on" || src != "repo" {
		t.Fatalf("after repo set = %q/%q", m, src)
	}
}

func TestSkillCreationCommand(t *testing.T) {
	app := skillTestApp(t, false, nil)
	run := func(fields ...string) string {
		msg := handleSkillCreationCommand(app, fields)()
		return msg.(SysNoteMsg).Text
	}
	if out := run("/skill-creation", "bogus"); !strings.Contains(out, "usage") {
		t.Errorf("bogus mode: %q", out)
	}
	if out := run("/skill-creation", "auto", "session"); !strings.Contains(out, "this session only") {
		t.Errorf("session set: %q", out)
	}
	if m, src := app.SkillCreationMode(); m != "auto" || src != "session" {
		t.Errorf("mode = %q/%q", m, src)
	}
	run("/skill-creation", "default", "session")
	if _, src := app.SkillCreationMode(); src != "config" {
		t.Errorf("source after default = %q", src)
	}
	if out := run("/skill-creation"); !strings.Contains(out, "skill creation: off") {
		t.Errorf("describe: %q", out)
	}
}

func TestSkillCreationConfigDefaults(t *testing.T) {
	var c config.SkillCreationConfig
	if c.MaxAutoPerDayOrDefault() != 3 || c.MaxDraftsOrDefault() != 10 || c.DraftTTLDaysOrDefault() != 30 || c.ReviewTimeoutSecondsOrDefault() != 180 {
		t.Errorf("defaults wrong: %+v", c)
	}
	c.MaxAutoPerDay = 7
	if c.MaxAutoPerDayOrDefault() != 7 {
		t.Error("override ignored")
	}
	if config.ValidSkillCreationMode("nope") || !config.ValidSkillCreationMode("auto-mashura") {
		t.Error("mode validation wrong")
	}
}

func TestSkillDraftsLifecycle(t *testing.T) {
	app := skillTestApp(t, false, nil)
	ctx := context.Background()
	sp := app.getSkillStore()

	d1, err := sp.putSkillDraft(ctx, "k1", "first", "auto", "s", memory.TaintUnknown, "auto-draft: x")
	if err != nil {
		t.Fatal(err)
	}
	d2, err := sp.putSkillDraft(ctx, "k1", "second", "auto", "s", memory.TaintUnknown, "auto-draft: y")
	if err != nil {
		t.Fatal(err)
	}
	drafts, _ := sp.listSkillDrafts(ctx)
	if len(drafts) != 1 || drafts[0].ID != d2.ID {
		t.Fatalf("expected only newest draft pending, got %d (d1=%d d2=%d)", len(drafts), d1.ID, d2.ID)
	}
	// Drafts are not active.
	if _, err := sp.getActiveSkill(ctx, "k1"); err == nil {
		t.Fatal("draft must not be active")
	}
	out := approveSkillDraft(ctx, app, sp, d2.ID, false)
	if !strings.Contains(out, "approved") {
		t.Fatalf("approve: %q", out)
	}
	if e, err := sp.getActiveSkill(ctx, "k1"); err != nil || e.Value != "second" {
		t.Fatalf("active after approve: %v %v", e, err)
	}
	// Reject path.
	d3, _ := sp.putSkillDraft(ctx, "k2", "x", "auto", "s", memory.TaintUnknown, "n")
	if out := handleSkillsCommand(ctx, app, []string{"/skills", "reject", skItoa(d3.ID), "meh"}); !strings.Contains(out, "rejected") {
		t.Fatalf("reject: %q", out)
	}
	if drafts, _ := sp.listSkillDrafts(ctx); len(drafts) != 0 {
		t.Fatalf("drafts left: %d", len(drafts))
	}
}

func TestSkillDraftApproveRefusesSecrets(t *testing.T) {
	app := skillTestApp(t, false, nil)
	ctx := context.Background()
	sp := app.getSkillStore()
	d, _ := sp.putSkillDraft(ctx, "k", "-----BEGIN RSA PRIVATE KEY-----\nabc\n-----END RSA PRIVATE KEY-----", "auto", "s", memory.TaintUnknown, "n")
	if out := approveSkillDraft(ctx, app, sp, d.ID, false); !strings.Contains(out, "secret") {
		t.Fatalf("expected secret refusal, got %q", out)
	}
}

func TestSkillsConfirmUndoAndRetrievalFilter(t *testing.T) {
	app := skillTestApp(t, false, nil)
	ctx := context.Background()
	sp := app.getSkillStore()

	e, err := sp.putActiveSkill(ctx, "auto-one", "body", "auto-mashura", "s", memory.TaintUnknown, false, autoSkillNoteUnconfirmed+"; digest=abc")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sp.putActiveSkill(ctx, "manual", "body", "main", "s", memory.TaintUnknown, false, ""); err != nil {
		t.Fatal(err)
	}
	got := dropUnconfirmedAutoSkills([]*memory.Entry{e, {Key: "manual"}})
	if len(got) != 1 || got[0].Key != "manual" {
		t.Fatalf("filter result: %+v", got)
	}
	if out := handleSkillsCommand(ctx, app, []string{"/skills", "confirm", "auto-one"}); !strings.Contains(out, "confirmed") {
		t.Fatalf("confirm: %q", out)
	}
	cur, _ := sp.getActiveSkill(ctx, "auto-one")
	if isUnconfirmedAutoSkill(cur) || !isAutoSkill(cur) {
		t.Fatalf("note after confirm = %q", cur.Note)
	}
	if out := handleSkillsCommand(ctx, app, []string{"/skills", "undo", "manual"}); !strings.Contains(out, "not created automatically") {
		t.Fatalf("undo of manual skill must be refused: %q", out)
	}
	if out := handleSkillsCommand(ctx, app, []string{"/skills", "undo", "auto-one"}); !strings.Contains(out, "forgot") {
		t.Fatalf("undo: %q", out)
	}
}

func TestReserveDaily(t *testing.T) {
	app := skillTestApp(t, false, nil)
	ctx := context.Background()
	sp := app.getSkillStore()
	for i := 0; i < 2; i++ {
		if ok, err := sp.reserveDaily(ctx, "t", "2026-10-31", 2); !ok || err != nil {
			t.Fatalf("reserve %d: %v %v", i, ok, err)
		}
	}
	if ok, _ := sp.reserveDaily(ctx, "t", "2026-10-31", 2); ok {
		t.Fatal("third reserve must fail")
	}
	sp.releaseDaily(ctx, "t", "2026-10-31")
	if ok, _ := sp.reserveDaily(ctx, "t", "2026-10-31", 2); !ok {
		t.Fatal("reserve after release must succeed")
	}
	if ok, _ := sp.reserveDaily(ctx, "t", "2026-11-01", 2); !ok {
		t.Fatal("new day must reset")
	}
}

func skItoa(n int64) string { return strconv.FormatInt(n, 10) }

// ─── audit fixes ────────────────────────────────────────────────────────────

func TestApproveAutoDraftBecomesConfirmedAndUndoable(t *testing.T) {
	app := skillTestApp(t, false, nil)
	ctx := context.Background()
	sp := app.getSkillStore()
	d, _ := sp.putSkillDraft(ctx, "k", "body", skillWriterAutoDraft, "s", memory.TaintUnknown, autoDraftNotePrefix+": review needed | note")
	if out := handleSkillsCommand(ctx, app, []string{"/skills", "approve", skItoa(d.ID)}); !strings.Contains(out, "approved") {
		t.Fatalf("approve: %q", out)
	}
	cur, _ := sp.getActiveSkill(ctx, "k")
	if !strings.HasPrefix(cur.Note, autoSkillNoteConfirmed) || !isAutoSkill(cur) || isUnconfirmedAutoSkill(cur) {
		t.Fatalf("note after approve = %q", cur.Note)
	}
	if out := handleSkillsCommand(ctx, app, []string{"/skills", "recent"}); !strings.Contains(out, "k ") {
		t.Fatalf("recent must list the approved skill: %q", out)
	}
	if out := handleSkillsCommand(ctx, app, []string{"/skills", "undo", "k"}); !strings.Contains(out, "forgot") {
		t.Fatalf("undo: %q", out)
	}
	// A hand-made draft keeps its note.
	d2, _ := sp.putSkillDraft(ctx, "h", "body", "main", "s", memory.TaintUnknown, "mine")
	handleSkillsCommand(ctx, app, []string{"/skills", "approve", skItoa(d2.ID)})
	if h, _ := sp.getActiveSkill(ctx, "h"); h.Note != "mine" || isAutoSkill(h) {
		t.Fatalf("hand-made note changed: %q", h.Note)
	}
}

func TestLegacyApprovedDraftNoteCountsAsConfirmedAuto(t *testing.T) {
	e := &memory.Entry{Status: memory.StatusActive, Note: autoDraftNotePrefix + ": x"}
	if !isAutoSkill(e) || isUnconfirmedAutoSkill(e) {
		t.Fatal("an active row with the draft prefix is a confirmed auto skill")
	}
	if isAutoSkill(&memory.Entry{Status: memory.StatusProposed, Note: autoDraftNotePrefix}) {
		t.Fatal("a pending draft is not an active auto skill")
	}
}

func TestApproveRefusesReplaceWithoutFlag(t *testing.T) {
	app := skillTestApp(t, false, nil)
	ctx := context.Background()
	sp := app.getSkillStore()
	if _, err := sp.putActiveSkill(ctx, "k", "old", "main", "s", memory.TaintUnknown, false, ""); err != nil {
		t.Fatal(err)
	}
	d, _ := sp.putSkillDraft(ctx, "k", "new", skillWriterAutoDraft, "s", memory.TaintUnknown, autoDraftNotePrefix+": x")
	out := handleSkillsCommand(ctx, app, []string{"/skills", "approve", skItoa(d.ID)})
	if !strings.Contains(out, "already active") || !strings.Contains(out, "replace") {
		t.Fatalf("expected replace guidance, got %q", out)
	}
	if cur, _ := sp.getActiveSkill(ctx, "k"); cur.Value != "old" {
		t.Fatal("active skill was replaced without the flag")
	}
	if review := handleSkillsCommand(ctx, app, []string{"/skills", "review"}); !strings.Contains(review, "REPLACES") {
		t.Fatalf("review must flag the replacement: %q", review)
	}
	if out := handleSkillsCommand(ctx, app, []string{"/skills", "approve", skItoa(d.ID), "bogus"}); !strings.Contains(out, "usage") {
		t.Fatalf("unknown argument must be rejected: %q", out)
	}
	if out := handleSkillsCommand(ctx, app, []string{"/skills", "approve", skItoa(d.ID), "replace"}); !strings.Contains(out, "replaced active version") {
		t.Fatalf("replace: %q", out)
	}
	if cur, _ := sp.getActiveSkill(ctx, "k"); cur.Value != "new" {
		t.Fatal("replace did not take effect")
	}
	if hist, _ := sp.historyForKey(ctx, "k"); len(hist) != 2 {
		t.Fatalf("old version must stay in history, got %d", len(hist))
	}
}

func TestSkillsShowIsRuneSafeAndFullPrintsAll(t *testing.T) {
	app := skillTestApp(t, false, nil)
	ctx := context.Background()
	sp := app.getSkillStore()
	body := strings.Repeat("é", 4000) // 8000 bytes; byte 6000 is a rune start, 5999 is not
	d, _ := sp.putSkillDraft(ctx, "k", body, "main", "s", memory.TaintUnknown, "n")
	out := handleSkillsCommand(ctx, app, []string{"/skills", "show", skItoa(d.ID)})
	if !utf8.ValidString(out) || !strings.Contains(out, "show "+skItoa(d.ID)+" full") {
		t.Fatalf("truncated show must be valid UTF-8 and mention full: %q", out[len(out)-120:])
	}
	if full := handleSkillsCommand(ctx, app, []string{"/skills", "show", skItoa(d.ID), "full"}); !strings.Contains(full, body) {
		t.Fatal("full must print the whole body")
	}
	odd := strings.Repeat("a", 5999) + "é" + "tail"
	if cut, _ := truncateForShow(odd, 6000, false); !utf8.ValidString(cut) {
		t.Fatal("cut inside a rune")
	}
}

func TestSkillsCommandDeniedInSubagent(t *testing.T) {
	sub := skillTestApp(t, true, nil)
	if out := handleSkillsCommand(context.Background(), sub, []string{"/skills", "review"}); !strings.Contains(out, "subagents") {
		t.Fatalf("got %q", out)
	}
	if out := handleSkillCreationCommand(sub, []string{"/skill-creation"})().(SysNoteMsg).Text; !strings.Contains(out, "subagents") {
		t.Fatalf("describe in subagent: %q", out)
	}
}

func TestUnconfirmedAutoSkillUnusableByModel(t *testing.T) {
	app, ws, _ := templateTestApp(t, true)
	ctx := context.Background()
	sp := app.getSkillStore()
	val := "Deploy recipe unique-zebra\n"
	if _, err := sp.putActiveSkill(ctx, "pgdeploy", val, "auto-mashura", "s", memory.TaintUnknown, false, autoSkillNoteUnconfirmed+"; digest=x"); err != nil {
		t.Fatal(err)
	}
	call := func(name, args string) proxy.ToolCall {
		return proxy.ToolCall{Function: proxy.FunctionCall{Name: name, Arguments: args}}
	}
	if out := app.handleListSkills(ctx, call("list_skills", `{}`)); !strings.Contains(out, "pgdeploy") || !strings.Contains(out, "/skills confirm") || strings.Contains(out, "zebra") {
		t.Fatalf("list: %q", out)
	}
	if out := app.handleSkillSearch(ctx, call("skill_search", `{"query":"zebra"}`)); strings.Contains(out, "Deploy recipe") || !strings.Contains(out, "pgdeploy") {
		t.Fatalf("search must show the marker but not content: %q", out)
	}
	if out := app.handleLoadSkill(ctx, call("load_skill", `{"key":"pgdeploy"}`)); strings.Contains(out, "zebra") || !strings.Contains(out, "/skills confirm pgdeploy") {
		t.Fatalf("load: %q", out)
	}
	if out := app.handleApplySkillTemplate(ctx, call("apply_skill_template", `{"key":"pgdeploy"}`)); !strings.Contains(out, "/skills confirm pgdeploy") {
		t.Fatalf("apply: %q", out)
	}
	_ = ws
	// After confirm everything works again.
	handleSkillsCommand(ctx, app, []string{"/skills", "confirm", "pgdeploy"})
	if out := app.handleLoadSkill(ctx, call("load_skill", `{"key":"pgdeploy"}`)); !strings.Contains(out, "zebra") {
		t.Fatalf("load after confirm: %q", out)
	}
}

func TestSkillCreationConfigValidate(t *testing.T) {
	if err := (config.SkillCreationConfig{MaxDrafts: -1}).Validate(); err == nil {
		t.Error("negative limit must be rejected")
	}
	if err := (config.SkillCreationConfig{AllowedFiles: []string{"[bad"}}).Validate(); err == nil {
		t.Error("invalid glob must be rejected")
	}
	if err := (config.SkillCreationConfig{AllowedFiles: []string{"*.sh"}, MaxDrafts: 3}).Validate(); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
}

func TestDraftExpiry(t *testing.T) {
	now := time.Now()
	old := &memory.Entry{CreatedAt: now.Add(-48 * time.Hour).UnixMilli()}
	fresh := &memory.Entry{CreatedAt: now.Add(-time.Hour).UnixMilli()}
	if !draftExpired(old, 1, now) || draftExpired(fresh, 1, now) || draftExpired(old, 30, now) {
		t.Fatal("draftExpired boundaries wrong")
	}
	// A fresh draft still approves through the real path.
	app := skillTestApp(t, false, nil)
	ctx := context.Background()
	sp := app.getSkillStore()
	d, _ := sp.putSkillDraft(ctx, "fresh", "body", "main", "s", memory.TaintUnknown, "n")
	if out := approveSkillDraft(ctx, app, sp, d.ID, false); !strings.Contains(out, "approved") {
		t.Fatalf("fresh draft: %q", out)
	}
}
