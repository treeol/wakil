package agent

// skill_creation.go — /skill-creation: automatic skill creation settings.
//
// Modes (config skill_creation.mode, per-repo saved value, per-session override):
//
//	off           no hints, suggestions or automatic runs (the detector still
//	              records its signal; the in-band B1/B2 model hints are suppressed)
//	on            (default) the existing in-band hint plus an end-of-turn note
//	              suggesting /skill-creation now
//	auto          builds a DRAFT automatically when recurring work is detected
//	auto-mashura  builds the draft, has Mashūra review it, and activates it only
//	              when the exact final bytes were approved; every failure falls
//	              back to a draft. The activated skill stays "unconfirmed" (not
//	              usable by the model) until the user runs /skills confirm.
//	              Choosing this mode (command, config or saved repo state) is the
//	              standing opt-in to send the sanitized draft to the Mashūra panel.
//
// Precedence: session override > per-repo saved value > config default.
//
// Safety model: the pipeline write path is plain Go code, never reachable from
// tool dispatch, so the model cannot call it. SuspendAuto (commands.go) is
// deliberately unchanged — the save_skill/update_skill/forget_skill tools keep
// their unconditional confirmation gate.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/memory"
)

// skillCreationState is the per-App skill-creation bookkeeping.
type skillCreationState struct {
	// repoMode / sessionMode are guarded by App.stateMu.
	repoMode    string
	sessionMode string

	// busy is set while a pipeline run is in flight (one at a time).
	busy atomic.Bool

	// mu guards written and pendingSignal.
	mu sync.Mutex
	// written lists workspace files written during the current session via
	// edit/write/template tools (canonical paths, insertion order, deduped).
	written []writtenPath
	// writeGen increments on every tracked write.
	writeGen uint64
	// pendingSignal records that the in-session detector fired this turn
	// ("repeat" or "long"); consumed by the end-of-turn hook.
	pendingSignal string
	// turnEnds counts end-of-turn hook invocations (tests assert it fires once
	// per finished turn, not for suspended ones).
	turnEnds atomic.Int32
	// lastAutoNote is the previous automatic run's note (dedup).
	lastAutoNote string
	// last is the most recent pipeline outcome.
	last skillCreationOutcome
	// hooks replaces the model-plan and Mashūra-review calls (tests).
	hooks skillCreationHooks
}

// SkillCreationMode returns the effective mode and its source
// ("session", "repo" or "config").
func (a *App) SkillCreationMode() (mode, source string) {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	switch {
	case a.skillCreate.sessionMode != "":
		return a.skillCreate.sessionMode, "session"
	case a.skillCreate.repoMode != "":
		return a.skillCreate.repoMode, "repo"
	}
	return a.Cfg.SkillCreation.ModeOrDefault(), "config"
}

// setSkillCreationMode sets (or with "" clears) the repo or session value.
func (a *App) setSkillCreationMode(mode string, sessionOnly bool) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	if sessionOnly {
		a.skillCreate.sessionMode = mode
		return
	}
	// A repo-level change must take effect now, so it clears the session override.
	a.skillCreate.repoMode = mode
	a.skillCreate.sessionMode = ""
}

