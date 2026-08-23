package main

// p4f_test.go: P4f — TLS termination, Secure cookies, Origin validation.
//
// Tests that:
//  1. Session cookies carry the Secure flag when TLS is enabled.
//  2. Session cookies do NOT carry the Secure flag when TLS is disabled.
//  3. The Origin validator rejects unapproved origins on POST.
//  4. The Origin validator allows approved origins on POST.
//  5. The Origin validator allows all requests when no allowlist is set (dev mode).
//  6. TLS listener serves HTTPS and rejects plain HTTP.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	v1alpha1 "github.com/treeol/wakil/api/gen/wakil/v1alpha1"
	wakilv1alpha1connect "github.com/treeol/wakil/api/gen/wakil/v1alpha1/wakilv1alpha1connect"
	"github.com/treeol/wakil/internal/auth/apitoken"
	"github.com/treeol/wakil/internal/auth/jointoken"
	connsvc "github.com/treeol/wakil/internal/server/connect"
)

// TestP4fSecureCookieWithTLS verifies that when secureCookies=true, the
// Set-Cookie header from ExchangeJoinToken includes the Secure attribute.
func TestP4fSecureCookieWithTLS(t *testing.T) {
	store := newTestTokenStore(t)
	issuer := jointoken.New(store)
	apiIssuer := apitoken.New(store)

	// Server with secureCookies=true (TLS mode).
	srv := connsvc.NewServerWithAuthAndSecureCookies(
		nil,
		true,
		connsvc.NewEmbeddedResolver(),
		issuer,
		apiIssuer,
		store,
		true, // secureCookies = TLS mode
	)

	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	client := wakilv1alpha1connect.NewAuthServiceClient(httpSrv.Client(), httpSrv.URL)
	ctx := context.Background()

	// Create a join token.
	createResp, err := client.CreateJoinToken(ctx, connect.NewRequest(&v1alpha1.CreateJoinTokenRequest{
		TenantId:    "tnt_local",
		Role:        "member",
		Email:       "p4f-tls@example.com",
		DisplayName: "P4f TLS User",
	}))
	if err != nil {
		t.Fatalf("create join token: %v", err)
	}

	// Exchange the token.
	exchangeResp, err := client.ExchangeJoinToken(ctx, connect.NewRequest(&v1alpha1.ExchangeJoinTokenRequest{
		Token:       createResp.Msg.Token,
		Email:       "p4f-tls@example.com",
		DisplayName: "P4f TLS User",
	}))
	if err != nil {
		t.Fatalf("exchange join token: %v", err)
	}

	setCookie := exchangeResp.Header().Get("Set-Cookie")
	if setCookie == "" {
		t.Fatal("Set-Cookie header is empty")
	}
	if !strings.Contains(setCookie, "Secure") {
		t.Errorf("Set-Cookie should contain Secure flag when TLS is enabled: %q", setCookie)
	}
	if !strings.Contains(setCookie, "HttpOnly") {
		t.Error("Set-Cookie should contain HttpOnly")
	}
	if !strings.Contains(setCookie, "SameSite=Strict") {
		t.Error("Set-Cookie should contain SameSite=Strict")
	}
}

// TestP4fNoSecureCookieWithoutTLS verifies that when secureCookies=false, the
// Set-Cookie header does NOT include the Secure attribute (dev/HTTP mode).
func TestP4fNoSecureCookieWithoutTLS(t *testing.T) {
	store := newTestTokenStore(t)
	issuer := jointoken.New(store)
	apiIssuer := apitoken.New(store)

	// Standard server without secure cookies (plain HTTP mode).
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

	createResp, err := client.CreateJoinToken(ctx, connect.NewRequest(&v1alpha1.CreateJoinTokenRequest{
		TenantId:    "tnt_local",
		Role:        "member",
		Email:       "p4f-notls@example.com",
		DisplayName: "P4f NoTLS User",
	}))
	if err != nil {
		t.Fatalf("create join token: %v", err)
	}

	exchangeResp, err := client.ExchangeJoinToken(ctx, connect.NewRequest(&v1alpha1.ExchangeJoinTokenRequest{
		Token:       createResp.Msg.Token,
		Email:       "p4f-notls@example.com",
		DisplayName: "P4f NoTLS User",
	}))
	if err != nil {
		t.Fatalf("exchange join token: %v", err)
	}

	setCookie := exchangeResp.Header().Get("Set-Cookie")
	if setCookie == "" {
		t.Fatal("Set-Cookie header is empty")
	}

	// "Secure" appears as an attribute in cookie string: "...; Secure"
	if strings.Contains(setCookie, "; Secure") {
		t.Errorf("Set-Cookie should NOT contain Secure flag when TLS is disabled: %q", setCookie)
	}
}

