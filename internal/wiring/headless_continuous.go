package wiring

// headless_continuous.go: the experimental bounded continuous coordinator for
// `wakil run --continue`.
//
// SCOPE (deliberately narrow, per the Mashūra plan review):
//   - one embedded headless session, one *agent.App, one goal, one process
//     attempt;
//   - no resume, no daemon integration, no TUI controls, no scheduling, no
//     parallel subagents, and no detached background jobs;
//   - completion is coordinator-owned: the model may only PROPOSE an outcome via
//     finalize_goal, and the coordinator decides acceptance;
//   - automatic verified completion requires explicit, user-declared verifier
//     commands — never auto-detected ones;
//   - every terminal record is marked resumable:false. Session persistence is
//     diagnostic/conversational only, and there is NO replay protection for
//     external side effects in this prototype (that is CM4).
//
// Because shell and verifier execution can perform external mutations whose
// effects are not tracked here, this mode is EXPERIMENTAL. It does not imply
// that --continue is safe for unattended destructive work.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionhost"
	wtools "github.com/treeol/wakil/internal/tools"
	"github.com/treeol/wakil/internal/verify"
)

// ExitIncomplete is the CM2 exit code for a bounded run that stopped without a
// verified goal: limits reached, verification exhausted, or an incomplete stop.
// It is only ever returned for --continue; ordinary one-shot codes are
// unchanged.
const ExitIncomplete = 5

// ContinuousSchemaVersion versions the continuous-mode terminal record so
// consumers can detect the contract change from ordinary headless output.
const ContinuousSchemaVersion = 1

// ContinuousOptions carries the explicit, CLI-only bounds for --continue.
// These are NOT config.Config fields: continuous execution is opt-in per run
// and is never activated by a config file.
type ContinuousOptions struct {
	// MaxTurns is the maximum number of admitted agent turns (SubmitInput
	// admissions). Required and must be > 0 when continuous mode is on.
	MaxTurns int
	// MaxTime bounds the whole run, including waiting and verification.
	// Required and must be > 0 when continuous mode is on.
	MaxTime time.Duration
	// VerifyCommands are the explicit, user-declared acceptance checks. Empty
	// means no verified completion is possible (complete_unverified).
	VerifyCommands []string
	// MaxProtocolCorrections bounds the corrective turns issued for a missing
	// or invalid finalize_goal proposal. Defaults to 1.
	MaxProtocolCorrections int
	// MaxVerificationBatches bounds verification attempts (initial batch plus
	// corrective batches). Defaults to 2.
	MaxVerificationBatches int
}

// continuousTerminal is the single terminal record for a continuous run.
type continuousTerminal struct {
	Outcome        string
	Reason         string
	Turns          int
	ModelCalls     int
	ElapsedMs      int64
	Verification   string
	Summary        string
	RequiredAction string
}

// submit admits one input to the host session. The host returns a TurnAck on
// acceptance; acceptance is not execution, so the coordinator always waits for
// the projected turn terminal before deciding.
func (s *continuousState) submit(ctx context.Context, p core.Principal, id event.SessionID, text string) (core.TurnAck, error) {
	return s.host.SubmitInput(ctx, p, core.SubmitInputRequest{SessionID: id, Text: text})
}

