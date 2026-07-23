package browser

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestNilManager_Navigate verifies that all operations on a nil Manager
// return the "not initialized" error instead of panicking.
func TestNilManager_OperationsReturnError(t *testing.T) {
	var m *Manager

	// Navigate
	_, _, err := m.Navigate(context.Background(), "http://localhost")
	if err == nil || err.Error() != "browser: manager is not initialized" {
		t.Errorf("Navigate: expected 'not initialized' error, got: %v", err)
	}

	// Screenshot
	_, err = m.Screenshot(context.Background(), false)
	if err == nil || err.Error() != "browser: manager is not initialized" {
		t.Errorf("Screenshot: expected 'not initialized' error, got: %v", err)
	}

	// SetViewport
	err = m.SetViewport(context.Background(), 800, 600)
	if err == nil || err.Error() != "browser: manager is not initialized" {
		t.Errorf("SetViewport: expected 'not initialized' error, got: %v", err)
	}

	// EmulateReducedMotion
	err = m.EmulateReducedMotion(context.Background(), true)
	if err == nil || err.Error() != "browser: manager is not initialized" {
		t.Errorf("EmulateReducedMotion: expected 'not initialized' error, got: %v", err)
	}

	// Click
	err = m.Click(context.Background(), "button")
	if err == nil || err.Error() != "browser: manager is not initialized" {
		t.Errorf("Click: expected 'not initialized' error, got: %v", err)
	}

	// EvalJS
	_, err = m.EvalJS(context.Background(), "1+1")
	if err == nil || err.Error() != "browser: manager is not initialized" {
		t.Errorf("EvalJS: expected 'not initialized' error, got: %v", err)
	}

	// GetText
	_, err = m.GetText(context.Background(), "h1")
	if err == nil || err.Error() != "browser: manager is not initialized" {
		t.Errorf("GetText: expected 'not initialized' error, got: %v", err)
	}

	// GetHTML
	_, err = m.GetHTML(context.Background(), "")
	if err == nil || err.Error() != "browser: manager is not initialized" {
		t.Errorf("GetHTML: expected 'not initialized' error, got: %v", err)
	}
}

func TestNilManager_Close(t *testing.T) {
	var m *Manager
	if err := m.Close(); err != nil {
		t.Errorf("Close on nil manager should return nil, got: %v", err)
	}
}

func TestScreenshotDir(t *testing.T) {
	dir := ScreenshotDir()
	if dir == "" {
		t.Fatal("ScreenshotDir returned empty string")
	}
	// Should be the system temp dir.
	if dir != filepath.Join(os.TempDir()) {
		t.Errorf("ScreenshotDir = %q, want %q", dir, filepath.Join(os.TempDir()))
	}
}

func TestManager_CloseWithUninitializedFields(t *testing.T) {
	// Manager with nil cancel funcs and empty userDataDir — Close should not panic.
	m := &Manager{}
	if err := m.Close(); err != nil {
		t.Errorf("Close with uninitialized fields: %v", err)
	}
}

func TestManager_opCtx_NilCallerCtx(t *testing.T) {
	// opCtx with nil caller ctx should still produce a usable context.
	// We can't fully test this without a real browser ctx, but we can
	// verify it doesn't panic when m.ctx is a valid context.
	m := &Manager{
		ctx: context.Background(),
	}
	octx, cancel := m.opCtx(context.TODO())
	defer cancel()
	if octx == nil {
		t.Fatal("opCtx returned nil context")
	}
	// The context should have a deadline (timeout applied).
	if _, ok := octx.Deadline(); !ok {
		t.Error("opCtx should have a deadline (timeout)")
	}
}

func TestManager_opCtx_WithCallerCtx(t *testing.T) {
	m := &Manager{
		ctx: context.Background(),
	}
	callerCtx, callerCancel := context.WithCancel(context.Background())
	defer callerCancel()

	octx, cancel := m.opCtx(callerCtx)
	defer cancel()
	if octx == nil {
		t.Fatal("opCtx returned nil context")
	}

	// Canceling the caller ctx should cause the opCtx to be done.
	callerCancel()
	<-octx.Done()
	// If we reach here, the context was canceled correctly.
}

func TestManager_opCtx_CallerCancelPropagates(t *testing.T) {
	m := &Manager{
		ctx: context.Background(),
	}

	callerCtx, callerCancel := context.WithCancel(context.Background())
	octx, cancel := m.opCtx(callerCtx)
	defer cancel()

	// The opCtx should not be done yet.
	select {
	case <-octx.Done():
		t.Fatal("opCtx done before caller cancel")
	default:
	}

	// Give the bridge goroutine time to start.
	// It bridges caller cancellation into the merged context.
	// Cancel the caller — opCtx should become done.
	callerCancel()

	// Wait for propagation (goroutine needs to wake on ctx.Done).
	// Use a timeout to avoid hanging if the bridge fails.
	done := make(chan struct{})
	go func() {
		<-octx.Done()
		close(done)
	}()
	select {
	case <-done:
		// Good — cancellation propagated.
	case <-time.After(2 * time.Second):
		t.Fatal("opCtx did not propagate caller cancellation within 2s")
	}
}
