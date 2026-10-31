package agent

// skill_creation_now.go — triggers for the skill-creation pipeline: the
// end-of-turn hook (driven by the in-session B1/B2 detector) and the manual
// `/skill-creation now [key] [paths…]` command.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/safe"
)

// setSkillSignal records that the detector fired this turn.
func (a *App) setSkillSignal(sig string) {
	a.skillCreate.mu.Lock()
	a.skillCreate.pendingSignal = sig
	a.skillCreate.mu.Unlock()
}

func (a *App) takeSkillSignal() string {
	a.skillCreate.mu.Lock()
	defer a.skillCreate.mu.Unlock()
	s := a.skillCreate.pendingSignal
	a.skillCreate.pendingSignal = ""
	return s
}

// onTurnEndSkillCreation runs after a successful main-agent turn. It consumes
// the detector signal and, depending on the mode, suggests or starts a run.
func (a *App) onTurnEndSkillCreation() {
	if a.IsSubagent || a.SkillStore == nil {
		return
	}
	sig := a.takeSkillSignal()
	if sig == "" || len(a.writtenSkillPaths()) == 0 {
		return
	}
	mode, _ := a.SkillCreationMode()
	switch mode {
	case config.SkillCreationOff:
	case config.SkillCreationOn:
		a.sendEvent(SysNoteMsg{Text: "This looks like reusable work. /skill-creation now builds a skill draft from the files written in this session (/skill-creation auto automates it)."})
	case config.SkillCreationAuto, config.SkillCreationAutoMashura:
		if msg := a.startSkillCreation(skillCreationRequest{Mode: mode}); msg != "" {
			a.sendEvent(SysNoteMsg{Text: msg})
		}
	}
}

// startSkillCreation launches one pipeline run in the background. It returns a
// one-line status note ("" = nothing to say). Only one run at a time.
func (a *App) startSkillCreation(req skillCreationRequest) string {
	if a.IsSubagent {
		return ""
	}
	if a.getSkillStore() == nil {
		return "skill creation: the skills store is not available"
	}
	if !a.skillCreate.busy.CompareAndSwap(false, true) {
		return "skill creation: a run is already in progress"
	}
	var snap []writtenPath
	if len(req.Paths) == 0 {
		_, snap = a.writtenSkillSnapshot()
	}
	timeout := time.Duration(a.Cfg.SkillCreation.ReviewTimeoutSecondsOrDefault()) * time.Second
	safe.Go("skill-creation", func() {
		defer a.skillCreate.busy.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		out := a.runSkillCreation(ctx, req)
		if ctx.Err() != nil && out.Kind == "aborted" {
			out.Text = "timed out after " + timeout.String() + " — " + out.Text
		}
		if out.Kind == "active" || out.Kind == "draft" {
			a.consumeWritten(snap)
		}
		prefix := "skill creation: "
		if out.Kind == "aborted" {
			prefix = "skill creation aborted: "
		}
		a.sendEvent(SysNoteMsg{Text: prefix + out.Text})
		a.skillCreate.recordOutcome(out)
	})
	return "skill creation: building a skill from this session's files in the background…"
}

// recordOutcome stores the last outcome (tests and /skill-creation status).
func (s *skillCreationState) recordOutcome(o skillCreationOutcome) {
	s.mu.Lock()
	s.last = o
	s.mu.Unlock()
}

// skillCreationNowCmd implements `/skill-creation now [key] [paths…]`.
// A first argument that does not look like a path is the skill key.
func skillCreationNowCmd(app *App, args []string) Cmd {
	if app.IsSubagent {
		return NoteCmd("/skill-creation now: not available in subagents")
	}
	req := skillCreationRequest{Manual: true}
	for i, a := range args {
		if i == 0 && skillKeyRE.MatchString(a) && !strings.ContainsAny(a, "./") {
			req.Key = a
			continue
		}
		req.Paths = append(req.Paths, a)
	}
	mode, _ := app.SkillCreationMode()
	req.Mode = config.SkillCreationAuto // draft
	if mode == config.SkillCreationAutoMashura {
		req.Mode = config.SkillCreationAutoMashura
	}
	if len(req.Paths) == 0 && len(app.writtenSkillPaths()) == 0 {
		return NoteCmd("/skill-creation now: no files were written in this session — pass paths: /skill-creation now [key] <path>…")
	}
	msg := app.startSkillCreation(req)
	if msg == "" {
		msg = fmt.Sprintf("skill creation: not started")
	}
	return NoteCmd(msg)
}
