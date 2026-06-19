package proxy

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWrapStreamErrIsBackendStream(t *testing.T) {
	err := wrapStreamErr(io.ErrUnexpectedEOF)
	if !errors.Is(err, ErrBackendStream) {
		t.Errorf("wrapStreamErr should match ErrBackendStream: %v", err)
	}
	if err.Error() == ErrBackendStream.Error() {
		t.Errorf("wrapped error should include cause detail, got bare sentinel: %q", err.Error())
	}
}

// TestStreamErrorClassification verifies the three classification buckets:
//   - 4xx → ErrBackendFatal (never retry)
//   - 5xx → ErrBackendStream (retry)
//   - transport error (pre-response) → ErrBackendStream (retry)
func TestStreamErrorClassification(t *testing.T) {
	sseOK := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\ndata: [DONE]\n\n"))
	}

	cases := []struct {
		name      string
		handler   http.HandlerFunc
		wantFatal bool   // ErrBackendFatal
		wantStream bool  // ErrBackendStream
	}{
		{
			name: "200 OK",
			handler: sseOK,
		},
		{
			name: "500 Internal Server Error",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "backend down", http.StatusInternalServerError)
			},
			wantStream: true,
		},
		{
			name: "503 Service Unavailable",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "overloaded", http.StatusServiceUnavailable)
			},
			wantStream: true,
		},
		{
			name: "400 Bad Request",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "bad request body", http.StatusBadRequest)
			},
			wantFatal: true,
		},
		{
			name: "401 Unauthorized",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "invalid api key", http.StatusUnauthorized)
			},
			wantFatal: true,
		},
		{
			name: "422 Unprocessable",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "context too long", http.StatusUnprocessableEntity)
			},
			wantFatal: true,
		},
		{
			name: "mid-stream body truncation",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				// Write a partial chunk then close without [DONE].
				w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"he"))
				// Hijack and close the connection abruptly.
				if h, ok := w.(http.Hijacker); ok {
					conn, _, _ := h.Hijack()
					conn.Close()
				}
			},
			wantStream: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()

			c := &Client{
				BaseURL: srv.URL,
				Model:   "ilm",
				ChatID:  "test",
				HTTP:    http.DefaultClient,
			}
			_, err := c.Stream(t.Context(), []Message{{Role: "user", Content: strPtr("hi")}}, nil, nil, nil)

			switch {
			case tc.wantFatal:
				if !errors.Is(err, ErrBackendFatal) {
					t.Errorf("want ErrBackendFatal, got: %v", err)
				}
				if errors.Is(err, ErrBackendStream) {
					t.Errorf("fatal error must NOT match ErrBackendStream")
				}
			case tc.wantStream:
				if !errors.Is(err, ErrBackendStream) {
					t.Errorf("want ErrBackendStream, got: %v", err)
				}
				if errors.Is(err, ErrBackendFatal) {
					t.Errorf("stream error must NOT match ErrBackendFatal")
				}
			default:
				if err != nil {
					t.Errorf("want nil error for 200 OK, got: %v", err)
				}
			}
		})
	}
}

// TestFatalErrMessageContainsStatus verifies the 4xx error message includes
// the HTTP status and body excerpt so the user can diagnose it.
func TestFatalErrMessageContainsStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "invalid api key", http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Model: "ilm", ChatID: "test", HTTP: http.DefaultClient}
	_, err := c.Stream(t.Context(), []Message{{Role: "user", Content: strPtr("hi")}}, nil, nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "401") {
		t.Errorf("error message should contain status code 401; got %q", msg)
	}
	if !strings.Contains(msg, "invalid api key") {
		t.Errorf("error message should contain body excerpt; got %q", msg)
	}
}

func strPtr(s string) *string { return &s }
