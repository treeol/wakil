package prompts

import (
	"strings"
	"testing"
)

func TestEmbeddedAgentPrompt(t *testing.T) {
	// The embedded prompt must be the full agent instructions, not the old
	// minimal 1-liner fallback. Verify it contains known section markers.
	if len(EmbeddedAgentPrompt) < 1000 {
		t.Errorf("EmbeddedAgentPrompt is only %d bytes — expected the full prompt (~27KB)", len(EmbeddedAgentPrompt))
	}
	wantMarkers := []string{
		"## Global precedence ladder",
		"## Core loop",
		"## Secrets",
		"## Shell",
		"## Subagents",
	}
	for _, m := range wantMarkers {
		if !strings.Contains(EmbeddedAgentPrompt, m) {
			t.Errorf("EmbeddedAgentPrompt missing marker %q", m)
		}
	}
}

func TestEmbeddedSplitPrompts(t *testing.T) {
	// The core prompt must contain safety-critical sections.
	if len(EmbeddedCorePrompt) < 1000 {
		t.Errorf("EmbeddedCorePrompt is only %d bytes — expected ~17KB", len(EmbeddedCorePrompt))
	}
	coreMarkers := []string{
		"## Global precedence ladder",
		"## Core loop",
		"## Secrets",
		"## Shell",
		"## Files",
		"## Honesty Invariants",
	}
	for _, m := range coreMarkers {
		if !strings.Contains(EmbeddedCorePrompt, m) {
			t.Errorf("EmbeddedCorePrompt missing marker %q", m)
		}
	}

	// The extension prompt must contain workflow guidance sections.
	if len(EmbeddedExtensionPrompt) < 1000 {
		t.Errorf("EmbeddedExtensionPrompt is only %d bytes — expected ~31KB", len(EmbeddedExtensionPrompt))
	}
	extMarkers := []string{
		"## Documentation upkeep",
		"## Subagents",
		"## Durable Memory",
		"## Async execution",
	}
	for _, m := range extMarkers {
		if !strings.Contains(EmbeddedExtensionPrompt, m) {
			t.Errorf("EmbeddedExtensionPrompt missing marker %q", m)
		}
	}

	// Core + extension should roughly equal the full prompt in size.
	combined := len(EmbeddedCorePrompt) + len(EmbeddedExtensionPrompt)
	full := len(EmbeddedAgentPrompt)
	if combined < full*9/10 {
		t.Errorf("core(%d) + ext(%d) = %d < 90%% of full(%d) — split may be incomplete", len(EmbeddedCorePrompt), len(EmbeddedExtensionPrompt), combined, full)
	}
}
