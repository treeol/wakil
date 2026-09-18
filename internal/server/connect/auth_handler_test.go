package connect

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"database/sql"
	v1alpha1 "github.com/treeol/wakil/api/gen/wakil/v1alpha1"
	"github.com/treeol/wakil/internal/auth"
	"github.com/treeol/wakil/internal/auth/apitoken"
	"github.com/treeol/wakil/internal/auth/jointoken"
	"github.com/treeol/wakil/internal/auth/tokenstore"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/store/migrations"

	_ "modernc.org/sqlite"
)

// authHandlerTestSetup creates an AuthHandler backed by in-memory SQLite.
// The resolver starts as tenantA owner. Returns handler, resolver, store, db.
func authHandlerTestSetup(t *testing.T) (*AuthHandler, *switchableResolver, *tokenstore.Store, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := migrations.Apply(context.Background(), db); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	tStore := tokenstore.New(db)
	jtIssuer := jointoken.New(tStore)
	apiIssuer := apitoken.New(tStore)

	// Seed tenant + owner user + membership.
	seedDB(t, db)

	resolver := &switchableResolver{principal: core.Principal{
		TenantID:   event.TenantID("tnt_a"),
		UserID:     event.UserID("usr_a"),
		Role:       core.RoleOwner,
		AuthMethod: core.AuthEmbedded,
	}}
	h := NewAuthHandler(jtIssuer, apiIssuer, tStore, resolver)
	t.Cleanup(func() { db.Close() })
	return h, resolver, tStore, db
}

// seedDB inserts the base tenant, owner user, and owner membership.
func seedDB(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	_, err := db.ExecContext(ctx,
		`INSERT INTO tenants (id, slug, display_name, status, created_at) VALUES ('tnt_a', 'test', 'Test', 'active', strftime('%s','now'))`)
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	_, err = db.ExecContext(ctx,
		`INSERT INTO users (id, email, display_name, auth_subject, password_hash, status, created_at)
		 VALUES ('usr_a', 'owner@example.com', 'Owner', NULL, NULL, 'active', strftime('%s','now'))`)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	_, err = db.ExecContext(ctx,
		`INSERT INTO memberships (tenant_id, user_id, role, created_at)
		 VALUES ('tnt_a', 'usr_a', 'owner', strftime('%s','now'))`)
	if err != nil {
		t.Fatalf("seed membership: %v", err)
	}
}

// seedMember seeds a member user + membership in tnt_a.
func seedMember(t *testing.T, db *sql.DB, userID, email string) {
	t.Helper()
	ctx := context.Background()
	_, err := db.ExecContext(ctx,
		`INSERT INTO users (id, email, display_name, auth_subject, password_hash, status, created_at)
		 VALUES (?, ?, ?, NULL, NULL, 'active', strftime('%s','now'))`,
		userID, email, email)
	if err != nil {
		t.Fatalf("seed member user %s: %v", userID, err)
	}
	_, err = db.ExecContext(ctx,
		`INSERT INTO memberships (tenant_id, user_id, role, created_at)
		 VALUES ('tnt_a', ?, 'member', strftime('%s','now'))`,
		userID)
	if err != nil {
		t.Fatalf("seed member membership %s: %v", userID, err)
	}
}

// isCode returns true if a Connect error carries the given code.
func isCode(err error, code connect.Code) bool {
	if err == nil {
		return false
	}
	var ce *connect.Error
	if ok := errors.As(err, &ce); ok {
		return ce.Code() == code
	}
	return false
}

// --- ExchangeJoinToken tests ---

