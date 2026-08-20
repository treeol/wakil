package sessionclient

import (
	"context"
	"errors"
	"time"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/proxy"
)

// ---- Neutral DTOs (mirror agent.* types the TUI consumes) ----
//
// Only agent.* types need neutralization — the TUI already imports proxy
// legitimately (proxy.Message, proxy.Tool, proxy.ImagePart,
// proxy.GroundingEntry, proxy.CostTracker stay as-is; Gate #1 does not touch
// proxy). These mirrors are the minimal set the TUI reads or the facade returns.

// Consent mirrors agent.ConsentSnapshot — the atomic consent state for the
// session. Three bools that must be read as a consistent set (no tearing).
type Consent struct {
	AutoApprove      bool
	AllowDestructive bool
	AllowReads       bool
}

// ContextLimit mirrors agent.ContextLimit — the authoritative per-slot context
// window. NCtx is the hard ceiling in tokens; Usable() is the budget for prompt
// assembly. The TUI's ctx gauge and compaction thresholds key off this.
type ContextLimit struct {
	NCtx            int
	NCtxTrain       int
	Source          string // "backend" or "fallback"
	ContextSource   string // "props", "model_meta", or ""
	UsableCtx       int
	ReasoningBudget int
	AnswerMargin    int
	ModelUnresolved bool
}

// FromBackend reports whether NCtx came from the live backend (true) rather
// than the configured fallback (false).
func (c ContextLimit) FromBackend() bool { return c.Source == "backend" }

// Usable returns the token budget available for assembling a turn's prompt.
// When the proxy reported a usable_ctx, it is authoritative; otherwise falls
// back to NCtx minus reservations. Never negative; clamps to 1.
func (c ContextLimit) Usable() int {
	if c.UsableCtx > 0 {
		return c.UsableCtx
	}
	u := c.NCtx - c.ReasoningBudget - c.AnswerMargin
	if u < 1 {
		return 1
	}
	return u
}

// Backend mirrors agent.BackendInfo — a backend entry in the backend list.
type Backend struct {
	Name     string
	External bool
	Caps     []string
}

// OpID is the neutral identifier for side questions and detached async jobs.
// It mirrors agent.SideQuestionID (a string) but is its own type so the TUI
// never imports agent for it.
type OpID string

// SessionSummary mirrors the subset of agent.Session the TUI's resume picker
// displays. The full transcript lives in proxy.Message slices (already
// proxy-typed), so it stays. SavedWorkflow is omitted from 7b1 (the TUI's
// resume path restores it through the facade, not by reading the field
// directly — that lands in 7b3).
type SessionSummary struct {
	ChatID    string
	Model     string
	Label     string
	Workspace string
	Created   time.Time
	Updated   time.Time
	Conv      []proxy.Message
}

// Turns counts the user turns in the session and returns the first user
// message text (the resume picker's "N turns · <first message>" row). Mirrors
// agent.SessionTurns without the agent import.
func (s SessionSummary) Turns() (int, string) {
	turns, first := 0, ""
	for _, m := range s.Conv {
		if m.Role == "user" {
			turns++
			if first == "" {
				if m.Content != nil {
					first = *m.Content
				}
			}
		}
	}
	return turns, first
}

// SessionScope narrows a session listing to one workspace, or everything.
// Mirrors agent.SessionScope.
type SessionScope struct {
	Workspace string
	All       bool
}

// ApprovalChoice mirrors agent.ConfirmChoice — the TUI's answer to an approval.
// It is its own type so the TUI never imports agent.ConfirmChoice.
type ApprovalChoice int

const (
	ChoiceDecline    ApprovalChoice = iota // do not run
	ChoiceApprove                          // run this one
	ChoiceAllowReads                       // run this one and auto-approve future reads
)

// ApprovalRequest mirrors the fields of agent.ConfirmReqMsg that the TUI
// displays. The response channel (RespCh) is deliberately absent — async
// approval is answered through the host's RespondToApproval, not a channel.
type ApprovalRequest struct {
	ToolName   string
	Headline   string
	Detail     string
	ReadAction bool
}

// ---- ClientSnapshot (D26) ----

