//go:build !leanfake

package hisn

// toolpath_test.go: the run_shell TOOL path (agent.App.ExecuteToolCall →
// runShellWithDeadline → BackendExecutor → hisn Backend), not Backend.Exec.
// This is the path a user's run_shell call takes; Backend.Exec-level tests
// (TestIntegrationStartExec) cannot see a loss between the tool runner and the
// backend.
//
// The same cases run against the fake supervisor (always) and against a REAL
// hisn >= 0.3.1 session started from a generated profile (TestIntegrationToolPath;
// skips when hisn is missing/unusable):
//
//	go test -count=1 -v -timeout 10m -run 'TestIntegrationToolPath' ./internal/sandbox/hisn/

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/exec"
	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/sandbox"
)

// toolApp builds an App whose executor is the hisn-backed BackendExecutor,
// bound to a chat, exactly as wiring does (minus the TUI).
func toolApp(t *testing.T, b *Backend, shellTimeoutSec int) *agent.App {
	t.Helper()
	ex := exec.NewBackendExecutor(b, exec.BackendOptions{Label: "hisn"})
	t.Cleanup(func() { _ = ex.Close() })
	chat := fmt.Sprintf("toolpath-%d", shellTimeoutSec)
	if err := ex.BindChat(context.Background(), chat); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.ShellTimeoutSec = shellTimeoutSec
	return &agent.App{
		Cfg:     cfg,
		Client:  &proxy.Client{ChatID: chat},
		Exec:    ex,
		Out:     io.Discard,
		Confirm: func(string, string, string, bool) bool { return true },
	}
}

func callRunShell(t *testing.T, a *agent.App, command string) string {
	t.Helper()
	args, _ := json.Marshal(map[string]string{"command": command})
	res := a.ExecuteToolCall(context.Background(), proxy.ToolCall{
		ID: "tc", Type: "function",
		Function: proxy.FunctionCall{Name: "run_shell", Arguments: string(args)},
	})
	return res.String()
}

// runToolPathCases is the contract: output and exit status both arrive, for
// every size and stream, on both shell paths (auto-background and blocking).
func runToolPathCases(t *testing.T, a *agent.App) {
	t.Helper()
	const unknown = "exit code unknown"

	t.Run("output and exit code", func(t *testing.T) {
		got := callRunShell(t, a, "echo hi; exit 3")
		if strings.Contains(got, unknown) || !strings.Contains(got, "hi") || !strings.Contains(got, "exit status 3") {
			t.Fatalf("want output %q and %q, got %q", "hi", "exit status 3", got)
		}
	})
	t.Run("success with output", func(t *testing.T) {
		got := callRunShell(t, a, "echo hello-world")
		if strings.HasPrefix(got, "ERROR") || strings.Contains(got, unknown) || !strings.Contains(got, "hello-world") {
			t.Fatalf("want plain output, got %q", got)
		}
	})
	t.Run("200 KB output", func(t *testing.T) {
		got := callRunShell(t, a, "head -c 204800 /dev/zero | tr '\\0' x; echo; echo tail-line")
		if strings.HasPrefix(got, "ERROR") || strings.Contains(got, unknown) {
			t.Fatalf("large output must succeed, got prefix %q", head(got))
		}
		if n := strings.Count(got, "x"); n < 204800 {
			t.Fatalf("lost output: %d of 204800 bytes arrived", n)
		}
		if !strings.Contains(got, "tail-line") {
			t.Fatalf("tail of the output missing: …%q", got[max(0, len(got)-80):])
		}
	})
	t.Run("stderr only", func(t *testing.T) {
		got := callRunShell(t, a, "echo err-only >&2")
		if strings.HasPrefix(got, "ERROR") || strings.Contains(got, unknown) || !strings.Contains(got, "err-only") {
			t.Fatalf("stderr must be captured and the exit status 0, got %q", got)
		}
	})
	t.Run("stderr only and failing", func(t *testing.T) {
		got := callRunShell(t, a, "echo err-fail >&2; exit 2")
		if strings.Contains(got, unknown) || !strings.Contains(got, "err-fail") || !strings.Contains(got, "exit status 2") {
			t.Fatalf("want stderr and exit status 2, got %q", got)
		}
	})
	t.Run("no output", func(t *testing.T) {
		if got := callRunShell(t, a, "true"); strings.Contains(got, unknown) || strings.HasPrefix(got, "ERROR") {
			t.Fatalf("got %q", got)
		}
	})
}

