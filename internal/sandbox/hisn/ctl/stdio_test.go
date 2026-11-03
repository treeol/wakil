package ctl

import (
	"os"
	"syscall"
	"testing"
)

// When a later os.Pipe fails, NewStdio must close the pipes it already made.
// The test exhausts the fd table down to exactly two free slots: the first
// pipe (2 fds) succeeds, the second cannot be created. A leak would leave no
// free slot afterwards.
func TestNewStdioClosesEarlierPipesOnFailure(t *testing.T) {
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &old); err != nil {
		t.Skipf("getrlimit: %v", err)
	}
	// Find the current highest-used fd so the lowered limit is above it.
	var probes []*os.File
	defer func() {
		for _, f := range probes {
			_ = f.Close()
		}
	}()
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	probes = append(probes, null)
	limit := uint64(null.Fd()) + 8
	if limit > old.Max {
		t.Skip("cannot lower RLIMIT_NOFILE here")
	}
	lowered := syscall.Rlimit{Cur: limit, Max: old.Max}
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lowered); err != nil {
		t.Skipf("setrlimit: %v", err)
	}
	defer func() { _ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &old) }()

	// Fill every free slot, then give exactly two back.
	for {
		f, err := os.Open(os.DevNull)
		if err != nil {
			break
		}
		probes = append(probes, f)
	}
	if len(probes) < 3 {
		t.Skip("fd table too small to stage the failure")
	}
	for i := 0; i < 2; i++ {
		last := probes[len(probes)-1]
		probes = probes[:len(probes)-1]
		_ = last.Close()
	}

	if s, err := NewStdio(); err == nil {
		s.CloseEnds()
		t.Fatal("NewStdio succeeded with only two free fds; the failure was not staged")
	}
	// The failed call must have released the first pipe: two slots are free again.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("NewStdio leaked the earlier pipe's descriptors: %v", err)
	}
	_ = r.Close()
	_ = w.Close()
}
