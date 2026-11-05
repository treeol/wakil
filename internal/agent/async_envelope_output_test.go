package agent

// T1 (0.4.4): completion envelopes for finished background shells carry the
// exit status AND the output itself (head + tail with an explicit omitted-bytes
// marker), not only a "use read_process_log" hint.

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/exec"
	"github.com/treeol/wakil/internal/proxy"
)

func TestShellOutputPreview(t *testing.T) {
	t.Run("whole output when small", func(t *testing.T) {
		got := shellOutputPreview("", 0, "a\nb\nc\n", "/tmp/x.log")
		if got != "a\nb\nc" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("whole log larger than the whole-max is head+marker+tail", func(t *testing.T) {
		body := strings.Repeat("x", 20000)
		got := shellOutputPreview("", 0, body, "/tmp/x.log")
		if !strings.Contains(got, "bytes omitted, full log: /tmp/x.log") {
			t.Fatalf("missing omitted marker: %.200q", got)
		}
		if !strings.HasPrefix(got, strings.Repeat("x", asyncShellHeadBytes)) {
			t.Error("head not preserved")
		}
		if !strings.HasSuffix(got, strings.Repeat("x", asyncShellTailBytes)) {
			t.Error("tail not preserved")
		}
		wantOmitted := 20000 - asyncShellHeadBytes - asyncShellTailBytes
		if !strings.Contains(got, fmt.Sprintf("%d bytes omitted", wantOmitted)) {
			t.Errorf("omitted count wrong, want %d: %.200q", wantOmitted, got)
		}
		if len(got) > 7000 {
			t.Errorf("preview %d bytes, want <= 7000", len(got))
		}
	})
	t.Run("bytes before the tail read count as omitted", func(t *testing.T) {
		head := strings.Repeat("h", asyncShellHeadBytes+10)
		got := shellOutputPreview(head, 1_000_000, strings.Repeat("t", 8000), "/tmp/y.log")
		if !strings.Contains(got, "full log: /tmp/y.log") {
			t.Fatalf("missing marker: %.100q", got)
		}
		if !strings.HasPrefix(got, strings.Repeat("h", asyncShellHeadBytes)+"\n") {
			t.Error("head not cut to head budget")
		}
	})
	t.Run("cuts stay valid UTF-8", func(t *testing.T) {
		body := strings.Repeat("ä€😀", 5000)
		got := shellOutputPreview("", 0, body, "/tmp/z.log")
		if !utf8.ValidString(got) {
			t.Fatal("preview is not valid UTF-8")
		}
	})
}

// finishedShellResult runs command as a notify_on_exit background job and
// returns the completion text the model receives.
func finishedShellResult(t *testing.T, command string) string {
	t.Helper()
	exe, err := exec.NewDirectExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer exe.Close()
	app := &App{
		Exec:    exe,
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
		Cfg:     config.DefaultConfig(),
	}
	args := fmt.Sprintf(`{"command":%q,"label":"t1","notify_on_exit":true}`, command)
	res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "run_background", Arguments: args,
	}})
	if !strings.Contains(res.text, "id: bg") {
		t.Fatalf("expected bg id, got: %s", res.text)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		app.asyncMu.Lock()
		n := len(app.asyncInbox)
		app.asyncMu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	app.asyncMu.Lock()
	defer app.asyncMu.Unlock()
	if len(app.asyncInbox) != 1 {
		t.Fatalf("asyncInbox has %d entries, want 1", len(app.asyncInbox))
	}
	op := app.asyncInbox[0]
	op.mu.Lock()
	defer op.mu.Unlock()
	return op.result
}

// Regression for the documented incident: the envelope for a finished job
// showed only the read_process_log hint (no exit code, no output).
func TestAsyncEnvelope_CarriesExitCodeAndOutput(t *testing.T) {
	got := finishedShellResult(t, "echo line-one; echo line-two; echo line-three; exit 3")
	for _, want := range []string{"exited with code 3", "line-one", "line-two", "line-three"} {
		if !strings.Contains(got, want) {
			t.Errorf("completion text missing %q:\n%s", want, got)
		}
	}
}

func TestAsyncEnvelope_LargeOutputHeadTailAndMarker(t *testing.T) {
	// ~200 KB: first line "HEAD-MARK", last line "TAIL-MARK".
	cmd := "echo HEAD-MARK; head -c 200000 /dev/zero | tr '\\0' x; echo; echo TAIL-MARK; exit 4"
	got := finishedShellResult(t, cmd)
	for _, want := range []string{"exited with code 4", "HEAD-MARK", "TAIL-MARK", "bytes omitted, full log: /tmp/wakil-bg-"} {
		if !strings.Contains(got, want) {
			t.Errorf("completion text missing %q (len %d)", want, len(got))
		}
	}
	// The fully rendered message must stay under both delivery caps.
	if len(got) >= asyncEnvelopeOpCap || len(got) >= asyncJobTabPreviewMaxBytes {
		t.Errorf("rendered completion %d bytes, want < %d", len(got), asyncEnvelopeOpCap)
	}
}
