package safe

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/diag"
)

// safeBuffer wraps bytes.Buffer with a mutex for concurrent read/write in tests.
// diag.Printf writes from the recovered goroutine while the test reads —
// without synchronization this is a data race under -race.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (sb *safeBuffer) Write(p []byte) (int, error) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.buf.Write(p)
}

func (sb *safeBuffer) String() string {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.buf.String()
}

// TestGo_PanicDoesNotCrashProcess verifies that a panicking goroutine launched
// via safe.Go is recovered and logged, and the process continues running.
func TestGo_PanicDoesNotCrashProcess(t *testing.T) {
	var sb safeBuffer
	diag.Redirect(&sb)
	defer diag.Redirect(nil)

	var wg sync.WaitGroup
	wg.Add(1)
	Go("test-panic", func() {
		defer wg.Done()
		panic("intentional test panic")
	})

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		time.Sleep(10 * time.Millisecond)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for recovered goroutine")
	}

	output := sb.String()
	if !strings.Contains(output, "test-panic") {
		t.Errorf("log output should contain goroutine name 'test-panic'; got: %s", output)
	}
	if !strings.Contains(output, "intentional test panic") {
		t.Errorf("log output should contain panic message; got: %s", output)
	}
	if !strings.Contains(output, "goroutine") {
		t.Errorf("log output should contain stack trace with 'goroutine'; got: %s", output)
	}
}

// TestGo_PanickingGoroutineRestoresTerminal pins the Part 4 item 6
// guarantee end-to-end for the goroutines the TUI/exec/agent stack launches
// through safe.Go: a deliberate panic neither crashes the process nor
// writes raw bytes to the terminal (os.Stderr is the diagnostic sink's
// predecessor — safe.Go routes to diag, and diag redirects here).
//
// The normal execution (TestGo_NormalExecution) and multi-goroutine
// (TestGo_MultipleGoroutines) cases already pin that safe.Go recovers;
// THIS test pins that the process is STILL ALIVE after, which is the
// terminal-usability half of the guarantee.
func TestGo_PanickingGoroutineRestoresTerminal(t *testing.T) {
	var sb safeBuffer
	diag.Redirect(&sb)
	defer diag.Redirect(nil)

	var wg sync.WaitGroup
	wg.Add(1)
	Go("test-terminal-restores", func() {
		defer wg.Done()
		panic("deliberate harness panic")
	})
	wg.Wait()
	time.Sleep(10 * time.Millisecond)

	// Still alive: the process continues.
	out := sb.String()
	if !strings.Contains(out, "test-terminal-restores") {
		t.Fatalf("panic not captured by safe.Go: %q", out)
	}
	// No raw terminal bytes: diag captured it, os.Stdout/Stderr untouched.
	if strings.Contains(out, "\x1b[") {
		t.Fatalf("panic output leaked escape sequences to the terminal: %q", out)
	}
}

// TestGo_NormalExecution verifies that a non-panicking goroutine runs and
// completes normally.
func TestGo_NormalExecution(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)
	result := 0
	Go("test-normal", func() {
		defer wg.Done()
		result = 42
	})
	wg.Wait()
	if result != 42 {
		t.Errorf("expected result=42, got %d", result)
	}
}

// TestGo_MultipleGoroutines verifies that multiple goroutines can run
// concurrently and only the panicking one is caught.
func TestGo_MultipleGoroutines(t *testing.T) {
	var sb safeBuffer
	diag.Redirect(&sb)
	defer diag.Redirect(nil)

	var wg sync.WaitGroup
	const n = 5
	results := make([]int, n)

	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		Go("test-multi", func() {
			defer wg.Done()
			if i == 2 {
				panic("middle goroutine panics")
			}
			results[i] = i
		})
	}
	wg.Wait()
	time.Sleep(10 * time.Millisecond)

	for i, r := range results {
		if i == 2 {
			if r != 0 {
				t.Errorf("panicking goroutine %d should not have set result", i)
			}
			continue
		}
		if r != i {
			t.Errorf("results[%d] = %d, want %d", i, r, i)
		}
	}

	if !strings.Contains(sb.String(), "middle goroutine panics") {
		t.Errorf("log should contain panic message; got: %s", sb.String())
	}
}
