package connect

import (
	"fmt"
	"reflect"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1alpha1 "github.com/treeol/wakil/api/gen/wakil/v1alpha1"
	"github.com/treeol/wakil/internal/core/event"
)

// eventToProto converts a core.Event to a proto Event.
func eventToProto(e event.Event) (*v1alpha1.Event, error) {
	pb := &v1alpha1.Event{
		TenantId:  string(e.TenantID),
		SessionId: string(e.SessionID),
		Seq:       uint64(e.Seq),
		Kind:      string(e.Kind),
		Ts:        timestamppb.New(e.Ts),
	}
	payload, err := payloadToProto(e.Kind, e.Payload)
	if err != nil {
		return nil, fmt.Errorf("eventToProto %s: %w", e.Kind, err)
	}
	// Set the oneof via reflection since isEvent_Payload is unexported.
	if payload != nil {
		// The generated wrapper types implement isEvent_Payload but the interface
		// is unexported. Use ProtoReflect to set the oneof field.
		setOneofPayload(pb, payload)
	}
	return pb, nil
}

// setOneofPayload sets the oneof payload on a proto Event.
// The Payload field is exported but its interface type is unexported
// (isEvent_Payload). We use reflection to set it since the wrapper types
// implement that interface.
func setOneofPayload(pb *v1alpha1.Event, payload any) {
	rv := reflect.ValueOf(pb).Elem()
	payloadField := rv.FieldByName("Payload")
	payloadField.Set(reflect.ValueOf(payload))
}