// ClientSnapshot is the immutable, version-stamped view of the session state
// the TUI renders. It is atomically constructed and reconciled on events; the
// TUI reads it rather than poking at *agent.App fields directly.
//
// Consent is the one field that can change mid-turn (a deferred /auto grant
// applies at AgentDoneMsg); it is exposed as a live read alongside the snapshot
// (see Consent() on the facade). Every other field is snapshotted.
//
// In 7b1 this is the contract; the wiring-side construction (populating it from
// a live *agent.App) lands in 7b3.
type ClientSnapshot struct {
	// Session identity.
	SessionID  event.SessionID
	ChatID     string // proxy chat ID (display only)
	Title      string
	Workspace  string
	Backend    string // selected backend
	Model      string // selected or effective model

	// Transcript.
	Conv []proxy.Message

	// Query-state fields (D24 class: snapshot, not events).
	ContextLimit  ContextLimit
	ModelList     []string
	BackendList   []Backend
	Tools         []proxy.Tool
	PendingImages []proxy.ImagePart
	RawTools      bool

	// Output mode (snapshotted once at session creation; never changes).
	OutputMode config.OutputMode

	// Costs.
	Costs *proxy.CostTracker

	// Workflow.
	Workflow *WorkflowSnapshot

	// Version is a monotonically increasing sequence number. The TUI can
	// detect a stale snapshot by comparing versions. In 7b1 this is a
	// placeholder (0); the wiring-side versioning lands in 7b3.
	Version uint64
}

// WorkflowSnapshot mirrors the subset of workflow.WorkflowState the TUI
// displays. It is its own type so the TUI never imports workflow transitively
// through agent.
type WorkflowSnapshot struct {
	Task      string
	Phase     string
	StepCount int
	StepIdx   int
	PlanPath  string
}

// ---- CommandResult (D23) ----

// CommandResult is the agent-free return from slash-command dispatch. It
// replaces the `(handled, quit, cmd agent.Cmd)` return of agent.HandleTUICommand.
//
// The TUI acts on the fields:
//   - Handled: the command was recognized.
//   - Quit: the TUI should exit.
//   - Notice: a status-line string to display (replaces SysNoteMsg text).
//   - Submit: non-empty → the TUI submits this as the next turn's input
//     (replaces the /learn, /remember, /recall, /plan workflow submissions).
//   - Rotate: non-nil → the TUI rotates the conversation (D27).
//   - SideQuestion: non-empty → the TUI starts a side question with this text.
//   - Compacted: true → the TUI runs its compaction-completed handler.
//   - OpID: non-empty → the command initiated an async operation (e.g.
//     /handoff, /remember, /recall). The TUI observes progress via events
//     keyed by this OpID. The command is not "done" until the corresponding
//     completion event arrives.
//
// No field carries an agent.* type. The TUI never imports agent to interpret
// this struct.
type CommandResult struct {
	Handled      bool
	Quit         bool
	Notice       string
	Submit       string         // non-empty → submit as next turn
	Rotate       *RotateRequest // non-nil → rotate the conversation
	SideQuestion string         // non-empty → start a side question
	Compacted    bool
	OpID         OpID           // non-empty → async op initiated; observe via events
}

// RotateRequest tells the TUI to rotate the conversation (D27). The rotation
// type determines state carryover (consent/auto survive /new and /handoff;
// pending images survive /new but clear on /handoff; /resume loads a
// pre-loaded session).
type RotateRequest struct {
	// Type is "new", "handoff", or "resume".
	Type string
	// Session is the pre-loaded session for /resume (nil for /new and /handoff,
	// which build a fresh App). Only the fields the TUI needs to display are
	// carried; the facade constructs the new App internally.
	Session *SessionSummary
	// HandoffContext is the folded context for /handoff's continuation turn
	// (non-empty only when Proceed is true).
	HandoffContext string
	// Proceed is true when the rotation should auto-start a continuation
	// turn (the /handoff proceed behavior).
	Proceed bool
}

// Validate checks that the CommandResult is not self-contradictory. It enforces
// mutual exclusivity of the action fields (at most one of Quit, Submit, Rotate,
// SideQuestion) — a command that tries to quit AND submit AND rotate at once is
// a programming error. Notice and Compacted are display-only and may appear
// alongside any action.
func (cr CommandResult) Validate() error {
	actions := 0
	if cr.Quit {
		actions++
	}
	if cr.Submit != "" {
		actions++
	}
	if cr.Rotate != nil {
		actions++
	}
	if cr.SideQuestion != "" {
		actions++
	}
	if actions > 1 {
		return ErrInvalidCommandResult
	}
	if cr.Rotate != nil {
		return cr.Rotate.Validate()
	}
	return nil
}

