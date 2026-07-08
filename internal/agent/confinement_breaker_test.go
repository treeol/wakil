package agent

// Regression tests for the path-confinement circuit breaker (see
// confinementBreakerThreshold, isConfinementError, confinementPathQuoted,
// confinementBreakerPrompt in app.go, and the tracking wired into Send's tool
// loop). These target the two confirmed budget-exhaustion root causes:
//
//  1. A subagent's own capped/stubbed tool result advertises a toolcache path
//     that is genuinely unreachable via ConfinePath (host-written, not under
//     any executor's workspace root) — the model retries it until
//     MaxToolIterations forces a generic, uninformative wrap-up.
//  2. A subagent dispatched against a path outside the executor's
//     workspaceRoot (e.g. a different repo/mount) — every read/list/search
//     call against it fails identically, forever, with no early exit.
//
// The breaker must trip fast (well before MaxToolIterations) on repeated
// hits on the SAME resolved path and produce a specific, honest final
// message — not fall through to the generic ToolLimitPrompt.

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
	wtools "github.com/treeol/wakil/internal/tools"
)

// TestIsConfinementError verifies the error-class detector matches the real
// error strings ConfinePath produces (see internal/exec/exec_ops.go) but does
// not false-positive on unrelated tool errors.
func TestIsConfinementError(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"docker resolving path", `ERROR: resolving path "/mnt/lims3/foo.go": readlink: No such file or directory`, true},
		{"docker outside workspace", `ERROR: path "/mnt/lims3/foo.go" (→ /mnt/lims3/foo.go) is outside workspace "/mnt/wakil" — traversal not allowed`, true},
		{"direct outside workspace", `ERROR: path "/mnt/lims3/foo.go" is outside workspace "/mnt/wakil" — traversal not allowed`, true},
		{"could not resolve", `ERROR: could not resolve path "/mnt/lims3/foo.go"`, true},
		{"unrelated error", `ERROR: no such file: config.go`, false},
		{"is a directory", `ERROR: "src" is a directory, not a file — use list_dir to see its contents or search_files to search within it.`, false},
		{"not an error at all", `package main\n\nfunc main() {}`, false},
	}
	for _, c := range cases {
		got := isConfinementError(c.in)
		if got != c.want {
			t.Errorf("%s: isConfinementError(%q) = %v, want %v", c.name, c.in, got, c.want)
		}
	}
}

// TestConfinementPathQuoted verifies the offending path is extracted from the
// real ConfinePath error formats.
func TestConfinementPathQuoted(t *testing.T) {
	in := `ERROR: path "/mnt/lims3/foo.go" is outside workspace "/mnt/wakil" — traversal not allowed`
	got := confinementPathQuoted(in)
	if got != "/mnt/lims3/foo.go" {
		t.Errorf("confinementPathQuoted(%q) = %q, want %q", in, got, "/mnt/lims3/foo.go")
	}
}

// TestConfinementBreakerTripsBeforeMaxToolIterations verifies the core claim:
// when a subagent repeatedly hits ConfinePath rejections on the SAME path,
// the breaker trips at confinementBreakerThreshold and force-finishes the
// turn WELL BEFORE MaxToolIterations would otherwise be exhausted — the
// scenario B failure mode (dispatch against an out-of-workspace path) never
// burns the full iteration budget.
func TestConfinementBreakerTripsBeforeMaxToolIterations(t *testing.T) {
	finalMsg := "I cannot access that path."
	// The model retries the same unreachable path with a different tool each
	// time — read_file, then read_file_full — neither of which can ever
	// succeed (deterministic ConfinePath rejection). MaxToolIterations is set
	// high (10) so if the breaker did NOT intervene, many more rounds would
	// follow; instead the breaker trips after the 2nd failure and forces
	// tools=nil + the honest wrap-up directive on the NEXT (3rd) Stream call —
	// mirroring forceFinish's existing "one more call to get the wrap-up
	// answer" semantics. If the breaker did not fire, this 3rd call would
	// never happen with tools=nil and the model would instead see a 3rd
	// tool-call opportunity (a list_dir frame is deliberately NOT provided
	// here, so the test would fail loudly with a mock-server exhaustion error
	// if the breaker regressed and the loop over-ran).
	srv := sseServer(t,
		toolCallFrames("c1", "read_file", `{"path":"/mnt/lims3/foo.go"}`),
		toolCallFrames("c2", "read_file_full", `{"path":"/mnt/lims3/foo.go"}`),
		[]string{contentChunk(finalMsg)},
	)
	defer srv.Close()

	exec := newFakeExecutor()
	exec.confineErrFn = func(path string) error {
		if strings.HasPrefix(path, "/mnt/lims3") {
			return fmt.Errorf(`path %q is outside workspace "/mnt/wakil" — traversal not allowed`, path)
		}
		return nil
	}

	cfg := config.DefaultConfig()
	cfg.MaxToolIterations = 10 // generous — must not be the thing that stops us
	app := &App{
		Cfg:     cfg,
		Client:  newTestClient(srv.URL),
		Exec:    exec,
		Tools:   wtools.DiscoveryTools("/work"),
		Confirm: func(_, _, _ string, _ bool) bool { return true },
		Out:     io.Discard,
	}

	final, err := app.Send(context.Background(), "read /mnt/lims3/foo.go")
	if err != nil {
		t.Fatal(err)
	}
	if final != finalMsg {
		t.Errorf("final = %q, want %q", final, finalMsg)
	}
	if !app.confinementTripped {
		t.Error("expected confinementTripped=true")
	}
	if len(app.confinementPathsHit) == 0 || app.confinementPathsHit[0] != "/mnt/lims3/foo.go" {
		t.Errorf("confinementPathsHit = %v, want [/mnt/lims3/foo.go]", app.confinementPathsHit)
	}

	// Count how many tool-call rounds actually executed by counting "tool"
	// messages in Conv — must be exactly 2 (read_file, read_file_full), NOT 3
	// (list_dir never should have been reached: the breaker fires after the
	// 2nd confinement failure, on the finalize step, forcing forceFinish on
	// the NEXT iteration).
	toolMsgs := 0
	for _, m := range app.Conv {
		if m.Role == "tool" {
			toolMsgs++
		}
	}
	if toolMsgs != 2 {
		t.Errorf("expected exactly 2 tool calls before breaker forced wrap-up, got %d", toolMsgs)
	}
}

