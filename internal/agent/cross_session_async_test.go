package agent

// Tests for card #129: cross-session async delivery/grounding. When the
// conversation is rotated (/new, /resume, handoff) while an async op is in
// flight, a result completing after rotation must be delivered inline (tagged
// as prior-session) but must NOT commit oracle grounding into the NEW session.

import (
	"strings"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/proxy"
)

// enqueueTerminalOp registers a Mashūra-style op with the given origin chatID,
// terminalizes it (no worker — direct terminal+inbox), and returns the op.
// Mirrors what a completed async op looks like at drain time.
func enqueueTerminalOp(t *testing.T, a *App, originChatID string, okModels []string, result string) *asyncOp {
	t.Helper()
	op, reason := a.registerAsyncOp("mashura__review", "cross-session panel")
	if reason != "" {
		t.Fatalf("register: %s", reason)
	}
	op.mu.Lock()
	op.originChatID = originChatID
	op.okModels = okModels
	op.terminal = true
	op.finishedAt = time.Now()
	op.result = result
	op.uiJob = false
	op.mu.Unlock()
	a.asyncMu.Lock()
	a.asyncInbox = append(a.asyncInbox, op)
	a.asyncMu.Unlock()
	return op
}

// TestCrossSessionAsyncSuppressesGroundingAndTags verifies the card #129 fix:
// an op originating in a PRIOR session (delivered after /new) is delivered
// inline with a prior-session tag but its oracle grounding is NOT committed to
// the new session.
func TestCrossSessionAsyncSuppressesGroundingAndTags(t *testing.T) {
	a := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })

	// Rotate to "session-B" (the new, current conversation). The app's chatID()
	// now reports B.
	a.Session = &Session{ChatID: "session-B"}

	// This op originated in the (now-rotated) "session-A".
	op := enqueueTerminalOp(t, a, "session-A", []string{"m1-codex"}, "answer from old session")

	env := a.drainAsyncInbox()

	// Delivered inline (not quarantined) and tagged as prior-session.
	if !strings.Contains(env, "prior-session result") {
		t.Errorf("cross-session envelope missing prior-session tag: %q", env)
	}
	if !strings.Contains(env, "answer from old session") {
		t.Errorf("cross-session envelope missing the result body: %q", env)
	}

	// Grounding must NOT be committed into the new session (session-B client).
	if got := len(a.Client.Grounding()); got != 0 {
		t.Errorf("cross-session op committed %d grounding entries into the new session, want 0", got)
	}

	// Op marked delivered (exactly-once).
	if !op.deliveredSnapshot() {
		t.Error("op not marked delivered after drain")
	}
}

// TestInSessionAsyncStillGrounds verifies the fix does NOT regress the
// in-session case: an op originating in the CURRENT session is delivered AND
// its grounding committed as before.
func TestInSessionAsyncStillGrounds(t *testing.T) {
	a := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	a.Session = &Session{ChatID: "session-B"}

	op := enqueueTerminalOp(t, a, "session-B", []string{"m1-codex"}, "in-session answer")

	env := a.drainAsyncInbox()

	if !strings.Contains(env, "in-session answer") {
		t.Errorf("in-session envelope missing result: %q", env)
	}
	if strings.Contains(env, "prior-session result") {
		t.Errorf("in-session envelope should NOT be tagged prior-session: %q", env)
	}
	if got := len(a.Client.Grounding()); got != 1 {
		t.Errorf("in-session op committed %d grounding entries, want 1", got)
	}
	if !op.deliveredSnapshot() {
		t.Error("in-session op not marked delivered")
	}
}

// TestCrossSessionCheckPendingSuppressesGrounding verifies the same
// grounding-suppression applies on the check_pending(op-N) retrieval path,
// which also routes through commitAsyncEffects.
func TestCrossSessionCheckPendingSuppressesGrounding(t *testing.T) {
	a := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	a.Session = &Session{ChatID: "session-B"}

	// Register directly WITHOUT putting in the inbox — check_pending retrieves
	// by id from the registry.
	a.Cfg.OracleTimeoutSeconds = 0
	op, reason := a.registerAsyncOp("mashura__review", "cross check")
	if reason != "" {
		t.Fatalf("register: %s", reason)
	}
	op.mu.Lock()
	op.originChatID = "session-A"
	op.okModels = []string{"m1-codex"}
	op.terminal = true
	op.finishedAt = time.Now()
	op.result = "cross-session via check_pending"
	op.mu.Unlock()

	out := a.handleCheckPending(tcArgs("check_pending", `{"id":"`+op.id+`"}`))
	if !strings.Contains(out, "cross-session via check_pending") {
		t.Errorf("check_pending lost the result: %q", out)
	}
	if got := len(a.Client.Grounding()); got != 0 {
		t.Errorf("check_pending cross-session committed %d grounding entries, want 0", got)
	}
}

