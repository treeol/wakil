package verify

import (
	"strings"
	"testing"
)

func TestCapOutput_NoTruncation(t *testing.T) {
	out := CapOutput("hello", 100)
	if out != "hello" {
		t.Errorf("expected no truncation, got %q", out)
	}
}

func TestCapOutput_Truncation(t *testing.T) {
	long := strings.Repeat("x", 200)
	out := CapOutput(long, 50)
	// 50 bytes of content + "\n[output truncated]" marker.
	if len(out) > 70 {
		t.Errorf("expected output under 70 chars after truncation, got %d", len(out))
	}
	if !strings.HasSuffix(out, "[output truncated]") {
		t.Errorf("expected truncation marker, got %q", out)
	}
	// The first 50 bytes should be from the original input.
	if !strings.HasPrefix(out, strings.Repeat("x", 50)) {
		t.Errorf("expected first 50 bytes preserved, got %q", out[:min(50, len(out))])
	}
}

func TestCapOutput_RuneSafe(t *testing.T) {
	// Multi-byte UTF-8: "é" is 2 bytes (0xC3 0xA9). Truncating at byte 5
	// (in the middle of the 3rd "é") should fall back to byte 4 to avoid
	// splitting a rune.
	input := "éééééé" // 12 bytes
	out := CapOutput(input, 5)
	// Should truncate to 4 bytes (2 complete "é" chars) + marker.
	if !strings.HasPrefix(out, "éé") {
		t.Errorf("expected rune-safe truncation to start with complete runes, got %q", out)
	}
	if !strings.HasSuffix(out, "[output truncated]") {
		t.Errorf("expected truncation marker, got %q", out)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestOutcome_Passed_AllPass(t *testing.T) {
	o := Outcome{
		Results: []Result{
			{Command: Command{Cmd: "go test"}, Status: StatusPass},
			{Command: Command{Cmd: "go vet"}, Status: StatusPass},
		},
	}
	if !o.Passed() {
		t.Error("expected Passed()=true when all pass")
	}
	if o.HasFailures() {
		t.Error("expected HasFailures()=false when all pass")
	}
}

func TestOutcome_Passed_OneFail(t *testing.T) {
	o := Outcome{
		Results: []Result{
			{Command: Command{Cmd: "go test"}, Status: StatusPass},
			{Command: Command{Cmd: "npm test"}, Status: StatusFail, ExitCode: 1},
		},
	}
	if o.Passed() {
		t.Error("expected Passed()=false when one fails")
	}
	if !o.HasFailures() {
		t.Error("expected HasFailures()=true when one fails")
	}
}

func TestOutcome_EmptySkipped(t *testing.T) {
	o := Outcome{}
	if o.Passed() {
		t.Error("empty outcome should not be Passed() — no silent pass")
	}
	if !o.WasSkipped() {
		t.Error("empty outcome should be WasSkipped()")
	}
	if o.HasFailures() {
		t.Error("empty outcome should not HasFailures()")
	}
}

func TestOutcome_TimeoutIsFailure(t *testing.T) {
	o := Outcome{
		Results: []Result{
			{Command: Command{Cmd: "go test"}, Status: StatusTimeout},
		},
	}
	if o.Passed() {
		t.Error("timeout should not be Passed()")
	}
	if !o.HasFailures() {
		t.Error("timeout should be HasFailures()")
	}
}

func TestOutcome_ErrorIsFailure(t *testing.T) {
	o := Outcome{
		Results: []Result{
			{Command: Command{Cmd: "go test"}, Status: StatusError, Reason: "executor unavailable"},
		},
	}
	if o.Passed() {
		t.Error("error should not be Passed()")
	}
	if !o.HasFailures() {
		t.Error("error should be HasFailures()")
	}
}

func TestOutcome_AnyDeclined(t *testing.T) {
	o := Outcome{
		Results: []Result{
			{Command: Command{Cmd: "go test"}, Status: StatusPass},
			{Command: Command{Cmd: "npm test"}, Status: StatusDeclined, Reason: "policy deny"},
		},
	}
	if !o.AnyDeclined() {
		t.Error("expected AnyDeclined()=true")
	}
	if o.HasFailures() {
		t.Error("declined should not be HasFailures() — it's a consent issue, not a test failure")
	}
}

func TestSummarize_AllPass(t *testing.T) {
	o := Outcome{
		Results: []Result{
			{Command: Command{Cmd: "go test ./...", Source: "detect"}, Status: StatusPass, DurationMs: 1200},
			{Command: Command{Cmd: "go vet ./...", Source: "detect"}, Status: StatusPass, DurationMs: 300},
		},
	}
	s := o.Summarize()
	if !strings.Contains(s, "go test ./...") {
		t.Errorf("summary should contain command name, got: %s", s)
	}
	if !strings.Contains(s, "PASS") {
		t.Errorf("summary should contain PASS, got: %s", s)
	}
	if !strings.Contains(s, "all checks passed") {
		t.Errorf("summary should contain success footer, got: %s", s)
	}
	if !strings.Contains(s, "1.2s") {
		t.Errorf("summary should contain duration, got: %s", s)
	}
}

func TestSummarize_WithFailure(t *testing.T) {
	o := Outcome{
		Results: []Result{
			{Command: Command{Cmd: "go test ./...", Source: "detect"}, Status: StatusPass, DurationMs: 1200},
			{Command: Command{Cmd: "npm test", Source: "detect"}, Status: StatusFail, ExitCode: 1, DurationMs: 12400,
				Output: "npm ERR! Test failed.\nSee above for more details."},
		},
	}
	s := o.Summarize()
	if !strings.Contains(s, "FAIL") {
		t.Errorf("summary should contain FAIL, got: %s", s)
	}
	if !strings.Contains(s, "exit=1") {
		t.Errorf("summary should contain exit code, got: %s", s)
	}
	if !strings.Contains(s, "12.4s") {
		t.Errorf("summary should contain duration, got: %s", s)
	}
	if !strings.Contains(s, "failures detected") {
		t.Errorf("summary should contain failure footer, got: %s", s)
	}
	if !strings.Contains(s, "npm ERR! Test failed.") {
		t.Errorf("summary should contain output tail, got: %s", s)
	}
}

func TestSummarize_Empty(t *testing.T) {
	o := Outcome{}
	s := o.Summarize()
	if !strings.Contains(s, "skipped") {
		t.Errorf("empty summary should contain 'skipped', got: %s", s)
	}
}