// TestConfinementBreakerToleratesOneRetry verifies the breaker does NOT trip
// on a single confinement failure — one legitimate retry with a corrected
// path is tolerated (threshold=2), so a model that self-corrects after one
// bad guess is not punished.
func TestConfinementBreakerToleratesOneRetry(t *testing.T) {
	summaryText := "Found it in the workspace file."
	srv := sseServer(t,
		toolCallFrames("c1", "read_file", `{"path":"/mnt/lims3/foo.go"}`), // fails
		toolCallFrames("c2", "read_file", `{"path":"good.go"}`),          // succeeds — different path
		[]string{contentChunk(summaryText)},
	)
	defer srv.Close()

	exec := newFakeExecutor()
	exec.files["good.go"] = "package main"
	exec.confineErrFn = func(path string) error {
		if strings.HasPrefix(path, "/mnt/lims3") {
			return fmt.Errorf(`path %q is outside workspace "/mnt/wakil" — traversal not allowed`, path)
		}
		return nil
	}

	cfg := config.DefaultConfig()
	cfg.MaxToolIterations = 10
	app := &App{
		Cfg:     cfg,
		Client:  newTestClient(srv.URL),
		Exec:    exec,
		Tools:   wtools.DiscoveryTools("/work"),
		Confirm: func(_, _, _ string, _ bool) bool { return true },
		Out:     io.Discard,
	}

	final, err := app.Send(context.Background(), "find something")
	if err != nil {
		t.Fatal(err)
	}
	if final != summaryText {
		t.Errorf("final = %q, want %q", final, summaryText)
	}
	if app.confinementTripped {
		t.Error("breaker should NOT trip on a single confinement failure followed by a successful different path")
	}
}

// TestConfinementBreakerDistinctPathsDoNotCombine verifies the breaker counts
// PER DISTINCT PATH — two different unreachable paths, each hit once, should
// not combine to trip the breaker (that would be a false trip on legitimate
// exploration of multiple candidate locations).
func TestConfinementBreakerDistinctPathsDoNotCombine(t *testing.T) {
	summaryText := "done"
	srv := sseServer(t,
		toolCallFrames("c1", "read_file", `{"path":"/mnt/lims3/a.go"}`),
		toolCallFrames("c2", "read_file", `{"path":"/mnt/other/b.go"}`),
		[]string{contentChunk(summaryText)},
	)
	defer srv.Close()

	exec := newFakeExecutor()
	exec.confineErrFn = func(path string) error {
		if strings.HasPrefix(path, "/mnt/") {
			return fmt.Errorf(`path %q is outside workspace "/mnt/wakil" — traversal not allowed`, path)
		}
		return nil
	}

	cfg := config.DefaultConfig()
	cfg.MaxToolIterations = 10
	app := &App{
		Cfg:     cfg,
		Client:  newTestClient(srv.URL),
		Exec:    exec,
		Tools:   wtools.DiscoveryTools("/work"),
		Confirm: func(_, _, _ string, _ bool) bool { return true },
		Out:     io.Discard,
	}

	_, err := app.Send(context.Background(), "find something")
	if err != nil {
		t.Fatal(err)
	}
	if app.confinementTripped {
		t.Error("breaker should not trip when two DIFFERENT unreachable paths are each hit only once")
	}
}

