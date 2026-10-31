package agent

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/memory"
)

func TestSkillCreationMode_Precedence(t *testing.T) {
	app := skillTestApp(t, false, nil)
	if m, src := app.SkillCreationMode(); m != "on" || src != "config" {
		t.Fatalf("default = %q/%q, want on/config", m, src)
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
	if out := run("/skill-creation"); !strings.Contains(out, "skill creation: on") {
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
	out := approveSkillDraft(ctx, app, sp, d2.ID)
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
	if out := approveSkillDraft(ctx, app, sp, d.ID); !strings.Contains(out, "secret") {
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