func TestExchangeJoinToken_Success(t *testing.T) {
	h, _, _, _ := authHandlerTestSetup(t)
	ctx := context.Background()

	result, err := h.issuer.Create(ctx, jointoken.CreateRequest{
		TenantID:    "tnt_a",
		Role:        "member",
		Email:       "invitee@example.com",
		DisplayName: "Invitee",
		CreatedBy:   "usr_a",
	})
	if err != nil {
		t.Fatalf("create join token: %v", err)
	}

	resp, err := h.ExchangeJoinToken(ctx, connect.NewRequest(&v1alpha1.ExchangeJoinTokenRequest{
		Token:       result.Token,
		Email:       "invitee@example.com",
		DisplayName: "Invitee",
	}))
	if err != nil {
		t.Fatalf("ExchangeJoinToken: %v", err)
	}
	if resp.Msg.Principal == nil {
		t.Fatal("Principal is nil")
	}
	if resp.Msg.Principal.TenantId != "tnt_a" {
		t.Errorf("TenantId = %q, want %q", resp.Msg.Principal.TenantId, "tnt_a")
	}
	if resp.Header().Get("Set-Cookie") == "" {
		t.Error("Set-Cookie header is empty")
	}
}

func TestExchangeJoinToken_InvalidFormat(t *testing.T) {
	h, _, _, _ := authHandlerTestSetup(t)
	_, err := h.ExchangeJoinToken(context.Background(), connect.NewRequest(&v1alpha1.ExchangeJoinTokenRequest{
		Token: "not-a-valid-token",
	}))
	if !isCode(err, connect.CodeInvalidArgument) {
		t.Errorf("expected CodeInvalidArgument, got %v", err)
	}
}

func TestExchangeJoinToken_EmptyToken(t *testing.T) {
	h, _, _, _ := authHandlerTestSetup(t)
	_, err := h.ExchangeJoinToken(context.Background(), connect.NewRequest(&v1alpha1.ExchangeJoinTokenRequest{
		Token: "",
	}))
	if !isCode(err, connect.CodeInvalidArgument) {
		t.Errorf("expected CodeInvalidArgument, got %v", err)
	}
}

func TestExchangeJoinToken_AlreadyUsed(t *testing.T) {
	h, _, _, _ := authHandlerTestSetup(t)
	ctx := context.Background()

	result, err := h.issuer.Create(ctx, jointoken.CreateRequest{
		TenantID:    "tnt_a",
		Role:        "member",
		Email:       "invitee@example.com",
		DisplayName: "Invitee",
		CreatedBy:   "usr_a",
	})
	if err != nil {
		t.Fatalf("create join token: %v", err)
	}

	// First exchange — success.
	_, err = h.ExchangeJoinToken(ctx, connect.NewRequest(&v1alpha1.ExchangeJoinTokenRequest{
		Token:       result.Token,
		Email:       "invitee@example.com",
		DisplayName: "Invitee",
	}))
	if err != nil {
		t.Fatalf("first exchange: %v", err)
	}

	// Second exchange — should fail.
	_, err = h.ExchangeJoinToken(ctx, connect.NewRequest(&v1alpha1.ExchangeJoinTokenRequest{
		Token:       result.Token,
		Email:       "invitee@example.com",
		DisplayName: "Invitee",
	}))
	if err == nil {
		t.Fatal("second exchange should fail")
	}
	if !isCode(err, connect.CodeUnauthenticated) {
		t.Errorf("expected CodeUnauthenticated, got %v", err)
	}
}

func TestExchangeJoinToken_Revoked(t *testing.T) {
	h, _, _, _ := authHandlerTestSetup(t)
	ctx := context.Background()

	result, err := h.issuer.Create(ctx, jointoken.CreateRequest{
		TenantID:    "tnt_a",
		Role:        "member",
		Email:       "invitee@example.com",
		DisplayName: "Invitee",
		CreatedBy:   "usr_a",
	})
	if err != nil {
		t.Fatalf("create join token: %v", err)
	}

	if err := h.issuer.Revoke(ctx, result.ID, "tnt_a"); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	_, err = h.ExchangeJoinToken(ctx, connect.NewRequest(&v1alpha1.ExchangeJoinTokenRequest{
		Token:       result.Token,
		Email:       "invitee@example.com",
		DisplayName: "Invitee",
	}))
	if err == nil {
		t.Fatal("exchange of revoked token should fail")
	}
	if !isCode(err, connect.CodeUnauthenticated) {
		t.Errorf("expected CodeUnauthenticated, got %v", err)
	}
}

// --- WhoAmI ---

