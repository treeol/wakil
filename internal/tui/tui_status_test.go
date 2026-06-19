package tui

import (
	"strings"
	"testing"

)

// plain strips ANSI codes so we can check segment content as plain text.

func TestBuildStatusLineIdleInitial(t *testing.T) {
	// Brand new session: dot only, no "awaiting input".
	s := buildStatusLine(statusLineInput{state: stateIdle, hadTurn: false})
	p := plain(s)
	if !strings.Contains(p, "•") {
		t.Error("dot must always be present")
	}
	if strings.Contains(p, "awaiting") {
		t.Error("should not show 'awaiting input' before any turn")
	}
}

func TestBuildStatusLineIdleAfterTurn(t *testing.T) {
	s := buildStatusLine(statusLineInput{state: stateIdle, hadTurn: true})
	p := plain(s)
	if !strings.Contains(p, "awaiting input") {
		t.Errorf("should show 'awaiting input' after a completed turn; got: %q", p)
	}
}

func TestBuildStatusLineStreaming(t *testing.T) {
	s := buildStatusLine(statusLineInput{state: stateStreaming, tps: 45})
	p := plain(s)
	if !strings.Contains(p, "streaming") {
		t.Error("streaming state must say 'streaming'")
	}
	if !strings.Contains(p, "45") {
		t.Errorf("t/s rate must appear; got: %q", p)
	}
}

func TestBuildStatusLineReasoning(t *testing.T) {
	s := buildStatusLine(statusLineInput{state: stateStreaming, reasoning: true})
	p := plain(s)
	if !strings.Contains(p, "reasoning") {
		t.Errorf("reasoning mode must say 'reasoning'; got: %q", p)
	}
	if strings.Contains(p, "streaming") {
		t.Error("should not say 'streaming' while reasoning")
	}
}

func TestBuildStatusLineConfirm(t *testing.T) {
	s := buildStatusLine(statusLineInput{state: stateConfirm})
	p := plain(s)
	if !strings.Contains(p, "confirming") {
		t.Errorf("confirm state must say 'confirming'; got: %q", p)
	}
}

func TestBuildStatusLineAutoMode(t *testing.T) {
	s := buildStatusLine(statusLineInput{state: stateIdle, autoApprove: true, hadTurn: true})
	p := plain(s)
	if !strings.Contains(p, "AUTO") {
		t.Errorf("AUTO must appear when autoApprove; got: %q", p)
	}
	// AUTO must come before "awaiting input" (static modes before activity).
	autoIdx := strings.Index(p, "AUTO")
	awaitIdx := strings.Index(p, "awaiting")
	if autoIdx > awaitIdx {
		t.Errorf("AUTO should appear before 'awaiting input'; got: %q", p)
	}
}

func TestBuildStatusLineWorkflowLabel(t *testing.T) {
	s := buildStatusLine(statusLineInput{
		state:         stateStreaming,
		workflowLabel: "implement 3/6",
	})
	p := plain(s)
	if !strings.Contains(p, "plan implement 3/6") {
		t.Errorf("workflow label must appear as 'plan <label>'; got: %q", p)
	}
	// Workflow label (static) must appear before activity (streaming).
	planIdx := strings.Index(p, "plan")
	streamIdx := strings.Index(p, "streaming")
	if planIdx > streamIdx {
		t.Errorf("workflow label should come before activity; got: %q", p)
	}
}

func TestBuildStatusLineFullExample(t *testing.T) {
	// • AUTO · plan 3/6 · streaming · 45 t/s
	s := buildStatusLine(statusLineInput{
		state:         stateStreaming,
		autoApprove:   true,
		workflowLabel: "3/6",
		tps:           45,
	})
	p := plain(s)
	for _, want := range []string{"•", "AUTO", "plan 3/6", "streaming", "45"} {
		if !strings.Contains(p, want) {
			t.Errorf("full example missing %q; got: %q", want, p)
		}
	}
}

func TestBuildStatusLineTpsOnlyDuringStreaming(t *testing.T) {
	// t/s should not appear when idle even if a value is set.
	s := buildStatusLine(statusLineInput{state: stateIdle, tps: 45, hadTurn: true})
	p := plain(s)
	if strings.Contains(p, "45") {
		t.Errorf("t/s must not appear when idle; got: %q", p)
	}
}

func TestBuildStatusLineDotAlwaysPresent(t *testing.T) {
	for _, state := range []agentState{stateIdle, stateStreaming, stateConfirm, stateCompacting} {
		s := buildStatusLine(statusLineInput{state: state})
		if !strings.Contains(plain(s), "•") {
			t.Errorf("dot missing in state %d", state)
		}
	}
}

