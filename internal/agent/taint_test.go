package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/memory"
	"github.com/treeol/wakil/internal/proxy"
)

// TestTaintCleanAgentStaysUnknownNoReasons verifies that a clean agent
// (no external content exposure) has no taint reasons and computeTainted
// returns TaintUnknown.
func TestTaintCleanAgentStaysUnknownNoReasons(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	if app.computeTainted() != memory.TaintUnknown {
		t.Error("clean agent should be TaintUnknown")
	}
	if app.touchedExternal.Load() {
		t.Error("clean agent should not have touchedExternal set")
	}
	app.taintMu.Lock()
	n := len(app.taintReasons)
	app.taintMu.Unlock()
	if n != 0 {
		t.Errorf("clean agent should have 0 taint reasons, got %d", n)
	}
}

// TestTaintReasonRecordedOnExternalGrounding verifies that addExternalGrounding
// records a taint reason when it latches touchedExternal.
func TestTaintReasonRecordedOnExternalGrounding(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.addExternalGrounding(proxy.GroundingEntry{Type: "web", Label: "https://example.com"})

	if !app.touchedExternal.Load() {
		t.Fatal("touchedExternal should be set by addExternalGrounding")
	}
	if app.computeTainted() != memory.TaintTrue {
		t.Fatal("computeTainted should return TaintTrue")
	}
	app.taintMu.Lock()
	defer app.taintMu.Unlock()
	if len(app.taintReasons) != 1 {
		t.Fatalf("should have 1 taint reason, got %d", len(app.taintReasons))
	}
	r := app.taintReasons[0]
	if r.Source != "web" || r.Label != "https://example.com" {
		t.Errorf("reason = {Source: %q, Label: %q}, want web/https://example.com", r.Source, r.Label)
	}
	if r.When.IsZero() {
		t.Error("reason When should be set")
	}
}

// TestTaintReasonDedup verifies that repeated exposures with the same
// (Source, Label) pair are deduplicated — the slice shows distinct exposures,
// not every call.
func TestTaintReasonDedup(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	for i := 0; i < 5; i++ {
		app.addExternalGrounding(proxy.GroundingEntry{Type: "web", Label: "https://example.com"})
	}
	app.addExternalGrounding(proxy.GroundingEntry{Type: "oracle", Label: "claude-opus-4"})

	app.taintMu.Lock()
	defer app.taintMu.Unlock()
	if len(app.taintReasons) != 2 {
		t.Errorf("dedup: should have 2 reasons (web + oracle), got %d", len(app.taintReasons))
	}
}

// TestTaintReasonBounded verifies that the taintReasons slice is bounded
// at maxTaintReasons — on overflow, the oldest entry is dropped.
func TestTaintReasonBounded(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	for i := 0; i < maxTaintReasons+10; i++ {
		app.addExternalGrounding(proxy.GroundingEntry{Type: "web", Label: "https://example.com/" + string(rune('a'+i%26)) + string(rune('0'+i%10))})
	}
	app.taintMu.Lock()
	defer app.taintMu.Unlock()
	if len(app.taintReasons) > maxTaintReasons {
		t.Errorf("taintReasons should be bounded at %d, got %d", maxTaintReasons, len(app.taintReasons))
	}
}

// TestRenderTaintStateNotTainted verifies the /taint output for a clean
// agent says "no known external exposure" (NOT "not tainted").
func TestRenderTaintStateNotTainted(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	out := app.renderTaintState()
	if strings.Contains(out, "not tainted") {
		t.Errorf("clean state should not say 'not tainted': %q", out)
	}
	if !strings.Contains(out, "no known external exposure") {
		t.Errorf("clean state should say 'no known external exposure': %q", out)
	}
}

// TestRenderTaintStateTainted verifies the /taint output for a tainted
// agent shows the exposure count and the reasons.
func TestRenderTaintStateTainted(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.addExternalGrounding(proxy.GroundingEntry{Type: "web", Label: "https://example.com"})
	app.addExternalGrounding(proxy.GroundingEntry{Type: "oracle", Label: "claude-opus-4"})

	out := app.renderTaintState()
	if !strings.Contains(out, "tainted (2 exposures)") {
		t.Errorf("tainted state should show count: %q", out)
	}
	if !strings.Contains(out, "web: https://example.com") {
		t.Errorf("tainted state should show web reason: %q", out)
	}
	if !strings.Contains(out, "oracle: claude-opus-4") {
		t.Errorf("tainted state should show oracle reason: %q", out)
	}
}

// TestTaintReasonTimestamp verifies that the /taint output includes a
// human-readable timestamp for each exposure.
func TestTaintReasonTimestamp(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.addExternalGrounding(proxy.GroundingEntry{Type: "web", Label: "https://example.com"})

	out := app.renderTaintState()
	// The timestamp format is "2006-01-02 15:04".
	if !strings.Contains(out, time.Now().Format("2006-01-02")) {
		t.Errorf("tainted state should include today's date: %q", out)
	}
}
