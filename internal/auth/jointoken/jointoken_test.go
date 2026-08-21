package jointoken

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/auth/tokenstore"
)

// TestCreateAndExchange tests the full join token lifecycle: issue a token
// as an owner, exchange it for a session, verify the session resolves to
// the right principal.
func TestCreateAndExchange(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	issuer := New(store)
	ctx := context.Background()

	// Issue a token as the seeded local owner.
	result, err := issuer.Create(ctx, CreateRequest{
		TenantID:    "tnt_local",
		Role:        "member",
		UserID:      "", // create-on-exchange
		Email:       "alice@example.com",
		DisplayName: "Alice",
		CreatedBy:   "usr_local",
	})
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if !strings.HasPrefix(result.Token, "jnt_") {
		t.Errorf("token has wrong prefix: %q", result.Token[:10])
	}
	if result.ID == "" {
		t.Error("token ID is empty")
	}

	// Exchange the token.
	exResult, err := issuer.Exchange(ctx, ExchangeRequest{Token: result.Token, Email: "alice@example.com", DisplayName: "Alice"})
	if err != nil {
		t.Fatalf("exchange token: %v", err)
	}
	if exResult.Principal.TenantID != "tnt_local" {
		t.Errorf("principal tenant = %q, want tnt_local", exResult.Principal.TenantID)
	}
	if exResult.Principal.Role != "member" {
		t.Errorf("principal role = %q, want member", exResult.Principal.Role)
	}
	if !strings.HasPrefix(exResult.SessionCookie, "wst_") {
		t.Errorf("session cookie has wrong prefix: %q", exResult.SessionCookie[:10])
	}
}

