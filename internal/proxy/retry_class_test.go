package proxy

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRetryClassification429And408Retryable: rate limits and request
// timeouts are transient — they must be ErrBackendStream (retryable), not
// fatal. 529 (overloaded) likewise. Plain request errors stay fatal.
func TestRetryClassification429And408Retryable(t *testing.T) {
	cases := []struct {
		status     int
		wantStream bool
	}{
		{http.StatusTooManyRequests, true}, // 429 — the direct-OpenRouter bite
		{http.StatusRequestTimeout, true},  // 408
		{529, true},                        // Anthropic/OpenRouter overloaded
		{http.StatusBadRequest, false},     // 400
		{http.StatusUnauthorized, false},   // 401
		{http.StatusForbidden, false},      // 403
		{http.StatusNotFound, false},       // 404 (e.g. bad model name)
		{http.StatusUnprocessableEntity, false},
	}

	for _, tc := range cases {
		t.Run(http.StatusText(tc.status)+"_"+itoa(tc.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "err body", tc.status)
			}))
			defer srv.Close()

			c := &Client{BaseURL: srv.URL, Model: "ilm", ChatID: "t", HTTP: http.DefaultClient}
			_, err := c.Stream(t.Context(), []Message{{Role: "user", Content: strPtr("hi")}}, nil, nil, nil)
			if err == nil {
				t.Fatal("want error")
			}
			if tc.wantStream {
				if !errors.Is(err, ErrBackendStream) {
					t.Errorf("status %d: want ErrBackendStream (retryable), got %v", tc.status, err)
				}
				if errors.Is(err, ErrBackendFatal) {
					t.Errorf("status %d must not be fatal", tc.status)
				}
			} else {
				if !errors.Is(err, ErrBackendFatal) {
					t.Errorf("status %d: want ErrBackendFatal, got %v", tc.status, err)
				}
			}
		})
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
