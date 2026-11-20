package wiring

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/prompts"
)

// TestStripPromptVersion verifies that the version line is stripped from
// the prompt body and the version is parsed correctly.
func TestStripPromptVersion(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantText string
		wantVer  int
	}{
		{
			name:     "with version line",
			input:    "# prompt-version: 3\nactual prompt content",
			wantText: "actual prompt content",
			wantVer:  3,
		},
		{
			name:     "without version line",
			input:    "just a prompt",
			wantText: "just a prompt",
			wantVer:  0,
		},
		{
			name:     "malformed version",
			input:    "# prompt-version: abc\ncontent",
			wantText: "# prompt-version: abc\ncontent",
			wantVer:  0,
		},
		{
			name:     "version only, no content",
			input:    "# prompt-version: 5",
			wantText: "",
			wantVer:  5,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotText, gotVer := stripPromptVersion(tt.input)
			if gotText != tt.wantText {
				t.Errorf("text = %q, want %q", gotText, tt.wantText)
			}
			if gotVer != tt.wantVer {
				t.Errorf("version = %d, want %d", gotVer, tt.wantVer)
			}
		})
	}
}

// TestLoadAgentPromptVersionComparison verifies that loadAgentPrompt
// strips the version line and returns the parsed override version.
func TestLoadAgentPromptVersionComparison(t *testing.T) {
	dir := t.TempDir()

	// Override with a lower version.
	lowerPath := filepath.Join(dir, "lower.txt")
	if err := os.WriteFile(lowerPath, []byte("# prompt-version: 0\ncustom v0 prompt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prompt, usingEmb, ver := loadAgentPrompt(config.Config{AgentPromptPath: lowerPath})
	if usingEmb {
		t.Error("usingEmbedded should be false for a custom file")
	}
	if ver != 0 {
		t.Errorf("override version = %d, want 0", ver)
	}
	if prompt != "custom v0 prompt" {
		t.Errorf("prompt = %q, want %q", prompt, "custom v0 prompt")
	}

	// Override with no version line.
	noVerPath := filepath.Join(dir, "nover.txt")
	if err := os.WriteFile(noVerPath, []byte("custom no-version prompt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, ver2 := loadAgentPrompt(config.Config{AgentPromptPath: noVerPath})
	if ver2 != 0 {
		t.Errorf("override version = %d, want 0 (no version line)", ver2)
	}

	// Override with matching version.
	matchPath := filepath.Join(dir, "match.txt")
	if err := os.WriteFile(matchPath, []byte("# prompt-version: "+strconv.Itoa(prompts.PromptVersion)+"\ncustom matching prompt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, ver3 := loadAgentPrompt(config.Config{AgentPromptPath: matchPath})
	if ver3 != prompts.PromptVersion {
		t.Errorf("override version = %d, want %d (matching)", ver3, prompts.PromptVersion)
	}

	// Embedded prompt: version is 0 (not an override).
	_, usingEmb4, ver4 := loadAgentPrompt(config.Config{AgentPromptPath: ""})
	if !usingEmb4 {
		t.Error("usingEmbedded should be true for empty path")
	}
	if ver4 != 0 {
		t.Errorf("override version = %d, want 0 (embedded)", ver4)
	}
}