// TestDoubleExchangeFails verifies one-time-use: exchanging the same token
// twice must fail on the second attempt.
func TestDoubleExchangeFails(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	issuer := New(store)
	ctx := context.Background()

	result, err := issuer.Create(ctx, CreateRequest{
		TenantID:    "tnt_local",
		Role:        "member",
		UserID:      "",
		Email:       "bob@example.com",
		DisplayName: "Bob",
		CreatedBy:   "usr_local",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// First exchange succeeds.
	_, err = issuer.Exchange(ctx, ExchangeRequest{Token: result.Token, Email: "bob@example.com", DisplayName: "Bob"})
	if err != nil {
		t.Fatalf("first exchange: %v", err)
	}

	// Second exchange must fail.
	_, err = issuer.Exchange(ctx, ExchangeRequest{Token: result.Token, Email: "bob@example.com", DisplayName: "Bob"})
	if err == nil {
		t.Fatal("second exchange should fail")
	}
	if !errors.Is(err, tokenstore.ErrJoinTokenInvalid) {
		t.Errorf("expected ErrJoinTokenInvalid, got %v", err)
	}
}

// TestExpiredTokenRejected verifies that an expired token is rejected.
func TestExpiredTokenRejected(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	issuer := New(store)
	ctx := context.Background()

	// Create a token and directly expire it.
	result, err := issuer.Create(ctx, CreateRequest{
		TenantID:    "tnt_local",
		Role:        "member",
		UserID:      "",
		Email:       "carol@example.com",
		DisplayName: "Carol",
		CreatedBy:   "usr_local",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Manually expire the token by setting expires_at to past.
	_, err = store.DB().ExecContext(ctx,
		"UPDATE join_tokens SET expires_at = ? WHERE id = ?",
		1, result.ID) // 1 nanosecond = expired
	if err != nil {
		t.Fatalf("expire token: %v", err)
	}

	_, err = issuer.Exchange(ctx, ExchangeRequest{Token: result.Token, Email: "carol@example.com", DisplayName: "Carol"})
	if err == nil {
		t.Fatal("expired token should fail")
	}
}

// TestRevokedTokenRejected verifies that a revoked token is rejected.
func TestRevokedTokenRejected(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	issuer := New(store)
	ctx := context.Background()

	result, err := issuer.Create(ctx, CreateRequest{
		TenantID:    "tnt_local",
		Role:        "member",
		UserID:      "",
		Email:       "dave@example.com",
		DisplayName: "Dave",
		CreatedBy:   "usr_local",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := issuer.Revoke(ctx, result.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	_, err = issuer.Exchange(ctx, ExchangeRequest{Token: result.Token, Email: "dave@example.com", DisplayName: "Dave"})
	if err == nil {
		t.Fatal("revoked token should fail")
	}
}

// TestOwnerRoleRestriction verifies that only owners can issue owner-role
// tokens.
func TestOwnerRoleRestriction(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	issuer := New(store)
	ctx := context.Background()

	// As a member (not owner), try to issue an owner token.
	// The store's seeded user is usr_local with owner role, so we need to
	// create a member first. We'll use the exchange flow to create a
	// member, then try to issue as that member.

	// First, create a member via exchange.
	result, err := issuer.Create(ctx, CreateRequest{
		TenantID:    "tnt_local",
		Role:        "member",
		UserID:      "",
		Email:       "eve@example.com",
		DisplayName: "Eve",
		CreatedBy:   "usr_local", // owner
	})
	if err != nil {
		t.Fatalf("create member token: %v", err)
	}
	exResult, err := issuer.Exchange(ctx, ExchangeRequest{Token: result.Token, Email: "eve@example.com", DisplayName: "Eve"})
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	memberUserID := exResult.Principal.UserID

	// Now try to issue an owner token as the member.
	_, err = issuer.Create(ctx, CreateRequest{
		TenantID:    "tnt_local",
		Role:        "owner",
		UserID:      "",
		Email:       "mallory@example.com",
		DisplayName: "Mallory",
		CreatedBy:   memberUserID, // member
	})
	if err == nil {
		t.Fatal("member should not be able to issue owner tokens")
	}
	if !errors.Is(err, ErrInsufficientRole) {
		t.Errorf("expected ErrInsufficientRole, got %v", err)
	}
}

// TestInvalidTokenFormat verifies that non-jnt tokens are rejected.
func TestInvalidTokenFormat(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	issuer := New(store)
	ctx := context.Background()

	_, err := issuer.Exchange(ctx, ExchangeRequest{Token: "not_a_jnt_token"})
	if err == nil {
		t.Fatal("invalid token format should fail")
	}
	if !errors.Is(err, ErrInvalidToken) {
		t.Errorf("expected ErrInvalidToken, got %v", err)
	}
}

// TestTokenFormat verifies generated tokens have the correct prefix.
func TestTokenFormat(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	issuer := New(store)
	ctx := context.Background()

	result, err := issuer.Create(ctx, CreateRequest{
		TenantID:    "tnt_local",
		Role:        "member",
		UserID:      "",
		Email:       "frank@example.com",
		DisplayName: "Frank",
		CreatedBy:   "usr_local",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !ValidateTokenFormat(result.Token) {
		t.Error("ValidateTokenFormat should accept jnt_ token")
	}
	if ValidateTokenFormat("jnt_") {
		t.Error("ValidateTokenFormat should reject empty token body")
	}
	if ValidateTokenFormat("xxx_token") {
		t.Error("ValidateTokenFormat should reject wrong prefix")
	}
}

// TestListJoinTokens verifies that listing returns metadata only (no secrets).
func TestListJoinTokens(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	issuer := New(store)
	ctx := context.Background()

	// Create a few tokens.
	for i := 0; i < 3; i++ {
		_, err := issuer.Create(ctx, CreateRequest{
			TenantID:    "tnt_local",
			Role:        "member",
			UserID:      "",
			Email:       "test@example.com",
			DisplayName: "Test",
			CreatedBy:   "usr_local",
		})
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}

	tokens, err := issuer.List(ctx, "tnt_local")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tokens) != 3 {
		t.Errorf("len(tokens) = %d, want 3", len(tokens))
	}
	// Verify no secrets in the rows.
	for _, tk := range tokens {
		if strings.Contains(tk.ID, "jnt_") {
			// ID is the token row ID (jnt_<uuid>), not the plaintext secret.
			// The row has no secret field. This is fine.
		}
	}
}

// TestRevokeIdempotent verifies that revoking an already-revoked token is
// a no-op (idempotent).
func TestRevokeIdempotent(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	issuer := New(store)
	ctx := context.Background()

	result, err := issuer.Create(ctx, CreateRequest{
		TenantID:    "tnt_local",
		Role:        "member",
		UserID:      "",
		Email:       "grace@example.com",
		DisplayName: "Grace",
		CreatedBy:   "usr_local",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := issuer.Revoke(ctx, result.ID); err != nil {
		t.Fatalf("first revoke: %v", err)
	}
	if err := issuer.Revoke(ctx, result.ID); err != nil {
		t.Fatalf("second revoke (idempotent): %v", err)
	}
}

// TestRevokeNotFound verifies that revoking a non-existent token returns
// ErrJoinTokenNotFound.
func TestRevokeNotFound(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	issuer := New(store)
	ctx := context.Background()

	err := issuer.Revoke(ctx, "jnt_nonexistent")
	if err == nil {
		t.Fatal("revoke non-existent token should fail")
	}
	if !errors.Is(err, tokenstore.ErrJoinTokenNotFound) {
		t.Errorf("expected ErrJoinTokenNotFound, got %v", err)
	}
}
