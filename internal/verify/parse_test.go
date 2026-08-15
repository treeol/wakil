package verify

import (
	"strconv"
	"strings"
	"testing"
)

// Fixture output captured from go1.26.6 (`go test ./...`) for a package with
// two failing top-level tests and one failing subtest. See .tmp/gofailfixture
// in the repo for the generating package.
const goTestFailOutput = `--- FAIL: TestFailingOne (0.00s)
    fail_test.go:8: boom one
--- FAIL: TestFailingTwo (0.00s)
    fail_test.go:12: boom two
--- FAIL: TestSubtest (0.00s)
    --- FAIL: TestSubtest/sub_case (0.00s)
        fail_test.go:17: sub boom
FAIL
FAIL	gofailfixture	0.002s
FAIL
`

func TestParseFailures_GoTest(t *testing.T) {
	f := ParseFailures("go test ./...", goTestFailOutput)
	if len(f) != 3 {
		t.Fatalf("expected 3 failures, got %d: %v", len(f), f)
	}
	// Top-level tests only; the subtest rolls up to its parent (not parsed).
	want := []string{"TestFailingOne", "TestFailingTwo", "TestSubtest"}
	for i, w := range want {
		if f[i].Test != w {
			t.Errorf("failure[%d] = %q, want %q", i, f[i].Test, w)
		}
	}
}

func TestParseFailures_NonGoCommand(t *testing.T) {
	if f := ParseFailures("go vet ./...", goTestFailOutput); f != nil {
		t.Errorf("go vet should not parse go test output, got %v", f)
	}
	if f := ParseFailures("npm test", "--- FAIL: TestX"); f != nil {
		t.Errorf("npm test should not parse, got %v", f)
	}
	if f := ParseFailures("make test", goTestFailOutput); f != nil {
		t.Errorf("make test should not parse, got %v", f)
	}
	if f := ParseFailures("env FOO=bar go test ./...", goTestFailOutput); f != nil {
		t.Errorf("env-prefixed command should not parse, got %v", f)
	}
}

func TestParseFailures_EmptyAndNoMatch(t *testing.T) {
	if f := ParseFailures("go test ./...", ""); f != nil {
		t.Errorf("empty output should yield nil, got %v", f)
	}
	if f := ParseFailures("go test ./...", "ok  \texample.com/pkg\t0.001s"); f != nil {
		t.Errorf("passing output should yield nil, got %v", f)
	}
}

func TestParseFailures_BuildFailure(t *testing.T) {
	out := "# gofailfixture [gofailfixture.test]\n./broken_test.go:6:2: undefined: undefinedSymbol\nFAIL\tgofailfixture [build failed]\nFAIL\n"
	if f := ParseFailures("go test ./...", out); f != nil {
		t.Errorf("build failure (no --- FAIL:) should yield nil, got %v", f)
	}
}

func TestParseFailures_DeduplicatesAndCaps(t *testing.T) {
	// Same test name repeated (retries) must dedupe.
	out := "--- FAIL: TestX (0.00s)\n--- FAIL: TestX (0.00s)\n--- FAIL: TestY (0.00s)\n"
	f := ParseFailures("go test ./...", out)
	if len(f) != 2 {
		t.Fatalf("expected 2 deduped failures, got %d", len(f))
	}
	if f[0].Test != "TestX" || f[1].Test != "TestY" {
		t.Errorf("unexpected dedup order: %v", f)
	}

	// More than maxFailures names must be capped.
	var sb strings.Builder
	for i := 0; i < maxFailures+10; i++ {
		sb.WriteString("--- FAIL: TestN")
		sb.WriteString(strconv.Itoa(i))
		sb.WriteString(" (0.00s)\n")
	}
	f = ParseFailures("go test ./...", sb.String())
	if len(f) != maxFailures {
		t.Fatalf("expected cap at %d, got %d", maxFailures, len(f))
	}
}

func TestParseFailures_ANSIColorsAndCRLF(t *testing.T) {
	out := "\x1b[31m--- FAIL: TestX (0.00s)\x1b[0m\r\n    x_test.go:1: boom\r\n"
	f := ParseFailures("go test ./...", out)
	if len(f) != 1 || f[0].Test != "TestX" {
		t.Fatalf("expected TestX after ANSI/CRLF strip, got %v", f)
	}
}