// payloadToProto converts a core payload to a proto oneof wrapper.
func payloadToProto(k event.Kind, p any) (any, error) {
	switch k {
	case event.KindSessionCreated:
		v := p.(event.SessionCreated)
		return &v1alpha1.Event_SessionCreated{SessionCreated: &v1alpha1.SessionCreatedPayload{
			WorkspaceId: string(v.WorkspaceID),
			AgentName:   v.AgentName,
			CreatedBy:   string(v.CreatedBy),
		}}, nil
	case event.KindTurnStarted:
		v := p.(event.TurnStarted)
		return &v1alpha1.Event_TurnStarted{TurnStarted: &v1alpha1.TurnStartedPayload{
			TurnId:    string(v.TurnID),
			TurnIndex: v.TurnIndex,
		}}, nil
	case event.KindMessageDelta:
		v := p.(event.MessageDelta)
		return &v1alpha1.Event_MessageDelta{MessageDelta: &v1alpha1.MessageDeltaPayload{Text: v.Text}}, nil
	case event.KindMessageCommitted:
		v := p.(event.MessageCommitted)
		return &v1alpha1.Event_MessageCommitted{MessageCommitted: &v1alpha1.MessageCommittedPayload{
			TurnId: string(v.TurnID),
			Text:   v.Text,
		}}, nil
	case event.KindReasoningDelta:
		v := p.(event.ReasoningDelta)
		return &v1alpha1.Event_ReasoningDelta{ReasoningDelta: &v1alpha1.ReasoningDeltaPayload{Text: v.Text}}, nil
	case event.KindToolCallStarted:
		v := p.(event.ToolCallStarted)
		return &v1alpha1.Event_ToolCallStarted{ToolCallStarted: &v1alpha1.ToolCallStartedPayload{
			TurnId:     string(v.TurnID),
			ToolCallId: string(v.ToolCallID),
			Name:       v.Name,
			ArgDigest:  v.ArgDigest,
		}}, nil
	case event.KindToolCallCompleted:
		v := p.(event.ToolCallCompleted)
		return &v1alpha1.Event_ToolCallCompleted{ToolCallCompleted: &v1alpha1.ToolCallCompletedPayload{
			ToolCallId:   string(v.ToolCallID),
			Name:         v.Name,
			Status:       v.Status,
			ResultPreview: v.ResultPreview,
			DurationMs:   v.DurationMs,
		}}, nil
	case event.KindApprovalRequested:
		v := p.(event.ApprovalRequested)
		return &v1alpha1.Event_ApprovalRequested{ApprovalRequested: &v1alpha1.ApprovalRequestedPayload{
			ApprovalId: string(v.ApprovalID),
			ToolName:   v.ToolName,
			Headline:    v.Headline,
			Detail:      v.Detail,
			ReadAction:  v.ReadAction,
		}}, nil
	case event.KindApprovalResolved:
		v := p.(event.ApprovalResolved)
		return &v1alpha1.Event_ApprovalResolved{ApprovalResolved: &v1alpha1.ApprovalResolvedPayload{
			ApprovalId: string(v.ApprovalID),
			Outcome:    v.Outcome,
			Reason:     v.Reason,
			Resolver:   string(v.Resolver),
		}}, nil
	case event.KindSubagentSpawned:
		v := p.(event.SubagentSpawned)
		return &v1alpha1.Event_SubagentSpawned{SubagentSpawned: &v1alpha1.SubagentSpawnedPayload{
			SubagentId: string(v.SubagentID),
			Task:       v.Task,
			Capability: v.Capability,
			Backend:    v.Backend,
			Model:      v.Model,
			ToolNames:  v.ToolNames,
		}}, nil
	case event.KindSubagentProgress:
		v := p.(event.SubagentProgress)
		return &v1alpha1.Event_SubagentProgress{SubagentProgress: &v1alpha1.SubagentProgressPayload{
			SubagentId:      string(v.SubagentID),
			Text:            v.Text,
			Finished:        v.Finished,
			FinishedStatus:  v.FinishedStatus,
			FinishedCostUsd: v.FinishedCostUSD,
			FinishedFilesN:  int32(v.FinishedFilesN),
		}}, nil
	case event.KindSubagentCompleted:
		v := p.(event.SubagentCompleted)
		return &v1alpha1.Event_SubagentCompleted{SubagentCompleted: &v1alpha1.SubagentCompletedPayload{
			SubagentId:    string(v.SubagentID),
			Status:        v.Status,
			SummaryPreview: v.SummaryPreview,
			Err:           v.Err,
			CostUsd:       v.CostUSD,
			FilesChanged:  v.FilesChanged,
			Grounding:     v.Grounding,
			CtxSize:       int32(v.CtxSize),
			HardMaxBytes:  int32(v.HardMaxBytes),
			UsedBackend:   v.UsedBackend,
		}}, nil
	case event.KindMemoryProposed:
		v := p.(event.MemoryProposed)
		return &v1alpha1.Event_MemoryProposed{MemoryProposed: &v1alpha1.MemoryProposedPayload{
			Key:    v.Key,
			Kind:   v.Kind,
			Writer: v.Writer,
		}}, nil
	case event.KindGuardTriggered:
		v := p.(event.GuardTriggered)
		return &v1alpha1.Event_GuardTriggered{GuardTriggered: &v1alpha1.GuardTriggeredPayload{
			Guard:   v.Guard,
			Message: v.Message,
		}}, nil
	case event.KindContextWarning:
		v := p.(event.ContextWarning)
		return &v1alpha1.Event_ContextWarning{ContextWarning: &v1alpha1.ContextWarningPayload{Message: v.Message}}, nil
	case event.KindTurnCompleted:
		v := p.(event.TurnCompleted)
		return &v1alpha1.Event_TurnCompleted{TurnCompleted: &v1alpha1.TurnCompletedPayload{
			TurnId:               string(v.TurnID),
			Outcome:              v.Outcome,
			Warn:                 v.Warn,
			WorkflowWillContinue: v.WorkflowWillContinue,
		}}, nil
	case event.KindSessionError:
		v := p.(event.SessionError)
		return &v1alpha1.Event_SessionError{SessionError: &v1alpha1.SessionErrorPayload{
			Reason: v.Reason,
			Err:    v.Err,
		}}, nil
	case event.KindSessionClosed:
		v := p.(event.SessionClosed)
		return &v1alpha1.Event_SessionClosed{SessionClosed: &v1alpha1.SessionClosedPayload{Reason: v.Reason}}, nil
	case event.KindUserMessageCommitted:
		v := p.(event.UserMessageCommitted)
		return &v1alpha1.Event_UserMessageCommitted{UserMessageCommitted: &v1alpha1.UserMessageCommittedPayload{
			TurnId: string(v.TurnID),
			Text:   v.Text,
		}}, nil
	case event.KindConversationCompacted:
		v := p.(event.ConversationCompacted)
		return &v1alpha1.Event_ConversationCompacted{ConversationCompacted: &v1alpha1.ConversationCompactedPayload{
			TurnId: string(v.TurnID),
		}}, nil
	case event.KindWorkflowTurnStarted:
		v := p.(event.WorkflowTurnStarted)
		return &v1alpha1.Event_WorkflowTurnStarted{WorkflowTurnStarted: &v1alpha1.WorkflowTurnStartedPayload{
			TurnId:   string(v.TurnID),
			UserText: v.UserText,
		}}, nil
	case event.KindWorkflowFinalReview:
		v := p.(event.WorkflowFinalReview)
		return &v1alpha1.Event_WorkflowFinalReview{WorkflowFinalReview: &v1alpha1.WorkflowFinalReviewPayload{
			TurnId: string(v.TurnID),
		}}, nil
	case event.KindAsyncJobStarted:
		v := p.(event.AsyncJobStarted)
		return &v1alpha1.Event_AsyncJobStarted{AsyncJobStarted: &v1alpha1.AsyncJobStartedPayload{
			OpId:  string(v.OpID),
			Label: v.Label,
		}}, nil
	case event.KindAsyncJobCompleted:
		v := p.(event.AsyncJobCompleted)
		return &v1alpha1.Event_AsyncJobCompleted{AsyncJobCompleted: &v1alpha1.AsyncJobCompletedPayload{
			OpId:           string(v.OpID),
			Status:         v.Status,
			SummaryPreview: v.SummaryPreview,
			Err:            v.Err,
		}}, nil
	case event.KindSideQuestionCompleted:
		v := p.(event.SideQuestionCompleted)
		return &v1alpha1.Event_SideQuestionCompleted{SideQuestionCompleted: &v1alpha1.SideQuestionCompletedPayload{
			OpId:          string(v.OpID),
			Status:        v.Status,
			AnswerPreview: v.AnswerPreview,
		}}, nil
	case event.KindTokRate:
		v := p.(event.TokRate)
		return &v1alpha1.Event_TokRate{TokRate: &v1alpha1.TokRatePayload{Rate: v.Rate}}, nil
	case event.KindAsyncJobProgress:
		v := p.(event.AsyncJobProgress)
		return &v1alpha1.Event_AsyncJobProgress{AsyncJobProgress: &v1alpha1.AsyncJobProgressPayload{
			OpId: string(v.OpID),
			Text: v.Text,
		}}, nil
	case event.KindSideQuestionProgress:
		v := p.(event.SideQuestionProgress)
		return &v1alpha1.Event_SideQuestionProgress{SideQuestionProgress: &v1alpha1.SideQuestionProgressPayload{
			OpId: string(v.OpID),
			Text: v.Text,
		}}, nil
	case event.KindLearnNudge:
		v := p.(event.LearnNudge)
		return &v1alpha1.Event_LearnNudge{LearnNudge: &v1alpha1.LearnNudgePayload{Text: v.Text}}, nil
	case event.KindSessionNote:
		v := p.(event.SessionNote)
		return &v1alpha1.Event_SessionNote{SessionNote: &v1alpha1.SessionNotePayload{Text: v.Text}}, nil
	case event.KindWorkflowOutcome:
		v := p.(event.WorkflowOutcome)
		return &v1alpha1.Event_WorkflowOutcome{WorkflowOutcome: &v1alpha1.WorkflowOutcomePayload{
			TurnId: string(v.TurnID),
			Outcome: v.Outcome,
			Reason:  v.Reason,
		}}, nil
	case event.KindWorkflowWarning:
		v := p.(event.WorkflowWarning)
		return &v1alpha1.Event_WorkflowWarning{WorkflowWarning: &v1alpha1.WorkflowWarningPayload{Message: v.Message}}, nil
	default:
		return nil, fmt.Errorf("payloadToProto: unknown kind %q", k)
	}
}

