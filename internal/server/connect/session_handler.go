package connect

import (
	"context"

	"connectrpc.com/connect"
	v1alpha1 "github.com/treeol/wakil/api/gen/wakil/v1alpha1"
	wakilv1alpha1connect "github.com/treeol/wakil/api/gen/wakil/v1alpha1/wakilv1alpha1connect"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
	"google.golang.org/protobuf/types/known/timestamppb"
	"time"
)

// SessionHandler implements SessionServiceHandler by delegating to the core
// SessionService + SessionReader interfaces (both implemented by *sessionhost.Host).
type SessionHandler struct {
	svc    core.SessionService
	reader core.SessionReader
}

// Compile-time assertion.
var _ wakilv1alpha1connect.SessionServiceHandler = (*SessionHandler)(nil)

func NewSessionHandler(svc core.SessionService, reader core.SessionReader) *SessionHandler {
	return &SessionHandler{svc: svc, reader: reader}
}

func (h *SessionHandler) CreateSession(ctx context.Context, req *connect.Request[v1alpha1.CreateSessionRequest]) (*connect.Response[v1alpha1.Session], error) {
	p := localPrincipal()
	s, err := h.svc.CreateSession(ctx, p, core.CreateSessionRequest{
		Workspace: event.WorkspaceID(req.Msg.Workspace),
		Title:    req.Msg.Title,
	})
	if err != nil {
		return nil, mapError(err)
	}
	return connect.NewResponse(sessionToProto(s)), nil
}

func (h *SessionHandler) GetSession(ctx context.Context, req *connect.Request[v1alpha1.GetSessionRequest]) (*connect.Response[v1alpha1.Session], error) {
	p := localPrincipal()
	s, err := h.reader.GetSession(ctx, p, event.SessionID(req.Msg.SessionId))
	if err != nil {
		return nil, mapError(err)
	}
	return connect.NewResponse(sessionToProto(s)), nil
}

func (h *SessionHandler) ListSessions(ctx context.Context, req *connect.Request[v1alpha1.ListSessionsRequest]) (*connect.Response[v1alpha1.ListSessionsResponse], error) {
	p := localPrincipal()
	sessions, err := h.reader.ListSessions(ctx, p)
	if err != nil {
		return nil, mapError(err)
	}
	pbSessions := make([]*v1alpha1.Session, 0, len(sessions))
	for _, s := range sessions {
		pbSessions = append(pbSessions, sessionToProto(s))
	}
	return connect.NewResponse(&v1alpha1.ListSessionsResponse{Sessions: pbSessions}), nil
}

func (h *SessionHandler) DeleteSession(ctx context.Context, req *connect.Request[v1alpha1.DeleteSessionRequest]) (*connect.Response[v1alpha1.DeleteSessionResponse], error) {
	p := localPrincipal()
	if err := h.svc.DeleteSession(ctx, p, event.SessionID(req.Msg.SessionId)); err != nil {
		return nil, mapError(err)
	}
	return connect.NewResponse(&v1alpha1.DeleteSessionResponse{}), nil
}

func (h *SessionHandler) SubmitInput(ctx context.Context, req *connect.Request[v1alpha1.SubmitInputRequest]) (*connect.Response[v1alpha1.SubmitAck], error) {
	p := localPrincipal()
	ack, err := h.svc.SubmitInput(ctx, p, core.SubmitInputRequest{
		SessionID:  event.SessionID(req.Msg.SessionId),
		Text:       req.Msg.Text,
		ReadAction: req.Msg.ReadAction,
		RequestID:  req.Msg.RequestId,
	})
	if err != nil {
		return nil, mapError(err)
	}
	return connect.NewResponse(&v1alpha1.SubmitAck{
		SessionId: string(ack.SessionID),
		TurnId:    string(ack.TurnID),
	}), nil
}

func (h *SessionHandler) RespondToApproval(ctx context.Context, req *connect.Request[v1alpha1.RespondToApprovalRequest]) (*connect.Response[v1alpha1.RespondToApprovalResponse], error) {
	p := localPrincipal()
	var outcome core.ApprovalOutcome
	switch req.Msg.Outcome {
	case v1alpha1.ApprovalOutcome_APPROVAL_OUTCOME_DENY:
		outcome = core.ApprovalDeny
	case v1alpha1.ApprovalOutcome_APPROVAL_OUTCOME_ALLOW_ONCE:
		outcome = core.ApprovalAllowOnce
	case v1alpha1.ApprovalOutcome_APPROVAL_OUTCOME_ALLOW_READS_ONCE:
		outcome = core.ApprovalAllowReadsOnce
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument, nil)
	}
	err := h.svc.RespondToApproval(ctx, p, core.ApprovalDecision{
		SessionID:  event.SessionID(req.Msg.SessionId),
		ApprovalID: event.ApprovalID(req.Msg.ApprovalId),
		Outcome:    outcome,
		Reason:     req.Msg.Reason,
	})
	if err != nil {
		return nil, mapError(err)
	}
	return connect.NewResponse(&v1alpha1.RespondToApprovalResponse{}), nil
}

func (h *SessionHandler) Interrupt(ctx context.Context, req *connect.Request[v1alpha1.InterruptRequest]) (*connect.Response[v1alpha1.InterruptResponse], error) {
	p := localPrincipal()
	if err := h.svc.Interrupt(ctx, p, event.SessionID(req.Msg.SessionId)); err != nil {
		return nil, mapError(err)
	}
	return connect.NewResponse(&v1alpha1.InterruptResponse{}), nil
}

func (h *SessionHandler) CloseSession(ctx context.Context, req *connect.Request[v1alpha1.CloseSessionRequest]) (*connect.Response[v1alpha1.CloseSessionResponse], error) {
	p := localPrincipal()
	if err := h.svc.CloseSession(ctx, p, event.SessionID(req.Msg.SessionId)); err != nil {
		return nil, mapError(err)
	}
	return connect.NewResponse(&v1alpha1.CloseSessionResponse{}), nil
}

// tsPtr converts a time.Time to a *timestamppb.Timestamp.
func tsPtr(t time.Time) *timestamppb.Timestamp {
	return timestamppb.New(t)
}

// sessionToProto converts a core.Session to a proto Session.
func sessionToProto(s core.Session) *v1alpha1.Session {
	pb := &v1alpha1.Session{
		Id:        string(s.ID),
		TenantId:  string(s.TenantID),
		Workspace: string(s.Workspace),
		State:     string(s.State),
		LastSeq:   uint64(s.LastSeq),
		CreatedBy: string(s.CreatedBy),
		Title:     s.Title,
	}
	if !s.CreatedAt.IsZero() {
		pb.CreatedAt = tsPtr(s.CreatedAt)
	}
	if !s.ClosedAt.IsZero() {
		pb.ClosedAt = tsPtr(s.ClosedAt)
	}
	return pb
}
