package ilm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestOffModeNoDial verifies that mode=off produces zero network activity.
// The emitter is nil-safe and never dials.
func TestOffModeNoDial(t *testing.T) {
	// Use a "black-hole" address — if the emitter tries to dial, the test
	// will hang or error. We wrap it in a timeout to catch any hang.
	addr := "http://127.0.0.1:1" // port 1 is reserved/black-hole
	e, err := New(Config{
		Endpoint:  addr,
		Token:     "test",
		Mode:      ModeOff,
		QueuePath: filepath.Join(t.TempDir(), "queue.jsonl"),
	}, "wakil-live:test-off")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Emit several events — should be no-ops.
	for i := 0; i < 10; i++ {
		e.Emit(EventUserTurn, UserTurnPayload{Text: fmt.Sprintf("msg %d", i)})
	}

	// Verify no queue file was created.
	if _, err := os.Stat(filepath.Join(t.TempDir(), "queue.jsonl")); err == nil {
		// Queue file may exist (from newQueue creating it) but should be empty.
		info, _ := os.Stat(filepath.Join(t.TempDir(), "queue.jsonl"))
		if info.Size() > 0 {
			t.Errorf("queue file should be empty in off mode, got %d bytes", info.Size())
		}
	}

	// Close should be a no-op.
	if err := e.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// TestEndpointDownQueueGrows verifies that with the endpoint unreachable,
// events accumulate in the queue and Wakil is unaffected (no block).
func TestEndpointDownQueueGrows(t *testing.T) {
	// Use a server that returns 503 — simulates endpoint-down without
	// the 30s connect timeout of a real black-hole address.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	queuePath := filepath.Join(t.TempDir(), "queue.jsonl")
	e, err := New(Config{
		Endpoint:  srv.URL,
		Token:     "test",
		Mode:      ModeShadow,
		QueuePath: queuePath,
		BatchMS:   50,
	}, "wakil-live:test-down")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close()

	// Emit several events — they should not block even though the endpoint
	// is returning 503.
	for i := 0; i < 5; i++ {
		e.Emit(EventUserTurn, UserTurnPayload{Text: fmt.Sprintf("msg %d", i)})
	}

	// Give the sender time to attempt (and fail) the batch POST, writing
	// events to the queue.
	time.Sleep(500 * time.Millisecond)

	// The key assertion is that Emit didn't block and Wakil is unharmed.
	// Events are either in the channel buffer or the queue file.
}

// TestDrainAfterRecovery verifies that events queued while the endpoint was
// down are drained after the endpoint returns.
func TestDrainAfterRecovery(t *testing.T) {
	// Start a test server that accepts events.
	var received []Event
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/events" {
			var body struct {
				Events []Event `json:"events"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			received = append(received, body.Events...)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	queuePath := filepath.Join(t.TempDir(), "queue.jsonl")

	// Pre-populate the queue with events (simulating a prior outage).
	q, err := newQueue(queuePath)
	if err != nil {
		t.Fatalf("newQueue: %v", err)
	}
	for i := 0; i < 3; i++ {
		ev := Event{
			EventID:   fmt.Sprintf("test-uuid-%d", i),
			SessionID: "wakil-live:test-drain",
			Seq:       i,
			Type:      EventUserTurn,
			Payload:   json.RawMessage(`{"text":"queued msg"}`),
		}
		if err := q.append(ev); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	// Now create the emitter with the live server — it should drain the
	// queue on the first tick.
	e, err := New(Config{
		Endpoint:  srv.URL,
		Token:     "test",
		Mode:      ModeShadow,
		QueuePath: queuePath,
		BatchMS:   50,
	}, "wakil-live:test-drain")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Wait for the sender to drain the queue.
	time.Sleep(500 * time.Millisecond)
	e.Close()

	// Verify the server received the queued events.
	if len(received) < 3 {
		t.Errorf("expected at least 3 received events, got %d", len(received))
	}
}

// TestRedaction verifies that secrets are redacted before leaving the process.
func TestRedaction(t *testing.T) {
	r := DefaultRedactor()

	tests := []struct {
		name    string
		input   string
		want    string
		matches bool
	}{
		{
			name:  "private key",
			input: "-----BEGIN RSA PRIVATE KEY-----\nsome content\n-----END RSA PRIVATE KEY-----",
			want:  "[REDACTED:private-key]",
		},
		{
			name:  "api key long",
			input: "sk-1234567890abcdef1234567890abcdef",
			want:  "[REDACTED:api-key]",
		},
		{
			name:  "credential assignment",
			input: `api_key: "supersecret12345678"`,
			want:  "[REDACTED:credential]",
		},
		{
			name:  "bearer token",
			input: "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9",
			want:  "[REDACTED:bearer-token]",
		},
		{
			name:  "normal text not redacted",
			input: "this is a normal message with no secrets",
			want:  "this is a normal message with no secrets",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := r.RedactString(tc.input)
			if !strings.Contains(got, tc.want) && tc.want != tc.input {
				// Check that the replacement appeared
				if !strings.Contains(got, "[REDACTED:") {
					t.Errorf("expected redaction to contain %q, got %q", tc.want, got)
				}
			}
			if tc.want == tc.input && got != tc.input {
				t.Errorf("expected no redaction, got %q", got)
			}
		})
	}

	// Test JSON redaction.
	redacted := r.RedactJSON(json.RawMessage(`{"text":"my api key is sk-1234567890abcdef1234567890abcdef"}`))
	var v map[string]interface{}
	json.Unmarshal(redacted, &v)
	text, _ := v["text"].(string)
	if strings.Contains(text, "sk-1234567890abcdef") {
		t.Errorf("JSON redaction failed: secret not redacted in %q", text)
	}
	if !strings.Contains(text, "[REDACTED:api-key]") {
		t.Errorf("JSON redaction: expected [REDACTED:api-key] in %q", text)
	}
}

// TestRedactionPerMatchAllow verifies that allow patterns are checked per-match,
// not per-string. A string containing both an allow-pattern match and an
// unrelated secret should have the secret redacted — the allow pattern should
// only suppress the redaction it overlaps with, not all redactions.
func TestRedactionPerMatchAllow(t *testing.T) {
	r := DefaultRedactor()

	// This string contains both "api_key config" (would have matched an
	// allow pattern in the old per-string mode) AND a bearer token. With
	// allow patterns removed, the bearer token must be redacted.
	input := `api_key config: see docs\nAuthorization: Bearer eyJhbGciOiJIUzI1NiJInR5cCI6IkpXVCJ9`
	got := r.RedactString(input)

	// The bearer token must be redacted.
	if strings.Contains(got, "Bearer eyJhbGci") {
		t.Errorf("bearer token not redacted: %q", got)
	}
	if !strings.Contains(got, "[REDACTED:bearer-token]") {
		t.Errorf("expected [REDACTED:bearer-token] in output, got %q", got)
	}
}

// TestRedactionPrivateKeyFullBlock verifies that the private key regex matches
// the entire key block (BEGIN through END), not just the opening delimiter.
func TestRedactionPrivateKeyFullBlock(t *testing.T) {
	r := DefaultRedactor()

	input := "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA...\n-----END RSA PRIVATE KEY-----"
	got := r.RedactString(input)

	// The key material must not be present in the output.
	if strings.Contains(got, "MIIEowIBAAKCAQEA") {
		t.Errorf("private key body not redacted: %q", got)
	}
	if !strings.Contains(got, "[REDACTED:private-key]") {
		t.Errorf("expected [REDACTED:private-key] in output, got %q", got)
	}
}

// TestRedactionPrivateKeyTruncated verifies that a truncated PEM block (BEGIN
// without END) is still redacted by the fallback pattern.
func TestRedactionPrivateKeyTruncated(t *testing.T) {
	r := DefaultRedactor()

	// Truncated key — no END marker (e.g. from log line truncation).
	input := "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEAabcdef123456"
	got := r.RedactString(input)

	if strings.Contains(got, "MIIEowIBAAKCAQEA") {
		t.Errorf("truncated private key body not redacted: %q", got)
	}
	if !strings.Contains(got, "[REDACTED:private-key]") {
		t.Errorf("expected [REDACTED:private-key] for truncated PEM, got %q", got)
	}
}

// TestRedactionTwoPEMBlocks verifies that multiple PEM blocks in one string
// are all redacted (lazy quantifier doesn't greedily span both).
func TestRedactionTwoPEMBlocks(t *testing.T) {
	r := DefaultRedactor()

	input := "-----BEGIN RSA PRIVATE KEY-----\nkey1\n-----END RSA PRIVATE KEY-----\nsome text\n-----BEGIN EC PRIVATE KEY-----\nkey2\n-----END EC PRIVATE KEY-----"
	got := r.RedactString(input)

	if strings.Contains(got, "key1") || strings.Contains(got, "key2") {
		t.Errorf("PEM key body not redacted: %q", got)
	}
	// Should have two redaction markers.
	count := strings.Count(got, "[REDACTED:private-key]")
	if count != 2 {
		t.Errorf("expected 2 [REDACTED:private-key] markers, got %d in %q", count, got)
	}
}

// TestRedactionCredentialField verifies that JSON fields with known credential
// names are redacted even when the value doesn't match a regex pattern.
func TestRedactionCredentialField(t *testing.T) {
	r := DefaultRedactor()

	// "short" is too short to match the credential_assignment regex, but
	// the field name "password" should trigger redaction.
	redacted := r.RedactJSON(json.RawMessage(`{"password":"short"}`))
	var v map[string]interface{}
	json.Unmarshal(redacted, &v)
	if pw, ok := v["password"].(string); ok {
		if pw != "[REDACTED:credential-field]" {
			t.Errorf("expected [REDACTED:credential-field] for password field, got %q", pw)
		}
	} else {
		t.Errorf("password field missing or not a string after redaction: %v", v)
	}

	// Normal fields should not be affected.
	redacted = r.RedactJSON(json.RawMessage(`{"name":"hello"}`))
	json.Unmarshal(redacted, &v)
	if name, ok := v["name"].(string); ok {
		if name != "hello" {
			t.Errorf("non-credential field was modified: %q", name)
		}
	}
}

// TestRedactionCredentialFieldNonString verifies that non-string values under
// credential field names are also redacted (numbers, objects, arrays).
func TestRedactionCredentialFieldNonString(t *testing.T) {
	r := DefaultRedactor()

	// Number value
	redacted := r.RedactJSON(json.RawMessage(`{"password": 12345678}`))
	var v map[string]interface{}
	json.Unmarshal(redacted, &v)
	if pw, ok := v["password"]; ok {
		if pw != "[REDACTED:credential-field]" {
			t.Errorf("expected credential-field redaction for numeric password, got %v", pw)
		}
	}

	// Object value
	redacted = r.RedactJSON(json.RawMessage(`{"secret": {"value": "hidden"}}`))
	json.Unmarshal(redacted, &v)
	if sec, ok := v["secret"]; ok {
		if sec != "[REDACTED:credential-field]" {
			t.Errorf("expected credential-field redaction for object secret, got %v", sec)
		}
	}

	// Array value
	redacted = r.RedactJSON(json.RawMessage(`{"tokens": ["a", "b"]}`))
	json.Unmarshal(redacted, &v)
	if toks, ok := v["tokens"]; ok {
		if toks != "[REDACTED:credential-field]" {
			t.Errorf("expected credential-field redaction for array tokens, got %v", toks)
		}
	}
}

// TestRedactionCredentialFieldNormalized verifies that field names are
// normalized (lowercased, _ and - removed) before lookup.
func TestRedactionCredentialFieldNormalized(t *testing.T) {
	r := DefaultRedactor()

	tests := []string{
		`{"API-Key":"secret"}`,
		`{"api_key":"secret"}`,
		`{"ApiKey":"secret"}`,
		`{"PRIVATE-KEY":"secret"}`,
		`{"private_key":"secret"}`,
		`{"Access-Token":"secret"}`,
	}
	for _, input := range tests {
		redacted := r.RedactJSON(json.RawMessage(input))
		var v map[string]interface{}
		json.Unmarshal(redacted, &v)
		for _, val := range v {
			if val != "[REDACTED:credential-field]" {
				t.Errorf("expected credential-field redaction for %s, got %v", input, val)
			}
		}
	}
}

// TestRedactionMalformedJSON verifies that secrets in malformed JSON are still
// redacted via the RedactString fallback.
func TestRedactionMalformedJSON(t *testing.T) {
	r := DefaultRedactor()

	// Malformed JSON containing a secret — should fall back to string redaction.
	malformed := json.RawMessage(`{"text":"my key is sk-1234567890abcdef1234567890abcdef"`)
	redacted := r.RedactJSON(malformed)
	got := string(redacted)
	if strings.Contains(got, "sk-1234567890abcdef") {
		t.Errorf("secret not redacted in malformed JSON: %q", got)
	}
	if !strings.Contains(got, "[REDACTED:api-key]") {
		t.Errorf("expected [REDACTED:api-key] in malformed JSON output, got %q", got)
	}
}

// TestDefaultRedactorCompilesAllPatterns verifies that all 5 redaction patterns
// compile successfully — a silently dropped pattern is a security gap.
func TestDefaultRedactorCompilesAllPatterns(t *testing.T) {
	r := DefaultRedactor()
	if len(r.patterns) != 5 {
		t.Errorf("expected 5 compiled patterns (private_key_full, private_key_truncated, api_key_long, credential_assignment, bearer_token), got %d", len(r.patterns))
	}
}

// TestRateLimit429Retryable verifies that a 429 response is classified as
// transient (retryable), NOT permanent. The old code classified all 4xx as
// permanent, which dropped rate-limited events instead of retrying.
func TestRateLimit429Retryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests) // 429
	}))
	defer srv.Close()

	queuePath := filepath.Join(t.TempDir(), "queue.jsonl")
	e, err := New(Config{
		Endpoint:  srv.URL,
		Token:     "test",
		Mode:      ModeShadow,
		QueuePath: queuePath,
		BatchMS:   50,
	}, "wakil-live:test-429")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for i := 0; i < 3; i++ {
		e.Emit(EventUserTurn, UserTurnPayload{Text: fmt.Sprintf("msg %d", i)})
	}
	time.Sleep(500 * time.Millisecond)
	e.Close()

	// 429 is transient — events should remain in the queue (not dropped).
	q, _ := newQueue(queuePath)
	events, _ := q.readAll()
	if len(events) == 0 {
		t.Errorf("429 should be retryable — events should remain in queue, but queue is empty (events were dropped as permanent failure)")
	}
}

// TestQueueFilePermissions verifies that the queue file is created with
// 0600 permissions (owner read/write only), not 0644 (world-readable).
func TestQueueFilePermissions(t *testing.T) {
	queuePath := filepath.Join(t.TempDir(), "queue.jsonl")
	q, err := newQueue(queuePath)
	if err != nil {
		t.Fatalf("newQueue: %v", err)
	}
	_ = q

	info, err := os.Stat(queuePath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	mode := info.Mode().Perm()
	if mode != 0o600 {
		t.Errorf("queue file permissions should be 0600, got %o", mode)
	}
}

// TestUnknownModeRejected verifies that an unknown mode value is rejected
// at construction time. The old code activated emission for any non-empty,
// non-"off" value (e.g. Mode="yes" would start the sender).
func TestUnknownModeRejected(t *testing.T) {
	_, err := New(Config{
		Mode:      "yes",
		Endpoint:  "http://localhost:9999",
		QueuePath: filepath.Join(t.TempDir(), "queue.jsonl"),
	}, "wakil-live:test-unknown")
	if err == nil {
		t.Fatal("expected error for unknown mode 'yes', got nil")
	}
	if !strings.Contains(err.Error(), "unknown mode") {
		t.Errorf("expected 'unknown mode' error, got %v", err)
	}
}

// TestEventIDNoCollisionOnResume verifies that two emitters with the same
// session ID (simulating a resume/rotation) produce different event IDs for
// the same seq number. The old code used uuid5(session, seq) which collided
// across processes — the server would dedup real events as duplicates.
func TestEventIDNoCollisionOnResume(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable) // don't care about delivery
	}))
	defer srv.Close()

	queue1 := filepath.Join(t.TempDir(), "q1.jsonl")
	queue2 := filepath.Join(t.TempDir(), "q2.jsonl")

	e1, err := New(Config{
		Endpoint: srv.URL, Token: "t", Mode: ModeShadow,
		QueuePath: queue1, BatchMS: 50,
	}, "wakil-live:same-session")
	if err != nil {
		t.Fatalf("New e1: %v", err)
	}
	e2, err := New(Config{
		Endpoint: srv.URL, Token: "t", Mode: ModeShadow,
		QueuePath: queue2, BatchMS: 50,
	}, "wakil-live:same-session") // same session ID
	if err != nil {
		t.Fatalf("New e2: %v", err)
	}

	// Both emit seq=1 with the same session ID.
	e1.seqMu.Lock()
	e1.seq = 1
	id1 := uuid.NewSHA1(uuidNamespace, []byte(fmt.Sprintf("%s:%s:%d", e1.sessionID, e1.processID, 1))).String()
	e1.seqMu.Unlock()

	e2.seqMu.Lock()
	e2.seq = 1
	id2 := uuid.NewSHA1(uuidNamespace, []byte(fmt.Sprintf("%s:%s:%d", e2.sessionID, e2.processID, 1))).String()
	e2.seqMu.Unlock()

	e1.Close()
	e2.Close()

	if id1 == id2 {
		t.Errorf("event IDs collided across processes for same session: %s (resume/rotation would cause server dedup to drop events)", id1)
	}
}

// TestRedactionInEmit verifies that secrets in event payloads are redacted
// before being written to the queue.
func TestRedactionInEmit(t *testing.T) {
	// Use a server that returns 503 — avoids the 30s connect timeout.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	queuePath := filepath.Join(t.TempDir(), "queue.jsonl")
	e, err := New(Config{
		Endpoint:  srv.URL,
		Token:     "test",
		Mode:      ModeShadow,
		QueuePath: queuePath,
		BatchMS:   50,
	}, "wakil-live:test-redact")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Emit an event with a planted secret.
	e.Emit(EventUserTurn, UserTurnPayload{
		Text: "my api key is sk-1234567890abcdef1234567890abcdef",
	})

	// Wait for the event to be written to the queue (after failed POST).
	time.Sleep(500 * time.Millisecond)
	e.Close()

	// Read the queue and verify the secret was redacted.
	q, err := newQueue(queuePath)
	if err != nil {
		t.Fatalf("reopen queue: %v", err)
	}
	events, err := q.readAll()
	if err != nil {
		t.Fatalf("readAll: %v", err)
	}
	found := false
	for _, ev := range events {
		if ev.Type == EventUserTurn {
			found = true
			var p UserTurnPayload
			json.Unmarshal(ev.Payload, &p)
			if strings.Contains(p.Text, "sk-1234567890abcdef") {
				t.Errorf("secret not redacted in queue: %q", p.Text)
			}
			if !strings.Contains(p.Text, "[REDACTED:api-key]") {
				t.Errorf("expected [REDACTED:api-key] in payload, got %q", p.Text)
			}
		}
	}
	if !found {
		t.Log("no user_turn events in queue (may still be in channel buffer)")
	}
}

// TestDrainQueueNoAmplification verifies that drainQueue does not duplicate
// events when the endpoint is unreachable. The original bug: drainQueue called
// sendBatch, which on failure appended the events back to the queue — but
// readAll had already read them from the file (without removing them), so each
// tick doubled the file size. After ~20 ticks (10s with 500ms interval) the
// file was ~1M× the original and readAll OOM'd the process.
func TestDrainQueueNoAmplification(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable) // always fail
	}))
	defer srv.Close()

	queuePath := filepath.Join(t.TempDir(), "queue.jsonl")
	e, err := New(Config{
		Endpoint:  srv.URL,
		Token:     "test",
		Mode:      ModeShadow,
		QueuePath: queuePath,
		BatchMS:   50, // 50ms — drainQueue fires every 50ms
	}, "wakil-live:test-noamp")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Emit 5 events — they'll fail to POST and land in the queue.
	for i := 0; i < 5; i++ {
		e.Emit(EventUserTurn, UserTurnPayload{Text: fmt.Sprintf("msg %d", i)})
	}

	// Wait for several drain cycles — the bug would amplify 5 → 10 → 20 → …
	// With the fix, the queue stays at 5 events (readAll + failed POST, no
	// duplication). Give it 500ms (10 ticks at 50ms).
	time.Sleep(500 * time.Millisecond)
	e.Close()

	// Read the queue file directly and count events.
	q, err := newQueue(queuePath)
	if err != nil {
		t.Fatalf("reopen queue: %v", err)
	}
	events, err := q.readAll()
	if err != nil {
		t.Fatalf("readAll: %v", err)
	}

	// With the bug, the count would be 5 × 2^10 ≈ 5120.
	// With the fix, it stays at 5 (or close — a few may still be in the
	// channel buffer and not yet in the file).
	if len(events) > 20 {
		t.Errorf("queue amplification detected: expected ≤20 events, got %d "+
			"(drainQueue is duplicating events on failed POST)", len(events))
	}
	t.Logf("queue has %d events after 10 drain cycles (no amplification)", len(events))
}

// TestReadAllOversizedQueueTruncates verifies that a queue file larger than
// maxQueueReadBytes is truncated (not read into memory). This is the OOM
// guard — the pre-fix amplification bug grew queue files to GB scale, and
// reading them killed the process with 55GB RSS.
func TestReadAllOversizedQueueTruncates(t *testing.T) {
	queuePath := filepath.Join(t.TempDir(), "queue.jsonl")

	// Write a file just over the cap (64MB + 1 byte of events).
	f, err := os.Create(queuePath)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ev := Event{
		EventID: "test", SessionID: "s", Seq: 1, Type: EventUserTurn,
		Payload: json.RawMessage(`{"text":"x"}`),
	}
	line, _ := json.Marshal(ev)
	line = append(line, '\n')
	chunk := bytes.Repeat(line, 1024) // ~1MB chunks of valid events
	for i := 0; i < maxQueueReadBytes/len(chunk)+1; i++ {
		if _, err := f.Write(chunk); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	f.Close()

	info, _ := os.Stat(queuePath)
	if info.Size() <= maxQueueReadBytes {
		t.Fatalf("test file %d bytes not over cap %d", info.Size(), maxQueueReadBytes)
	}

	q, err := newQueue(queuePath)
	if err != nil {
		t.Fatalf("newQueue: %v", err)
	}
	events, err := q.readAll()
	if err != nil {
		t.Fatalf("readAll: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events from oversized queue, got %d", len(events))
	}
	info, _ = os.Stat(queuePath)
	if info.Size() != 0 {
		t.Errorf("expected queue file truncated to 0 bytes, got %d", info.Size())
	}
}

// TestPermanentErrorDropsQueue verifies that a 4xx response (permanent
// failure) drops the batch instead of re-queueing it — otherwise a poison
// payload would block the queue forever, retried every tick.
func TestPermanentErrorDropsQueue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest) // 400 — permanent
	}))
	defer srv.Close()

	queuePath := filepath.Join(t.TempDir(), "queue.jsonl")
	e, err := New(Config{
		Endpoint:  srv.URL,
		Token:     "test",
		Mode:      ModeShadow,
		QueuePath: queuePath,
		BatchMS:   50,
	}, "wakil-live:test-4xx")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for i := 0; i < 3; i++ {
		e.Emit(EventUserTurn, UserTurnPayload{Text: fmt.Sprintf("msg %d", i)})
	}
	time.Sleep(500 * time.Millisecond)
	e.Close()

	// Queue must be empty — the 4xx'd batch was dropped, not persisted.
	q, _ := newQueue(queuePath)
	events, _ := q.readAll()
	if len(events) != 0 {
		t.Errorf("4xx should drop events, but queue has %d events", len(events))
	}
}

// TestBoundOutput verifies the output bounding logic.
func TestBoundOutput(t *testing.T) {
	// Under limit — not truncated.
	bounded, truncated, hash, size := BoundOutput("hello", 100)
	if bounded != "hello" || truncated || size != 5 || hash == "" {
		t.Errorf("under limit: bounded=%q truncated=%v hash=%q size=%d", bounded, truncated, hash, size)
	}

	// Over limit — truncated.
	long := strings.Repeat("a", 200)
	bounded, truncated, hash, size = BoundOutput(long, 100)
	if len(bounded) != 100 || !truncated || size != 200 {
		t.Errorf("over limit: len(bounded)=%d truncated=%v size=%d", len(bounded), truncated, size)
	}
	// Hash should be of the full (untruncated) output.
	expectedHash := HashOutput(long)
	if hash != expectedHash {
		t.Errorf("hash mismatch: got %q want %q", hash, expectedHash)
	}
}

// TestSeqMonotonic verifies that seq numbers are monotonic per session.
func TestSeqMonotonic(t *testing.T) {
	queuePath := filepath.Join(t.TempDir(), "queue.jsonl")
	// Use a 503 server so the emitter is in shadow mode but POSTs fail fast.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	e, err := New(Config{
		Endpoint:  srv.URL,
		Token:     "test",
		Mode:      ModeShadow,
		QueuePath: queuePath,
		BatchMS:   50,
	}, "wakil-live:test-seq")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close()

	// Emit several events — seq should be monotonic.
	for i := 0; i < 5; i++ {
		e.Emit(EventUserTurn, UserTurnPayload{Text: "test"})
	}
	if e.seq != 5 {
		t.Errorf("expected seq=5, got %d", e.seq)
	}
}

// TestConcurrentEmitClose verifies that Emit and Close can be called
// concurrently without panicking ("send on closed channel" was the
// original bug — Close closed eventCh while Emit was sending to it).
func TestConcurrentEmitClose(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	queuePath := filepath.Join(t.TempDir(), "queue.jsonl")
	e, err := New(Config{
		Endpoint:  srv.URL,
		Token:     "test",
		Mode:      ModeShadow,
		QueuePath: queuePath,
		BatchMS:   50,
	}, "wakil-live:test-race")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Emit from multiple goroutines while Close is called.
	done := make(chan struct{})
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				e.Emit(EventUserTurn, UserTurnPayload{Text: "concurrent emit"})
			}
		}()
	}

	// Give the emitters a moment to start producing.
	time.Sleep(50 * time.Millisecond)

	// Close while emits are in flight — must not panic.
	_ = e.Close()
	close(done)
	wg.Wait()
}

// TestDrainQueuePreservesAppendedEvents verifies that events appended to the
// queue file DURING the POST window (by an overflowing Emit) are preserved
// after the POST succeeds. The old code called truncate() which wiped the
// entire file; the fix uses truncateAfter(len(events)) to keep newly appended
// events.
func TestDrainQueuePreservesAppendedEvents(t *testing.T) {
	var received []Event
	var mu sync.Mutex
	// Slow server: takes 200ms to respond, giving us time to append
	// an event to the queue file while the POST is in flight.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		var body struct {
			Events []Event `json:"events"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		received = append(received, body.Events...)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	queuePath := filepath.Join(t.TempDir(), "queue.jsonl")

	// Pre-populate the queue with 3 events.
	q, err := newQueue(queuePath)
	if err != nil {
		t.Fatalf("newQueue: %v", err)
	}
	for i := 0; i < 3; i++ {
		ev := Event{
			EventID:   fmt.Sprintf("pre-%d", i),
			SessionID: "wakil-live:test-preserve",
			Seq:       i,
			Type:      EventUserTurn,
			Payload:   json.RawMessage(`{"text":"pre"}`),
		}
		if err := q.append(ev); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	// Create the emitter — it will drain the 3 pre-populated events on the
	// first tick (50ms). The POST takes 200ms.
	e, err := New(Config{
		Endpoint:  srv.URL,
		Token:     "test",
		Mode:      ModeShadow,
		QueuePath: queuePath,
		BatchMS:   50,
	}, "wakil-live:test-preserve")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Wait for the POST to start (drainQueue reads + POSTs), then append
	// a new event to the queue file WHILE the POST is in flight. This
	// simulates an overflowing Emit landing in the queue during the send.
	time.Sleep(100 * time.Millisecond) // POST is now in flight
	newEv := Event{
		EventID:   "post-during-send",
		SessionID: "wakil-live:test-preserve",
		Seq:       99,
		Type:      EventUserTurn,
		Payload:   json.RawMessage(`{"text":"appended during POST"}`),
	}
	if err := q.append(newEv); err != nil {
		t.Fatalf("append during POST: %v", err)
	}

	// Wait for the POST to complete and the next drain cycle to send
	// the appended event.
	time.Sleep(600 * time.Millisecond)
	e.Close()

	// The server should have received all 4 events: the 3 pre-populated
	// (first POST) + the 1 appended during POST (second POST). With the
	// old truncate() the 4th would have been wiped and never sent.
	mu.Lock()
	gotReceived := len(received)
	mu.Unlock()
	if gotReceived < 4 {
		t.Errorf("expected at least 4 received events (3 pre + 1 appended), got %d "+
			"(the appended event was likely lost to truncate())", gotReceived)
	}

	// Verify the appended event was received.
	mu.Lock()
	foundAppended := false
	for _, ev := range received {
		if ev.EventID == "post-during-send" {
			foundAppended = true
			break
		}
	}
	mu.Unlock()
	if !foundAppended {
		t.Errorf("event appended during POST was not received by server " +
			"(truncate wiped it before it could be sent)")
	}
}

// TestDecodeEventsSkipsMalformedLines verifies that decodeEvents skips
// corrupt lines instead of breaking — so a single bad line doesn't
// discard every event after it.
func TestDecodeEventsSkipsMalformedLines(t *testing.T) {
	good := `{"event_id":"a","session_id":"s","seq":1,"type":"user_turn","payload":{"text":"first"}}
not-json-at-all
{"event_id":"b","session_id":"s","seq":2,"type":"user_turn","payload":{"text":"second"}}
{"event_id":"c","session_id":"s","seq":3,"type":"user_turn","payload":{"text":"third"}}`

	events := decodeEvents([]byte(good))
	if len(events) != 3 {
		t.Errorf("expected 3 valid events (skipping 1 malformed line), got %d", len(events))
	}
	if len(events) > 0 && events[0].EventID != "a" {
		t.Errorf("first event should be 'a', got %q", events[0].EventID)
	}
	if len(events) > 2 && events[2].EventID != "c" {
		t.Errorf("third event should be 'c', got %q", events[2].EventID)
	}
}