// TestInSessionCheckPendingStillGrounds verifies the fix does not regress
// in-session grounding on the check_pending retrieval path.
func TestInSessionCheckPendingStillGrounds(t *testing.T) {
	a := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	a.Session = &Session{ChatID: "session-B"}

	op, reason := a.registerAsyncOp("mashura__review", "in-session check")
	if reason != "" {
		t.Fatalf("register: %s", reason)
	}
	op.mu.Lock()
	op.originChatID = "session-B"
	op.okModels = []string{"m1-codex"}
	op.terminal = true
	op.finishedAt = time.Now()
	op.result = "in-session via check_pending"
	op.mu.Unlock()

	out := a.handleCheckPending(tcArgs("check_pending", `{"id":"`+op.id+`"}`))
	if !strings.Contains(out, "in-session via check_pending") {
		t.Errorf("check_pending lost the result: %q", out)
	}
	if got := len(a.Client.Grounding()); got != 1 {
		t.Errorf("in-session check_pending committed %d grounding entries, want 1", got)
	}
}

// TestEmptyOriginFallsBackToInSession verifies the empty-origin fallback: an op
// with an empty originChatID (legacy/constructed without registerAsyncOp) is
// treated as in-session — grounded and untagged, matching pre-fix behavior.
func TestEmptyOriginFallsBackToInSession(t *testing.T) {
	a := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	a.Session = &Session{ChatID: "session-B"}

	op := enqueueTerminalOp(t, a, "", []string{"m1-codex"}, "empty-origin answer")
	_ = op // not otherwise referenced beyond the drain envelope
	env := a.drainAsyncInbox()

	if strings.Contains(env, "prior-session result") {
		t.Errorf("empty-origin op should NOT be tagged prior-session: %q", env)
	}
	if got := len(a.Client.Grounding()); got != 1 {
		t.Errorf("empty-origin op committed %d grounding entries, want 1 (in-session fallback)", got)
	}
}

// TestMixedDrainDistinguishesSessions verifies a single drain batch correctly
// tags/suppresses the cross-session op while grounding the in-session op.
func TestMixedDrainDistinguishesSessions(t *testing.T) {
	a := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	a.Session = &Session{ChatID: "session-B"}

	cross := enqueueTerminalOp(t, a, "session-A", []string{"m-old"}, "old answer")
	in := enqueueTerminalOp(t, a, "session-B", []string{"m-new"}, "new answer")

	env := a.drainAsyncInbox()

	if !strings.Contains(env, "prior-session result") || !strings.Contains(env, "old answer") {
		t.Errorf("mixed drain missing cross-session tag/body: %q", env)
	}
	if !strings.Contains(env, "new answer") {
		t.Errorf("mixed drain missing in-session body: %q", env)
	}
	if got := len(a.Client.Grounding()); got != 1 {
		t.Errorf("mixed drain grounded %d entries, want 1 (only in-session)", got)
	}
	if !cross.deliveredSnapshot() || !in.deliveredSnapshot() {
		t.Error("both ops should be marked delivered")
	}
}

// TestCrossSessionSubagentGroundingSuppressed verifies the per-child grounding
// of a DISCOVERY-SUBAGENT batch op is also suppressed for cross-session ops
// (commitAsyncSubagentEffects), while completing normally otherwise.
func TestCrossSessionSubagentGroundingSuppressed(t *testing.T) {
	a := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	col := &collectEvents{}
	a.EventSink = col.sink
	a.Session = &Session{ChatID: "session-B"}

	op, reason := a.registerAsyncOp("dispatch_subagents", "cross subagent batch")
	if reason != "" {
		t.Fatalf("register: %s", reason)
	}
	op.mu.Lock()
	op.originChatID = "session-A"
	op.subagents = []asyncSubagentResult{
		{ChatID: "child-1", Grounding: []proxy.GroundingEntry{{Type: "web", Label: "foo.com"}}},
	}
	op.terminal = true
	op.finishedAt = time.Now()
	op.result = "subagent summary"
	op.mu.Unlock()

	a.asyncMu.Lock()
	a.asyncInbox = append(a.asyncInbox, op)
	a.asyncMu.Unlock()

	a.drainAsyncInbox()

	// Cross-session subagent grounding must not reach the new session.
	if got := len(a.Client.Grounding()); got != 0 {
		t.Errorf("cross-session subagent grounded %d entries into new session, want 0", got)
	}
	// But the taint signal is still set (external content delivered), and the
	// SubagentDoneMsg event still fires (per-child completion bookkeeping kept).
	if !a.touchedExternal {
		t.Error("cross-session subagent delivery should still set touchedExternal")
	}
	foundDone := false
	for _, e := range col.snapshot() {
		if m, ok := e.(SubagentDoneMsg); ok && m.ChatID == "child-1" {
			foundDone = true
		}
	}
	if !foundDone {
		t.Error("SubagentDoneMsg should still be emitted for cross-session batch")
	}
}