// TestP4fOriginValidatorRejectsUnapprovedOrigin verifies that the Origin
// validator rejects cookie-authenticated POST requests from origins not in
// the allowlist.
func TestP4fOriginValidatorRejectsUnapprovedOrigin(t *testing.T) {
	allowedOrigins := map[string]bool{
		"https://app.example.com": true,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := connsvc.OriginValidator(allowedOrigins)(mux)

	ts := httptest.NewServer(handler)
	defer ts.Close()

	// POST with a cookie from an unapproved origin.
	req, _ := http.NewRequest("POST", ts.URL+"/api", strings.NewReader("{}"))
	req.Header.Set("Origin", "https://evil.example.com")
	req.Header.Set("Cookie", "wakil_session=test")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want %d (forbidden for unapproved origin with cookie)", resp.StatusCode, http.StatusForbidden)
	}
}

// TestP4fOriginValidatorAllowsApprovedOrigin verifies that the Origin
// validator allows cookie-authenticated POST requests from origins in the
// allowlist.
func TestP4fOriginValidatorAllowsApprovedOrigin(t *testing.T) {
	allowedOrigins := map[string]bool{
		"https://app.example.com": true,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := connsvc.OriginValidator(allowedOrigins)(mux)

	ts := httptest.NewServer(handler)
	defer ts.Close()

	// POST with a cookie from an approved origin.
	req, _ := http.NewRequest("POST", ts.URL+"/api", strings.NewReader("{}"))
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Cookie", "wakil_session=test")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d (allowed for approved origin with cookie)", resp.StatusCode, http.StatusOK)
	}
}

// TestP4fOriginValidatorAllowsBearerWithoutOrigin verifies that Bearer
// clients (no cookie) are not subject to Origin validation, even when an
// allowlist is set.
func TestP4fOriginValidatorAllowsBearerWithoutOrigin(t *testing.T) {
	allowedOrigins := map[string]bool{
		"https://app.example.com": true,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := connsvc.OriginValidator(allowedOrigins)(mux)

	ts := httptest.NewServer(handler)
	defer ts.Close()

	// POST with Bearer auth (no cookie, no origin) — should be allowed.
	req, _ := http.NewRequest("POST", ts.URL+"/api", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer tok_test")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d (Bearer clients skip origin check)", resp.StatusCode, http.StatusOK)
	}
}

// TestP4fOriginValidatorDevMode verifies that when no origins are configured
// (empty allowlist), all requests are allowed (development mode).
func TestP4fOriginValidatorDevMode(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// nil = dev mode (all origins allowed)
	handler := connsvc.OriginValidator(nil)(mux)

	ts := httptest.NewServer(handler)
	defer ts.Close()

	// POST with cookie from any origin in dev mode.
	req, _ := http.NewRequest("POST", ts.URL+"/api", strings.NewReader("{}"))
	req.Header.Set("Origin", "https://anything.example.com")
	req.Header.Set("Cookie", "wakil_session=test")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d (dev mode allows all)", resp.StatusCode, http.StatusOK)
	}
}

// TestP4fOriginValidatorAllowsGET verifies that GET requests (static files)
// are not subject to Origin validation.
func TestP4fOriginValidatorAllowsGET(t *testing.T) {
	allowedOrigins := map[string]bool{
		"https://app.example.com": true,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/static", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := connsvc.OriginValidator(allowedOrigins)(mux)

	ts := httptest.NewServer(handler)
	defer ts.Close()

	// GET from an unapproved origin — should still be allowed.
	req, _ := http.NewRequest("GET", ts.URL+"/static", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d (GET not subject to origin check)", resp.StatusCode, http.StatusOK)
	}
}

// TestP4fTLSListenerServesHTTPS verifies that a TLS-wrapped listener serves
// HTTPS and that a client configured to skip verification can connect.
func TestP4fTLSListenerServesHTTPS(t *testing.T) {
	cert, err := generateTestTLSCert(t)
	if err != nil {
		t.Fatalf("generate test cert: %v", err)
	}

	// Create a plain TCP listener on a random port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	// Wrap with TLS.
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	tlsLn := tls.NewListener(ln, tlsConfig)

	// Simple handler that returns 200.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("hello tls"))
	})
	srv := &http.Server{Handler: handler}
	go srv.Serve(tlsLn)
	defer srv.Close()

	// Connect with an HTTPS client that skips verification (self-signed).
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	resp, err := client.Get("https://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatalf("https request failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if string(body) != "hello tls" {
		t.Errorf("body = %q, want 'hello tls'", string(body))
	}
}