func TestWhoAmI(t *testing.T) {
	h, resolver, _, _ := authHandlerTestSetup(t)
	resp, err := h.WhoAmI(context.Background(), connect.NewRequest(&v1alpha1.WhoAmIRequest{}))
	if err != nil {
		t.Fatalf("WhoAmI: %v", err)
	}
	if resp.Msg.Principal == nil {
		t.Fatal("Principal is nil")
	}
	p := resolver.principal
	if resp.Msg.Principal.TenantId != string(p.TenantID) {
		t.Errorf("TenantId = %q, want %q", resp.Msg.Principal.TenantId, p.TenantID)
	}
	if resp.Msg.Principal.UserId != string(p.UserID) {
		t.Errorf("UserId = %q, want %q", resp.Msg.Principal.UserId, p.UserID)
	}
	if resp.Msg.Principal.Role != string(p.Role) {
		t.Errorf("Role = %q, want %q", resp.Msg.Principal.Role, p.Role)
	}
}

// --- Logout ---

func TestLogout_NoCookie(t *testing.T) {
	h, _, _, _ := authHandlerTestSetup(t)
	resp, err := h.Logout(context.Background(), connect.NewRequest(&v1alpha1.LogoutRequest{}))
	if err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if resp.Header().Get("Set-Cookie") == "" {
		t.Error("Set-Cookie should be set to clear cookie")
	}
}

func TestLogout_WithCookie(t *testing.T) {
	h, _, tStore, db := authHandlerTestSetup(t)
	ctx := context.Background()

	sessionSecret := "wst_testsecret123"
	sessionHash := jointoken.HashToken(sessionSecret)

	farFuture := int64(1 << 62)
	_, err := db.ExecContext(ctx,
		`INSERT INTO web_sessions (id, token_hash, tenant_id, user_id, created_at, last_seen_at, idle_expires_at, expires_at, revoked_at)
		 VALUES ('wst_test1', ?, 'tnt_a', 'usr_a', ?, ?, ?, ?, NULL)`,
		sessionHash, 0, 0, farFuture, farFuture)
	if err != nil {
		t.Fatalf("insert web session: %v", err)
	}

	// Verify session is valid BEFORE logout (using a time before expiry).
	validNow := int64(1 << 60)
	row, err := tStore.LookupWebSession(ctx, sessionHash, validNow)
	if err != nil {
		t.Fatalf("session should be valid before logout: %v", err)
	}
	if row.ID != "wst_test1" {
		t.Errorf("session ID = %q, want %q", row.ID, "wst_test1")
	}

	headers := http.Header{}
	headers.Set("Cookie", "wakil_session="+sessionSecret)
	ctxWithHeaders := auth.WithHTTPHeaders(ctx, headers)

	resp, err := h.Logout(ctxWithHeaders, connect.NewRequest(&v1alpha1.LogoutRequest{}))
	if err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if resp.Header().Get("Set-Cookie") == "" {
		t.Error("Set-Cookie should be set to clear cookie")
	}

	// Verify the session is now revoked — LookupWebSession should reject it
	// even at a time before the original expiry.
	_, err = tStore.LookupWebSession(ctx, sessionHash, validNow)
	if err == nil {
		t.Error("session should be revoked after logout — LookupWebSession returned nil error")
	}
}

// --- RevokeAPIToken ---

func TestRevokeAPIToken_OwnerCanRevokeAny(t *testing.T) {
	h, _, _, db := authHandlerTestSetup(t)
	ctx := context.Background()

	seedMember(t, db, "usr_other", "other@example.com")

	result, err := h.apiIssuer.Create(ctx, apitoken.CreateRequest{
		TenantID: "tnt_a",
		UserID:   "usr_other",
		Name:     "Other's token",
		Scopes:   []string{"sessions:read"},
	})
	if err != nil {
		t.Fatalf("create api token: %v", err)
	}

	_, err = h.RevokeAPIToken(ctx, connect.NewRequest(&v1alpha1.RevokeAPITokenRequest{
		Id: result.ID,
	}))
	if err != nil {
		t.Errorf("owner should be able to revoke any token: %v", err)
	}

	// Verify the token was actually revoked in the DB.
	tok, err := h.apiIssuer.GetByID(ctx, result.ID, "tnt_a")
	if err != nil {
		t.Fatalf("GetByID after revocation: %v", err)
	}
	if tok.RevokedAt == 0 {
		t.Error("token RevokedAt should be non-zero after successful revocation")
	}
}

