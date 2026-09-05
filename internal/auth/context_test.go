package auth

import (
	"context"
	"errors"
	"net/http"
	"os"
	"testing"

	"github.com/treeol/wakil/internal/auth/peercred"
	"github.com/treeol/wakil/internal/core"
)

// fakeResolver is a stub PrincipalResolver for testing MultiResolver.
type fakeResolver struct {
	principal core.Principal
	err       error
}

func (f *fakeResolver) Resolve(ctx context.Context) (core.Principal, error) {
	return f.principal, f.err
}

// TestWithHTTPHeadersRoundTrip verifies the context set/get cycle for HTTP headers.
func TestWithHTTPHeadersRoundTrip(t *testing.T) {
	hdrs := http.Header{}
	hdrs.Set("Authorization", "Bearer test-token")
	hdrs.Set("Cookie", "session=abc123")

	ctx := WithHTTPHeaders(context.Background(), hdrs)
	got, ok := HTTPHeadersFromContext(ctx)
	if !ok {
		t.Fatal("HTTPHeadersFromContext: ok=false, want true")
	}
	if got.Get("Authorization") != "Bearer test-token" {
		t.Errorf("Authorization = %q, want %q", got.Get("Authorization"), "Bearer test-token")
	}
	if got.Get("Cookie") != "session=abc123" {
		t.Errorf("Cookie = %q, want %q", got.Get("Cookie"), "session=abc123")
	}
}

// TestHTTPHeadersFromContextAbsent verifies absent headers return (nil, false).
func TestHTTPHeadersFromContextAbsent(t *testing.T) {
	_, ok := HTTPHeadersFromContext(context.Background())
	if ok {
		t.Error("HTTPHeadersFromContext should return ok=false when no headers set")
	}
}

// TestWithPeerCredentialsRoundTrip verifies the context set/get cycle for peer creds.
func TestWithPeerCredentialsRoundTrip(t *testing.T) {
	creds := peercred.Credentials{UID: 1000, GID: 2000, PID: 9999}
	ctx := WithPeerCredentials(context.Background(), creds)
	got, ok := PeerCredentialsFromContext(ctx)
	if !ok {
		t.Fatal("PeerCredentialsFromContext: ok=false, want true")
	}
	if got.UID != 1000 || got.GID != 2000 || got.PID != 9999 {
		t.Errorf("creds = %+v, want %+v", got, creds)
	}
}

// TestPeerCredentialsFromContextAbsent verifies absent creds return (zero, false).
func TestPeerCredentialsFromContextAbsent(t *testing.T) {
	_, ok := PeerCredentialsFromContext(context.Background())
	if ok {
		t.Error("PeerCredentialsFromContext should return ok=false when no creds set")
	}
}

// TestNewLocalResolver verifies the constructor captures os.Geteuid().
func TestNewLocalResolver(t *testing.T) {
	r := NewLocalResolver()
	if r == nil {
		t.Fatal("NewLocalResolver returned nil")
	}
	// Should accept the current euid (it captured it at construction).
	uid := uint32(os.Geteuid())
	ctx := WithPeerCredentials(context.Background(), peercred.Credentials{UID: uid, GID: uid, PID: 1})
	p, err := r.Resolve(ctx)
	if err != nil {
		t.Fatalf("Resolve with matching UID: %v", err)
	}
	if p.TenantID != "tnt_local" {
		t.Errorf("TenantID = %q, want tnt_local", p.TenantID)
	}
}

// TestMultiResolverFirstWins verifies the first resolver that succeeds wins.
func TestMultiResolverFirstWins(t *testing.T) {
	winner := &fakeResolver{principal: core.Principal{TenantID: "tnt_a", UserID: "usr_a", Role: core.RoleOwner}}
	loser := &fakeResolver{err: ErrCredentialAbsent}
	mr := NewMultiResolver(winner, loser)
	p, err := mr.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if p.TenantID != "tnt_a" {
		t.Errorf("TenantID = %q, want tnt_a", p.TenantID)
	}
}

// TestMultiResolverFallThrough verifies ErrCredentialAbsent falls through to next.
func TestMultiResolverFallThrough(t *testing.T) {
	absent := &fakeResolver{err: ErrCredentialAbsent}
	winner := &fakeResolver{principal: core.Principal{TenantID: "tnt_b", UserID: "usr_b", Role: core.RoleOwner}}
	mr := NewMultiResolver(absent, winner)
	p, err := mr.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if p.TenantID != "tnt_b" {
		t.Errorf("TenantID = %q, want tnt_b", p.TenantID)
	}
}

// TestMultiResolverHardFail verifies ErrInvalidCredential does NOT fall through.
func TestMultiResolverHardFail(t *testing.T) {
	invalid := &fakeResolver{err: ErrInvalidCredential}
	shouldNotBeCalled := &fakeResolver{principal: core.Principal{TenantID: "tnt_never"}}
	mr := NewMultiResolver(invalid, shouldNotBeCalled)
	_, err := mr.Resolve(context.Background())
	if !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("expected ErrInvalidCredential, got %v", err)
	}
}

// TestMultiResolverAllAbsent verifies all-absent returns ErrUnauthenticated.
func TestMultiResolverAllAbsent(t *testing.T) {
	a := &fakeResolver{err: ErrCredentialAbsent}
	b := &fakeResolver{err: ErrCredentialAbsent}
	mr := NewMultiResolver(a, b)
	_, err := mr.Resolve(context.Background())
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expected ErrUnauthenticated, got %v", err)
	}
}

// TestMultiResolverEmpty verifies an empty chain returns ErrUnauthenticated.
func TestMultiResolverEmpty(t *testing.T) {
	mr := NewMultiResolver()
	_, err := mr.Resolve(context.Background())
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expected ErrUnauthenticated, got %v", err)
	}
}