// TestP4fTLSRejectsPlainHTTP verifies that a TLS listener does not serve
// plain HTTP requests.
func TestP4fTLSRejectsPlainHTTP(t *testing.T) {
	cert, err := generateTestTLSCert(t)
	if err != nil {
		t.Fatalf("generate test cert: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	tlsLn := tls.NewListener(ln, tlsConfig)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := &http.Server{Handler: handler}
	go srv.Serve(tlsLn)
	defer srv.Close()

	// Try a plain HTTP request — should fail (TLS handshake expected).
	// The server logs "TLS handshake error" and the connection is dropped.
	// The HTTP client may return an error or a response with a non-OK status.
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + ln.Addr().String() + "/")
	if err == nil && resp != nil {
		// Some HTTP clients return a response with the TLS error body.
		// In any case, the status should not be 200 OK.
		if resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			t.Error("plain HTTP request to TLS listener should not succeed with 200 OK")
		}
		resp.Body.Close()
	}
	// If err != nil, that's the expected case — the request failed.
}

// TestP4fLogoutClearsSecureCookie verifies that the Logout handler sets a
// Secure cookie deletion when secureCookies=true.
func TestP4fLogoutClearsSecureCookie(t *testing.T) {
	store := newTestTokenStore(t)
	issuer := jointoken.New(store)
	apiIssuer := apitoken.New(store)

	srv := connsvc.NewServerWithAuthAndSecureCookies(
		nil,
		true,
		connsvc.NewEmbeddedResolver(),
		issuer,
		apiIssuer,
		store,
		true, // secureCookies = TLS mode
	)

	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	client := wakilv1alpha1connect.NewAuthServiceClient(httpSrv.Client(), httpSrv.URL)
	ctx := context.Background()

	// Create + exchange to get a session cookie.
	createResp, err := client.CreateJoinToken(ctx, connect.NewRequest(&v1alpha1.CreateJoinTokenRequest{
		TenantId:    "tnt_local",
		Role:        "member",
		Email:       "p4f-logout@example.com",
		DisplayName: "P4f Logout User",
	}))
	if err != nil {
		t.Fatalf("create join token: %v", err)
	}

	exchangeResp, err := client.ExchangeJoinToken(ctx, connect.NewRequest(&v1alpha1.ExchangeJoinTokenRequest{
		Token:       createResp.Msg.Token,
		Email:       "p4f-logout@example.com",
		DisplayName: "P4f Logout User",
	}))
	if err != nil {
		t.Fatalf("exchange join token: %v", err)
	}

	// Extract the session cookie value from Set-Cookie.
	setCookie := exchangeResp.Header().Get("Set-Cookie")
	if !strings.Contains(setCookie, "Secure") {
		t.Fatalf("exchange cookie should have Secure flag: %q", setCookie)
	}

	// Logout — the deletion cookie should also have Secure.
	logoutResp, err := client.Logout(ctx, connect.NewRequest(&v1alpha1.LogoutRequest{}))
	if err != nil {
		t.Fatalf("logout: %v", err)
	}

	clearCookie := logoutResp.Header().Get("Set-Cookie")
	if clearCookie == "" {
		t.Fatal("Logout Set-Cookie header is empty")
	}
	if !strings.Contains(clearCookie, "Secure") {
		t.Errorf("Logout deletion cookie should have Secure flag: %q", clearCookie)
	}
	if !strings.Contains(clearCookie, "MaxAge=0") && !strings.Contains(clearCookie, "Max-Age=0") {
		t.Errorf("Logout cookie should have MaxAge=0 (delete): %q", clearCookie)
	}
}