// --- Backend segment (P29) ---

func TestBuildStatusLineBackendOmittedWhenDefault(t *testing.T) {
	// When backendUsed == backendDefault, the backend is not shown (reduce noise).
	s := buildStatusLine(statusLineInput{
		state:          stateIdle,
		hadTurn:        true,
		backendUsed:    "local",
		backendDefault: "local",
	})
	p := plain(s)
	if strings.Contains(p, "local") {
		t.Errorf("default backend should be omitted from status line; got %q", p)
	}
	if !strings.Contains(p, "awaiting") {
		t.Error("awaiting input should still appear")
	}
}

func TestBuildStatusLineBackendShownWhenNonDefault(t *testing.T) {
	// backendUsed != backendDefault: show at normal brightness.
	s := buildStatusLine(statusLineInput{
		state:          stateIdle,
		hadTurn:        true,
		backendUsed:    "openrouter",
		backendDefault: "local",
	})
	p := plain(s)
	if !strings.Contains(p, "openrouter") {
		t.Errorf("non-default backend should appear in status line; got %q", p)
	}
}

func TestBuildStatusLineBackendOverrideMarker(t *testing.T) {
	// backendRequested != backendUsed: proxy overrode — show with "!" marker.
	s := buildStatusLine(statusLineInput{
		state:            stateIdle,
		hadTurn:          true,
		backendUsed:      "openrouter",
		backendRequested: "together",
		backendDefault:   "",
	})
	p := plain(s)
	if !strings.Contains(p, "openrouter!") {
		t.Errorf("override marker expected; got %q", p)
	}
}

func TestBuildStatusLineBackendOmittedWhenEmpty(t *testing.T) {
	// No backendUsed header: nothing backend-related shown.
	s := buildStatusLine(statusLineInput{
		state:   stateIdle,
		hadTurn: true,
	})
	p := plain(s)
	// Should not contain "!" or any backend label.
	if strings.Contains(p, "!") {
		t.Errorf("no backend header means no override marker; got %q", p)
	}
}

func TestBuildStatusLineBackendBeforeAuto(t *testing.T) {
	// Backend segment (static) must appear before AUTO.
	s := buildStatusLine(statusLineInput{
		state:          stateIdle,
		autoApprove:    true,
		hadTurn:        true,
		backendUsed:    "openrouter",
		backendDefault: "local",
	})
	p := plain(s)
	backendIdx := strings.Index(p, "openrouter")
	autoIdx := strings.Index(p, "AUTO")
	if backendIdx < 0 || autoIdx < 0 {
		t.Fatalf("both backend and AUTO must appear; got %q", p)
	}
	if backendIdx > autoIdx {
		t.Errorf("backend segment should come before AUTO; got %q", p)
	}
}

// TestLeadingAnsiCodes checks the helper used by wrapAnsiLine.
func TestLeadingAnsiCodes(t *testing.T) {
	cases := []struct{ input, want string }{
		{"\x1b[2mtext", "\x1b[2m"},             // dim
		{"\x1b[2m\x1b[33mtext", "\x1b[2m\x1b[33m"}, // stacked
		{"\x1b[0mtext", ""},                    // reset: nothing to carry
		{"plain text", ""},                     // no ANSI
		{"", ""},
	}
	for _, c := range cases {
		if got := leadingAnsiCodes(c.input); got != c.want {
			t.Errorf("leadingAnsiCodes(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

// TestWrapAnsiLinePreservesStyle verifies that a dim-wrapped long line keeps
// the dim style on the continuation segment (P23 bug fix).
func TestWrapAnsiLinePreservesStyle(t *testing.T) {
	// Construct a dim line that will wrap at width 20.
	longText := "· run_shell → this is a very long tool result line"
	dimLine := "\x1b[2m" + longText + "\x1b[0m"

	wrapped := wrapAnsiLine(dimLine, 20)
	lines := strings.Split(wrapped, "\n")
	if len(lines) < 2 {
		t.Fatal("expected wrapping to produce ≥2 lines")
	}
	// Each continuation line must start with the dim code.
	for i, l := range lines[1:] {
		if !strings.HasPrefix(l, "\x1b[2m") {
			t.Errorf("continuation line %d lost dim style: %q", i+1, l)
		}
	}
	// First segment must end with a reset so it is self-contained.
	if !strings.HasSuffix(lines[0], "\x1b[0m") {
		t.Errorf("first segment should close the dim code: %q", lines[0])
	}
}
