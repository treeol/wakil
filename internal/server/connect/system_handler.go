package connect

import (
	"context"
	"time"

	"connectrpc.com/connect"
	v1alpha1 "github.com/treeol/wakil/api/gen/wakil/v1alpha1"
	wakilv1alpha1connect "github.com/treeol/wakil/api/gen/wakil/v1alpha1/wakilv1alpha1connect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// SystemHandler implements SystemServiceHandler.
type SystemHandler struct {
	ephemeral bool
	startedAt time.Time
}

var _ wakilv1alpha1connect.SystemServiceHandler = (*SystemHandler)(nil)

func NewSystemHandler(ephemeral bool) *SystemHandler {
	return &SystemHandler{ephemeral: ephemeral, startedAt: time.Now()}
}

func (h *SystemHandler) GetServerInfo(ctx context.Context, req *connect.Request[v1alpha1.GetServerInfoRequest]) (*connect.Response[v1alpha1.ServerInfo], error) {
	return connect.NewResponse(&v1alpha1.ServerInfo{
		ApiVersion:   "v1alpha1",
		Capabilities: []string{"session_service", "event_service", "delete_session"},
		Ephemeral:    h.ephemeral,
		AuthMethod:   "embedded",
	}), nil
}

func (h *SystemHandler) Health(ctx context.Context, req *connect.Request[v1alpha1.HealthRequest]) (*connect.Response[v1alpha1.HealthStatus], error) {
	return connect.NewResponse(&v1alpha1.HealthStatus{
		Status:    "ready",
		StartedAt: timestamppb.New(h.startedAt),
	}), nil
}
