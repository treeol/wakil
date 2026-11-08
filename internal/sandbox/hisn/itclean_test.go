package hisn

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	osexec "os/exec"
	"strings"
	"testing"
	"time"
)

// Session hygiene for the integration tests against a REAL hisn. Every test
// that starts sessions registers cleanupSessions (realBackend and
// realProfileBackend do it), and TestMain sweeps leftovers of earlier runs at
// start and end of an integration run, so `hisn ls` is empty after `go test`
// whatever failed.
//
// Scope: per test, exactly the names of that test's backend (the name prefix
// "wakil-<key>-" where key hashes the test's own t.TempDir() workspace); in
// the sweep, the ids the suite uses (wakil-it-*, toolpath-*). Pruning is by
// name prefix on hisn >= 0.4.1; on 0.4.0, and for unnamed sessions, it falls
// back to `hisn ls --prune`, which archives every PROVABLY DEAD directory of
// the user (never a live one) — the same prune Wakil runs at every start.

// itSweepPrefixes are the session ids the integration tests use.
var itSweepPrefixes = []string{"wakil-it-", "toolpath-"}

// itHisnBin is the real hisn the integration tests use: WAKIL_HISN_BIN, else
// the hisn on PATH; "" when there is none.
func itHisnBin() string {
	if bin := os.Getenv("WAKIL_HISN_BIN"); bin != "" {
		return bin
	}
	p, err := osexec.LookPath("hisn")
	if err != nil {
		return ""
	}
	return p
}

type itSession struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
}

func (s itSession) over() bool {
	return s.State == "stopped" || s.State == "crashed" || s.State == "stale"
}

// itList is `hisn ls --json`; an error means the listing could not be read
// (never an empty list).
func itList(ctx context.Context, bin string) ([]itSession, error) {
	out, err := osexec.CommandContext(ctx, bin, "ls", "--json").Output()
	if err != nil {
		return nil, fmt.Errorf("hisn ls --json: %w", err)
	}
	var doc struct {
		Schema   string      `json:"schema"`
		Sessions []itSession `json:"sessions"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("hisn ls --json: %w", err)
	}
	if !lsSchemas[doc.Schema] {
		return nil, fmt.Errorf("hisn ls --json: unsupported schema %q", doc.Schema)
	}
	return doc.Sessions, nil
}

// itSupportsNamePrefix reports whether bin has `hisn ls --prune --name-prefix`
// (hisn >= 0.4.1).
func itSupportsNamePrefix(ctx context.Context, bin string) bool {
	out, err := osexec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return false
	}
	v, err := ParseVersion(string(out))
	return err == nil && VersionAtLeast(v, [3]int{0, 4, 1})
}

// itStopAndPrune stops every matching session that is not over yet, waits
// until they are over (a "stopping" one is not prunable), then prunes them. It
// returns the matching sessions still listed afterwards.
func itStopAndPrune(bin string, match func(itSession) bool, prefixes []string) ([]itSession, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	list, err := itList(ctx, bin)
	if err != nil {
		return nil, err
	}
	unnamed, matched := false, false
	for _, s := range list {
		if !match(s) {
			continue
		}
		matched = true
		if s.Name == "" {
			unnamed = true
		}
		if !s.over() {
			_ = osexec.CommandContext(ctx, bin, "stop", "--session", s.ID).Run()
		}
	}
	if !matched {
		return nil, nil
	}
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		list, err = itList(ctx, bin)
		if err != nil {
			return nil, err
		}
		pending := false
		for _, s := range list {
			if match(s) && !s.over() {
				pending = true
			}
		}
		if !pending || time.Now().After(end) {
			break
		}
	}
	if !itSupportsNamePrefix(ctx, bin) || unnamed || len(prefixes) == 0 {
		_ = osexec.CommandContext(ctx, bin, "ls", "--prune").Run()
	} else {
		for _, p := range prefixes {
			_ = osexec.CommandContext(ctx, bin, "ls", "--prune", "--name-prefix", p).Run()
		}
	}
	if list, err = itList(ctx, bin); err != nil {
		return nil, err
	}
	var left []itSession
	for _, s := range list {
		if match(s) {
			left = append(left, s)
		}
	}
	return left, nil
}

// cleanupSessions registers, for this test, the stop and prune of every
// session b started (they share b's name prefix, unique to the test's
// workspace). Registered BEFORE b.Close's cleanup, so it runs after it
// (cleanups are LIFO): Close stops what it tracks, this catches what it did
// not and prunes the stopped directories. A leftover fails the test.
func cleanupSessions(t *testing.T, b *Backend) {
	t.Helper()
	prefix := strings.TrimSuffix(b.sessionName("x"), "x")
	bin := b.opts.Binary
	t.Cleanup(func() {
		left, err := itStopAndPrune(bin, func(s itSession) bool { return strings.HasPrefix(s.Name, prefix) }, []string{prefix})
		if err != nil {
			t.Errorf("session cleanup: %v", err)
		}
		for _, s := range left {
			t.Errorf("session %s (%s) left behind in state %s", s.ID, s.Name, s.State)
		}
	})
}

// integrationRun reports whether this `go test` selects integration tests
// (-test.run naming TestIntegration). Unit runs never touch real hisn state.
func integrationRun() bool {
	f := flag.Lookup("test.run")
	return f != nil && strings.Contains(f.Value.String(), "Integration")
}

// sweepITSessions stops and prunes leftovers of integration tests (ids
// wakil-it-* and toolpath-*). TestMain runs it before and after an
// integration run; it returns the leftovers it could not remove.
func sweepITSessions() ([]itSession, error) {
	bin := itHisnBin()
	if bin == "" || !integrationRun() {
		return nil, nil
	}
	match := func(s itSession) bool {
		for _, p := range itSweepPrefixes {
			if strings.HasPrefix(s.ID, p) {
				return true
			}
		}
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	list, err := itList(ctx, bin)
	if err != nil {
		// No usable hisn here (the integration tests skip): nothing to sweep.
		return nil, nil
	}
	var names []string
	for _, s := range list {
		if match(s) && s.Name != "" {
			names = append(names, s.Name)
		}
	}
	return itStopAndPrune(bin, match, names)
}
