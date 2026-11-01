package agent

// skill_creation_now.go — triggers for the skill-creation pipeline: the
// end-of-turn hook (driven by the in-session B1/B2 detector) and the manual
// `/skill-creation now [key] [paths…]` command.

import (
	"context"
	"strings"
	"time"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/safe"
)

// setSkillSignal records that the detector fired this turn.
// skillCreationBusyMsg is returned while a run is in flight.
const skillCreationBusyMsg = "skill creation: a run is already in progress"

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
	a.skillCreate.turnEnds.Add(1)
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
		// A run already in flight is not worth a note on every turn.
		if msg := a.startSkillCreation(skillCreationRequest{Mode: mode}); msg != "" && msg != skillCreationBusyMsg {
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
	// Admission first, snapshot second: a caller that waited for the flag must
	// not act on generations a finished run has already consumed.
	if !a.skillCreate.busy.CompareAndSwap(false, true) {
		return skillCreationBusyMsg
	}
	var snap []writtenPath
	var snapPaths []string
	if len(req.Paths) == 0 {
		snapPaths, snap = a.writtenSkillSnapshot()
		if len(snap) == 0 {
			a.skillCreate.busy.Store(false)
			return ""
		}
	}
	// The run captures exactly the snapshot taken here, so what it reads and
	// what it later consumes are the same set.
	if len(req.Paths) == 0 {
		req.Paths = snapPaths
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
		// Consume what the run captured. An automatic run that failed on its
		// input (secret, judged one-off, draft checks) also consumes it so the
		// same files do not re-trigger the same failure every turn; a rewrite
		// re-tracks them, and /skill-creation now <paths> still works.
		consumed := out.Kind == "active" || out.Kind == "draft"
		if out.Terminal && !req.Manual {
			consumed = true
			out.Text += " — automatic retries for these files are off; edit them or run /skill-creation now <paths>"
		}
		if consumed && snap != nil {
			a.consumeWritten(snap)
		}
		prefix := "skill creation: "
		if out.Kind == "aborted" {
			prefix = "skill creation aborted: "
		}
		if req.Manual || !a.skillCreate.sameAutoNote(prefix+out.Text) {
			a.sendEvent(SysNoteMsg{Text: prefix + out.Text})
		}
		a.skillCreate.recordOutcome(out)
	})
	return "skill creation: building a skill from this session's files in the background…"
}

// sameAutoNote reports whether text equals the previous automatic run's note
// (and remembers it), so identical automatic notes are shown once.
func (s *skillCreationState) sameAutoNote(text string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	same := s.lastAutoNote == text
	s.lastAutoNote = text
	return same
}

// recordOutcome stores the last outcome (read by tests).
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
		msg = "skill creation: not started"
	}
	return NoteCmd(msg)
}
