// Package protoconv provides shared proto↔domain event conversion (card #148 P2e).
//
// The conversion logic is shared between the Connect server adapter
// (internal/server/connect, which converts domain→proto for responses) and
// the remote client (internal/remote, which converts proto→domain for
// inbound events). Extracting it here avoids duplicating the 32-kind switch
// and prevents them from drifting apart.
//
// The functions are pure value conversions — no state, no I/O. They are safe
// for concurrent use.
package protoconv

import (
	"fmt"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1alpha1 "github.com/treeol/wakil/api/gen/wakil/v1alpha1"
	"github.com/treeol/wakil/internal/core/event"
)

// EventToProto converts a core domain event to a proto Event. The payload
// oneof wrapper is set via reflection because the isEvent_Payload interface
// is unexported in the generated code.
func EventToProto(e event.Event) (*v1alpha1.Event, error) {
	payload, err := PayloadToProto(e.Kind, e.Payload)
	if err != nil {
		return nil, fmt.Errorf("EventToProto %s: %w", e.Kind, err)
	}
	pb := &v1alpha1.Event{
		TenantId:  string(e.TenantID),
		SessionId: string(e.SessionID),
		Seq:       uint64(e.Seq),
		Kind:      string(e.Kind),
		Ts:        timestamppb.New(e.Ts),
	}
	if payload != nil {
		setPayload(pb, payload)
	}
	return pb, nil
}

// EventFromProto converts a proto Event to a core domain event.
func EventFromProto(pb *v1alpha1.Event) (event.Event, error) {
	k := event.Kind(pb.Kind)
	payload, err := PayloadFromProto(k, pb.Payload)
	if err != nil {
		return event.Event{}, fmt.Errorf("EventFromProto %s: %w", k, err)
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

// SessionToProto converts a core.Session-shaped value to a proto Session.
// Accepts the fields directly to avoid importing internal/core.
type SessionFields struct {
	ID        string
	TenantID  string
	Workspace string
	State     string
	LastSeq   uint64
	CreatedBy string
	Title     string
	CreatedAt time.Time
	ClosedAt  time.Time
}

func SessionToProto(s SessionFields) *v1alpha1.Session {
	pb := &v1alpha1.Session{
		Id:        s.ID,
		TenantId:  s.TenantID,
		Workspace: s.Workspace,
		State:     s.State,
		LastSeq:   s.LastSeq,
		CreatedBy: s.CreatedBy,
		Title:     s.Title,
	}
	if !s.CreatedAt.IsZero() {
		pb.CreatedAt = timestamppb.New(s.CreatedAt)
	}
	if !s.ClosedAt.IsZero() {
		pb.ClosedAt = timestamppb.New(s.ClosedAt)
	}
	return pb
}

// SessionFromProto converts a proto Session to SessionFields.
func SessionFromProto(pb *v1alpha1.Session) SessionFields {
	s := SessionFields{
		ID:        pb.Id,
		TenantID:  pb.TenantId,
		Workspace: pb.Workspace,
		State:     pb.State,
		LastSeq:   pb.LastSeq,
		CreatedBy: pb.CreatedBy,
		Title:     pb.Title,
	}
	if pb.CreatedAt != nil {
		s.CreatedAt = pb.CreatedAt.AsTime()
	}
	if pb.ClosedAt != nil {
		s.ClosedAt = pb.ClosedAt.AsTime()
	}
	return s
}
