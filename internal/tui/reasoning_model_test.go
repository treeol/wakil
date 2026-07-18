package tui

import (
	"testing"

	"github.com/treeol/wakil/internal/agent"
)

// TestTuiModelCopy_SharesReasoningBuilder locks the Bubble Tea value-copy
// invariant for the extracted reasoningModel (WP-6.6): copying tuiModel by value
// must share the single in-flight reasoning builder across all copies, because
// reasoning is a *strings.Builder. If someone changes the field to a value
// strings.Builder (or the extraction breaks pointer sharing), this test fails.
func TestTuiModelCopy_SharesReasoningBuilder(t *testing.T) {
	m := NewTUIModel(&agent.App{})
	if m.reasoning == nil {
		t.Fatal("NewTUIModel must initialize reasoning to a non-nil *strings.Builder")
	}

	// Value copy — exactly what Bubble Tea does on every Update.
	m2 := m

	// Same pointer: both copies address the one builder.
	if m.reasoning != m2.reasoning {
		t.Fatalf("copied model must share the reasoning builder pointer: %p vs %p", m.reasoning, m2.reasoning)
	}

	// A write through one copy is visible through the other.
	m.reasoning.WriteString("thinking")
	if got := m2.reasoning.String(); got != "thinking" {
		t.Fatalf("write via one copy not visible via the other: m2 sees %q", got)
	}
	if m2.reasoning.Len() != len("thinking") {
		t.Fatalf("shared builder length mismatch: got %d", m2.reasoning.Len())
	}

	// Reset through the other copy is visible through the first.
	m2.reasoning.Reset()
	if m.reasoning.Len() != 0 {
		t.Fatalf("reset via one copy not visible via the other: m still has %d bytes", m.reasoning.Len())
	}
}