// handleSkillCreationCommand implements /skill-creation.
func handleSkillCreationCommand(app *App, fields []string) Cmd {
	usage := "usage: /skill-creation [off|on|auto|auto-mashura|default] [session] | now [key] [paths…]"
	if app.IsSubagent {
		return NoteCmd("/skill-creation: not available in subagents")
	}
	if len(fields) < 2 {
		return NoteCmd(describeSkillCreation(app))
	}
	arg := config.NormalizeSkillCreationMode(fields[1])
	if arg == "now" {
		return skillCreationNowCmd(app, fields[2:])
	}
	sessionOnly := false
	for _, f := range fields[2:] {
		if strings.EqualFold(f, "session") {
			sessionOnly = true
		} else {
			return NoteCmd(usage)
		}
	}
	if arg == "default" {
		app.setSkillCreationMode("", sessionOnly)
		if !sessionOnly {
			app.saveRepoState(func(s *RepoState) { s.SkillCreation = "" })
		}
		return NoteCmd("skill creation reset to default\n" + describeSkillCreation(app))
	}
	if arg == "" || !config.ValidSkillCreationMode(arg) {
		return NoteCmd(usage)
	}
	app.setSkillCreationMode(arg, sessionOnly)
	scope := "this repo"
	if sessionOnly {
		scope = "this session only"
	} else {
		app.saveRepoState(func(s *RepoState) { s.SkillCreation = arg })
	}
	msg := fmt.Sprintf("skill creation: %s (%s)", arg, scope)
	if arg == config.SkillCreationAutoMashura && !app.mashuraAvailable() {
		msg += "\n⚠ Mashūra is not configured — auto-mashura will behave like auto (saves drafts)"
	} else if arg == config.SkillCreationAutoMashura {
		msg += "\n⚠ Each automatic run sends the sanitized draft (file contents with secrets/IPs/e-mails screened; hostnames and names are NOT screened) to the Mashūra panel without asking again. Use /skill-creation auto to review drafts yourself instead."
	}
	return NoteCmd(msg)
}

// describeSkillCreation renders the current mode, source and effective limits.
func describeSkillCreation(app *App) string {
	mode, src := app.SkillCreationMode()
	sc := app.Cfg.SkillCreation
	var b strings.Builder
	fmt.Fprintf(&b, "skill creation: %s (from %s)\n", mode, src)
	fmt.Fprintf(&b, "  limits: %d auto/day, %d drafts max, drafts expire after %d days, review timeout %ds\n",
		sc.MaxAutoPerDayOrDefault(), sc.MaxDraftsOrDefault(), sc.DraftTTLDaysOrDefault(), sc.ReviewTimeoutSecondsOrDefault())
	if mode == config.SkillCreationAutoMashura {
		if !app.mashuraAvailable() {
			b.WriteString("  ⚠ Mashūra not configured: behaves like auto (drafts)\n")
		} else {
			b.WriteString("  ⚠ each run sends the sanitized draft to the Mashūra panel without asking again\n")
		}
	}
	b.WriteString("  /skill-creation off|on|auto|auto-mashura [session] · now [key] [paths…] · default\n")
	b.WriteString("  /skills review|show|approve|reject|recent|undo|confirm")
	return b.String()
}

// dropUnconfirmedAutoSkills filters auto-created skills the user has not yet
// confirmed out of automatic retrieval.
func dropUnconfirmedAutoSkills(in []*memory.Entry) []*memory.Entry {
	out := in[:0:0]
	for _, e := range in {
		if !isUnconfirmedAutoSkill(e) {
			out = append(out, e)
		}
	}
	return out
}

// writtenPath is one tracked workspace write; gen lets a finished run consume
// only the writes it actually captured, not ones that landed during the run.
type writtenPath struct {
	Path string
	Gen  uint64
}

// noteSkillFileWritten records a workspace path written this session so the
// skill-creation builder knows what the work produced.
func (a *App) noteSkillFileWritten(canonical string) {
	if a.IsSubagent || canonical == "" {
		return
	}
	a.skillCreate.mu.Lock()
	defer a.skillCreate.mu.Unlock()
	a.skillCreate.writeGen++
	for i, w := range a.skillCreate.written {
		if w.Path == canonical {
			a.skillCreate.written[i].Gen = a.skillCreate.writeGen
			return
		}
	}
	const maxTracked = 200
	if len(a.skillCreate.written) >= maxTracked {
		a.skillCreate.written = a.skillCreate.written[1:]
	}
	a.skillCreate.written = append(a.skillCreate.written, writtenPath{canonical, a.skillCreate.writeGen})
}

// writtenSkillPaths returns the tracked written paths.
func (a *App) writtenSkillPaths() []string {
	paths, _ := a.writtenSkillSnapshot()
	return paths
}