func TestRevokeAPIToken_MemberCanRevokeOwn(t *testing.T) {
	h, resolver, _, db := authHandlerTestSetup(t)
	ctx := context.Background()

	seedMember(t, db, "usr_member", "member@example.com")
	resolver.set(core.Principal{
		TenantID:   event.TenantID("tnt_a"),
		UserID:     event.UserID("usr_member"),
		Role:       core.RoleMember,
		AuthMethod: core.AuthEmbedded,
	})

	result, err := h.apiIssuer.Create(ctx, apitoken.CreateRequest{
		TenantID: "tnt_a",
		UserID:   "usr_member",
		Name:     "My token",
		Scopes:   []string{"sessions:read"},
	})
	if err != nil {
		t.Fatalf("create api token: %v", err)
	}

	_, err = h.RevokeAPIToken(ctx, connect.NewRequest(&v1alpha1.RevokeAPITokenRequest{
		Id: result.ID,
	}))
	if err != nil {
		t.Errorf("member should be able to revoke own token: %v", err)
	}

	// Verify the token was actually revoked.
	tok, err := h.apiIssuer.GetByID(ctx, result.ID, "tnt_a")
	if err != nil {
		t.Fatalf("GetByID after revocation: %v", err)
	}
	if tok.RevokedAt == 0 {
		t.Error("token RevokedAt should be non-zero after successful revocation")
	}
}

func TestRevokeAPIToken_MemberCannotRevokeOther(t *testing.T) {
	h, resolver, _, db := authHandlerTestSetup(t)
	ctx := context.Background()

	// Owner creates a token.
	result, err := h.apiIssuer.Create(ctx, apitoken.CreateRequest{
		TenantID: "tnt_a",
		UserID:   "usr_a",
		Name:     "Owner's token",
		Scopes:   []string{"sessions:read"},
	})
	if err != nil {
		t.Fatalf("create api token: %v", err)
	}

	// Switch to a member.
	seedMember(t, db, "usr_member2", "member2@example.com")
	resolver.set(core.Principal{
		TenantID:   event.TenantID("tnt_a"),
		UserID:     event.UserID("usr_member2"),
		Role:       core.RoleMember,
		AuthMethod: core.AuthEmbedded,
	})

	// Member tries to revoke owner's token — must be denied.
	_, err = h.RevokeAPIToken(ctx, connect.NewRequest(&v1alpha1.RevokeAPITokenRequest{
		Id: result.ID,
	}))
	if err == nil {
		t.Fatal("member should NOT be able to revoke another user's token")
	}
	if !isCode(err, connect.CodePermissionDenied) {
		t.Errorf("expected CodePermissionDenied, got %v", err)
	}

	// Verify the token is still active using GetByID (which includes revoked tokens).
	tok, err := h.apiIssuer.GetByID(ctx, result.ID, "tnt_a")
	if err != nil {
		t.Fatalf("GetByID after failed revocation: %v", err)
	}
	if tok.RevokedAt != 0 {
		t.Error("token was revoked despite permission denial — RevokedAt should be 0")
	}
}

func TestRevokeAPIToken_NotFound(t *testing.T) {
	h, _, _, _ := authHandlerTestSetup(t)
	_, err := h.RevokeAPIToken(context.Background(), connect.NewRequest(&v1alpha1.RevokeAPITokenRequest{
		Id: "tok_nonexistent",
	}))
	if !isCode(err, connect.CodeNotFound) {
		t.Errorf("expected CodeNotFound, got %v", err)
	}
}