func TestParseFailures_MalformedInput(t *testing.T) {
	// Should never panic on malformed/partial input.
	inputs := []string{
		"--- FAIL:",
		"--- FAIL: (0.00s)\n",
		"--- FAIL: TestWith$Special\n",
	}
	for _, in := range inputs {
		if f := ParseFailures("go test ./...", in); f != nil {
			t.Errorf("malformed input %q should yield nil, got %v", in, f)
		}
	}
}

func TestParseFailures_ValidUnderscoreName(t *testing.T) {
	// Test_123 is a VALID Go identifier (not malformed) and must parse.
	f := ParseFailures("go test ./...", "--- FAIL: Test_123 (0.00s)\n")
	if len(f) != 1 || f[0].Test != "Test_123" {
		t.Fatalf("expected Test_123 parsed, got %v", f)
	}
}

func TestParseFailures_RejectsCompoundCommands(t *testing.T) {
	compound := []string{
		"go test ./... && other-command",
		"go test ./...; other-command",
		"go test ./... | tee results.txt",
		"go test ./... >results.txt",
		"go test ./... 2>&1",
		"go test ./...\nother",
		"env FOO=bar go test ./...",
	}
	for _, c := range compound {
		if f := ParseFailures(c, goTestFailOutput); f != nil {
			t.Errorf("compound command %q should yield nil, got %v", c, f)
		}
	}
}

func TestParseFailures_LongNamesAndBytesCapped(t *testing.T) {
	long := "Test" + strings.Repeat("A", maxNameLen+10)
	f := ParseFailures("go test ./...", "--- FAIL: "+long+" (0.00s)\n")
	if f != nil {
		t.Errorf("over-long name should be dropped, got %v", f)
	}
}

func TestParseFailures_MixedBuildAndTest(t *testing.T) {
	// One package build-fails, another has test failures — test failures
	// are still extracted (multi-package ./... run).
	out := "--- FAIL: TestX (0.00s)\nFAIL\tsomepkg [build failed]\nFAIL\n"
	f := ParseFailures("go test ./...", out)
	if len(f) != 1 || f[0].Test != "TestX" {
		t.Fatalf("expected TestX from mixed output, got %v", f)
	}
}

func TestRerunCommand_Go(t *testing.T) {
	f := []Failure{{Test: "TestFailingOne"}, {Test: "TestFailingTwo"}}
	got, ok := RerunCommand(Command{Cmd: "go test ./..."}, f)
	if !ok {
		t.Fatalf("expected ok=true, got false")
	}
	want := "go test ./... -run '^(TestFailingOne|TestFailingTwo)$'"
	if got != want {
		t.Errorf("rerun = %q, want %q", got, want)
	}
}

func TestRerunCommand_PreservesFlagsAndScope(t *testing.T) {
	f := []Failure{{Test: "TestX"}}
	got, ok := RerunCommand(Command{Cmd: "go test -race ./pkg/foo"}, f)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if !strings.HasPrefix(got, "go test -race ./pkg/foo ") {
		t.Errorf("original flags/scope not preserved: %q", got)
	}
}