// runContinuousTask drives the bounded continuous loop. It mirrors
// runSingleTask's bootstrap (host turn function, session, subscription) and
// then admits successive turns until the coordinator reaches exactly one
// terminal decision.
func runContinuousTask(ctx context.Context, app *agent.App, task string, opts HeadlessOptions, cont ContinuousOptions, out io.Writer) int {
	hw := NewHeadlessWriter(out)
	if opts.TranscriptFile != "" {
		defer app.SaveSession()
	}
	defer hw.Flush()
	defer app.StopAllAsyncOps()

	// Guard the install once: the finalization tool is only valid in this mode.
	app.Tools = append(app.Tools, wtools.ContinuousTools()...)

	state := &continuousState{app: app, opts: opts, cont: cont, out: out, start: time.Now()}
	// Deadline covers waiting and verification, so it must be the root context
	// for the whole loop rather than a per-turn context.
	runCtx, cancel := context.WithTimeout(ctx, cont.MaxTime)
	defer cancel()
	state.runCtx = runCtx
	app.SetFinalizeGoal(state.recordProposal)

	turnFn, err := HostTurnFunc(app, WithResolver(headlessResolver(app, opts)))
	if err != nil {
		emitEvent(out, map[string]any{"type": "error", "message": err.Error()})
		return ExitError
	}
	state.hw = hw
	h := sessionhost.New(turnFn, headlessStoreOpts(app.SessionWorkspace())...)
	defer h.Close(context.Background())
	p := core.EmbeddedPrincipal()

	sess, err := h.CreateSession(runCtx, p, core.CreateSessionRequest{
		Workspace: event.WorkspaceID("wsp_local"),
		Title:     "headless-continuous",
	})
	if err != nil {
		emitEvent(out, map[string]any{"type": "error", "message": err.Error()})
		return ExitError
	}
	sub, err := h.Subscribe(runCtx, p, sess.ID, 0)
	if err != nil {
		emitEvent(out, map[string]any{"type": "error", "message": err.Error()})
		return ExitError
	}
	defer sub.Close()

	// Experimental warning first: consumers must not read this mode as a
	// durable, replay-safe autonomous runner.
	emitEvent(out, map[string]any{
		"type":    "warning",
		"message": "continuous mode is experimental: no resume, no replay protection for external side effects, soft cost budget",
	})

	code := state.loop(h, sub, sess, p, task)
	term := state.finish()
	state.emitTerminal(term, code)

	// Token + cost summary last, matching the legacy single-task ordering.
	if app.Costs != nil {
		_, rows := app.Costs.Snapshot()
		var inTok, outTok int64
		for _, r := range rows {
			inTok += r.InputTok
			outTok += r.OutputTok
		}
		total, _ := app.Costs.Snapshot()
		record := map[string]any{"type": "tokens", "input": inTok, "output": outTok}
		if total > 0 || app.BudgetUSD > 0 {
			record["cost_usd"] = total
		}
		if app.BudgetUSD > 0 {
			record["budget_usd"] = app.BudgetUSD
			record["budget_exhausted"] = app.BudgetExhausted()
		}
		emitEvent(out, record)
	}
	return code
}

// continuousState is the per-run coordinator state. All of it lives in memory
// for this prototype: no durable goal record, no lease, no operation ledger.
type continuousState struct {
	app    *agent.App
	opts   HeadlessOptions
	cont   ContinuousOptions
	out    io.Writer
	start  time.Time
	runCtx context.Context

	turns         int
	protocolFixes int
	verifyBatches int
	proposal      *agent.GoalFinalization

	// Session handles captured at loop start so the verification-retry path can
	// admit a corrective turn without re-plumbing them through every helper.
	// hw is the ONE writer used for every turn, so a newline-free trailing line
	// is flushed per turn instead of being dropped at process exit.
	sub       core.EventSubscription
	sessionID event.SessionID
	principal core.Principal
	host      *sessionhost.Host
	hw        *HeadlessWriter

	// verification is the coordinator's receipt, recorded only when the
	// coordinator itself ran the checks.
	verification string
	term         continuousTerminal
}

// recordProposal captures a validated finalize_goal proposal. A second
// proposal in the same turn is a contradiction, not a later update.
func (s *continuousState) recordProposal(p agent.GoalFinalization) error {
	if s.proposal != nil {
		return fmt.Errorf("a finalize_goal proposal was already recorded for this turn")
	}
	cp := p
	s.proposal = &cp
	return nil
}

// takeProposal returns and clears the recorded proposal for the current turn.
func (s *continuousState) takeProposal() *agent.GoalFinalization {
	p := s.proposal
	s.proposal = nil
	return p
}

func (s *continuousState) finish() continuousTerminal {
	if s.term.Outcome == "" {
		s.term.Outcome = "failed"
		s.term.Reason = "internal_no_terminal"
	}
	s.term.Turns = s.turns
	// Model-call accounting is derived from the session cost ledger. An unpriced
	// source can still have recorded usage, so rows — not USD — are counted.
	if s.app.Costs != nil {
		_, rows := s.app.Costs.Snapshot()
		s.term.ModelCalls = len(rows)
	}
	s.term.ElapsedMs = time.Since(s.start).Milliseconds()
	if s.term.Verification == "" {
		s.term.Verification = "not_run"
	}
	return s.term
}