// TestConfinementBreakerPromptNamesPaths verifies the injected wrap-up
// message is specific (names the unreachable path) rather than the generic
// ToolLimitPrompt, so the model's final answer is honest about WHY it is
// stopping rather than producing an unexplained truncated response.
func TestConfinementBreakerPromptNamesPaths(t *testing.T) {
	msg := confinementBreakerPrompt([]string{"/mnt/lims3/foo.go"})
	if !strings.Contains(msg, "/mnt/lims3/foo.go") {
		t.Errorf("breaker prompt does not name the unreachable path: %q", msg)
	}
	if !strings.Contains(msg, "Stop retrying") {
		t.Errorf("breaker prompt does not instruct the model to stop retrying: %q", msg)
	}
}

// TestConfinementBreakerReportedAsInaccessibleNotBudgetExhausted verifies that
// through the real dispatchSubagent path, a confinement-breaker trip is
// reported to the parent with Reason:"inaccessible" and a path-specific
// uncertainty note — distinct from the generic "budget-exhausted" reason used
// for MaxToolIterations exhaustion. The parent needs this distinction: a
// budget-exhausted subagent might succeed on a narrower re-dispatch: an
// inaccessible-path subagent will not, no matter how it's re-dispatched.
func TestConfinementBreakerReportedAsInaccessibleNotBudgetExhausted(t *testing.T) {
	summaryJSON := `{"objective":"read the sibling repo","findings":[]}`
	srv := sseServer(t,
		toolCallFrames("c1", "read_file", `{"path":"/mnt/lims3/foo.go"}`),
		toolCallFrames("c2", "read_file_full", `{"path":"/mnt/lims3/foo.go"}`),
		[]string{contentChunk(summaryJSON)},
	)
	defer srv.Close()

	exec := newFakeExecutor()
	exec.confineErrFn = func(path string) error {
		if strings.HasPrefix(path, "/mnt/lims3") {
			return fmt.Errorf(`path %q is outside workspace "/mnt/wakil" — traversal not allowed`, path)
		}
		return nil
	}

	parent := &App{
		Cfg:     config.DefaultConfig(),
		Client:  newTestClient(srv.URL),
		Exec:    exec,
		Tools:   wtools.DiscoveryTools("/work"),
		Confirm: func(_, _, _ string, _ bool) bool { return true },
		Out:     io.Discard,
	}

	summary, _, _, _ := parent.dispatchSubagent(context.Background(), "read the sibling repo", io.Discard, "")

	if summary.Status != "incomplete" {
		t.Errorf("status = %q, want incomplete", summary.Status)
	}
	foundInaccessible := false
	for _, s := range summary.Skipped {
		if s.Reason == "inaccessible" {
			foundInaccessible = true
			if s.Path != "/mnt/lims3/foo.go" {
				t.Errorf("skipped path = %q, want /mnt/lims3/foo.go", s.Path)
			}
		}
		if s.Reason == "budget-exhausted" {
			t.Error("confinement-breaker trip must not be reported as budget-exhausted")
		}
	}
	if !foundInaccessible {
		t.Error("expected an \"inaccessible\" Skipped entry")
	}
	foundNote := false
	for _, u := range summary.Uncertainty {
		if strings.Contains(u, "permanently unreachable") {
			foundNote = true
		}
	}
	if !foundNote {
		t.Errorf("expected an uncertainty note distinguishing this from budget exhaustion; got %v", summary.Uncertainty)
	}
}

// TestConfinementBreakerResetsAcrossSendCalls verifies confinementTripped and
// confinementPathsHit are reset at the start of Send (same lifecycle as
// exhausted), so a prior turn's trip doesn't leak into a fresh turn.
func TestConfinementBreakerResetsAcrossSendCalls(t *testing.T) {
	app := &App{
		Cfg:                 config.DefaultConfig(),
		Client:              newTestClient(""),
		Exec:                newFakeExecutor(),
		Out:                 io.Discard,
		confinementTripped:  true,
		confinementPathsHit: []string{"/some/stale/path"},
	}
	// A minimal Send that errors immediately (bad client URL) still must reset
	// the flags before doing anything else — verify via the reset happening
	// prior to any network call, by checking state right after construction
	// logic that Send performs at entry. We can't easily isolate just the
	// reset without a working server, so use a real server for a clean turn.
	srv := sseServer(t, []string{contentChunk("ok")})
	defer srv.Close()
	app.Client = newTestClient(srv.URL)

	_, err := app.Send(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if app.confinementTripped {
		t.Error("confinementTripped should reset to false at the start of a fresh Send")
	}
	if app.confinementPathsHit != nil {
		t.Error("confinementPathsHit should reset to nil at the start of a fresh Send")
	}
}

// Sanity: ensure proxy.Message import path stays used if test file is edited.
var _ = proxy.Message{}