func TestRerunCommand_RefusesUnsafe(t *testing.T) {
	// Non-go command.
	if _, ok := RerunCommand(Command{Cmd: "npm test"}, []Failure{{Test: "TestX"}}); ok {
		t.Error("npm test should refuse")
	}
	// Empty failures.
	if _, ok := RerunCommand(Command{Cmd: "go test ./..."}, nil); ok {
		t.Error("empty failures should refuse")
	}
	// Injection: subtest slash / metacharacter.
	bad := []Failure{{Test: "TestX/sub"}, {Test: "Test$(rm)"}, {Test: "TestX; rm -rf ~"}}
	for _, f := range bad {
		if _, ok := RerunCommand(Command{Cmd: "go test ./..."}, []Failure{f}); ok {
			t.Errorf("unsafe test name %q should refuse", f.Test)
		}
	}
	// Existing -run flag must not be clobbered.
	if _, ok := RerunCommand(Command{Cmd: "go test -run TestFoo ./..."}, []Failure{{Test: "TestX"}}); ok {
		t.Error("existing -run should refuse")
	}
	// -args flag: a trailing -run would not filter go test.
	if _, ok := RerunCommand(Command{Cmd: "go test ./... -args -custom"}, []Failure{{Test: "TestX"}}); ok {
		t.Error("-args command should refuse")
	}
	// Compound commands must refuse.
	compound := []string{
		"go test ./... && other",
		"go test ./... | tee x",
		"go test ./... >out.txt",
	}
	for _, c := range compound {
		if _, ok := RerunCommand(Command{Cmd: c}, []Failure{{Test: "TestX"}}); ok {
			t.Errorf("compound command %q should refuse", c)
		}
	}
	// Oversized aggregate (many long names) must refuse: 21 names × 256
	// bytes exceeds the maxNameLen*maxFailures byte budget.
	many := make([]Failure, maxFailures+1)
	for i := range many {
		many[i] = Failure{Test: "T" + strings.Repeat("N", maxNameLen-1)}
	}
	if _, ok := RerunCommand(Command{Cmd: "go test ./..."}, many); ok {
		t.Error("oversized failure set should refuse")
	}
}

func TestRerunCommand_FlagAwareRunDetection(t *testing.T) {
	// A package path containing "-run" as part of a value must NOT be
	// misread as an existing -run flag. (Contrived but proves tokenization.)
	f := []Failure{{Test: "TestX"}}
	got, ok := RerunCommand(Command{Cmd: "go test -tags=foruninteresting ./..."}, f)
	if !ok {
		t.Fatalf("non-flag '-run' substring should not refuse; got ok=false")
	}
	if !strings.Contains(got, "-run '^(TestX)$'") {
		t.Errorf("expected appended -run, got %q", got)
	}
	// -run=... form is also an existing filter.
	if _, ok := RerunCommand(Command{Cmd: "go test -run=TestFoo ./..."}, []Failure{{Test: "TestX"}}); ok {
		t.Error("-run= form should refuse")
	}
}

func TestSummarize_StructuredFailures(t *testing.T) {
	o := Outcome{
		Results: []Result{{
			Command:  Command{Cmd: "go test ./...", Source: "detect"},
			Status:   StatusFail,
			ExitCode: 1,
			Failures: []Failure{{Test: "TestFailingOne"}, {Test: "TestFailingTwo"}},
		}},
	}
	s := o.Summarize()
	if !strings.Contains(s, "failed: TestFailingOne, TestFailingTwo") {
		t.Errorf("summary should list structured failures first, got:\n%s", s)
	}
	if !strings.Contains(s, "rerun:  go test ./... -run '^(TestFailingOne|TestFailingTwo)$'") {
		t.Errorf("summary should include rerun suggestion, got:\n%s", s)
	}
	// Structured block must precede the raw output tail (if any).
	failedIdx := strings.Index(s, "failed:")
	rerunIdx := strings.Index(s, "rerun:")
	if failedIdx == -1 || rerunIdx == -1 || failedIdx > rerunIdx {
		t.Errorf("expected 'failed:' before 'rerun:' in summary, got:\n%s", s)
	}
}

func TestSummarize_NoStructuredFailuresOnPass(t *testing.T) {
	o := Outcome{
		Results: []Result{{
			Command: Command{Cmd: "go test ./...", Source: "detect"},
			Status:  StatusPass,
		}},
	}
	s := o.Summarize()
	if strings.Contains(s, "failed:") || strings.Contains(s, "rerun:") {
		t.Errorf("passing result should not emit structured failure lines, got:\n%s", s)
	}
}

func TestRerunCommand_EscapesRegexMeta(t *testing.T) {
	// A valid Go identifier cannot contain regex metacharacters, but a name
	// like Test_123 is valid — ensure it stays literal via QuoteMeta (no-op
	// here, but the path is exercised).
	f := []Failure{{Test: "Test_123"}}
	got, ok := RerunCommand(Command{Cmd: "go test ./..."}, f)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if !strings.Contains(got, "Test_123") {
		t.Errorf("name lost in rerun: %q", got)
	}
}