func (s *continuousState) stop(outcome, reason string) {
	if s.term.Outcome == "" {
		s.term.Outcome = outcome
		s.term.Reason = reason
	}
}

// emitTerminal writes exactly one terminal record. resumable is always false
// for CM2: this prototype has no durable goal state to resume from.
func (s *continuousState) emitTerminal(t continuousTerminal, code int) {
	ev := map[string]any{
		"type":           "done",
		"schema_version": ContinuousSchemaVersion,
		"mode":           "continuous",
		"outcome":        t.Outcome,
		"reason":         t.Reason,
		"turns":          t.Turns,
		"model_calls":    t.ModelCalls,
		"elapsed_ms":     t.ElapsedMs,
		"resumable":      false,
		"exit_code":      code,
		"verification":   map[string]any{"status": t.Verification},
	}
	if t.Summary != "" {
		ev["summary"] = t.Summary
	}
	if t.RequiredAction != "" {
		ev["required_user_action"] = t.RequiredAction
	}
	emitEvent(s.out, ev)
}

// admitNext reports whether another turn may be admitted. The boundary rule is
// strict: reaching the last allowed turn prohibits the NEXT admission, it never
// discards a completion from the last allowed turn.
func (s *continuousState) admitNext() (bool, string) {
	if s.term.Outcome != "" {
		return false, ""
	}
	if s.turns >= s.cont.MaxTurns {
		return false, "max_turns_reached"
	}
	if err := s.runCtx.Err(); err != nil {
		if err == context.DeadlineExceeded {
			return false, "deadline_exceeded"
		}
		return false, "cancelled"
	}
	if s.app.BudgetExhausted() {
		return false, "budget_exhausted"
	}
	return true, ""
}

// loop admits successive turns until exactly one terminal decision is reached.
func (s *continuousState) loop(h *sessionhost.Host, sub core.EventSubscription, sess core.Session, p core.Principal, firstInput string) int {
	s.sub, s.sessionID, s.principal = sub, sess.ID, p
	s.host = h
	out, hw := s.out, s.hw
	next := firstInput
	for {
		if ok, reason := s.admitNext(); !ok {
			s.stop("incomplete", reason)
			return ExitIncomplete
		}
		if _, err := s.submit(s.runCtx, p, sess.ID, next); err != nil {
			if reason, bounded := s.stopReasonForContext(); bounded {
				s.stop("incomplete", reason)
				return ExitIncomplete
			}
			s.stop("failed", "submit_rejected")
			emitEvent(s.out, map[string]any{"type": "error", "message": err.Error()})
			return ExitError
		}
		s.turns++

		code, msg := consumeTurnEvents(s.runCtx, sub, hw)
		hw.Flush()
		emitEvent(out, map[string]any{"type": "turn", "n": s.turns, "exit_hint": code})
		if code == ExitBackendFailure {
			s.stop("failed", "backend_failure")
			s.term.RequiredAction = msg
			return ExitBackendFailure
		}
		if code == ExitError {
			// A context failure during consumption is a bounded stop, not a
			// generic turn error.
			if reason, bounded := s.stopReasonForContext(); bounded {
				s.stop("incomplete", reason)
				return ExitIncomplete
			}
			s.stop("failed", "turn_error")
			s.term.RequiredAction = msg
			return ExitError
		}
		if code == ExitDeclined {
			// A decline is terminal for the run: an unattended coordinator must
			// not route around a permission decision.
			s.stop("blocked", "permission_declined")
			s.term.RequiredAction = msg
			return ExitDeclined
		}

		// The turn returned cleanly. Only a clean final may be considered; an
		// abnormal outcome must never be promoted to a completion candidate.
		if !s.app.Quiesced() {
			s.stop("incomplete", "async_work_pending")
			return ExitIncomplete
		}

		proposal := s.takeProposal()
		if proposal == nil {
			if s.protocolFixes >= s.protocolBudget() {
				s.stop("failed", "finalization_invalid")
				return ExitIncomplete
			}
			s.protocolFixes++
			next = protocolCorrectionPrompt
			continue
		}

		switch proposal.Status {
		case agent.FinalizeGoalBlocked:
			s.stop("blocked", "requires_user_input")
			s.term.RequiredAction = proposal.RequiredUserAction
			return ExitDeclined
		case agent.FinalizeGoalContinue:
			if s.turns >= s.cont.MaxTurns {
				s.stop("incomplete", "max_turns_reached")
				return ExitIncomplete
			}
			next = continuePrompt(proposal)
			continue
		case agent.FinalizeGoalComplete:
			return s.decideCompletion(proposal)
		default:
			s.stop("failed", "finalization_invalid")
			return ExitIncomplete
		}
	}
}

