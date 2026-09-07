package format

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/proxy"
)

func TestDerefStr(t *testing.T) {
	s := "world"
	if got := DerefStr(&s); got != "world" {
		t.Errorf("DerefStr(&\"world\") = %q, want %q", got, "world")
	}
	if got := DerefStr(nil); got != "" {
		t.Errorf("DerefStr(nil) = %q, want %q", got, "")
	}
}

func TestIndent(t *testing.T) {
	got := Indent("line1\nline2\nline3")
	want := "  line1\n  line2\n  line3"
	if got != want {
		t.Errorf("Indent = %q, want %q", got, want)
	}
	// Single line.
	if got := Indent("solo"); got != "  solo" {
		t.Errorf("Indent(\"solo\") = %q, want %q", got, "  solo")
	}
	// Empty string.
	if got := Indent(""); got != "  " {
		t.Errorf("Indent(\"\") = %q, want %q", got, "  ")
	}
}

func TestTruncate(t *testing.T) {
	// Shorter than n — returned unchanged.
	if got := Truncate("short", 10); got != "short" {
		t.Errorf("Truncate(\"short\", 10) = %q, want %q", got, "short")
	}
	// Exactly n — returned unchanged.
	if got := Truncate("12345", 5); got != "12345" {
		t.Errorf("Truncate(\"12345\", 5) = %q, want %q", got, "12345")
	}
	// Longer than n — truncated with ellipsis.
	if got := Truncate("abcdefghij", 4); got != "abcd…" {
		t.Errorf("Truncate(\"abcdefghij\", 4) = %q, want %q", got, "abcd…")
	}
	// Rune-safe: n=2 on "héllo" (h, é, l, l, o) keeps h + é.
	if got := Truncate("héllo", 2); got != "hé…" {
		t.Errorf("Truncate(\"héllo\", 2) = %q, want %q", got, "hé…")
	}
}

func strPtr(s string) *string { return &s }

func TestTranscriptSize(t *testing.T) {
	conv := []proxy.Message{
		{Role: "user", Content: strPtr("hello world")}, // 11
		{Role: "assistant", Content: strPtr("hi")},     // 2
		{
			Role: "tool",
			ToolCalls: []proxy.ToolCall{
				{Function: proxy.FunctionCall{Arguments: `{"x":1}`}}, // 7
			},
		},
	}
	// 11 + 2 + 7 = 20
	if got := TranscriptSize(conv); got != 20 {
		t.Errorf("TranscriptSize = %d, want 20", got)
	}
	// Nil content is treated as empty.
	conv2 := []proxy.Message{{Role: "user"}}
	if got := TranscriptSize(conv2); got != 0 {
		t.Errorf("TranscriptSize with nil content = %d, want 0", got)
	}
	// Empty slice.
	if got := TranscriptSize(nil); got != 0 {
		t.Errorf("TranscriptSize(nil) = %d, want 0", got)
	}
}

// TestPackageIsAgentFree verifies the format package does not transitively
// import internal/agent.
func TestPackageIsAgentFree(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps failed: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.Contains(line, "internal/agent") {
			t.Errorf("format transitively imports %s — agent-free invariant violated", line)
		}
	}
}
