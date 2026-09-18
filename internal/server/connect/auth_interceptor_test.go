package connect

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOriginValidator_EmptyAllowlist(t *testing.T) {
	called := false
	handler := OriginValidator(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Cookie", "session=abc")
	req.Header.Set("Origin", "https://evil.com")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if !called {
		t.Error("handler should be called when allowlist is empty (dev mode)")
	}
}

func TestOriginValidator_AllowedOrigin(t *testing.T) {
	called := false
	handler := OriginValidator(map[string]bool{"https://app.example.com": true})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
		}))

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Cookie", "session=abc")
	req.Header.Set("Origin", "https://app.example.com")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if !called {
		t.Error("handler should be called for allowed origin")
	}
}

func TestOriginValidator_BlockedOrigin(t *testing.T) {
	called := false
	handler := OriginValidator(map[string]bool{"https://app.example.com": true})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
		}))

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Cookie", "session=abc")
	req.Header.Set("Origin", "https://evil.com")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if called {
		t.Error("handler should NOT be called for blocked origin")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestOriginValidator_NoOriginWithCookie(t *testing.T) {
	called := false
	handler := OriginValidator(map[string]bool{"https://app.example.com": true})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
		}))

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Cookie", "session=abc")
	// No Origin header.
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if called {
		t.Error("handler should NOT be called when cookie present but no Origin")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestOriginValidator_NonCookieBypass(t *testing.T) {
	called := false
	handler := OriginValidator(map[string]bool{"https://app.example.com": true})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
		}))

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	// No Cookie header — Bearer auth client.
	req.Header.Set("Origin", "https://evil.com")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if !called {
		t.Error("handler should be called for non-cookie requests regardless of origin")
	}
}

func TestOriginValidator_GetMethodBypass(t *testing.T) {
	called := false
	handler := OriginValidator(map[string]bool{"https://app.example.com": true})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
		}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Cookie", "session=abc")
	req.Header.Set("Origin", "https://evil.com")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if !called {
		t.Error("handler should be called for GET method even with cookie + unapproved origin")
	}
}

func TestOriginValidator_PutMethodEnforced(t *testing.T) {
	called := false
	handler := OriginValidator(map[string]bool{"https://app.example.com": true})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
		}))

	req := httptest.NewRequest(http.MethodPut, "/", nil)
	req.Header.Set("Cookie", "session=abc")
	req.Header.Set("Origin", "https://evil.com")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if called {
		t.Error("handler should NOT be called for PUT with cookie + unapproved origin")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestOriginValidator_DeleteMethodEnforced(t *testing.T) {
	called := false
	handler := OriginValidator(map[string]bool{"https://app.example.com": true})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
		}))

	req := httptest.NewRequest(http.MethodDelete, "/", nil)
	req.Header.Set("Cookie", "session=abc")
	req.Header.Set("Origin", "https://evil.com")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if called {
		t.Error("handler should NOT be called for DELETE with cookie + unapproved origin")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestOriginValidator_PatchMethodEnforced(t *testing.T) {
	called := false
	handler := OriginValidator(map[string]bool{"https://app.example.com": true})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
		}))

	req := httptest.NewRequest(http.MethodPatch, "/", nil)
	req.Header.Set("Cookie", "session=abc")
	req.Header.Set("Origin", "https://evil.com")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if called {
		t.Error("handler should NOT be called for PATCH with cookie + unapproved origin")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

// TestOriginValidator_RejectionDoesNotCallDownstream verifies that when
// the middleware rejects a request, the downstream handler is never invoked.
func TestOriginValidator_RejectionDoesNotCallDownstream(t *testing.T) {
	called := false
	handler := OriginValidator(map[string]bool{"https://app.example.com": true})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
		}))

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Cookie", "session=abc")
	req.Header.Set("Origin", "https://evil.com")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if called {
		t.Error("downstream handler was called despite rejection")
	}
}