// decideCompletion is the coordinator-owned completion decision. The model
// proposal is only a claim; verification evidence is the coordinator's.
func (s *continuousState) decideCompletion(p *agent.GoalFinalization) int {
	if len(s.cont.VerifyCommands) == 0 {
		// No declared acceptance contract means "all declared checks passed"
		// cannot be asserted. This is NOT a silent success.
		s.stop("complete_unverified", "no_verification_declared")
		s.term.Summary = p.Summary
		s.term.Verification = "not_declared"
		return ExitIncomplete
	}
	if !s.app.Quiesced() {
		s.stop("incomplete", "async_work_pending")
		return ExitIncomplete
	}

	for {
		if s.verifyBatches >= s.verifyBudget() {
			s.stop("incomplete", "verification_exhausted")
			s.term.Verification = "exhausted"
			s.term.Summary = p.Summary
			return ExitIncomplete
		}
		if s.runCtx.Err() != nil {
			if reason, bounded := s.stopReasonForContext(); bounded {
				s.stop("incomplete", reason)
			} else {
				s.stop("incomplete", "deadline_exceeded")
			}
			return ExitIncomplete
		}
		s.verifyBatches++

		outcome, err := s.runVerifiers()
		if err != nil {
			s.stop("failed", "verification_error")
			s.term.Verification = "error"
			emitEvent(s.out, map[string]any{"type": "error", "message": err.Error()})
			return ExitError
		}
		emitEvent(s.out, map[string]any{
			"type":    "verify",
			"batch":   s.verifyBatches,
			"passed":  outcome.Passed(),
			"summary": strings.TrimRight(outcome.Summarize(), "\n"),
		})
		// Recheck the deadline/cancellation BEFORE accepting a verification
		// result. A verifier that returns after cancellation must not be able to
		// close the goal, otherwise a verified completion could outlive the
		// run's own bound.
		if reason, bounded := s.stopReasonForContext(); bounded {
			s.stop("incomplete", reason)
			s.term.Verification = "not_accepted"
			return ExitIncomplete
		}
		if outcome.Passed() {
			s.term.Outcome = "complete_verified"
			s.term.Reason = "goal_complete"
			s.term.Verification = "passed"
			s.term.Summary = p.Summary
			return ExitOK
		}
		if outcome.AnyDeclined() {
			s.stop("blocked", "verification_declined")
			s.term.Verification = "declined"
			return ExitDeclined
		}
		// Failed. A corrective turn is only worth admitting if a verification
		// batch still remains to judge its result: otherwise the repair could
		// mutate the workspace with no remaining opportunity to check it.
		if s.verifyBatches >= s.verifyBudget() {
			s.stop("incomplete", "verification_exhausted")
			s.term.Verification = "failed"
			s.term.Summary = p.Summary
			return ExitIncomplete
		}
		if ok, reason := s.admitNext(); !ok {
			s.stop("incomplete", reason)
			s.term.Verification = "failed"
			return ExitIncomplete
		}
		if _, err := s.submit(s.runCtx, s.principal, s.sessionID, verificationCorrectionPrompt(outcome)); err != nil {
			if r, bounded := s.stopReasonForContext(); bounded {
				s.stop("incomplete", r)
				return ExitIncomplete
			}
			s.stop("failed", "submit_rejected")
			return ExitError
		}
		s.turns++
		code, msg := consumeTurnEvents(s.runCtx, s.sub, s.hw)
		s.hw.Flush()
		emitEvent(s.out, map[string]any{"type": "turn", "n": s.turns, "exit_hint": code})
		if code != ExitOK {
			// Route the repair turn through the same outcome mapping as an
			// ordinary turn: a decline is a block, not a generic failure, and
			// the decline reason must survive.
			if code == ExitDeclined {
				s.stop("blocked", "permission_declined")
				s.term.RequiredAction = msg
				return ExitDeclined
			}
			if r, bounded := s.stopReasonForContext(); bounded {
				s.stop("incomplete", r)
				return ExitIncomplete
			}
			s.stop("failed", "verification_repair_turn_failed")
			return code
		}
		if !s.app.Quiesced() {
			s.stop("incomplete", "async_work_pending")
			return ExitIncomplete
		}
		next := s.takeProposal()
		if next == nil {
			s.stop("failed", "finalization_invalid")
			s.term.Verification = "failed"
			return ExitIncomplete
		}
		switch next.Status {
		case agent.FinalizeGoalComplete:
			p = next
		case agent.FinalizeGoalBlocked:
			s.stop("blocked", "requires_user_input")
			s.term.RequiredAction = next.RequiredUserAction
			s.term.Verification = "failed"
			return ExitDeclined
		case agent.FinalizeGoalContinue:
			// A repair turn may legitimately report more remaining work; the
			// normal loop continues from there.
			s.stop("incomplete", "verification_failed_continue")
			s.term.Verification = "failed"
			return ExitIncomplete
		default:
			s.stop("failed", "finalization_invalid")
			s.term.Verification = "failed"
			return ExitIncomplete
		}
	}
}

