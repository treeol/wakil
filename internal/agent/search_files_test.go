package agent

import (
	"context"
	"fmt"
	"io"
	osexec "os/exec"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/proxy"
	wtools "github.com/treeol/wakil/internal/tools"
)

// exitErr builds the same error shape a real sh -c failure produces, so
// shellExitCode is exercised through the exec.ExitError path rather than the
// textual fallback.
func exitErr(t *testing.T, code int) error {
	t.Helper()
	err := osexec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	if err == nil {
		t.Fatalf("expected sh -c 'exit %d' to fail", code)
	}
	return err
}

// searchCall invokes the search_files handler with a fixed executor result.
func searchCall(t *testing.T, ex *fakeExecutor) string {
	t.Helper()
	app := &App{Exec: ex, Out: io.Discard}
	res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "search_files", Arguments: `{"pattern":"needle","path":"."}`,
	}})
	return res.text
}

// TestSearchFilesNoMatchesIsNotAnError pins the exit-1 contract: grep finding
// zero matches is a successful search, reported as "(no matches)".
func TestSearchFilesNoMatchesIsNotAnError(t *testing.T) {
	ex := newFakeExecutor()
	// grep writes nothing to stdout when it finds no matches. The fake
	// substitutes "ran: <cmd>" for an empty result, so use whitespace only.
	ex.shellResult = " \n"
	ex.shellErr = exitErr(t, 1)

	got := searchCall(t, ex)
	if got != "(no matches)" {
		t.Fatalf("exit 1 = %q, want %q", got, "(no matches)")
	}
}

// TestSearchFilesMissingGrepIsNotReportedAsNoMatches is the regression test for
// the substring-match defect: "exit status 1" is a prefix of "exit status 127",
// so a missing or unrunnable grep used to be reported as "(no matches)".
func TestSearchFilesMissingGrepIsNotReportedAsNoMatches(t *testing.T) {
	ex := newFakeExecutor()
	ex.shellResult = "grep: command not found"
	ex.shellErr = exitErr(t, 127)

	got := searchCall(t, ex)
	if got == "(no matches)" {
		t.Fatal("exit 127 reported as (no matches) — exit code is being substring-matched")
	}
	if !strings.HasPrefix(got, "ERROR:") {
		t.Fatalf("exit 127 should surface as an error, got %q", got)
	}
}

// TestSearchFilesErrorWithPartialOutputIsFlagged verifies that matches found
// before a failure are preserved but marked incomplete, so a truncated result
// set is never read as the whole truth.
func TestSearchFilesErrorWithPartialOutputIsFlagged(t *testing.T) {
	ex := newFakeExecutor()
	ex.shellResult = "a.go:1:needle\nb.go:2:needle"
	ex.shellErr = exitErr(t, 2)

	got := searchCall(t, ex)
	if !strings.Contains(got, "a.go:1:needle") || !strings.Contains(got, "b.go:2:needle") {
		t.Fatalf("partial matches dropped: %q", got)
	}
	if !strings.Contains(got, "incomplete") {
		t.Fatalf("partial results not flagged incomplete: %q", got)
	}
	if !strings.HasPrefix(got, "ERROR:") {
		t.Fatalf("failed search must keep the ERROR prefix: %q", got)
	}
}

// TestSearchFilesCleanSuccessHasNoMarker verifies exit 0 output is passed
// through untouched — no error prefix and no incompleteness marker.
func TestSearchFilesCleanSuccessHasNoMarker(t *testing.T) {
	ex := newFakeExecutor()
	ex.shellResult = "a.go:1:needle"
	ex.shellErr = nil

	got := searchCall(t, ex)
	if got != "a.go:1:needle" {
		t.Fatalf("exit 0 = %q, want the raw output", got)
	}
}

// TestSearchFilesSkipsBinaryFiles pins the -I flag: binary artifacts must not
// flood the result set with unreadable matches.
func TestSearchFilesSkipsBinaryFiles(t *testing.T) {
	ex := newFakeExecutor()
	ex.shellResult = "a.go:1:needle"
	ex.shellErr = nil

	searchCall(t, ex)
	if len(ex.shellCalls) != 1 {
		t.Fatalf("expected 1 shell call, got %d", len(ex.shellCalls))
	}
	if !strings.Contains(ex.shellCalls[0], "grep -rn -I") {
		t.Fatalf("grep command missing -I: %q", ex.shellCalls[0])
	}
}