// eventFromProto converts a proto Event to a core.Event.
func eventFromProto(pb *v1alpha1.Event) (event.Event, error) {
	k := event.Kind(pb.Kind)
	payload, err := payloadFromProto(k, pb.Payload)
	if err != nil {
		return event.Event{}, fmt.Errorf("eventFromProto %s: %w", k, err)
	}
	var ts time.Time
	if pb.Ts != nil {
		ts = pb.Ts.AsTime()
	}
	return event.Event{
		TenantID:  event.TenantID(pb.TenantId),
		SessionID: event.SessionID(pb.SessionId),
		Seq:       event.Seq(pb.Seq),
		Ts:        ts,
		Kind:      k,
		Payload:   payload,
	}, nil
}

// payloadFromProto converts a proto oneof payload to a core payload.
func payloadFromProto(k event.Kind, p any) (any, error) {
	if p == nil {
		return nil, fmt.Errorf("payloadFromProto: nil payload for kind %q", k)
	}
	switch v := p.(type) {
	case *v1alpha1.Event_SessionCreated:
		return event.SessionCreated{
			WorkspaceID: event.WorkspaceID(v.SessionCreated.WorkspaceId),
			AgentName:   v.SessionCreated.AgentName,
			CreatedBy:   event.UserID(v.SessionCreated.CreatedBy),
		}, nil
	case *v1alpha1.Event_TurnStarted:
		return event.TurnStarted{
			TurnID:    event.TurnID(v.TurnStarted.TurnId),
			TurnIndex: v.TurnStarted.TurnIndex,
		}, nil
	case *v1alpha1.Event_MessageDelta:
		return event.MessageDelta{Text: v.MessageDelta.Text}, nil
	case *v1alpha1.Event_MessageCommitted:
		return event.MessageCommitted{
			TurnID: event.TurnID(v.MessageCommitted.TurnId),
			Text:   v.MessageCommitted.Text,
		}, nil
	case *v1alpha1.Event_ReasoningDelta:
		return event.ReasoningDelta{Text: v.ReasoningDelta.Text}, nil
	case *v1alpha1.Event_ToolCallStarted:
		return event.ToolCallStarted{
			TurnID:     event.TurnID(v.ToolCallStarted.TurnId),
			ToolCallID: event.ToolCallID(v.ToolCallStarted.ToolCallId),
			Name:       v.ToolCallStarted.Name,
			ArgDigest:  v.ToolCallStarted.ArgDigest,
		}, nil
	case *v1alpha1.Event_ToolCallCompleted:
		return event.ToolCallCompleted{
			ToolCallID:    event.ToolCallID(v.ToolCallCompleted.ToolCallId),
			Name:          v.ToolCallCompleted.Name,
			Status:        v.ToolCallCompleted.Status,
			ResultPreview: v.ToolCallCompleted.ResultPreview,
			DurationMs:    v.ToolCallCompleted.DurationMs,
		}, nil
	case *v1alpha1.Event_ApprovalRequested:
		return event.ApprovalRequested{
			ApprovalID: event.ApprovalID(v.ApprovalRequested.ApprovalId),
			ToolName:   v.ApprovalRequested.ToolName,
			Headline:   v.ApprovalRequested.Headline,
			Detail:     v.ApprovalRequested.Detail,
			ReadAction:  v.ApprovalRequested.ReadAction,
		}, nil
	case *v1alpha1.Event_ApprovalResolved:
		return event.ApprovalResolved{
			ApprovalID: event.ApprovalID(v.ApprovalResolved.ApprovalId),
			Outcome:    v.ApprovalResolved.Outcome,
			Reason:     v.ApprovalResolved.Reason,
			Resolver:   event.UserID(v.ApprovalResolved.Resolver),
		}, nil
	case *v1alpha1.Event_SubagentSpawned:
		return event.SubagentSpawned{
			SubagentID: event.SubagentID(v.SubagentSpawned.SubagentId),
			Task:       v.SubagentSpawned.Task,
			Capability: v.SubagentSpawned.Capability,
			Backend:    v.SubagentSpawned.Backend,
			Model:      v.SubagentSpawned.Model,
			ToolNames:  v.SubagentSpawned.ToolNames,
		}, nil
	case *v1alpha1.Event_SubagentProgress:
		return event.SubagentProgress{
			SubagentID:        event.SubagentID(v.SubagentProgress.SubagentId),
			Text:              v.SubagentProgress.Text,
			Finished:          v.SubagentProgress.Finished,
			FinishedStatus:    v.SubagentProgress.FinishedStatus,
			FinishedCostUSD:   v.SubagentProgress.FinishedCostUsd,
			FinishedFilesN:    int(v.SubagentProgress.FinishedFilesN),
		}, nil
	case *v1alpha1.Event_SubagentCompleted:
		return event.SubagentCompleted{
			SubagentID:    event.SubagentID(v.SubagentCompleted.SubagentId),
			Status:        v.SubagentCompleted.Status,
			SummaryPreview: v.SubagentCompleted.SummaryPreview,
			Err:           v.SubagentCompleted.Err,
			CostUSD:       v.SubagentCompleted.CostUsd,
			FilesChanged:  v.SubagentCompleted.FilesChanged,
			Grounding:     v.SubagentCompleted.Grounding,
			CtxSize:       int(v.SubagentCompleted.CtxSize),
			HardMaxBytes:   int(v.SubagentCompleted.HardMaxBytes),
			UsedBackend:   v.SubagentCompleted.UsedBackend,
		}, nil
	case *v1alpha1.Event_MemoryProposed:
		return event.MemoryProposed{
			Key:    v.MemoryProposed.Key,
			Kind:   v.MemoryProposed.Kind,
			Writer: v.MemoryProposed.Writer,
		}, nil
	case *v1alpha1.Event_GuardTriggered:
		return event.GuardTriggered{Guard: v.GuardTriggered.Guard, Message: v.GuardTriggered.Message}, nil
	case *v1alpha1.Event_ContextWarning:
		return event.ContextWarning{Message: v.ContextWarning.Message}, nil
	case *v1alpha1.Event_TurnCompleted:
		return event.TurnCompleted{
			TurnID:               event.TurnID(v.TurnCompleted.TurnId),
			Outcome:              v.TurnCompleted.Outcome,
			Warn:                 v.TurnCompleted.Warn,
			WorkflowWillContinue: v.TurnCompleted.WorkflowWillContinue,
		}, nil
	case *v1alpha1.Event_SessionError:
		return event.SessionError{Reason: v.SessionError.Reason, Err: v.SessionError.Err}, nil
	case *v1alpha1.Event_SessionClosed:
		return event.SessionClosed{Reason: v.SessionClosed.Reason}, nil
	case *v1alpha1.Event_UserMessageCommitted:
		return event.UserMessageCommitted{
			TurnID: event.TurnID(v.UserMessageCommitted.TurnId),
			Text:   v.UserMessageCommitted.Text,
		}, nil
	case *v1alpha1.Event_ConversationCompacted:
		return event.ConversationCompacted{TurnID: event.TurnID(v.ConversationCompacted.TurnId)}, nil
	case *v1alpha1.Event_WorkflowTurnStarted:
		return event.WorkflowTurnStarted{
			TurnID:   event.TurnID(v.WorkflowTurnStarted.TurnId),
			UserText: v.WorkflowTurnStarted.UserText,
		}, nil
	case *v1alpha1.Event_WorkflowFinalReview:
		return event.WorkflowFinalReview{TurnID: event.TurnID(v.WorkflowFinalReview.TurnId)}, nil
	case *v1alpha1.Event_AsyncJobStarted:
		return event.AsyncJobStarted{
			OpID:  event.OpID(v.AsyncJobStarted.OpId),
			Label: v.AsyncJobStarted.Label,
		}, nil
	case *v1alpha1.Event_AsyncJobCompleted:
		return event.AsyncJobCompleted{
			OpID:           event.OpID(v.AsyncJobCompleted.OpId),
			Status:         v.AsyncJobCompleted.Status,
			SummaryPreview: v.AsyncJobCompleted.SummaryPreview,
			Err:            v.AsyncJobCompleted.Err,
		}, nil
	case *v1alpha1.Event_SideQuestionCompleted:
		return event.SideQuestionCompleted{
			OpID:          event.OpID(v.SideQuestionCompleted.OpId),
			Status:        v.SideQuestionCompleted.Status,
			AnswerPreview: v.SideQuestionCompleted.AnswerPreview,
		}, nil
	case *v1alpha1.Event_TokRate:
		return event.TokRate{Rate: v.TokRate.Rate}, nil
	case *v1alpha1.Event_AsyncJobProgress:
		return event.AsyncJobProgress{
			OpID: event.OpID(v.AsyncJobProgress.OpId),
			Text: v.AsyncJobProgress.Text,
		}, nil
	case *v1alpha1.Event_SideQuestionProgress:
		return event.SideQuestionProgress{
			OpID: event.OpID(v.SideQuestionProgress.OpId),
			Text: v.SideQuestionProgress.Text,
		}, nil
	case *v1alpha1.Event_LearnNudge:
		return event.LearnNudge{Text: v.LearnNudge.Text}, nil
	case *v1alpha1.Event_SessionNote:
		return event.SessionNote{Text: v.SessionNote.Text}, nil
	case *v1alpha1.Event_WorkflowOutcome:
		return event.WorkflowOutcome{
			TurnID: event.TurnID(v.WorkflowOutcome.TurnId),
			Outcome: v.WorkflowOutcome.Outcome,
			Reason:  v.WorkflowOutcome.Reason,
		}, nil
	case *v1alpha1.Event_WorkflowWarning:
		return event.WorkflowWarning{Message: v.WorkflowWarning.Message}, nil
	default:
		return nil, fmt.Errorf("payloadFromProto: unknown payload type %T for kind %q", p, k)
	}
}