func head(s string) string {
	if len(s) > 120 {
		return s[:120]
	}
	return s
}

// Both shell paths: shell_timeout_sec > 0 (auto-background, the default) and
// 0 (blocking RunShell).
func TestToolPathFake(t *testing.T) {
	for _, sec := range []int{10, 0} {
		t.Run(fmt.Sprintf("shell_timeout_sec=%d", sec), func(t *testing.T) {
			b, _ := openFake(t)
			runToolPathCases(t, toolApp(t, b, sec))
		})
	}
}

// Real hisn, generated profile (the default path on a host with hisn >= 0.3.1).
func TestIntegrationToolPath(t *testing.T) {
	for _, sec := range []int{10, 0} {
		t.Run(fmt.Sprintf("shell_timeout_sec=%d", sec), func(t *testing.T) {
			b, _, err := realProfileBackend(t, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !b.profileMode() {
				t.Fatal("hisn >= 0.3.1 must start in profile mode")
			}
			runToolPathCases(t, toolApp(t, b, sec))
		})
	}
}

// slowWriter models a consumer that is slower than the program (a busy copier,
// a TUI sink): the exit line reaches Wakil while output is still in the pipe.
type slowWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *slowWriter) Write(p []byte) (int, error) {
	time.Sleep(50 * time.Millisecond)
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *slowWriter) Len() int { w.mu.Lock(); defer w.mu.Unlock(); return w.buf.Len() }

// The exit status must not be reported before the output is drained to EOF:
// every byte the program wrote arrives, whatever the consumer's speed.
func TestExecDrainsOutputBeforeExit(t *testing.T) {
	b, _ := openFake(t)
	if err := b.Start(ctxT(t), sandbox.Session{ID: "drain", HostCwd: b.opts.HostCwd}); err != nil {
		t.Fatal(err)
	}
	const total = 300 << 10
	for _, merged := range []bool{false, true} {
		w := &slowWriter{}
		spec := sandbox.ExecSpec{Argv: []string{"sh", "-c", fmt.Sprintf("head -c %d /dev/zero | tr '\\0' x; echo oops >&2; exit 3", total)}, Stdout: w}
		if merged {
			spec.Stderr = w // BackendExecutor.run passes the same writer for both
		}
		h, err := b.Exec(ctxT(t), spec)
		if err != nil {
			t.Fatal(err)
		}
		info, err := h.Wait()
		if err != nil {
			t.Fatal(err)
		}
		if info.Code != 3 || info.Lost || info.BackendError {
			t.Fatalf("merged=%v: exit %+v, want code 3", merged, info)
		}
		want := total
		if merged {
			want += len("oops\n")
		}
		if got := w.Len(); got != want {
			t.Fatalf("merged=%v: %d of %d bytes arrived before Wait returned", merged, got, want)
		}
	}
}

// A child that inherited the pipes and outlives the program holds them open:
// the drain is bounded (idle), the output written so far still arrives, and
// the program's own exit status is reported.
func TestExecDrainBoundedByStrayChild(t *testing.T) {
	b, _ := openFake(t)
	if err := b.Start(ctxT(t), sandbox.Session{ID: "stray", HostCwd: b.opts.HostCwd}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	h, err := b.Exec(ctxT(t), sandbox.ExecSpec{Argv: []string{"sh", "-c", "sleep 20 & echo before; exit 4"}, Stdout: &out, Stderr: &out})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	info, err := h.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("Wait hung %v on a pipe held by a stray child", d)
	}
	if info.Code != 4 || !strings.Contains(out.String(), "before") {
		t.Fatalf("exit %+v, output %q: want code 4 and %q", info, out.String(), "before")
	}
}
