//go:build browser

package browser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These integration tests require a real Chromium installation.
// Run with: go test -tags=browser ./internal/browser/
//
// They are excluded from the default test suite because the sandbox
// CI environment may not have Chromium installed, and these tests
// would fail or hang if the browser cannot launch.

func TestBrowserIntegration_LaunchAndNavigate(t *testing.T) {
	mgr, err := NewManager()
	if err != nil {
		t.Skipf("Chromium not available: %v", err)
	}
	defer mgr.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head><title>Test Page</title></head><body><h1>Hello World</h1></body></html>`))
	}))
	defer srv.Close()

	title, finalURL, err := mgr.Navigate(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	if title != "Test Page" {
		t.Errorf("title = %q, want 'Test Page'", title)
	}
	if !strings.HasPrefix(finalURL, srv.URL) {
		t.Errorf("finalURL = %q, want prefix %q", finalURL, srv.URL)
	}
}

func TestBrowserIntegration_GetText(t *testing.T) {
	mgr, err := NewManager()
	if err != nil {
		t.Skipf("Chromium not available: %v", err)
	}
	defer mgr.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html><body><h1 id="title">Hello Test</h1></body></html>`))
	}))
	defer srv.Close()

	mgr.Navigate(context.Background(), srv.URL)
	text, err := mgr.GetText(context.Background(), "#title")
	if err != nil {
		t.Fatalf("GetText: %v", err)
	}
	if text != "Hello Test" {
		t.Errorf("text = %q, want 'Hello Test'", text)
	}
}

func TestBrowserIntegration_EvalJS(t *testing.T) {
	mgr, err := NewManager()
	if err != nil {
		t.Skipf("Chromium not available: %v", err)
	}
	defer mgr.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html><body></body></html>`))
	}))
	defer srv.Close()

	mgr.Navigate(context.Background(), srv.URL)
	result, err := mgr.EvalJS(context.Background(), "1 + 1")
	if err != nil {
		t.Fatalf("EvalJS: %v", err)
	}
	if result != "2" {
		t.Errorf("EvalJS result = %q, want '2'", result)
	}
}

func TestBrowserIntegration_Screenshot(t *testing.T) {
	mgr, err := NewManager()
	if err != nil {
		t.Skipf("Chromium not available: %v", err)
	}
	defer mgr.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html><body><h1>Screenshot Test</h1></body></html>`))
	}))
	defer srv.Close()

	mgr.Navigate(context.Background(), srv.URL)
	path, err := mgr.Screenshot(context.Background(), false)
	if err != nil {
		t.Fatalf("Screenshot: %v", err)
	}
	if path == "" {
		t.Fatal("expected non-empty screenshot path")
	}
}

func TestBrowserIntegration_SetViewport(t *testing.T) {
	mgr, err := NewManager()
	if err != nil {
		t.Skipf("Chromium not available: %v", err)
	}
	defer mgr.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html><body></body></html>`))
	}))
	defer srv.Close()

	mgr.Navigate(context.Background(), srv.URL)
	if err := mgr.SetViewport(context.Background(), 375, 812); err != nil {
		t.Fatalf("SetViewport: %v", err)
	}
}

func TestBrowserIntegration_EmulateReducedMotion(t *testing.T) {
	mgr, err := NewManager()
	if err != nil {
		t.Skipf("Chromium not available: %v", err)
	}
	defer mgr.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html><body></body></html>`))
	}))
	defer srv.Close()

	mgr.Navigate(context.Background(), srv.URL)
	if err := mgr.EmulateReducedMotion(context.Background(), true); err != nil {
		t.Fatalf("EmulateReducedMotion: %v", err)
	}
}

func TestBrowserIntegration_Click(t *testing.T) {
	mgr, err := NewManager()
	if err != nil {
		t.Skipf("Chromium not available: %v", err)
	}
	defer mgr.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html><body><button id="btn" onclick="this.textContent='Clicked'">Click Me</button></body></html>`))
	}))
	defer srv.Close()

	mgr.Navigate(context.Background(), srv.URL)
	if err := mgr.Click(context.Background(), "#btn"); err != nil {
		t.Fatalf("Click: %v", err)
	}
}
