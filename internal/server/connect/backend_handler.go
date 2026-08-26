package connect

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	v1alpha1 "github.com/treeol/wakil/api/gen/wakil/v1alpha1"
	wakilv1alpha1connect "github.com/treeol/wakil/api/gen/wakil/v1alpha1/wakilv1alpha1connect"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/crypto"
	"github.com/treeol/wakil/internal/store/backendstore"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// BackendHandler implements BackendServiceHandler. It manages per-tenant
// LLM backend configurations with envelope-encrypted API keys (design §6.4).
//
// Authentication policy:
//   - All RPCs: authenticated, owner/admin role within the caller's tenant.
//   - All queries are tenant-scoped: the caller's tenant_id filters every
//     operation.
//
// The API key is never returned by any RPC; only last_four is exposed for
// display.
type BackendHandler struct {
	store    *backendstore.Store
	mk       *crypto.MasterKey // nil = encryption disabled (backends not available)
	resolver principalResolver
}

// Compile-time assertion.
var _ wakilv1alpha1connect.BackendServiceHandler = (*BackendHandler)(nil)

// NewBackendHandler creates a backend handler. mk may be nil if no master
// key is configured (backend RPCs will return Unimplemented).
func NewBackendHandler(store *backendstore.Store, mk *crypto.MasterKey, resolver principalResolver) *BackendHandler {
	return &BackendHandler{
		store:    store,
		mk:       mk,
		resolver: resolver,
	}
}

// CreateBackend creates a new LLM backend with an encrypted API key.
func (h *BackendHandler) CreateBackend(ctx context.Context, req *connect.Request[v1alpha1.CreateBackendRequest]) (*connect.Response[v1alpha1.CreateBackendResponse], error) {
	if h.mk == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("backend management requires a master key (configure --master-key-file)"))
	}
	if h.store == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("backend management not configured"))
	}

	p, err := resolvePrincipal(ctx, h.resolver)
	if err != nil {
		return nil, mapError(err)
	}
	if p.Role != core.RoleOwner && p.Role != core.RoleAdmin {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("only owners and admins can manage backends"))
	}

	// Validate input.
	label := strings.TrimSpace(req.Msg.Label)
	if label == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("label is required"))
	}
	if req.Msg.BackendType == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("backend_type is required"))
	}
	if req.Msg.ApiKey == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("api_key is required"))
	}

	backendID := "be_" + uuid.NewString()
	tenantID := string(p.TenantID)

	// Encrypt the API key with AAD bound to tenant + backend ID.
	ek, err := backendstore.EncryptAPIKey(h.mk, []byte(req.Msg.ApiKey), tenantID, backendID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("encrypt API key: %w", err))
	}

	row := backendstore.CreateParams{
		ID:           backendID,
		TenantID:     tenantID,
		Label:        label,
		BackendType:  req.Msg.BackendType,
		BaseURL:      req.Msg.BaseUrl,
		EncryptedKey: ek,
		LastFour:     backendstore.LastFour(req.Msg.ApiKey),
	}
	if err := h.store.Create(ctx, row); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&v1alpha1.CreateBackendResponse{
		Backend: backendRowToProto(backendstore.BackendRow{
			ID:             backendID,
			TenantID:       tenantID,
			Label:          label,
			BackendType:    req.Msg.BackendType,
			BaseURL:        req.Msg.BaseUrl,
			APIKeyLastFour: backendstore.LastFour(req.Msg.ApiKey),
			CreatedAt:      time.Now().UTC().Format(time.RFC3339),
			UpdatedAt:      time.Now().UTC().Format(time.RFC3339),
		}),
	}), nil
}

// ListBackends returns all backends for the caller's tenant. No API keys.
func (h *BackendHandler) ListBackends(ctx context.Context, req *connect.Request[v1alpha1.ListBackendsRequest]) (*connect.Response[v1alpha1.ListBackendsResponse], error) {
	if h.store == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("backend management not configured"))
	}

	p, err := resolvePrincipal(ctx, h.resolver)
	if err != nil {
		return nil, mapError(err)
	}
	if p.Role != core.RoleOwner && p.Role != core.RoleAdmin {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("only owners and admins can list backends"))
	}

	rows, err := h.store.List(ctx, string(p.TenantID))
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	protos := make([]*v1alpha1.Backend, 0, len(rows))
	for _, row := range rows {
		protos = append(protos, backendRowToProto(row))
	}
	return connect.NewResponse(&v1alpha1.ListBackendsResponse{Backends: protos}), nil
}

