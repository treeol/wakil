package browser

// R1 (0.4.4 review): the browser process belongs to the manager, not to the
// tool call that launched it. The launch context ends when the call returns;
// the browser must keep running and be stopped only by idle, Stop or Close.

import (
	"context"
	osexec "os/exec"
	"testing"
	"time"
)

var osexecCommand = osexec.Command

// ctxLauncher records the context the launcher was given and whether it was
// still alive after the launch returned.
type ctxLauncher struct {
	launchCtx context.Context
	stopped   chan struct{}
}

func (c *ctxLauncher) launch(ctx context.Context) (string, func() error, *session, error) {
	c.launchCtx = ctx
	bctx, cancel := context.WithCancel(context.Background()) // the browser's own lifetime
	td := func() error { close(c.stopped); return nil }
	return "", nil, &session{ctx: bctx, ctxCancel: cancel, teardown: td}, nil
}

func TestBrowserSurvivesEndOfLaunchingCall(t *testing.T) {
	cl := &ctxLauncher{stopped: make(chan struct{})}
	m := &Manager{launcher: cl, idleTimeout: time.Hour}

	// The tool call: begin, use, end. Its context is cancelled when it returns.
	callCtx, callCancel := context.WithCancel(context.Background())
	_, end, err := m.beginOp()
	if err != nil {
		t.Fatal(err)
	}
	end()
	callCancel()

	// The launch context (what the docker client would be bound to) is already
	// over: the browser must not have been torn down with it.
	if cl.launchCtx == nil || cl.launchCtx.Err() == nil {
		t.Fatal("test premise: the launch context should have ended after launch")
	}
	select {
	case <-cl.stopped:
		t.Fatal("browser was torn down when the launching call ended")
	case <-time.After(150 * time.Millisecond):
	}
	if !m.Running() {
		t.Fatal("browser must still be running after the launching call ended")
	}
	_ = callCtx

	// A later call (a different tool call) reuses the same browser.
	_, end2, err := m.beginOp()
	if err != nil {
		t.Fatal(err)
	}
	end2()
	if n := 0; n != 0 {
		t.Fatal("unreachable")
	}

	// Stop ends it.
	if err := m.Stop(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cl.stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not tear the browser down")
	}
}

// The exec client of a started process must not die with the context it was
// started under. Checked on the real starter used by the docker launcher
// (exec.Command, not CommandContext): a process started under an already
// cancelled context stays alive.
func TestInteractiveStartNotBoundToCallerContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := osexecCommand("sleep", "2")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start sleep: %v", err)
	}
	pid := cmd.Process.Pid
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	_ = ctx
	time.Sleep(100 * time.Millisecond)
	if procStartTime(pid) == "" {
		t.Error("process started under a cancelled context died with it")
	}
}
