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
	"github.com/treeol/wakil/internal/store/workspacestore"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// WorkspaceHandler implements WorkspaceServiceHandler. It manages per-tenant
// project workspaces (design §4.3).
//
// Authentication policy:
//   - All RPCs: authenticated, owner/admin role within the caller's tenant.
//   - All queries are tenant-scoped: the caller's tenant_id filters every
//     operation.
type WorkspaceHandler struct {
	store    *workspacestore.Store
	resolver principalResolver
}

// Compile-time assertion.
var _ wakilv1alpha1connect.WorkspaceServiceHandler = (*WorkspaceHandler)(nil)

// NewWorkspaceHandler creates a workspace handler. store may be nil if
// workspace management is not configured (workspace RPCs will return
// Unimplemented).
func NewWorkspaceHandler(store *workspacestore.Store, resolver principalResolver) *WorkspaceHandler {
	return &WorkspaceHandler{
		store:    store,
		resolver: resolver,
	}
}

// CreateWorkspace creates a new workspace.
func (h *WorkspaceHandler) CreateWorkspace(ctx context.Context, req *connect.Request[v1alpha1.CreateWorkspaceRequest]) (*connect.Response[v1alpha1.CreateWorkspaceResponse], error) {
	if h.store == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("workspace management not configured"))
	}

	p, err := resolvePrincipal(ctx, h.resolver)
	if err != nil {
		return nil, mapError(err)
	}
	if p.Role != core.RoleOwner && p.Role != core.RoleAdmin {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("only owners and admins can manage workspaces"))
	}

	name := strings.TrimSpace(req.Msg.Name)
	if name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name is required"))
	}
	hostPath := strings.TrimSpace(req.Msg.HostPath)
	if hostPath == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("host_path is required"))
	}

	workspaceID := "wsp_" + uuid.NewString()
	tenantID := string(p.TenantID)

	row := workspacestore.CreateParams{
		ID:        workspaceID,
		TenantID:  tenantID,
		Name:      name,
		HostPath:  hostPath,
		VCSRemote: req.Msg.VcsRemote,
	}
	if err := h.store.Create(ctx, row); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&v1alpha1.CreateWorkspaceResponse{
		Workspace: &v1alpha1.Workspace{
			Id:        workspaceID,
			TenantId:  tenantID,
			Name:      name,
			HostPath:  hostPath,
			VcsRemote: req.Msg.VcsRemote,
			CreatedAt: timestamppb.Now(),
		},
	}), nil
}

// ListWorkspaces returns all workspaces for the caller's tenant.
func (h *WorkspaceHandler) ListWorkspaces(ctx context.Context, req *connect.Request[v1alpha1.ListWorkspacesRequest]) (*connect.Response[v1alpha1.ListWorkspacesResponse], error) {
	if h.store == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("workspace management not configured"))
	}

	p, err := resolvePrincipal(ctx, h.resolver)
	if err != nil {
		return nil, mapError(err)
	}
	if p.Role != core.RoleOwner && p.Role != core.RoleAdmin {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("only owners and admins can list workspaces"))
	}

	rows, err := h.store.List(ctx, string(p.TenantID))
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	protos := make([]*v1alpha1.Workspace, 0, len(rows))
	for _, row := range rows {
		protos = append(protos, workspaceRowToProto(row))
	}
	return connect.NewResponse(&v1alpha1.ListWorkspacesResponse{Workspaces: protos}), nil
}

// GetWorkspace returns a single workspace by ID.
func (h *WorkspaceHandler) GetWorkspace(ctx context.Context, req *connect.Request[v1alpha1.GetWorkspaceRequest]) (*connect.Response[v1alpha1.GetWorkspaceResponse], error) {
	if h.store == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("workspace management not configured"))
	}

	p, err := resolvePrincipal(ctx, h.resolver)
	if err != nil {
		return nil, mapError(err)
	}
	if p.Role != core.RoleOwner && p.Role != core.RoleAdmin {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("only owners and admins can view workspaces"))
	}

	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id is required"))
	}

	row, err := h.store.Get(ctx, req.Msg.Id, string(p.TenantID))
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("workspace not found"))
	}

	return connect.NewResponse(&v1alpha1.GetWorkspaceResponse{
		Workspace: workspaceRowToProto(row),
	}), nil
}

// DeleteWorkspace removes a workspace.
func (h *WorkspaceHandler) DeleteWorkspace(ctx context.Context, req *connect.Request[v1alpha1.DeleteWorkspaceRequest]) (*connect.Response[v1alpha1.DeleteWorkspaceResponse], error) {
	if h.store == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("workspace management not configured"))
	}

	p, err := resolvePrincipal(ctx, h.resolver)
	if err != nil {
		return nil, mapError(err)
	}
	if p.Role != core.RoleOwner && p.Role != core.RoleAdmin {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("only owners and admins can delete workspaces"))
	}

	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id is required"))
	}

	if err := h.store.Delete(ctx, req.Msg.Id, string(p.TenantID)); err != nil {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("workspace not found"))
	}

	return connect.NewResponse(&v1alpha1.DeleteWorkspaceResponse{}), nil
}

// workspaceRowToProto converts a WorkspaceRow to a proto Workspace message.
func workspaceRowToProto(row workspacestore.WorkspaceRow) *v1alpha1.Workspace {
	w := &v1alpha1.Workspace{
		Id:        row.ID,
		TenantId:  row.TenantID,
		Name:      row.Name,
		HostPath:  row.HostPath,
		VcsRemote: row.VCSRemote,
	}
	if row.CreatedAt != "" {
		if t, err := time.Parse(time.RFC3339, row.CreatedAt); err == nil {
			w.CreatedAt = timestamppb.New(t)
		}
	}
	return w
}