// TestShellExitCodeFallsBackToText covers executors that flatten the error into
// a string instead of returning a typed *exec.ExitError.
func TestShellExitCodeFallsBackToText(t *testing.T) {
	cases := []struct {
		in   error
		want int
	}{
		{nil, -1},
		{fmt.Errorf("exit status 2"), 2},
		{fmt.Errorf("exit status 127"), 127},
		{fmt.Errorf("some other failure"), -1},
		// Trailing junk must not parse as a status.
		{fmt.Errorf("exit status 1garbage"), -1},
		// A code quoted inside prose is not the process status; the trailing
		// fragment wins, and anything unparseable stays unknown (-1).
		{fmt.Errorf(`command "echo exit status 1" failed: exit status 127`), 127},
	}
	for _, c := range cases {
		if got := shellExitCode(c.in); got != c.want {
			t.Errorf("shellExitCode(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestShellExitCodePrefersTypedError pins that a real wrapped *exec.ExitError
// is read from the typed exit code, not from whatever text surrounds it.
func TestShellExitCodePrefersTypedError(t *testing.T) {
	inner := exitErr(t, 2)
	wrapped := fmt.Errorf("docker exec failed: %w", inner)
	if got := shellExitCode(wrapped); got != 2 {
		t.Fatalf("shellExitCode(wrapped) = %d, want 2 (typed error must win)", got)
	}
}

// TestSearchFilesExitOneWithOutputIsPassedThrough covers the ambiguous case:
// a non-zero status of 1 that nonetheless produced output must not be silently
// rewritten to "(no matches)" — that would discard real evidence. It is
// reported as a failed search so the diagnostic is never mistaken for matches.
func TestSearchFilesExitOneWithOutputIsPassedThrough(t *testing.T) {
	ex := newFakeExecutor()
	ex.shellResult = "grep: recursive directory loop\n"
	ex.shellErr = exitErr(t, 1)

	got := searchCall(t, ex)
	if got == "(no matches)" {
		t.Fatal("exit 1 with output must not be reported as (no matches)")
	}
	if !strings.Contains(got, "recursive directory loop") {
		t.Fatalf("diagnostic dropped: %q", got)
	}
	if !strings.HasPrefix(got, "ERROR:") {
		t.Fatalf("exit 1 with output must be flagged as a failed search: %q", got)
	}
}

// TestSearchFilesExitZeroWithErrorFailsClosed covers the textual-fallback trap:
// a non-nil error that parses as status 0 must not be treated as success, which
// would silently drop a real failure.
func TestSearchFilesExitZeroWithErrorFailsClosed(t *testing.T) {
	ex := newFakeExecutor()
	ex.shellResult = "partial\n"
	ex.shellErr = fmt.Errorf("wrapper: exit status 0")

	got := searchCall(t, ex)
	if !strings.HasPrefix(got, "ERROR:") {
		t.Fatalf("exit 0 on a non-nil error must not read as success: %q", got)
	}
}

// TestSearchFilesUnknownErrorWithPartialOutputIsIncomplete covers an error that
// carries no recognisable exit code (-1, e.g. a signal kill): it must still be
// reported as a failure, never as a clean result.
func TestSearchFilesUnknownErrorWithPartialOutputIsIncomplete(t *testing.T) {
	ex := newFakeExecutor()
	ex.shellResult = "a.go:1:needle\n"
	ex.shellErr = fmt.Errorf("signal: killed")

	got := searchCall(t, ex)
	if !strings.HasPrefix(got, "ERROR:") {
		t.Fatalf("unknown error must keep the ERROR prefix: %q", got)
	}
	if !strings.Contains(got, "incomplete") {
		t.Fatalf("unknown error with output must be flagged incomplete: %q", got)
	}
}

// TestIncompleteSearchResultSurvivesTruncation pins the ordering: the warning
// must sit immediately after the ERROR: prefix so CapToolResult — which keeps
// only the leading characters of an oversized result — cannot drop it.
func TestIncompleteSearchResultSurvivesTruncation(t *testing.T) {
	long := strings.Repeat("match line here\n", 500)
	got := incompleteSearchResult(long, fmt.Errorf("exit status 2"))
	if !strings.HasPrefix(got, "ERROR: [search incomplete — matches may be missing] ") {
		t.Fatalf("marker must directly follow the ERROR: prefix, got %q", got[:60])
	}
	// Run it through the real cap so the guarantee is proven end to end.
	capped := wtools.CapToolResult(got, "search_files", "chat-cap", 200)
	if !strings.Contains(capped, "incomplete") {
		t.Fatalf("incompleteness warning lost under ToolResultCap: %q", capped)
	}
}