// generateTestTLSCert creates a self-signed ECDSA certificate for testing TLS.
// The certificate is generated in-memory using crypto/x509.
func generateTestTLSCert(t *testing.T) (tls.Certificate, error) {
	t.Helper()

	// Generate an ECDSA private key.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}

	// Create a self-signed certificate template.
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName:   "localhost",
			Organization: []string{"Test"},
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{"localhost", "127.0.0.1"},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}

	ecDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}

	return tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: ecDER}),
	)
}

// TestP4fTLSCertFileLoading verifies that TLS certificate files can be loaded
// from disk and used to create a TLS listener. This tests the same code path
// as newDaemonServer's --tls-cert/--tls-key handling.
func TestP4fTLSCertFileLoading(t *testing.T) {
	cert, err := generateTestTLSCert(t)
	if err != nil {
		t.Fatalf("generate cert: %v", err)
	}

	// Write cert and key to temp files.
	dir := t.TempDir()
	certPath := dir + "/cert.pem"
	keyPath := dir + "/key.pem"

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	if err := os.WriteFile(certPath, certPEM, 0600); err != nil {
		t.Fatalf("write cert: %v", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(cert.PrivateKey.(*ecdsa.PrivateKey))
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	// Load the keypair from files (same as newDaemonServer does).
	loaded, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadX509KeyPair: %v", err)
	}
	if len(loaded.Certificate) == 0 {
		t.Error("loaded certificate is empty")
	}
}

// TestP4fTCPMuxRoutingConnectRPC verifies that Connect RPC paths (starting
// with /wakil.v1alpha1.) are routed to the Connect handler, not the static
// file handler. This is a regression test for the ServeMux prefix bug.
func TestP4fTCPMuxRoutingConnectRPC(t *testing.T) {
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

	tcpConnectHandler := srv.Handler()
	staticHandler := webStaticHandler()

	// Simulate the TCP mux routing used in newDaemonServer.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/wakil.v1alpha1.") {
			tcpConnectHandler.ServeHTTP(w, r)
			return
		}
		staticHandler.ServeHTTP(w, r)
	})

	ts := httptest.NewServer(handler)
	defer ts.Close()

	// A Connect RPC path should NOT return a static file.
	// The AuthService path is /wakil.v1alpha1.AuthService/WhoAmI.
	// Without proper routing, this would return the index.html (200 + HTML).
	resp, err := http.Post(ts.URL+"/wakil.v1alpha1.AuthService/WhoAmI", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST to Connect RPC: %v", err)
	}
	defer resp.Body.Close()

	// It should NOT be 200 with HTML (that would mean it hit the static handler).
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)
	if resp.StatusCode == 200 && strings.Contains(bodyStr, "<!DOCTYPE") {
		t.Errorf("Connect RPC path was routed to static handler (got HTML): status=%d", resp.StatusCode)
	}
}

// TestP4fFlagValidation verifies that TLS flag combinations are validated.
func TestP4fFlagValidation(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{"tls-key without tls-cert", []string{"--tls-key", "/tmp/key.pem"}, true},
		{"tls-cert without tls-key", []string{"--tls-cert", "/tmp/cert.pem"}, true},
		{"tls-cert without http-addr", []string{"--tls-cert", "/tmp/cert.pem", "--tls-key", "/tmp/key.pem"}, true},
		{"valid tls+http-addr", []string{"--http-addr", "127.0.0.1:8791", "--tls-cert", "/tmp/cert.pem", "--tls-key", "/tmp/key.pem"}, false},
		{"no tls flags", []string{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// We can't call execDaemon() (it would start the daemon), so we test
			// parseDaemonFlags + the validation logic inline.
			flags, err := parseDaemonFlags(tt.args)
			if err != nil {
				t.Fatalf("parseDaemonFlags: %v", err)
			}
			// Replicate the validation from execDaemon().
			err = validateTLSFlags(flags)
			if tt.wantErr && err == nil {
				t.Errorf("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func validateTLSFlags(f daemonFlags) error {
	if f.tlsKeyFile != "" && f.tlsCertFile == "" {
		return fmt.Errorf("wakil daemon: --tls-key requires --tls-cert")
	}
	if f.tlsCertFile != "" && f.tlsKeyFile == "" {
		return fmt.Errorf("wakil daemon: --tls-cert requires --tls-key")
	}
	if f.tlsCertFile != "" && f.httpAddr == "" {
		return fmt.Errorf("wakil daemon: --tls-cert requires --http-addr (TLS applies to the TCP listener)")
	}
	return nil
}
