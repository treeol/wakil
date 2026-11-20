package prompts

import (
	"strconv"
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
		"## External side-effect gating, grants & session policy",
		"## Subagents",
		"Automatic Mashūra rounds",
		"apply_skill_template",
		"git_status",
		"/zdr",
		"/region",
		"/network",
		"/grants",
		"/handoff",
		"What survives in active context",
		"When a task gives numbered steps, start with step 1",
		"Blanket session approval",
		"Never assume an\n  unfamiliar tool is safe because its name sounds safe.",
	}
	for _, m := range wantMarkers {
		if !strings.Contains(EmbeddedAgentPrompt, m) {
			t.Errorf("EmbeddedAgentPrompt missing marker %q", m)
		}
	}
	for _, stale := range []string{
		"personality.txt (proxy)",
		"verification.txt (proxy)",
		"DockerExecutor",
	} {
		if strings.Contains(EmbeddedAgentPrompt, stale) {
			t.Errorf("EmbeddedAgentPrompt still contains stale phrase %q", stale)
		}
	}
}

// extractMarkdownSection returns the verbatim text of a level-2 markdown
// section (from `## <heading>` up to the next `\n## ` or EOF).
func extractMarkdownSection(doc, heading string) (string, bool) {
	marker := "## " + heading + "\n"
	idx := strings.Index(doc, marker)
	if idx < 0 {
		return "", false
	}
	rest := doc[idx:]
	if next := strings.Index(rest[len(marker):], "\n## "); next >= 0 {
		return strings.TrimRight(rest[:len(marker)+next], "\n"), true
	}
	return strings.TrimRight(rest, "\n"), true
}

func TestEmbeddedSplitPromptsParity(t *testing.T) {
	// Sections that live verbatim in core.txt or extension.txt must match
	// agent.txt so updates never drift between full and /prompt core.
	// Three sections are intentionally trimmed/rearranged for the split
	// ("Core loop" carries an inline TTL-renewal line in core.txt because
	// "## Durable Memory" lives in extension.txt; "## Files" moves AGENTS.md
	// verification into "## What \"verified\" means" in extension.txt).
	coreShared := []string{
		"Global precedence ladder",
		"Role",
		"Secrets (precedence tier 1 — overrides everything)",
		"Shell (run_shell, run_background)",
		"External side-effect gating, grants & session policy",
		"Honesty Invariants (precedence tier 4)",
		"Recovery",
	}
	for _, h := range coreShared {
		fullSec, okFull := extractMarkdownSection(EmbeddedAgentPrompt, h)
		coreSec, okCore := extractMarkdownSection(EmbeddedCorePrompt, h)
		if !okFull || !okCore {
			t.Fatalf("section %q presence: full=%v core=%v", h, okFull, okCore)
		}
		if fullSec != coreSec {
			t.Errorf("section %q differs between agent.txt and core.txt", h)
		}
	}
	// Core loop: step 0 (numbered-task scoping) must match verbatim.
	fullCL, okFull := extractMarkdownSection(EmbeddedAgentPrompt, "Core loop")
	coreCL, okCore := extractMarkdownSection(EmbeddedCorePrompt, "Core loop")
	if !okFull || !okCore {
		t.Fatalf("Core loop presence: full=%v core=%v", okFull, okCore)
	}
	wantStep0 := "0. When a task gives numbered steps, start with step 1; read only what the\n" +
		"   current step needs; treat facts stated in the task as given unless a\n" +
		"   command result contradicts them; do not ask for permission the task already\n" +
		"   gives (tool approval gates still run normally)."
	if !strings.Contains(fullCL, wantStep0) || !strings.Contains(coreCL, wantStep0) {
		t.Errorf("Core loop step 0 must match verbatim in agent.txt and core.txt")
	}
	extShared := []string{
		"Documentation upkeep",
		"Search & Research",
		"File vs. Inline Decision",
		"Skills (global reference docs)",
		"Tool / Cost Discipline",
		"Durable Memory",
		"Truncated tool results (spill files) & Context Collapse (`collapse_context`)",
		"Async execution (check_pending / wait_for_completion / read_process_log)",
		"Subagents",
		"Interaction & Output",
	}
	for _, h := range extShared {
		fullSec, okFull := extractMarkdownSection(EmbeddedAgentPrompt, h)
		extSec, okExt := extractMarkdownSection(EmbeddedExtensionPrompt, h)
		if !okFull || !okExt {
			t.Fatalf("section %q presence: full=%v extension=%v", h, okFull, okExt)
		}
		if fullSec != extSec {
			t.Errorf("section %q differs between agent.txt and extension.txt", h)
		}
	}
}

func TestPromptVersionMatches(t *testing.T) {
	// The PromptVersion constant must match the first line of agent.txt.
	firstLine := strings.SplitN(EmbeddedAgentPrompt, "\n", 2)[0]
	if !strings.HasPrefix(firstLine, "# prompt-version: ") {
		t.Fatalf("agent.txt first line is not a version line: %q", firstLine)
	}
	v, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(firstLine, "# prompt-version: ")))
	if err != nil {
		t.Fatalf("cannot parse version from %q: %v", firstLine, err)
	}
	if v != PromptVersion {
		t.Errorf("agent.txt version %d != PromptVersion constant %d", v, PromptVersion)
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
		"## External side-effect gating, grants & session policy",
		"## Honesty Invariants",
		"Signing is enforced",
		"/grants",
		"/zdr",
		"/region",
		"/network",
		"When a task gives numbered steps, start with step 1",
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
