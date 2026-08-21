package connect

import (
	"context"
	"errors"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	v1alpha1 "github.com/treeol/wakil/api/gen/wakil/v1alpha1"
	wakilv1alpha1connect "github.com/treeol/wakil/api/gen/wakil/v1alpha1/wakilv1alpha1connect"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/store/agentstore"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// AgentHandler implements AgentServiceHandler. It manages per-tenant agent
// configurations and their immutable revisions (design §7).
//
// Authentication policy:
//   - All RPCs: authenticated, owner/admin role within the caller's tenant.
//   - All queries are tenant-scoped: the caller's tenant_id filters every
//     operation.
type AgentHandler struct {
	store    *agentstore.Store
	resolver principalResolver
}

// Compile-time assertion.
var _ wakilv1alpha1connect.AgentServiceHandler = (*AgentHandler)(nil)

// NewAgentHandler creates an agent handler. store may be nil if agent
// management is not configured (agent RPCs will return Unimplemented).
func NewAgentHandler(store *agentstore.Store, resolver principalResolver) *AgentHandler {
	return &AgentHandler{
		store:    store,
		resolver: resolver,
	}
}

// CreateAgent creates a new agent with no revisions.
func (h *AgentHandler) CreateAgent(ctx context.Context, req *connect.Request[v1alpha1.CreateAgentRequest]) (*connect.Response[v1alpha1.CreateAgentResponse], error) {
	if h.store == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("agent management not configured"))
	}

	p, err := resolvePrincipal(ctx, h.resolver)
	if err != nil {
		return nil, mapError(err)
	}
	if p.Role != core.RoleOwner && p.Role != core.RoleAdmin {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("only owners and admins can manage agents"))
	}

	name := strings.TrimSpace(req.Msg.Name)
	if name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name is required"))
	}

	agentID := "agt_" + uuid.NewString()
	tenantID := string(p.TenantID)

	if err := h.store.Create(ctx, agentstore.CreateParams{
		ID:       agentID,
		TenantID: tenantID,
		Name:     name,
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&v1alpha1.CreateAgentResponse{
		Agent: &v1alpha1.Agent{
			Id:        agentID,
			TenantId:  tenantID,
			Name:      name,
			CreatedAt: timestamppb.Now(),
		},
	}), nil
}

// GetAgent returns a single agent by ID.
func (h *AgentHandler) GetAgent(ctx context.Context, req *connect.Request[v1alpha1.GetAgentRequest]) (*connect.Response[v1alpha1.GetAgentResponse], error) {
	if h.store == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("agent management not configured"))
	}

	p, err := resolvePrincipal(ctx, h.resolver)
	if err != nil {
		return nil, mapError(err)
	}
	if p.Role != core.RoleOwner && p.Role != core.RoleAdmin {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("only owners and admins can view agents"))
	}

	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id is required"))
	}

	row, err := h.store.Get(ctx, req.Msg.Id, string(p.TenantID))
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("agent not found"))
	}

	return connect.NewResponse(&v1alpha1.GetAgentResponse{
		Agent: agentRowToProto(row),
	}), nil
}

// ListAgents returns all agents for the caller's tenant.
func (h *AgentHandler) ListAgents(ctx context.Context, req *connect.Request[v1alpha1.ListAgentsRequest]) (*connect.Response[v1alpha1.ListAgentsResponse], error) {
	if h.store == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("agent management not configured"))
	}

	p, err := resolvePrincipal(ctx, h.resolver)
	if err != nil {
		return nil, mapError(err)
	}
	if p.Role != core.RoleOwner && p.Role != core.RoleAdmin {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("only owners and admins can list agents"))
	}

	rows, err := h.store.List(ctx, string(p.TenantID))
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	protos := make([]*v1alpha1.Agent, 0, len(rows))
	for _, row := range rows {
		protos = append(protos, agentRowToProto(row))
	}
	return connect.NewResponse(&v1alpha1.ListAgentsResponse{Agents: protos}), nil
}

