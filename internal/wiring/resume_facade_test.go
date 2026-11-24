package wiring

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/core/sessionclient"
	"github.com/treeol/wakil/internal/proxy"
)

// The facade lists from the resume index: summaries carry the typed title and
// turn count but no transcript; reconcile=false never touches session files.
func TestFacadeListSessionsFromIndex(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WAKIL_SESSIONS_DIR", dir)
	ws := t.TempDir()
	content := func(s string) *string { return &s }
	for _, s := range []*agent.Session{
		{ChatID: "fac-0001-aaaaaaaa", Workspace: ws, Updated: time.Now(), Conv: []proxy.Message{{Role: "user", Content: content("typed question")}, {Role: "assistant", Content: content("a")}, {Role: "user", Content: content("again")}}},
		{ChatID: "fac-0002-aaaaaaaa", Workspace: t.TempDir(), Updated: time.Now().Add(-time.Hour), Conv: []proxy.Message{{Role: "user", Content: content("other repo")}}},
	} {
		if err := agent.WriteSession(s); err != nil {
			t.Fatal(err)
		}
	}
	f := &wiringFacade{}
	scope := sessionclient.SessionScope{Workspace: ws}

	got, hidden, err := f.ListSessions(scope, false)
	if err != nil || len(got) != 1 || hidden != 1 {
		t.Fatalf("workspace scope: %d sessions hidden %d err %v", len(got), hidden, err)
	}
	if got[0].Title != "typed question" || got[0].TurnCount != 2 || got[0].Conv != nil {
		t.Errorf("summary = %+v (want title, 2 turns, no transcript)", got[0])
	}
	if all, _, _ := f.ListSessions(sessionclient.SessionScope{All: true}, false); len(all) != 2 {
		t.Errorf("all scope: %d sessions, want 2", len(all))
	}

	// A session saved behind the index (older wakil, another process) shows up
	// only after a reconcile; reconcile=false must not read the file.
	late := filepath.Join(dir, "fac-0003-aaaaaaaa.json")
	_ = os.WriteFile(late, []byte(`{"chat_id":"fac-0003-aaaaaaaa","workspace":`+quote(ws)+`,"updated":"2026-11-24T10:00:00Z","conv":[{"role":"user","content":"late"}]}`), 0o644)
	if got, _, _ := f.ListSessions(scope, false); len(got) != 1 {
		t.Errorf("index-only listing saw %d sessions; the late file must wait for a reconcile", len(got))
	}
	if got, _, _ := f.ListSessions(scope, true); len(got) != 2 {
		t.Errorf("after reconcile: %d sessions, want 2", len(got))
	}
}

func quote(s string) string { return `"` + s + `"` }