func TestRevokeAPIToken_APITokenAuthDenied(t *testing.T) {
	h, resolver, _, _ := authHandlerTestSetup(t)
	ctx := context.Background()

	result, err := h.apiIssuer.Create(ctx, apitoken.CreateRequest{
		TenantID: "tnt_a",
		UserID:   "usr_a",
		Name:     "Test",
		Scopes:   []string{"sessions:read"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	resolver.set(core.Principal{
		TenantID:   event.TenantID("tnt_a"),
		UserID:     event.UserID("usr_a"),
		Role:       core.RoleOwner,
		AuthMethod: core.AuthAPIToken,
	})

	_, err = h.RevokeAPIToken(ctx, connect.NewRequest(&v1alpha1.RevokeAPITokenRequest{
		Id: result.ID,
	}))
	if !isCode(err, connect.CodePermissionDenied) {
		t.Errorf("expected CodePermissionDenied, got %v", err)
	}
}

// --- ListAPITokens ---

func TestListAPITokens_Own(t *testing.T) {
	h, _, _, _ := authHandlerTestSetup(t)
	ctx := context.Background()

	_, err := h.apiIssuer.Create(ctx, apitoken.CreateRequest{
		TenantID: "tnt_a",
		UserID:   "usr_a",
		Name:     "token1",
		Scopes:   []string{"sessions:read"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	resp, err := h.ListAPITokens(ctx, connect.NewRequest(&v1alpha1.ListAPITokensRequest{}))
	if err != nil {
		t.Fatalf("ListAPITokens: %v", err)
	}
	if len(resp.Msg.Tokens) == 0 {
		t.Error("expected at least 1 token")
	}
}

func TestListAPITokens_AdminListsOther(t *testing.T) {
	h, _, _, db := authHandlerTestSetup(t)
	ctx := context.Background()

	seedMember(t, db, "usr_list2", "list2@example.com")
	_, err := h.apiIssuer.Create(ctx, apitoken.CreateRequest{
		TenantID: "tnt_a",
		UserID:   "usr_list2",
		Name:     "other's token",
		Scopes:   []string{"sessions:read"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Owner lists all — with a specific user_id to see another user's tokens.
	resp, err := h.ListAPITokens(ctx, connect.NewRequest(&v1alpha1.ListAPITokensRequest{
		UserId: "usr_list2",
	}))
	if err != nil {
		t.Fatalf("ListAPITokens: %v", err)
	}
	if len(resp.Msg.Tokens) == 0 {
		t.Error("expected tokens for usr_list2")
	}
}

func TestListAPITokens_MemberDeniedOther(t *testing.T) {
	h, resolver, _, db := authHandlerTestSetup(t)
	ctx := context.Background()

	_, err := h.apiIssuer.Create(ctx, apitoken.CreateRequest{
		TenantID: "tnt_a",
		UserID:   "usr_a",
		Name:     "owner token",
		Scopes:   []string{"sessions:read"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	seedMember(t, db, "usr_list3", "list3@example.com")
	resolver.set(core.Principal{
		TenantID:   event.TenantID("tnt_a"),
		UserID:     event.UserID("usr_list3"),
		Role:       core.RoleMember,
		AuthMethod: core.AuthEmbedded,
	})

	_, err = h.ListAPITokens(ctx, connect.NewRequest(&v1alpha1.ListAPITokensRequest{
		UserId: "usr_a",
	}))
	if !isCode(err, connect.CodePermissionDenied) {
		t.Errorf("expected CodePermissionDenied, got %v", err)
	}
}

func TestListAPITokens_APITokenAuthDenied(t *testing.T) {
	h, resolver, _, _ := authHandlerTestSetup(t)
	resolver.set(core.Principal{
		TenantID:   event.TenantID("tnt_a"),
		UserID:     event.UserID("usr_a"),
		Role:       core.RoleOwner,
		AuthMethod: core.AuthAPIToken,
	})
	_, err := h.ListAPITokens(context.Background(), connect.NewRequest(&v1alpha1.ListAPITokensRequest{}))
	if !isCode(err, connect.CodePermissionDenied) {
		t.Errorf("expected CodePermissionDenied, got %v", err)
	}
}

// --- CreateAPIToken edge cases ---

func TestCreateAPIToken_MissingName(t *testing.T) {
	h, _, _, _ := authHandlerTestSetup(t)
	_, err := h.CreateAPIToken(context.Background(), connect.NewRequest(&v1alpha1.CreateAPITokenRequest{
		Name:   "",
		Scopes: []string{"sessions:read"},
	}))
	if !isCode(err, connect.CodeInvalidArgument) {
		t.Errorf("expected CodeInvalidArgument, got %v", err)
	}
}

func TestCreateAPIToken_NonOwnerWildcardDenied(t *testing.T) {
	h, resolver, _, db := authHandlerTestSetup(t)
	ctx := context.Background()

	seedMember(t, db, "usr_wild", "wild@example.com")
	resolver.set(core.Principal{
		TenantID:   event.TenantID("tnt_a"),
		UserID:     event.UserID("usr_wild"),
		Role:       core.RoleMember,
		AuthMethod: core.AuthEmbedded,
	})

	_, err := h.CreateAPIToken(ctx, connect.NewRequest(&v1alpha1.CreateAPITokenRequest{
		Name:   "wildcard",
		Scopes: []string{"*"},
	}))
	if !isCode(err, connect.CodePermissionDenied) {
		t.Errorf("expected CodePermissionDenied, got %v", err)
	}
}

func TestCreateAPIToken_APITokenAuthDenied(t *testing.T) {
	h, resolver, _, _ := authHandlerTestSetup(t)
	resolver.set(core.Principal{
		TenantID:   event.TenantID("tnt_a"),
		UserID:     event.UserID("usr_a"),
		Role:       core.RoleOwner,
		AuthMethod: core.AuthAPIToken,
	})
	_, err := h.CreateAPIToken(context.Background(), connect.NewRequest(&v1alpha1.CreateAPITokenRequest{
		Name:   "test",
		Scopes: []string{"sessions:read"},
	}))
	if !isCode(err, connect.CodePermissionDenied) {
		t.Errorf("expected CodePermissionDenied, got %v", err)
	}
}

// --- Cookie helper tests ---

func TestReadCookieFromHeaders(t *testing.T) {
	tests := []struct {
		name   string
		cookie string
		want   string
	}{
		{"present", "wakil_session=abc123; other=def", "abc123"},
		{"missing", "other=def", ""},
		{"empty", "", ""},
		{"multiple", "a=1; wakil_session=secret; b=2", "secret"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			headers := http.Header{}
			if tc.cookie != "" {
				headers.Set("Cookie", tc.cookie)
			}
			got := readCookieFromHeaders(headers, "wakil_session")
			if got != tc.want {
				t.Errorf("readCookieFromHeaders = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSetSessionCookie(t *testing.T) {
	resp := connect.NewResponse(&v1alpha1.ExchangeJoinTokenResponse{})
	setSessionCookie(resp, "wakil_session", "secret123", false)
	cookie := resp.Header().Get("Set-Cookie")
	if cookie == "" {
		t.Fatal("Set-Cookie header is empty")
	}
	if !strings.Contains(cookie, "wakil_session=secret123") {
		t.Errorf("cookie missing name=value: %q", cookie)
	}
	if !strings.Contains(cookie, "Path=/") {
		t.Errorf("cookie missing Path=/: %q", cookie)
	}
	if !strings.Contains(cookie, "HttpOnly") {
		t.Errorf("cookie missing HttpOnly: %q", cookie)
	}
	if !strings.Contains(cookie, "SameSite=Strict") {
		t.Errorf("cookie missing SameSite=Strict: %q", cookie)
	}
}

func TestSetSessionCookie_Secure(t *testing.T) {
	resp := connect.NewResponse(&v1alpha1.ExchangeJoinTokenResponse{})
	setSessionCookie(resp, "wakil_session", "secret123", true)
	cookie := resp.Header().Get("Set-Cookie")
	if !strings.Contains(cookie, "Secure") {
		t.Errorf("secure cookie missing Secure flag: %q", cookie)
	}
}

func TestClearSessionCookie(t *testing.T) {
	resp := connect.NewResponse(&v1alpha1.LogoutResponse{})
	clearSessionCookie(resp, "wakil_session", false)
	cookie := resp.Header().Get("Set-Cookie")
	if cookie == "" {
		t.Fatal("Set-Cookie header is empty")
	}
	if !strings.Contains(cookie, "Max-Age=0") && !strings.Contains(cookie, "expires=") {
		t.Errorf("cookie missing deletion semantics: %q", cookie)
	}
}
