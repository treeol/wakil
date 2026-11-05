package exec

// B1 (review): only a conclusive probe answer is cached. A cancelled probe
// must not pin "sh" for the generation; a clean "no bash" must.

import (
	"context"
	"errors"
	"fmt"
	osexec "os/exec"
	"testing"
)

func TestUserShellCache_InconclusiveNotCached(t *testing.T) {
	var c userShellCache
	calls := 0
	probe := func() (bool, bool) {
		calls++
		if calls == 1 {
			return false, false // timeout: inconclusive
		}
		return true, true
	}
	if got := c.get(1, probe); got != "sh" {
		t.Fatalf("inconclusive probe must answer sh for this call, got %q", got)
	}
	if got := c.get(1, probe); got != "bash" {
		t.Fatalf("after an inconclusive probe the next call must probe again and get bash, got %q (cache pinned to sh)", got)
	}
	if calls != 2 {
		t.Errorf("probe ran %d times, want 2", calls)
	}
}

func TestUserShellCache_ConclusiveNoBashIsCached(t *testing.T) {
	var c userShellCache
	calls := 0
	probe := func() (bool, bool) { calls++; return false, true }
	c.get(1, probe)
	c.get(1, probe)
	if calls != 1 {
		t.Errorf("a conclusive answer must be cached: probe ran %d times", calls)
	}
	c.get(2, probe) // generation change re-probes
	if calls != 2 {
		t.Errorf("generation change must re-probe: %d", calls)
	}
}

// exitErr returns a real *exec.ExitError with the given status.
func exitErr(t *testing.T, code int) error {
	t.Helper()
	cmd := osexec.Command("sh", "-c", fmt.Sprintf("exit %d", code))
	err := cmd.Run()
	var ee *osexec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("no exit error for %d: %v", code, err)
	}
	return ee
}

func TestProbeVerdict(t *testing.T) {
	ctx := context.Background()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	cases := []struct {
		name    string
		pctx    context.Context
		err     error
		hasBash bool
		ok      bool
	}{
		{"success", ctx, nil, true, true},
		{"bash absent (exit 1)", ctx, &ExitStatusError{Code: 1}, false, true},
		{"exit 2 is not a clean no", ctx, &ExitStatusError{Code: 2}, false, false},
		{"docker exec: bash absent (os/exec exit 1)", ctx, exitErr(t, 1), false, true},
		{"docker exec: container gone (exit 125)", ctx, exitErr(t, 125), false, false},
		{"transport error", ctx, fmt.Errorf("docker: boom"), false, false},
		{"cancelled probe", cancelled, fmt.Errorf("signal: killed"), false, false},
	}
	for _, c := range cases {
		hb, ok := probeVerdict(c.pctx, c.err)
		if hb != c.hasBash || ok != c.ok {
			t.Errorf("%s: got (%v,%v) want (%v,%v)", c.name, hb, ok, c.hasBash, c.ok)
		}
	}
}
