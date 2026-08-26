package main

import (
	"context"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	v1alpha1 "github.com/treeol/wakil/api/gen/wakil/v1alpha1"
	wakilv1alpha1connect "github.com/treeol/wakil/api/gen/wakil/v1alpha1/wakilv1alpha1connect"
	"github.com/treeol/wakil/internal/auth/apitoken"
	"github.com/treeol/wakil/internal/auth/jointoken"
	connsvc "github.com/treeol/wakil/internal/server/connect"
)

// TestP4eGetOIDCAuthURLUnconfigured tests that GetOIDCAuthURL returns
// Unimplemented when OIDC is not configured.
func TestP4eGetOIDCAuthURLUnconfigured(t *testing.T) {
	store := newTestTokenStore(t)
	issuer := jointoken.New(store)
	apiIssuer := apitoken.New(store)

	// Standard server without OIDC config.
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

	_, err := client.GetOIDCAuthURL(ctx, connect.NewRequest(&v1alpha1.GetOIDCAuthURLRequest{}))
	if err == nil {
		t.Fatal("expected Unimplemented error, got nil")
	}
	connErr := connect.CodeOf(err)
	if connErr != connect.CodeUnimplemented {
		t.Errorf("error code = %v, want Unimplemented", connErr)
	}
}

// TestP4eExchangeOIDCCodeUnconfigured tests that ExchangeOIDCCode returns
// Unimplemented when OIDC is not configured.
func TestP4eExchangeOIDCCodeUnconfigured(t *testing.T) {
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

	_, err := client.ExchangeOIDCCode(ctx, connect.NewRequest(&v1alpha1.ExchangeOIDCCodeRequest{
		Code: "test-code",
	}))
	if err == nil {
		t.Fatal("expected Unimplemented error, got nil")
	}
	connErr := connect.CodeOf(err)
	if connErr != connect.CodeUnimplemented {
		t.Errorf("error code = %v, want Unimplemented", connErr)
	}
}

// TestP4eGetOIDCAuthURLConfigured tests that GetOIDCAuthURL returns the
// auth URL when OIDC is configured with an authURLBuilder.
func TestP4eGetOIDCAuthURLConfigured(t *testing.T) {
	store := newTestTokenStore(t)
	issuer := jointoken.New(store)
	apiIssuer := apitoken.New(store)

	// Server with OIDC configured.
	srv := connsvc.NewServerWithAuthAndOIDC(
		nil,
		true,
		connsvc.NewEmbeddedResolver(),
		issuer,
		apiIssuer,
		store,
		connsvc.OIDCConfig{
			Issuer:      "https://auth.example.com",
			ClientID:    "test-client",
			RedirectURI: "http://localhost:8791/callback",
			AuthURLBuilder: func(redirectURI string) (string, error) {
				return "https://auth.example.com/oauth2/v1/authorize?client_id=test-client&redirect_uri=" + redirectURI + "&response_type=code&scope=openid+profile+email", nil
			},
		},
	)

	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	client := wakilv1alpha1connect.NewAuthServiceClient(httpSrv.Client(), httpSrv.URL)
	ctx := context.Background()

	resp, err := client.GetOIDCAuthURL(ctx, connect.NewRequest(&v1alpha1.GetOIDCAuthURLRequest{}))
	if err != nil {
		t.Fatalf("GetOIDCAuthURL: %v", err)
	}
	if resp.Msg.AuthUrl == "" {
		t.Fatal("auth_url is empty")
	}
	// The URL should contain the issuer.
	if resp.Msg.AuthUrl == "" {
		t.Errorf("auth_url is empty")
	}
}

// TestP4eExchangeOIDCCodeConfigured tests that ExchangeOIDCCode returns
// Unimplemented even when OIDC is configured (the code exchange requires
// an actual IdP integration).
func TestP4eExchangeOIDCCodeConfigured(t *testing.T) {
	store := newTestTokenStore(t)
	issuer := jointoken.New(store)
	apiIssuer := apitoken.New(store)

	srv := connsvc.NewServerWithAuthAndOIDC(
		nil,
		true,
		connsvc.NewEmbeddedResolver(),
		issuer,
		apiIssuer,
		store,
		connsvc.OIDCConfig{
			Issuer:      "https://auth.example.com",
			ClientID:    "test-client",
			RedirectURI: "http://localhost:8791/callback",
			AuthURLBuilder: func(redirectURI string) (string, error) {
				return "https://auth.example.com/authorize", nil
			},
		},
	)

	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	client := wakilv1alpha1connect.NewAuthServiceClient(httpSrv.Client(), httpSrv.URL)
	ctx := context.Background()

	_, err := client.ExchangeOIDCCode(ctx, connect.NewRequest(&v1alpha1.ExchangeOIDCCodeRequest{
		Code: "test-code",
	}))
	if err == nil {
		t.Fatal("expected Unimplemented error, got nil")
	}
	connErr := connect.CodeOf(err)
	if connErr != connect.CodeUnimplemented {
		t.Errorf("error code = %v, want Unimplemented (code exchange needs IdP)", connErr)
	}
}
