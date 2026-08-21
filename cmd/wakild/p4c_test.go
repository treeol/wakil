package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	v1alpha1 "github.com/treeol/wakil/api/gen/wakil/v1alpha1"
	wakilv1alpha1connect "github.com/treeol/wakil/api/gen/wakil/v1alpha1/wakilv1alpha1connect"
	"github.com/treeol/wakil/internal/auth/jointoken"
	"github.com/treeol/wakil/internal/auth/tokenstore"
	connsvc "github.com/treeol/wakil/internal/server/connect"
)

// TestP4cJoinTokenExchangeFlow tests the full P4c flow through Connect RPC:
// 1. Admin creates a join token
// 2. Client exchanges the token for a session cookie
// 3. Double exchange fails (one-time-use)
func TestP4cJoinTokenExchangeFlow(t *testing.T) {
	store := newTestTokenStore(t)
	issuer := jointoken.New(store)

	// Use the exported NewServerWithAuth with the embedded resolver (test-only).
	srv := connsvc.NewServerWithAuth(
		nil,  // no session host needed for auth-only tests
		true, // ephemeral
		connsvc.NewEmbeddedResolver(),
		issuer,
		store,
	)

	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	client := wakilv1alpha1connect.NewAuthServiceClient(httpSrv.Client(), httpSrv.URL)
	ctx := context.Background()

	// 1. Create a join token as the local owner (embedded resolver → owner).
	createResp, err := client.CreateJoinToken(ctx, connect.NewRequest(&v1alpha1.CreateJoinTokenRequest{
		TenantId:    "tnt_local",
		Role:        "member",
		Email:       "p4c@example.com",
		DisplayName: "P4c User",
	}))
	if err != nil {
		t.Fatalf("create join token: %v", err)
	}
	token := createResp.Msg.Token
	if !strings.HasPrefix(token, "jnt_") {
		t.Errorf("token prefix = %q, want jnt_", token[:4])
	}

	// 2. Exchange the token.
	exchangeResp, err := client.ExchangeJoinToken(ctx, connect.NewRequest(&v1alpha1.ExchangeJoinTokenRequest{
		Token:       token,
		Email:       "p4c@example.com",
		DisplayName: "P4c User",
	}))
	if err != nil {
		t.Fatalf("exchange join token: %v", err)
	}
	if exchangeResp.Msg.Principal == nil {
		t.Fatal("principal is nil")
	}
	if exchangeResp.Msg.Principal.TenantId != "tnt_local" {
		t.Errorf("principal tenant = %q, want tnt_local", exchangeResp.Msg.Principal.TenantId)
	}
	if exchangeResp.Msg.Principal.Role != "member" {
		t.Errorf("principal role = %q, want member", exchangeResp.Msg.Principal.Role)
	}

	// The Set-Cookie header should be set on the response.
	setCookie := exchangeResp.Header().Get("Set-Cookie")
	if setCookie == "" {
		t.Fatal("Set-Cookie header is empty")
	}
	if !strings.Contains(setCookie, "wakild_session=") {
		t.Errorf("Set-Cookie doesn't contain wakild_session: %q", setCookie)
	}
	if !strings.Contains(setCookie, "HttpOnly") {
		t.Error("Set-Cookie is not HttpOnly")
	}
	if !strings.Contains(setCookie, "SameSite=Strict") {
		t.Error("Set-Cookie is not SameSite=Strict")
	}

	// 3. Double exchange must fail (one-time-use).
	_, err = client.ExchangeJoinToken(ctx, connect.NewRequest(&v1alpha1.ExchangeJoinTokenRequest{
		Token:       token,
		Email:       "p4c@example.com",
		DisplayName: "P4c User",
	}))
	if err == nil {
		t.Fatal("second exchange should fail")
	}
}

// TestP4cListJoinTokens tests listing tokens as an admin.
func TestP4cListJoinTokens(t *testing.T) {
	store := newTestTokenStore(t)
	issuer := jointoken.New(store)

	srv := connsvc.NewServerWithAuth(
		nil,
		true,
		connsvc.NewEmbeddedResolver(),
		issuer,
		store,
	)

	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	client := wakilv1alpha1connect.NewAuthServiceClient(httpSrv.Client(), httpSrv.URL)
	ctx := context.Background()

	// Create a token.
	_, err := client.CreateJoinToken(ctx, connect.NewRequest(&v1alpha1.CreateJoinTokenRequest{
		TenantId:    "tnt_local",
		Role:        "member",
		Email:       "list@example.com",
		DisplayName: "List User",
	}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// List tokens.
	listResp, err := client.ListJoinTokens(ctx, connect.NewRequest(&v1alpha1.ListJoinTokensRequest{
		TenantId: "tnt_local",
	}))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listResp.Msg.Tokens) != 1 {
		t.Errorf("len(tokens) = %d, want 1", len(listResp.Msg.Tokens))
	}
}

// newTestTokenStore creates a file-based SQLite DB with migrations for P4c tests.
func newTestTokenStore(t *testing.T) *tokenstore.Store {
	t.Helper()
	dir := t.TempDir()
	dbPath := dir + "/p4c_test.db"
	ts, err := tokenstore.OpenTestDB(dbPath)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { ts.Close() })
	return ts
}