// stopReasonForContext maps the run context state to a terminal reason. ok is
// false when the context is still live.
func (s *continuousState) stopReasonForContext() (reason string, ok bool) {
	switch {
	case errors.Is(s.runCtx.Err(), context.DeadlineExceeded):
		return "deadline_exceeded", true
	case errors.Is(s.runCtx.Err(), context.Canceled):
		return "cancelled", true
	default:
		return "", false
	}
}

// runVerifiers runs the frozen, user-declared commands through the agent's
// consent-gated verification path. The headless confirmer is installed for the
// duration so verification cannot fall through to a default confirmer.
func (s *continuousState) runVerifiers() (verify.Outcome, error) {
	cmds := make([]verify.Command, 0, len(s.cont.VerifyCommands))
	for _, c := range s.cont.VerifyCommands {
		cmds = append(cmds, verify.Command{Cmd: c, Source: "continuous"})
	}
	prev := s.app.Confirm
	s.app.Confirm = func(tool, headline, detail string, readAction bool) bool {
		choice, _ := HeadlessDecision(s.app, s.opts, ApprovalRequest{
			ToolName: tool, Headline: headline, Detail: detail, ReadAction: readAction,
		})
		return choice == agent.ChoiceApprove
	}
	defer func() { s.app.Confirm = prev }()
	return agent.RunVerification(s.runCtx, s.app, cmds), nil
}

func (s *continuousState) protocolBudget() int {
	if s.cont.MaxProtocolCorrections > 0 {
		return s.cont.MaxProtocolCorrections
	}
	return 1
}

func (s *continuousState) verifyBudget() int {
	if s.cont.MaxVerificationBatches > 0 {
		return s.cont.MaxVerificationBatches
	}
	return 2
}

const protocolCorrectionPrompt = "[continuous] Your previous turn ended without a finalize_goal proposal. " +
	"Call finalize_goal exactly once with status \"continue\" (with remaining_work) or \"complete\". " +
	"Prose without the tool call is not a decision."

func continuePrompt(p *agent.GoalFinalization) string {
	var b strings.Builder
	b.WriteString("[continuous] finalize_goal(status=continue) received. Summary: ")
	b.WriteString(p.Summary)
	if len(p.RemainingWork) > 0 {
		b.WriteString("\n\nRemaining work you reported:\n")
		for _, item := range p.RemainingWork {
			b.WriteString("· " + item + "\n")
		}
	}
	b.WriteString("\nContinue working, then call finalize_goal again.")
	return b.String()
}

func verificationCorrectionPrompt(o verify.Outcome) string {
	return "[continuous] Declared verification did not pass:\n" +
		strings.TrimRight(o.Summarize(), "\n") +
		"\n\nFix the cause, then call finalize_goal again. Do not weaken or rewrite the verification commands."
}
