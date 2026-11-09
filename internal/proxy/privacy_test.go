package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// privacyBody is the body OpenRouter sent in the observed failure.
const privacyBody = `{"error":{"message":"0 endpoints out of 1 requested are available matching your guardrail restrictions and data policy.","code":404,"metadata":{"ineligibility_reasons":[{"provider_name":"Chutes","reason":"free-model-training-violation"}]}}}`

func TestIsPrivacyPolicyError(t *testing.T) {
	if !IsPrivacyPolicyError(404, privacyBody) {
		t.Error("privacy 404 not recognised")
	}
	if IsPrivacyPolicyError(404, `{"error":"no such model"}`) || IsPrivacyPolicyError(500, privacyBody) {
		t.Error("only the privacy 404 may match")
	}
}

// TestStreamPrivacy404ShowsOneLine runs the real request path against a fake
// server and checks that the error is the one-line message (still fatal), not
// the raw JSON.
func TestStreamPrivacy404ShowsOneLine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(privacyBody))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Kind: KindOpenAI, ConfiguredModel: "x:free", Model: "x:free", HTTP: srv.Client()}
	_, err := c.Stream(context.Background(), []Message{{Role: "user", Content: strPtr("hi")}}, nil, nil, nil)
	if err == nil {
		t.Fatal("want an error")
	}
	if !errors.Is(err, ErrBackendFatal) {
		t.Errorf("must stay a fatal (non-retried) error: %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "https://openrouter.ai/settings/privacy") || strings.Contains(msg, "ineligibility") {
		t.Errorf("want the one-line message with the URL, got %q", msg)
	}
}