// UpdateBackend updates a backend's label, base_url, or API key.
func (h *BackendHandler) UpdateBackend(ctx context.Context, req *connect.Request[v1alpha1.UpdateBackendRequest]) (*connect.Response[v1alpha1.UpdateBackendResponse], error) {
	if h.mk == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("backend management requires a master key"))
	}
	if h.store == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("backend management not configured"))
	}

	p, err := resolvePrincipal(ctx, h.resolver)
	if err != nil {
		return nil, mapError(err)
	}
	if p.Role != core.RoleOwner && p.Role != core.RoleAdmin {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("only owners and admins can manage backends"))
	}

	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id is required"))
	}

	tenantID := string(p.TenantID)

	// Verify the backend exists and belongs to this tenant.
	existing, _, err := h.store.Get(ctx, req.Msg.Id, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("backend not found"))
	}

	params := backendstore.UpdateParams{
		ID:       req.Msg.Id,
		TenantID: tenantID,
		Label:    req.Msg.Label,
		BaseURL:  req.Msg.BaseUrl,
	}

	// If a new API key is provided, re-encrypt.
	if req.Msg.ApiKey != "" {
		ek, err := backendstore.EncryptAPIKey(h.mk, []byte(req.Msg.ApiKey), tenantID, req.Msg.Id)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("encrypt API key: %w", err))
		}
		params.EncryptedKey = &ek
		params.LastFour = backendstore.LastFour(req.Msg.ApiKey)
	}

	if err := h.store.Update(ctx, params); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	// Return updated view.
	if req.Msg.Label != "" {
		existing.Label = req.Msg.Label
	}
	if req.Msg.BaseUrl != "" {
		existing.BaseURL = req.Msg.BaseUrl
	}
	if req.Msg.ApiKey != "" {
		existing.APIKeyLastFour = backendstore.LastFour(req.Msg.ApiKey)
	}
	existing.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

	return connect.NewResponse(&v1alpha1.UpdateBackendResponse{
		Backend: backendRowToProto(existing),
	}), nil
}

// DeleteBackend removes a backend.
func (h *BackendHandler) DeleteBackend(ctx context.Context, req *connect.Request[v1alpha1.DeleteBackendRequest]) (*connect.Response[v1alpha1.DeleteBackendResponse], error) {
	if h.store == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("backend management not configured"))
	}

	p, err := resolvePrincipal(ctx, h.resolver)
	if err != nil {
		return nil, mapError(err)
	}
	if p.Role != core.RoleOwner && p.Role != core.RoleAdmin {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("only owners and admins can delete backends"))
	}

	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id is required"))
	}

	if err := h.store.Delete(ctx, req.Msg.Id, string(p.TenantID)); err != nil {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("backend not found"))
	}

	return connect.NewResponse(&v1alpha1.DeleteBackendResponse{}), nil
}

// backendRowToProto converts a BackendRow to a proto Backend message.
func backendRowToProto(row backendstore.BackendRow) *v1alpha1.Backend {
	b := &v1alpha1.Backend{
		Id:             row.ID,
		TenantId:       row.TenantID,
		Label:          row.Label,
		BackendType:    row.BackendType,
		BaseUrl:        row.BaseURL,
		ApiKeyLastFour: row.APIKeyLastFour,
	}
	if row.CreatedAt != "" {
		if t, err := time.Parse(time.RFC3339, row.CreatedAt); err == nil {
			b.CreatedAt = timestamppb.New(t)
		}
	}
	if row.UpdatedAt != "" {
		if t, err := time.Parse(time.RFC3339, row.UpdatedAt); err == nil {
			b.UpdatedAt = timestamppb.New(t)
		}
	}
	return b
}