// writtenSkillSnapshot returns the paths plus the exact entries (for consume).
func (a *App) writtenSkillSnapshot() ([]string, []writtenPath) {
	a.skillCreate.mu.Lock()
	defer a.skillCreate.mu.Unlock()
	snap := append([]writtenPath(nil), a.skillCreate.written...)
	paths := make([]string, len(snap))
	for i, w := range snap {
		paths[i] = w.Path
	}
	return paths, snap
}

// consumeWritten removes the snapshot entries a finished run captured. An
// entry rewritten (new generation) or added during the run stays tracked.
func (a *App) consumeWritten(snap []writtenPath) {
	a.skillCreate.mu.Lock()
	defer a.skillCreate.mu.Unlock()
	done := make(map[writtenPath]bool, len(snap))
	for _, w := range snap {
		done[w] = true
	}
	kept := a.skillCreate.written[:0]
	for _, w := range a.skillCreate.written {
		if !done[w] {
			kept = append(kept, w)
		}
	}
	a.skillCreate.written = kept
}

// ─── /skills ────────────────────────────────────────────────────────────────

// expireSkillDrafts rejects pending drafts older than the configured TTL.
func (a *App) expireSkillDrafts(ctx context.Context) {
	sp := a.getSkillStore()
	if sp == nil {
		return
	}
	ttlDays := a.Cfg.SkillCreation.DraftTTLDaysOrDefault()
	drafts, err := sp.listSkillDrafts(ctx)
	if err != nil {
		return
	}
	now := time.Now()
	for _, d := range drafts {
		if draftExpired(d, ttlDays, now) {
			_ = sp.rejectSkillDraft(ctx, d.ID, "expired (draft TTL)")
		}
	}
}

// handleSkillsCommand implements /skills.
func handleSkillsCommand(ctx context.Context, app *App, fields []string) string {
	if app.IsSubagent {
		return "/skills: not available in subagents"
	}
	sp := app.getSkillStore()
	if sp == nil {
		return "/skills: the skills store is not available"
	}
	sub := "review"
	if len(fields) > 1 {
		sub = strings.ToLower(fields[1])
	}
	args := []string{}
	if len(fields) > 2 {
		args = fields[2:]
	}
	idArg := func() (int64, bool) {
		if len(args) == 0 {
			return 0, false
		}
		n, err := strconv.ParseInt(strings.TrimPrefix(args[0], "#"), 10, 64)
		return n, err == nil && n > 0
	}

	switch sub {
	case "review", "list":
		app.expireSkillDrafts(ctx)
		return renderSkillReview(ctx, app, sp)

	case "show":
		id, ok := idArg()
		if !ok || len(args) > 2 || (len(args) == 2 && !strings.EqualFold(args[1], "full")) {
			return "usage: /skills show <draft-id> [full]"
		}
		e, err := sp.getSkillByID(ctx, id)
		if err != nil {
			return fmt.Sprintf("/skills show: no skill entry #%d", id)
		}
		body, trunc := truncateForShow(e.Value, 6000, len(args) == 2)
		if trunc != "" {
			trunc = fmt.Sprintf("\n… (%s more — /skills show %d full prints everything)", trunc, id)
		}
		return fmt.Sprintf("#%d %s %s\nnote: %s\n---\n%s%s", e.ID, e.Key, renderSkillProvenance(e), e.Note, body, trunc)

	case "approve":
		id, ok := idArg()
		if !ok || len(args) > 2 || (len(args) == 2 && !strings.EqualFold(args[1], "replace")) {
			return "usage: /skills approve <draft-id> [replace]"
		}
		return approveSkillDraft(ctx, app, sp, id, len(args) == 2)

	case "reject":
		id, ok := idArg()
		if !ok {
			return "usage: /skills reject <draft-id> [reason]"
		}
		reason := "rejected by user"
		if len(args) > 1 {
			reason += ": " + strings.Join(args[1:], " ")
		}
		if err := sp.rejectSkillDraft(ctx, id, reason); err != nil {
			return "/skills reject: " + err.Error()
		}
		return fmt.Sprintf("rejected draft #%d", id)

	case "recent":
		return renderRecentAutoSkills(ctx, sp)

	case "undo":
		if len(args) == 0 {
			return "usage: /skills undo <key>"
		}
		e, err := sp.getActiveSkill(ctx, args[0])
		if err != nil {
			return fmt.Sprintf("/skills undo: no active skill %q", args[0])
		}
		if !isAutoSkill(e) {
			return fmt.Sprintf("/skills undo: %q was not created automatically — use the forget_skill tool", args[0])
		}
		if err := sp.forgetSkill(ctx, args[0]); err != nil {
			return "/skills undo: " + err.Error()
		}
		return fmt.Sprintf("forgot auto-created skill %q (tombstoned — still visible in skill_history)", args[0])

	case "confirm":
		if len(args) == 0 {
			return "usage: /skills confirm <key>"
		}
		e, err := sp.getActiveSkill(ctx, args[0])
		if err != nil {
			return fmt.Sprintf("/skills confirm: no active skill %q", args[0])
		}
		if !isUnconfirmedAutoSkill(e) {
			return fmt.Sprintf("/skills confirm: %q is not an unconfirmed auto-created skill", args[0])
		}
		note := autoSkillNoteConfirmed + strings.TrimPrefix(e.Note, autoSkillNoteUnconfirmed)
		if err := sp.setSkillNote(ctx, e.ID, note); err != nil {
			return "/skills confirm: " + err.Error()
		}
		return fmt.Sprintf("confirmed %q — it will now be injected into matching turns", args[0])
	}
	return "usage: /skills [review|show <id> [full]|approve <id> [replace]|reject <id> [reason]|recent|undo <key>|confirm <key>]"
}

