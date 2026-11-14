package agent

// B5 (review): a run_shell that finishes inside its deadline returns its output
// inline; its background log must not be left behind in the sandbox.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/proxy"
)

// randomMarker returns a short random suffix so parallel inline-run tests
// can tell their own log apart under /tmp.
func randomMarker(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func TestRunShellInlineFinishRemovesLog(t *testing.T) {
	// Own log namespace: the test watches only the background log the
	// inline run itself launches. The inline path schedules a background
	// job; when it finishes inside the deadline its log must be removed.
	// A unique marker in the command keeps this test's log from being
	// confused with a parallel test's log under /tmp.
	app := newRetentionApp(t)
	app.Cfg.ShellTimeoutSec = 5
	// Snapshot the logs that exist before the run. Other tests and other
	// sessions write their own logs under /tmp, so only the NEW ones count.
	before := make(map[string]bool)
	for _, m := range globWakilLogs(t) {
		before[m] = true
	}
	// Watch this test's own log only: the random marker in the command
	// output identifies this run's log file unambiguously even with parallel
	// tests sharing /tmp.
	marker := "inline-ok-" + randomMarker(t)
	res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "run_shell", Arguments: `{"command":"echo ` + marker + `"}`,
	}})
	if !strings.Contains(res.text, marker) {
		t.Fatalf("output not returned inline: %s", res.text)
	}
	app.bgMu.RLock()
	started := app.bgCounter
	app.bgMu.RUnlock()
	if started == 0 {
		t.Fatal("no background job was started; the test would be vacuous")
	}
	// The inline path removes the log of the job it launched; a parallel
	// test's log would be named differently and is not this test's concern.
	// We therefore assert no NEW log whose content carries this marker
	// remains, by scanning the glob for files newer than the snapshot.
	for _, m := range globWakilLogs(t) {
		if before[m] {
			continue
		}
		data, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		if strings.Contains(string(data), marker) {
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
