package safe

import (
	"bytes"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

// safeBuffer wraps bytes.Buffer with a mutex for concurrent read/write in tests.
// log.Printf writes from the recovered goroutine while the test reads — without
// synchronization this is a data race under -race.
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
	log.SetOutput(&sb)
	defer log.SetOutput(nil)

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
	log.SetOutput(&sb)
	defer log.SetOutput(nil)

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
