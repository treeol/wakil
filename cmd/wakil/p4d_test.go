package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	v1alpha1 "github.com/treeol/wakil/api/gen/wakil/v1alpha1"
	wakilv1alpha1connect "github.com/treeol/wakil/api/gen/wakil/v1alpha1/wakilv1alpha1connect"
	"github.com/treeol/wakil/internal/auth/apitoken"
	"github.com/treeol/wakil/internal/auth/jointoken"
	connsvc "github.com/treeol/wakil/internal/server/connect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestP4dCreateAndListAPIToken tests the full P4d flow through Connect RPC:
// 1. Create an API token as the local owner
// 2. List tokens and verify it appears
// 3. Revoke the token
// 4. List again and verify it's gone
func TestP4dCreateAndListAPIToken(t *testing.T) {
	store := newTestTokenStore(t)
	issuer := jointoken.New(store)
	apiIssuer := apitoken.New(store)

	srv := connsvc.NewServerWithAuth(
		nil,
		true,
		connsvc.NewEmbeddedResolver(),
		issuer,
		apiIssuer,
		store,
	)

	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	client := wakilv1alpha1connect.NewAuthServiceClient(httpSrv.Client(), httpSrv.URL)
	ctx := context.Background()

	// 1. Create an API token.
	createResp, err := client.CreateAPIToken(ctx, connect.NewRequest(&v1alpha1.CreateAPITokenRequest{
		Name:   "CI pipeline",
		Scopes: []string{"sessions:read", "sessions:write"},
	}))
	if err != nil {
		t.Fatalf("create api token: %v", err)
	}
	token := createResp.Msg.Token
	if !strings.HasPrefix(token, "tok_") {
		t.Errorf("token prefix = %q, want tok_", token[:4])
	}
	if createResp.Msg.Id == "" {
		t.Error("token ID is empty")
	}

	// 2. List tokens.
	listResp, err := client.ListAPITokens(ctx, connect.NewRequest(&v1alpha1.ListAPITokensRequest{}))
	if err != nil {
		t.Fatalf("list api tokens: %v", err)
	}
	if len(listResp.Msg.Tokens) != 1 {
		t.Fatalf("len(tokens) = %d, want 1", len(listResp.Msg.Tokens))
	}
	tk := listResp.Msg.Tokens[0]
	if tk.Name != "CI pipeline" {
		t.Errorf("token name = %q, want 'CI pipeline'", tk.Name)
	}
	if tk.Id != createResp.Msg.Id {
		t.Errorf("token id = %q, want %q", tk.Id, createResp.Msg.Id)
	}
	if len(tk.Scopes) != 2 {
		t.Fatalf("len(scopes) = %d, want 2", len(tk.Scopes))
	}
	if tk.Scopes[0] != "sessions:read" || tk.Scopes[1] != "sessions:write" {
		t.Errorf("scopes = %v, want [sessions:read, sessions:write]", tk.Scopes)
	}

	// 3. Revoke the token.
	_, err = client.RevokeAPIToken(ctx, connect.NewRequest(&v1alpha1.RevokeAPITokenRequest{
		Id: createResp.Msg.Id,
	}))
	if err != nil {
		t.Fatalf("revoke api token: %v", err)
	}

	// 4. List again — revoked tokens are excluded by default.
	listResp2, err := client.ListAPITokens(ctx, connect.NewRequest(&v1alpha1.ListAPITokensRequest{}))
	if err != nil {
		t.Fatalf("list after revoke: %v", err)
	}
	if len(listResp2.Msg.Tokens) != 0 {
		t.Errorf("len(tokens) = %d, want 0 (revoked excluded)", len(listResp2.Msg.Tokens))
	}
}

// TestP4dCreateAPITokenWithExpiry verifies that expiry is handled correctly.
func TestP4dCreateAPITokenWithExpiry(t *testing.T) {
	store := newTestTokenStore(t)
	issuer := jointoken.New(store)
	apiIssuer := apitoken.New(store)

	srv := connsvc.NewServerWithAuth(
		nil,
		true,
		connsvc.NewEmbeddedResolver(),
		issuer,
		apiIssuer,
		store,
	)

	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	client := wakilv1alpha1connect.NewAuthServiceClient(httpSrv.Client(), httpSrv.URL)
	ctx := context.Background()

	expiresAt := time.Now().Add(30 * 24 * time.Hour) // 30 days

	createResp, err := client.CreateAPIToken(ctx, connect.NewRequest(&v1alpha1.CreateAPITokenRequest{
		Name:      "expiring token",
		ExpiresAt: timestamppbPtr(expiresAt),
	}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if createResp.Msg.ExpiresAt == nil {
		t.Fatal("expires_at is nil")
	}
	// Verify the expiry is approximately 30 days from now.
	diff := createResp.Msg.ExpiresAt.AsTime().Sub(expiresAt)
	if diff > time.Second || diff < -time.Second {
		t.Errorf("expires_at diff = %v, want < 1s", diff)
	}
}

// TestP4dRevokeNonexistentAPIToken verifies that revoking a non-existent
// token returns NotFound.
func TestP4dRevokeNonexistentAPIToken(t *testing.T) {
	store := newTestTokenStore(t)
	issuer := jointoken.New(store)
	apiIssuer := apitoken.New(store)

	srv := connsvc.NewServerWithAuth(
		nil,
		true,
		connsvc.NewEmbeddedResolver(),
		issuer,
		apiIssuer,
		store,
	)

	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	client := wakilv1alpha1connect.NewAuthServiceClient(httpSrv.Client(), httpSrv.URL)
	ctx := context.Background()

	_, err := client.RevokeAPIToken(ctx, connect.NewRequest(&v1alpha1.RevokeAPITokenRequest{
		Id: "tok_nonexistent",
	}))
	if err == nil {
		t.Fatal("revoke non-existent token should fail")
	}
	if !strings.Contains(err.Error(), "NotFound") && !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected NotFound error, got %v", err)
	}
}

// TestP4dCreateAPITokenMissingName verifies that creating a token without
// a name returns InvalidArgument.
func TestP4dCreateAPITokenMissingName(t *testing.T) {
	store := newTestTokenStore(t)
	issuer := jointoken.New(store)
	apiIssuer := apitoken.New(store)

	srv := connsvc.NewServerWithAuth(
		nil,
		true,
		connsvc.NewEmbeddedResolver(),
		issuer,
		apiIssuer,
		store,
	)

	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	client := wakilv1alpha1connect.NewAuthServiceClient(httpSrv.Client(), httpSrv.URL)
	ctx := context.Background()

	_, err := client.CreateAPIToken(ctx, connect.NewRequest(&v1alpha1.CreateAPITokenRequest{
		Name: "",
	}))
	if err == nil {
		t.Fatal("create with empty name should fail")
	}
}

// timestamppbPtr returns a pointer to a timestamppb.Timestamp for the given time.
func timestamppbPtr(t time.Time) *timestamppb.Timestamp {
	return timestamppb.New(t)
}