// approveSkillDraft validates and promotes a draft. The user's explicit
// command is the approval, so the skill becomes a normal active skill.
func approveSkillDraft(ctx context.Context, app *App, sp *skillsProfile, id int64, replace bool) string {
	e, err := sp.getSkillByID(ctx, id)
	if err != nil || e.Status != memory.StatusProposed {
		return fmt.Sprintf("/skills approve: #%d is not a pending skill draft", id)
	}
	if draftExpired(e, app.Cfg.SkillCreation.DraftTTLDaysOrDefault(), time.Now()) {
		_ = sp.rejectSkillDraft(ctx, id, "expired (draft TTL)")
		return fmt.Sprintf("/skills approve: draft #%d is older than the draft TTL (%d days) and was rejected — re-create it with /skill-creation now", id, app.Cfg.SkillCreation.DraftTTLDaysOrDefault())
	}
	if err := validateSkillTemplateValue(e.Value); err != nil {
		return fmt.Sprintf("/skills approve: draft #%d failed template validation: %v", id, err)
	}
	if containsSkillSecret(e.Value) {
		return fmt.Sprintf("/skills approve: draft #%d contains secret-like content — reject it or edit and re-create", id)
	}
	// Approval is the user's confirmation: an auto-created draft becomes an
	// "auto-confirmed" skill so /skills recent and /skills undo track it and the
	// retrieval pass may use it. Hand-made drafts keep their note.
	note := ""
	if strings.HasPrefix(e.Note, autoDraftNotePrefix) {
		rest := strings.TrimLeft(strings.TrimPrefix(e.Note, autoDraftNotePrefix), ": ")
		note = snippet(autoSkillNoteConfirmed+"; approved by user; "+rest, 900)
	}
	// One transaction: the replace guard, the supersede and the note update.
	_, old, err := sp.promoteSkillDraftAtomic(ctx, id, replace, note, "user")
	switch {
	case errors.Is(err, memory.ErrSkillExists):
		return fmt.Sprintf("/skills approve: skill %q is already active (%s) — approving draft #%d would replace it. "+
			"The old version stays in skill_history, but /skills undo would forget the key rather than restore it. "+
			"Review with /skills show %d, then run /skills approve %d replace to replace it.",
			e.Key, describeActiveSkill(old), id, id, id)
	case err != nil:
		return "/skills approve: " + err.Error()
	}
	replaced := ""
	if old != nil {
		replaced = fmt.Sprintf(" (replaced active version #%d — old version stays in skill_history)", old.ID)
	}
	return fmt.Sprintf("approved draft #%d → active skill %q%s", id, e.Key, replaced)
}

