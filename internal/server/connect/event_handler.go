package connect

import (
	"context"
	"io"

	"connectrpc.com/connect"
	v1alpha1 "github.com/treeol/wakil/api/gen/wakil/v1alpha1"
	wakilv1alpha1connect "github.com/treeol/wakil/api/gen/wakil/v1alpha1/wakilv1alpha1connect"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
)

// EventHandler implements EventServiceHandler by delegating to the core
// EventReader + SessionReader interfaces.
type EventHandler struct {
	reader core.EventReader
	snap   core.SessionReader
}

var _ wakilv1alpha1connect.EventServiceHandler = (*EventHandler)(nil)

func NewEventHandler(reader core.EventReader, snap core.SessionReader) *EventHandler {
	return &EventHandler{reader: reader, snap: snap}
}

func (h *EventHandler) StreamEvents(ctx context.Context, req *connect.Request[v1alpha1.StreamEventsRequest], stream *connect.ServerStream[v1alpha1.Event]) error {
	p := localPrincipal()
	sub, err := h.reader.Subscribe(ctx, p, event.SessionID(req.Msg.SessionId), event.Seq(req.Msg.AfterSeq))
	if err != nil {
		return mapError(err)
	}
	defer sub.Close()

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		ev, err := sub.Next(ctx)
		if err != nil {
			if err == io.EOF || err == core.ErrSubscriptionClosed {
				return nil
			}
			// Context cancellation is expected on client disconnect.
			if isContextCanceled(err) {
				return nil
			}
			return mapError(err)
		}
		pb, err := eventToProto(ev)
		if err != nil {
			return connect.NewError(connect.CodeInternal, err)
		}
		if err := stream.Send(pb); err != nil {
			return err
		}
	}
}

func (h *EventHandler) ListEvents(ctx context.Context, req *connect.Request[v1alpha1.ListEventsRequest]) (*connect.Response[v1alpha1.ListEventsResponse], error) {
	p := localPrincipal()
	events, err := h.reader.ListEvents(ctx, p, event.SessionID(req.Msg.SessionId), event.Seq(req.Msg.AfterSeq), int(req.Msg.Limit))
	if err != nil {
		return nil, mapError(err)
	}
	pbEvents := make([]*v1alpha1.Event, 0, len(events))
	for _, ev := range events {
		pb, err := eventToProto(ev)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		pbEvents = append(pbEvents, pb)
	}
	return connect.NewResponse(&v1alpha1.ListEventsResponse{Events: pbEvents}), nil
}

func (h *EventHandler) GetSessionSnapshot(ctx context.Context, req *connect.Request[v1alpha1.GetSessionSnapshotRequest]) (*connect.Response[v1alpha1.SessionSnapshot], error) {
	p := localPrincipal()
	snap, err := h.snap.SessionSnapshot(ctx, p, event.SessionID(req.Msg.SessionId))
	if err != nil {
		return nil, mapError(err)
	}
	pbEvents := make([]*v1alpha1.Event, 0, len(snap.Events))
	for _, ev := range snap.Events {
		pb, err := eventToProto(ev)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		pbEvents = append(pbEvents, pb)
	}
	return connect.NewResponse(&v1alpha1.SessionSnapshot{
		Session: sessionToProto(snap.Session),
		Events:  pbEvents,
		LastSeq: uint64(snap.LastSeq),
	}), nil
}

// isContextCanceled reports whether err is a context cancellation.
func isContextCanceled(err error) bool {
	return err == context.Canceled || err == context.DeadlineExceeded
}
