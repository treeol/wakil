package wiring

// sessions_test.go: covers the package-level session wrappers (sessions.go)
// that keep internal/agent out of package main (card #148 Gate #1, cmd half).
// Fixtures use the same pattern as internal/agent/session_scope_test.go:
// an isolated WAKIL_SESSIONS_DIR plus agent.WriteSession. The wrappers
// delegate verbatim, so these tests pin the contract main.go relies on —
// not agent internals.

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/agent"
)

// seedSessions writes one session per (id, workspace) pair with staggered
// Updated timestamps so "most recent" is deterministic: later seeds are newer.
func seedSessions(t *testing.T, pairs ...agent.Session) {
	t.Helper()
	base := time.Now().Add(-time.Hour)
	for i := range pairs {
		s := pairs[i]
		s.Updated = base.Add(time.Duration(i) * time.Minute)
		if err := agent.WriteSession(&s); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPrintSessionsScopedVsAll(t *testing.T) {
	t.Setenv("WAKIL_SESSIONS_DIR", t.TempDir())
	wsA, wsB := t.TempDir(), t.TempDir()
	seedSessions(t,
		agent.Session{ChatID: "aaaa1111-first", Workspace: wsA},
		agent.Session{ChatID: "bbbb2222-second", Workspace: wsB},
	)

	var scoped bytes.Buffer
	PrintSessions(&scoped, wsA, false)
	if !strings.Contains(scoped.String(), "aaaa1111") {
		t.Errorf("scoped listing should include wsA's session; got:\n%s", scoped.String())
	}
	if strings.Contains(scoped.String(), "bbbb2222") {
		t.Errorf("scoped listing must not show wsB's session; got:\n%s", scoped.String())
	}
	if !strings.Contains(scoped.String(), "1 session(s) in other folders hidden") {
		t.Errorf("scoped listing should report the hidden count; got:\n%s", scoped.String())
	}

	var all bytes.Buffer
	PrintSessions(&all, wsA, true)
	if !strings.Contains(all.String(), "aaaa1111") || !strings.Contains(all.String(), "bbbb2222") {
		t.Errorf("all=true listing should show every session; got:\n%s", all.String())
	}
	if strings.Contains(all.String(), "hidden") {
		t.Errorf("all=true listing must not report hidden sessions; got:\n%s", all.String())
	}
}

func TestPrintSessionsEmptyDir(t *testing.T) {
	t.Setenv("WAKIL_SESSIONS_DIR", t.TempDir())
	ws := t.TempDir()

	var out bytes.Buffer
	PrintSessions(&out, ws, false)
	if !strings.Contains(out.String(), "no saved sessions") {
		t.Errorf("empty dir should print a no-sessions notice; got:\n%s", out.String())
	}
}

func TestResolveRecentSession(t *testing.T) {
	t.Setenv("WAKIL_SESSIONS_DIR", t.TempDir())
	wsA, wsB := t.TempDir(), t.TempDir()
	seedSessions(t,
		agent.Session{ChatID: "oldA0001-in-wsA", Workspace: wsA},
		agent.Session{ChatID: "newA0002-in-wsA", Workspace: wsA},
		agent.Session{ChatID: "newest03-in-wsB", Workspace: wsB},
	)

	// Scoped: most recent in wsA (seeded later ⇒ newer), never wsB's.
	got, err := ResolveRecentSession(wsA, false)
	if err != nil {
		t.Fatal(err)
	}
	if got != "newA0002-in-wsA" {
		t.Errorf("scoped resolve = %q, want newA0002-in-wsA", got)
	}

	// All: newest overall lives in wsB.
	got, err = ResolveRecentSession(wsA, true)
	if err != nil {
		t.Fatal(err)
	}
	if got != "newest03-in-wsB" {
		t.Errorf("all-scope resolve = %q, want newest03-in-wsB", got)
	}
}

func TestResolveRecentSessionNoneInScope(t *testing.T) {
	t.Setenv("WAKIL_SESSIONS_DIR", t.TempDir())
	wsA, wsB := t.TempDir(), t.TempDir()
	seedSessions(t, agent.Session{ChatID: "onlyInB001", Workspace: wsB})

	// Empty scope: error, not a silent "" (main.go treats "" as no-resume).
	got, err := ResolveRecentSession(wsA, false)
	if err == nil {
		t.Fatalf("expected an error when no session matches the workspace; got id=%q", got)
	}
	if !strings.Contains(err.Error(), "no saved sessions for") {
		t.Errorf("error should mention the workspace scope; got: %v", err)
	}
}

func TestShortID(t *testing.T) {
	cases := []struct{ in, want string }{
		{"abcdef1234567890", "abcdef12"},
		{"short", "short"},   // fewer than 8 chars passes through
		{"12345678", "12345678"}, // exactly 8 stays unchanged
		{"", ""},
	}
	for _, c := range cases {
		if got := ShortID(c.in); got != c.want {
			t.Errorf("ShortID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