// CreateAgentRevision creates a new immutable agent revision.
func (h *AgentHandler) CreateAgentRevision(ctx context.Context, req *connect.Request[v1alpha1.CreateAgentRevisionRequest]) (*connect.Response[v1alpha1.CreateAgentRevisionResponse], error) {
	if h.store == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("agent management not configured"))
	}

	p, err := resolvePrincipal(ctx, h.resolver)
	if err != nil {
		return nil, mapError(err)
	}
	if p.Role != core.RoleOwner && p.Role != core.RoleAdmin {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("only owners and admins can manage agents"))
	}

	if req.Msg.AgentId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("agent_id is required"))
	}
	spec := strings.TrimSpace(req.Msg.Spec)
	if spec == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("spec is required"))
	}

	revID := "rev_" + uuid.NewString()
	tenantID := string(p.TenantID)

	row, err := h.store.CreateRevision(ctx, agentstore.CreateRevisionParams{
		ID:        revID,
		TenantID:  tenantID,
		AgentID:   req.Msg.AgentId,
		Spec:      spec,
		CreatedBy: string(p.UserID),
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("agent not found"))
	}

	return connect.NewResponse(&v1alpha1.CreateAgentRevisionResponse{
		Revision: agentRevisionRowToProto(row),
	}), nil
}

// ListAgentRevisions returns all revisions for an agent.
func (h *AgentHandler) ListAgentRevisions(ctx context.Context, req *connect.Request[v1alpha1.ListAgentRevisionsRequest]) (*connect.Response[v1alpha1.ListAgentRevisionsResponse], error) {
	if h.store == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("agent management not configured"))
	}

	p, err := resolvePrincipal(ctx, h.resolver)
	if err != nil {
		return nil, mapError(err)
	}
	if p.Role != core.RoleOwner && p.Role != core.RoleAdmin {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("only owners and admins can list agent revisions"))
	}

	if req.Msg.AgentId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("agent_id is required"))
	}

	rows, err := h.store.ListRevisions(ctx, req.Msg.AgentId, string(p.TenantID))
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	protos := make([]*v1alpha1.AgentRevision, 0, len(rows))
	for _, row := range rows {
		protos = append(protos, agentRevisionRowToProto(row))
	}
	return connect.NewResponse(&v1alpha1.ListAgentRevisionsResponse{Revisions: protos}), nil
}

// DeleteAgent removes an agent and its revisions.
func (h *AgentHandler) DeleteAgent(ctx context.Context, req *connect.Request[v1alpha1.DeleteAgentRequest]) (*connect.Response[v1alpha1.DeleteAgentResponse], error) {
	if h.store == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("agent management not configured"))
	}

	p, err := resolvePrincipal(ctx, h.resolver)
	if err != nil {
		return nil, mapError(err)
	}
	if p.Role != core.RoleOwner && p.Role != core.RoleAdmin {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("only owners and admins can delete agents"))
	}

	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id is required"))
	}

	if err := h.store.Delete(ctx, req.Msg.Id, string(p.TenantID)); err != nil {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("agent not found"))
	}

	return connect.NewResponse(&v1alpha1.DeleteAgentResponse{}), nil
}

// agentRowToProto converts an AgentRow to a proto Agent message.
func agentRowToProto(row agentstore.AgentRow) *v1alpha1.Agent {
	a := &v1alpha1.Agent{
		Id:        row.ID,
		TenantId:  row.TenantID,
		Name:      row.Name,
		HeadRevId: row.HeadRevID,
	}
	if row.CreatedAt != "" {
		if t, err := time.Parse(time.RFC3339, row.CreatedAt); err == nil {
			a.CreatedAt = timestamppb.New(t)
		}
	}
	return a
}

// agentRevisionRowToProto converts an AgentRevisionRow to a proto AgentRevision message.
func agentRevisionRowToProto(row agentstore.AgentRevisionRow) *v1alpha1.AgentRevision {
	r := &v1alpha1.AgentRevision{
		Id:        row.ID,
		TenantId:  row.TenantID,
		AgentId:   row.AgentID,
		RevNumber: row.RevNumber,
		Spec:      row.Spec,
		SpecHash:  row.SpecHash,
		CreatedBy: row.CreatedBy,
	}
	if row.CreatedAt != "" {
		if t, err := time.Parse(time.RFC3339, row.CreatedAt); err == nil {
			r.CreatedAt = timestamppb.New(t)
		}
	}
	return r
}
