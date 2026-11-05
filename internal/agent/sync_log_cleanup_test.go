package agent

// B5 (review): a run_shell that finishes inside its deadline returns its output
// inline; its background log must not be left behind in the sandbox.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/proxy"
)

func TestRunShellInlineFinishRemovesLog(t *testing.T) {
	app := newRetentionApp(t)
	app.Cfg.ShellTimeoutSec = 5
	// Snapshot the logs that exist before the run. Other tests and other
	// sessions write their own logs under /tmp, so only the NEW ones count.
	before := make(map[string]bool)
	for _, m := range globWakilLogs(t) {
		before[m] = true
	}
	res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "run_shell", Arguments: `{"command":"echo inline-ok"}`,
	}})
	if !strings.Contains(res.text, "inline-ok") {
		t.Fatalf("output not returned inline: %s", res.text)
	}
	app.bgMu.RLock()
	started := app.bgCounter
	app.bgMu.RUnlock()
	if started == 0 {
		t.Fatal("no background job was started; the test would be vacuous")
	}
	for _, m := range globWakilLogs(t) {
		if !before[m] {
			t.Errorf("inline run left its background log behind: %s", m)
		}
	}
}

func globWakilLogs(t *testing.T) []string {
	t.Helper()
	matches, err := filepath.Glob("/tmp/wakil-bg-*.log")
	if err != nil {
		t.Skipf("cannot list /tmp: %v", err)
	}
	return matches
}
