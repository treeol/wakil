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