// ErrInvalidCommandResult is returned by CommandResult.Validate when the result
// carries contradictory action fields.
var ErrInvalidCommandResult = errors.New("sessionclient: invalid CommandResult — at most one of Quit/Submit/Rotate/SideQuestion")

// Validate checks that the RotateRequest has a valid Type.
func (rr *RotateRequest) Validate() error {
	switch rr.Type {
	case "new", "handoff", "resume":
		return nil
	default:
		return ErrInvalidRotateType
	}
}

// ErrInvalidRotateType is returned by RotateRequest.Validate when Type is not
// one of the supported rotation kinds.
var ErrInvalidRotateType = errors.New("sessionclient: invalid RotateRequest type")

// Facade is the agent-free client surface the TUI consumes. It composes the
// session host's three service interfaces (SessionService, EventReader,
// SessionReader) with the TUI-specific operations that today go through
// *agent.App directly.
//
// The TUI holds one Facade per conversation. Rotation (/new, /resume,
// /handoff) swaps the whole facade. The implementation (in internal/wiring)
// owns the triple (App, Host, subscription) and bridges between this contract
// and the real agent loop.
//
// 7b1: the interface is defined here and the DTOs are complete. The wiring
// implementation lands in 7b3. In 7b1 the TUI does NOT consume this yet — it
// keeps driving *agent.App until 7b3. This package compiles standalone.
type Facade interface {
	// ---- Session host surfaces (delegated to core) ----

	// CreateSession creates a new session and returns its snapshot.
	CreateSession(ctx context.Context, principal core.Principal, req core.CreateSessionRequest) (core.Session, error)
	// SubmitInput enqueues one input and returns a TurnAck immediately.
	SubmitInput(ctx context.Context, principal core.Principal, req core.SubmitInputRequest) (core.TurnAck, error)
	// RespondToApproval answers a pending approval.
	RespondToApproval(ctx context.Context, principal core.Principal, d core.ApprovalDecision) error
	// Interrupt cancels the in-flight turn.
	Interrupt(ctx context.Context, principal core.Principal, sessionID event.SessionID) error
	// CloseSession ends the session.
	CloseSession(ctx context.Context, principal core.Principal, sessionID event.SessionID) error

	// Subscribe returns a live event stream for the session.
	Subscribe(ctx context.Context, principal core.Principal, sessionID event.SessionID, after event.Seq) (core.EventSubscription, error)
	// ListEvents returns the durable history.
	ListEvents(ctx context.Context, principal core.Principal, sessionID event.SessionID, after event.Seq, limit int) ([]event.Event, error)
	// SessionSnapshot returns the session metadata + durable events.
	SessionSnapshot(ctx context.Context, principal core.Principal, sessionID event.SessionID) (core.SessionSnapshot, error)

	// ---- TUI-specific surfaces (replace direct *agent.App reads) ----

	// Snapshot returns the immutable, version-stamped view of the session
	// state. The TUI reads this rather than poking at App fields. Reconciled
	// on events; the TUI re-fetches after an event batch.
	Snapshot() ClientSnapshot

	// Consent returns the current consent state. This is the one live read
	// alongside the snapshot (consent can change mid-turn via a deferred
	// /auto grant). Safe to call from any goroutine.
	Consent() Consent

	// CompletionSource returns the per-keystroke completion source backed by
	// the snapshot. Staleness is bounded by one keystroke (acceptable — the
	// old path read App fields directly with the same staleness).
	CompletionSource() CompletionSource

	// Info returns the deep-state view the info panel and status line render
	// (endpoint, cwd, context gauge, workflow label, MCP servers, grounding,
	// costs). Fetched on demand; NOT part of Snapshot — the fields are cheap
	// but numerous, and Snapshot is re-fetched on every event batch. All
	// slices are defensive copies.
	Info() InfoSnapshot

	// ---- Client-initiated mutations (D26, from grounding #11) ----
	// These map onto the agent.Control + agent.StateApply methods the TUI
	// currently calls. Each is a facade op, not a direct App field write.

	SetAutoApprove(v bool)
	SetAllowDestructive(v bool)
	RevokeAuto()
	SetWorkflow(wf *WorkflowSnapshot) // nil clears
	AppendSystemMessage(m proxy.Message)
	SaveSession()
	ConsumeStartupNote() string
	SaveRepoState(mutate func(*RepoStateMutator))
	SetInfoPanelOpen(open bool)

	SetCtxLimit(lim ContextLimit)
	SetModelList(models []string)
	SetTools(tools []proxy.Tool)
	ReplacePendingImages(imgs []proxy.ImagePart)
	AddPendingImage(img proxy.ImagePart)
	ClearPendingImages()

	// ---- Side questions (D29) ----
	// StartSideQuestion starts a concurrent side-question stream. Returns
	// the operation ID and a cancellation function. The TUI observes progress
	// via events (not yet wired — 7b2).
	StartSideQuestion(ctx context.Context, question string) (OpID, context.CancelFunc)
	CancelSideQuestion(id OpID)

	// ---- Session listing (resume picker) ----
	ListSessions(scope SessionScope) ([]SessionSummary, int, error)
	LoadSession(idOrPrefix string) (*SessionSummary, error)

	// ---- Slash-command dispatch (D23) ----
	// DispatchCommand classifies a slash command and returns a neutral
	// CommandResult. It never returns an agent.Cmd or agent.Msg. The TUI
	// interprets the fields and acts. This is the agent-free replacement for
	// agent.HandleTUICommand.
	DispatchCommand(line string) CommandResult

	// ---- Lifecycle ----
	// Close releases all resources (App, Host, subscription). Called when the
	// TUI exits or rotates to a new facade. After Close, the facade is unusable.
	Close() error
}