// draftExpired reports whether a pending draft is older than ttlDays at now.
func draftExpired(e *memory.Entry, ttlDays int, now time.Time) bool {
	return e.CreatedAt < now.Add(-time.Duration(ttlDays)*24*time.Hour).UnixMilli()
}

// describeActiveSkill renders "#id, by writer, date[, unconfirmed auto-created]".
func describeActiveSkill(e *memory.Entry) string {
	if e == nil {
		return "unknown"
	}
	s := fmt.Sprintf("#%d by %s, %s", e.ID, e.Writer, time.UnixMilli(e.CreatedAt).UTC().Format("2006-01-02"))
	if isUnconfirmedAutoSkill(e) {
		s += ", unconfirmed auto-created"
	}
	return s
}

// truncateForShow cuts s to at most max bytes on a rune boundary unless full.
// The second result describes the cut-off remainder ("" when nothing was cut).
func truncateForShow(s string, max int, full bool) (string, string) {
	if full || len(s) <= max {
		return s, ""
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], humanBytes(len(s) - cut)
}

func renderSkillReview(ctx context.Context, app *App, sp *skillsProfile) string {
	var b strings.Builder
	drafts, err := sp.listSkillDrafts(ctx)
	if err != nil {
		return "/skills review: " + err.Error()
	}
	if len(drafts) == 0 {
		b.WriteString("no pending skill drafts\n")
	} else {
		fmt.Fprintf(&b, "%d pending skill draft%s:\n", len(drafts), plural(len(drafts)))
		for _, d := range drafts {
			mark := ""
			if cur, err := sp.getActiveSkill(ctx, d.Key); err == nil && cur != nil {
				mark = " [REPLACES active skill — approve needs `replace`]"
			}
			fmt.Fprintf(&b, "  #%d %s%s  %s  %s  — %s\n", d.ID, d.Key, mark,
				time.UnixMilli(d.CreatedAt).UTC().Format("2006-01-02"), humanBytes(len(d.Value)),
				snippet(firstLine(d.Note), 100))
		}
		b.WriteString("  /skills show <id> [full] · approve <id> [replace] · reject <id> [reason]\n")
	}
	if recent := renderRecentAutoSkills(ctx, sp); recent != "" {
		b.WriteString(recent)
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderRecentAutoSkills(ctx context.Context, sp *skillsProfile) string {
	all, err := sp.listActiveSkills(ctx)
	if err != nil {
		return "/skills recent: " + err.Error()
	}
	var auto []*memory.Entry
	for _, e := range all {
		if isAutoSkill(e) {
			auto = append(auto, e)
		}
	}
	if len(auto) == 0 {
		return "no auto-created skills"
	}
	sort.Slice(auto, func(i, j int) bool { return auto[i].CreatedAt > auto[j].CreatedAt })
	var b strings.Builder
	fmt.Fprintf(&b, "%d auto-created skill%s:\n", len(auto), plural(len(auto)))
	for _, e := range auto {
		state := "confirmed"
		if isUnconfirmedAutoSkill(e) {
			state = "unconfirmed (not usable by the model, not auto-injected)"
		}
		fmt.Fprintf(&b, "  %s  %s  %s\n", e.Key, time.UnixMilli(e.CreatedAt).UTC().Format("2006-01-02"), state)
	}
	b.WriteString("  /skills confirm <key> · undo <key>\n")
	return b.String()
}
