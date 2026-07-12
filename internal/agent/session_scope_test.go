package agent

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/proxy"
)

// TestListSessionsScoped_FiltersByWorkspace verifies that a scoped listing
// only returns sessions whose Workspace canonically matches, and reports the
// hidden count for the rest.
func TestListSessionsScoped_FiltersByWorkspace(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WAKIL_SESSIONS_DIR", dir)

	wsA := t.TempDir()
	wsB := t.TempDir()

	inA := &Session{ChatID: "inA1111a", Workspace: wsA, Updated: time.Now()}
	inB := &Session{ChatID: "inB2222b", Workspace: wsB, Updated: time.Now().Add(-time.Minute)}
	noWS := &Session{ChatID: "noWS3333", Updated: time.Now().Add(-2 * time.Minute)} // legacy, empty workspace
	for _, s := range []*Session{inA, inB, noWS} {
		if err := WriteSession(s); err != nil {
			t.Fatal(err)
		}
	}

	matched, hidden, err := ListSessionsScoped(SessionScope{Workspace: wsA})
	if err != nil {
		t.Fatal(err)
	}
	if len(matched) != 1 || matched[0].ChatID != "inA1111a" {
		t.Fatalf("scoped listing should return only wsA's session; got %+v", matched)
	}
	if hidden != 2 {
		t.Errorf("hidden = %d, want 2 (wsB + no-workspace)", hidden)
	}

	// All=true bypasses filtering entirely.
	all, hiddenAll, err := ListSessionsScoped(SessionScope{Workspace: wsA, All: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || hiddenAll != 0 {
		t.Errorf("All=true should return every session with hidden=0; got %d sessions, hidden=%d", len(all), hiddenAll)
	}
}

// TestListSessionsScoped_SymlinkEquivalence verifies that a workspace path
// and its symlinked alias resolve to the same canonical identity.
func TestListSessionsScoped_SymlinkEquivalence(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WAKIL_SESSIONS_DIR", dir)

	real := t.TempDir()
	link := real + "-link"
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}
	t.Cleanup(func() { os.Remove(link) })

	s := &Session{ChatID: "symlink1", Workspace: real, Updated: time.Now()}
	if err := WriteSession(s); err != nil {
		t.Fatal(err)
	}

	// Query via the symlinked path — should still match the session saved
	// under the real path, since both canonicalize to the same target.
	matched, hidden, err := ListSessionsScoped(SessionScope{Workspace: link})
	if err != nil {
		t.Fatal(err)
	}
	if len(matched) != 1 {
		t.Fatalf("symlinked workspace should match the real path's session; got %d matches, hidden=%d", len(matched), hidden)
	}
}

// TestLoadSessionScoped_EmptyIDUsesWorkspaceLatest verifies that an empty
// idOrPrefix resolves to the most recent session IN SCOPE, not globally.
func TestLoadSessionScoped_EmptyIDUsesWorkspaceLatest(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WAKIL_SESSIONS_DIR", dir)

	wsA := t.TempDir()
	wsB := t.TempDir()

	// Globally newest session lives in wsB; wsA's own newest is older overall.
	older := &Session{ChatID: "wsA-old1", Workspace: wsA, Updated: time.Now().Add(-time.Hour)}
	newerB := &Session{ChatID: "wsB-new1", Workspace: wsB, Updated: time.Now()}
	for _, s := range []*Session{older, newerB} {
		if err := WriteSession(s); err != nil {
			t.Fatal(err)
		}
	}

	got, err := LoadSessionScoped("", SessionScope{Workspace: wsA})
	if err != nil {
		t.Fatal(err)
	}
	if got.ChatID != "wsA-old1" {
		t.Errorf("scoped empty-id load should return wsA's own latest, not the global latest; got %s", got.ChatID)
	}
}

// TestLoadSessionScoped_ExplicitIDIsGlobal verifies that an explicit
// id/prefix resolves globally even under a workspace scope that wouldn't
// otherwise include it.
func TestLoadSessionScoped_ExplicitIDIsGlobal(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WAKIL_SESSIONS_DIR", dir)

	wsA := t.TempDir()
	wsB := t.TempDir()
	other := &Session{ChatID: "otherWS9", Workspace: wsB, Updated: time.Now()}
	if err := WriteSession(other); err != nil {
		t.Fatal(err)
	}

	// Scope is wsA, but the explicit prefix names a session that lives in wsB.
	got, err := LoadSessionScoped("otherWS", SessionScope{Workspace: wsA})
	if err != nil {
		t.Fatalf("explicit id/prefix should resolve globally regardless of scope; got error: %v", err)
	}
	if got.ChatID != "otherWS9" {
		t.Errorf("got %s, want otherWS9", got.ChatID)
	}
}

// TestLoadSessionScoped_NoMatchInScope verifies the scoped "no sessions"
// error names the escape hatch.
func TestLoadSessionScoped_NoMatchInScope(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WAKIL_SESSIONS_DIR", dir)

	wsA := t.TempDir()
	wsB := t.TempDir()
	if err := WriteSession(&Session{ChatID: "wsBonly1", Workspace: wsB, Updated: time.Now()}); err != nil {
		t.Fatal(err)
	}

	_, err := LoadSessionScoped("", SessionScope{Workspace: wsA})
	if err == nil || !strings.Contains(err.Error(), "--all") {
		t.Errorf("expected a 'no sessions in scope' error mentioning --all; got %v", err)
	}
}

// TestPrintSessions_OldestFirstAndScoped verifies the CLI dump prints
// oldest-first (so the newest session lands at the bottom, near the prompt)
// and is scoped by default.
func TestPrintSessions_OldestFirstAndScoped(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WAKIL_SESSIONS_DIR", dir)

	ws := t.TempDir()
	older := &Session{ChatID: "oldest01", Workspace: ws, Updated: time.Now().Add(-time.Hour),
		Conv: []proxy.Message{{Role: "user", Content: StrPtr("first")}}}
	newer := &Session{ChatID: "newest02", Workspace: ws, Updated: time.Now(),
		Conv: []proxy.Message{{Role: "user", Content: StrPtr("second")}}}
	for _, s := range []*Session{older, newer} {
		if err := WriteSession(s); err != nil {
			t.Fatal(err)
		}
	}

	var buf strings.Builder
	PrintSessions(&buf, ws, false)
	out := buf.String()
	oldIdx := strings.Index(out, "oldest01")
	newIdx := strings.Index(out, "newest02")
	if oldIdx < 0 || newIdx < 0 || oldIdx >= newIdx {
		t.Errorf("expected oldest-first ordering (oldest before newest); got:\n%s", out)
	}
}

// TestPrintSessions_HiddenCount verifies the scoped dump reports sessions
// hidden by the workspace filter.
func TestPrintSessions_HiddenCount(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WAKIL_SESSIONS_DIR", dir)

	wsA := t.TempDir()
	wsB := t.TempDir()
	if err := WriteSession(&Session{ChatID: "inA00001", Workspace: wsA, Updated: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := WriteSession(&Session{ChatID: "inB00002", Workspace: wsB, Updated: time.Now()}); err != nil {
		t.Fatal(err)
	}

	var buf strings.Builder
	PrintSessions(&buf, wsA, false)
	if !strings.Contains(buf.String(), "1 session(s) in other folders") {
		t.Errorf("expected a hidden-count hint; got:\n%s", buf.String())
	}
}

// TestSessionListText_ScopedHiddenHint verifies the TUI /sessions note
// mentions hidden sessions and the "/sessions all" escape hatch.
func TestSessionListText_ScopedHiddenHint(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WAKIL_SESSIONS_DIR", dir)

	wsA := t.TempDir()
	wsB := t.TempDir()
	if err := WriteSession(&Session{ChatID: "inA00001", Workspace: wsA, Updated: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := WriteSession(&Session{ChatID: "inB00002", Workspace: wsB, Updated: time.Now()}); err != nil {
		t.Fatal(err)
	}

	out := SessionListText("", SessionScope{Workspace: wsA})
	if !strings.Contains(out, "/sessions all") {
		t.Errorf("expected hidden-count hint mentioning /sessions all; got %q", out)
	}
}