// CompletionSource provides per-keystroke completion candidates from the
// snapshot. It mirrors the subset of complete.go's compSources that reads App
// state — model list, backend list, sessions, commands.
type CompletionSource interface {
	Models() []string
	Backends() []Backend
	Sessions() []SessionSummary
}

// RepoStateMutator is a neutral callback for SaveRepoState. It mirrors the
// subset of agent.RepoState fields the TUI mutates through the
// SaveRepoState(func(*RepoState)) callback. Using its own type keeps the TUI
// from importing agent.RepoState.
type RepoStateMutator struct {
	Model            string
	Backend          string
	SubagentEndpoint string
	SubagentModel    string
	RawTools         bool

	MaxParallelSubagents int

	AutoApprove   bool
	InfoPanelOpen bool
}

// ---- ConversationManager (D27) ----

// ConversationManager sits above the facade and handles conversation lifecycle
// operations that create, resume, or fold entire sessions. The TUI calls these
// when a CommandResult carries a RotateRequest (from /new, /resume, /handoff).
//
// The manager owns the facade lifecycle: each method returns a new Facade
// (except Close, which releases the current one). The TUI never constructs
// a facade directly — it goes through the manager.
//
// This interface is agent-free. The implementation lives in internal/wiring
// (7b3 m3) and bridges to *agent.App internally.
//
// Detached-job policy (P0): rotation and close CANCEL any in-flight detached
// async jobs (side questions, /handoff, /remember, /recall). The caller does
// not need to drain them. This is the simplest correct policy: the old host
// is going away, so its detached jobs are cancelled rather than retained.
// A future revision may support job migration to the new host.
type ConversationManager interface {
	// NewConversation creates a fresh session and returns its facade. The
	// caller's current facade (if any) must be closed first via Close.
	NewConversation(ctx context.Context, principal core.Principal) (Facade, error)

	// ResumeConversation loads an existing session by ID or prefix and
	// returns a facade backed by it. Returns an error if the session is
	// not found.
	ResumeConversation(ctx context.Context, principal core.Principal, idOrPrefix string) (Facade, error)

	// HandoffConversation folds the current conversation into a summary and
	// creates a new session that carries the folded context. If proceed is
	// true, the new session auto-starts a continuation turn with the
	// handoff context. Returns the new facade.
	HandoffConversation(ctx context.Context, principal core.Principal, current Facade, proceed bool) (Facade, error)

	// Close releases the facade and all its resources (App, Host,
	// subscription). Detached async jobs are cancelled. After Close, the
	// facade is unusable.
	Close(f Facade) error
}