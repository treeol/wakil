package hisn

import (
	"bytes"
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/sandbox"
)

// pidGone reports whether pid no longer exists (polls briefly: the fake reaps
// the program asynchronously).
func pidGone(pid int) bool {
	for i := 0; i < 50; i++ {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// A ctl exec whose ctx ends AFTER acceptance is stopped through the
// TERM → grace → KILL sequence by exec_id, and Wait returns promptly with the
// real (signalled) status — not "lost", not hung.
func TestCtlCancelAfterAcceptStopsExec(t *testing.T) {
	b, fe := openFake(t)
	if err := b.Start(ctxT(t), sandbox.Session{ID: "cx", HostCwd: fe.cwd}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out bytes.Buffer
	h, err := b.Exec(ctx, sandbox.ExecSpec{Argv: []string{"sleep", "60"}, Stdout: &out})
	if err != nil {
		t.Fatal(err)
	}
	pid := h.Pid()
	if pid <= 0 {
		t.Fatalf("accepted exec has no pid: %d", pid)
	}
	start := time.Now()
	cancel()
	type res struct {
		info sandbox.ExitInfo
		err  error
	}
	done := make(chan res, 1)
	go func() { i, e := h.Wait(); done <- res{i, e} }()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Wait: %v", r.err)
		}
		if r.info.Lost || r.info.Code != 128+15 {
			t.Fatalf("exit = %+v, want the latched SIGTERM status (143)", r.info)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("Wait still blocked 6s after the context was cancelled")
	}
	if d := time.Since(start); d > 6*time.Second {
		t.Fatalf("cancel took %v", d)
	}
	if !pidGone(pid) {
		t.Fatalf("program %d still running after cancel", pid)
	}
}

// A program that ignores TERM is KILLed after the grace period.
func TestCtlCancelAfterAcceptEscalatesToKill(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the 5 s TERM grace")
	}
	b, fe := openFake(t)
	if err := b.Start(ctxT(t), sandbox.Session{ID: "cxk", HostCwd: fe.cwd}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := b.Exec(ctx, sandbox.ExecSpec{Argv: []string{"sh", "-c", "trap '' TERM; while :; do sleep 1; done"}})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // let the trap install
	cancel()
	done := make(chan sandbox.ExitInfo, 1)
	go func() { i, _ := h.Wait(); done <- i }()
	select {
	case info := <-done:
		if info.Lost || info.Code != 128+9 {
			t.Fatalf("exit = %+v, want SIGKILL (137)", info)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("TERM-ignoring exec not killed after cancel")
	}
}

// A caller deadline that ends the exec's context does the same, and an exec
// that finishes BEFORE its deadline keeps its real status (the deadline does
// not turn it into "lost").
func TestCtlDeadlineAfterAccept(t *testing.T) {
	b, fe := openFake(t)
	if err := b.Start(ctxT(t), sandbox.Session{ID: "cd", HostCwd: fe.cwd}); err != nil {
		t.Fatal(err)
	}
	t.Run("deadline stops a long exec", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
		defer cancel()
		h, err := b.Exec(ctx, sandbox.ExecSpec{Argv: []string{"sleep", "60"}})
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan sandbox.ExitInfo, 1)
		go func() { i, _ := h.Wait(); done <- i }()
		select {
		case info := <-done:
			if info.Lost || info.Code != 128+15 {
				t.Fatalf("exit = %+v, want 143", info)
			}
		case <-time.After(6 * time.Second):
			t.Fatal("exec outlived its deadline")
		}
	})
	t.Run("exit before the deadline keeps its status", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		h, err := b.Exec(ctx, sandbox.ExecSpec{Argv: []string{"sh", "-c", "sleep 1; exit 7"}})
		if err != nil {
			t.Fatal(err)
		}
		info, err := h.Wait()
		if err != nil || info.Lost || info.Code != 7 {
			t.Fatalf("exit = %+v err=%v, want 7", info, err)
		}
	})
}
