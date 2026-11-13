package proxy

// usage_stamp_test.go pins the per-request usage publication that B1's
// failed-stream cost recording relies on: the stamp a caller captures before
// a Stream call only changes when that call actually published usage, so a
// policy refusal never attributes an earlier request's tokens to this one.
// RequestsSent is the cumulative dispatch count B5 uses; the stamp is the
// per-request discriminator B1 uses.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestUsageStampChangesOnlyOnPublish: a Stream that reaches its provisional
// publication moves the stamp and counts the dispatch; a request refused
// before that point (policy enforcement) moves neither.
func TestUsageStampChangesOnlyOnPublish(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sseOK(w, "ok")
	}))
	defer srv.Close()

	msgs := []Message{{Role: "user", Content: strPtr("hi")}}
	c := chatClient(t, srv, "off", "off")
	before := c.UsageStamp()
	if _, err := c.Stream(context.Background(), msgs, nil, nil, nil); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if c.UsageStamp() == before {
		t.Error("successful Stream must move the usage stamp")
	}
	if got := c.RequestsSent(); got != 1 {
		t.Errorf("RequestsSent = %d, want 1", got)
	}

	// Policy refusal: zdr "on" against a non-local host is refused before
	// any byte is sent, so the provisional estimate is never published.
	refused := &Client{BaseURL: "https://api.example.com/v1/chat/completions", HTTP: srv.Client(), ZDR: "on", Region: "off", Model: "m"}
	stamp := refused.UsageStamp()
	if _, err := refused.Stream(context.Background(), msgs, nil, nil, nil); err == nil {
		t.Fatal("policy refusal must error")
	}
	if refused.UsageStamp() != stamp {
		t.Error("policy-refused Stream must not move the usage stamp")
	}
	if got := refused.RequestsSent(); got != 0 {
		t.Errorf("RequestsSent after refusal = %d, want 0", got)
	}
}

// TestUsageStampDistinguishesConsecutiveRequests: each Stream call gets its
// own stamp, so a caller that captured the stamp before request N can tell
// N's usage apart from N-1's even when both succeed.
func TestUsageStampDistinguishesConsecutiveRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sseOK(w, "ok")
	}))
	defer srv.Close()

	msgs := []Message{{Role: "user", Content: strPtr("hi")}}
	c := chatClient(t, srv, "off", "off")
	first := c.UsageStamp()
	if _, err := c.Stream(context.Background(), msgs, nil, nil, nil); err != nil {
		t.Fatalf("first stream: %v", err)
	}
	afterFirst := c.UsageStamp()
	if afterFirst == first {
		t.Fatal("first Stream must move the stamp")
	}
	if _, err := c.Stream(context.Background(), msgs, nil, nil, nil); err != nil {
		t.Fatalf("second stream: %v", err)
	}
	if c.UsageStamp() == afterFirst {
		t.Error("second Stream must move the stamp again")
	}
	if got := c.RequestsSent(); got != 2 {
		t.Errorf("RequestsSent = %d, want 2", got)
	}
}
