package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/memory"
	"github.com/treeol/wakil/internal/sessionhistory"
)

// TestSlashCommandTiming times every slash command that opens a list or reads
// disk, against generated fixtures in temp dirs (the package's TestMain points
// HOME/XDG at a temp tree and fails the run if the real Wakil dirs change).
// Nothing here reaches the network: the only endpoint is a local httptest
// server, and /model and /mcp are exercised in their read-only forms.
//
// It is a measurement, not a gate: it logs a table and asserts only that each
// command is handled (wall-clock bounds are meaningless under -race). Run with
// -v to see the table:
//
//	go test -run TestSlashCommandTiming -v ./internal/agent/
func TestSlashCommandTiming(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 500-session fixture")
	}
	fx := genResumeFixture(t, 500, 20)

	srv := sseServer(t, []string{contentChunk("ok")})
	defer srv.Close()
	app := newSessionHistoryApp(t, fx.Workspace)
	app.Client = newTestClient(srv.URL)
	app.Client.ChatID = "current"
	app.Out = os.Stderr
	app.Cfg.Endpoints = nil

	// Skills store with a few drafts and skills, in a temp dir.
	skillDB := filepath.Join(t.TempDir(), "skills", "skills.db")
	skills, err := memory.Open(skillDB, "")
	if err != nil {
		t.Fatal(err)
	}
	defer skills.Close()
	app.SkillStore = newSkillsProfile(skills)

	// Repo-state file for the workspace.
	app.Cfg.WorkDir = fx.Workspace
	app.saveRepoState(func(s *RepoState) { s.MashuraLevel = "medium" })

	type row struct {
		name string
		dur  time.Duration
		note string
	}
	var rows []row
	timeCmd := func(name, line, note string) {
		t.Helper()
		start := time.Now()
		handled, _, cmd := HandleTUICommand(line, app)
		sync := time.Since(start) // work done on the caller's (UI) goroutine
		if !handled {
			t.Errorf("%s: not handled", line)
			return
		}
		var cmdDur time.Duration
		if cmd != nil {
			s2 := time.Now()
			_ = cmd()
			cmdDur = time.Since(s2)
		}
		rows = append(rows, row{name: name + " [UI goroutine]", dur: sync, note: note})
		if cmd != nil {
			rows = append(rows, row{name: name + " [Cmd goroutine]", dur: cmdDur, note: note})
		}
	}

	// Cold index first (the first-run-after-upgrade case), then warm.
	_ = os.Remove(filepath.Join(fx.Dir, resumeIndexFile))
	timeCmd("/resume (cold index)", "/resume", "index only: the cold reconcile runs in the TUI refresh Cmd")
	timeCmd("/resume (warm index)", "/resume", "stat only")
	timeCmd("/resume all", "/resume all", "")
	timeCmd("/sessions", "/sessions", "")
	timeCmd("/sessions all", "/sessions all", "")
	timeCmd("/resume <id>", "/resume chat-0250", "loads one file by name")
	timeCmd("/model (show)", "/model", "in memory")
	timeCmd("/mcp", "/mcp", "in memory")
	timeCmd("/skills", "/skills", "skill store")
	timeCmd("/history", "/history", "in memory")
	timeCmd("/help", "/help", "static")
	timeCmd("/repostate", "/repostate", "one small file")
	timeCmd("/mashura", "/mashura", "in memory")

	// /remember and /recall reconcile the session-history index (SQLite) from
	// the session files: cold = index every session, warm = hash compare only.
	app.SessionHistory = mustOpenHistory(t)
	timeCmd("/remember (cold index)", "/remember thing", "indexes every session once")
	timeCmd("/remember (warm index)", "/remember thing", "stat + hash compare (index vouches for unchanged)")
	timeCmd("/recall", "/recall chat-0250", "reconcile + one GetTurns")

	// Completion: /resume <tab> reads the index via the facade (index only).
	start := time.Now()
	ents := ReadResumeIndex()
	rows = append(rows, row{name: "/resume <tab> completion (index read)", dur: time.Since(start), note: fmt.Sprintf("%d entries", len(ents))})

	var b strings.Builder
	fmt.Fprintf(&b, "\n%-48s %12s  %s\n", "command", "time", "note")
	for _, r := range rows {
		flag := ""
		if r.dur > 200*time.Millisecond {
			flag = "  <-- over 200 ms"
		}
		fmt.Fprintf(&b, "%-48s %12v  %s%s\n", r.name, r.dur.Round(10*time.Microsecond), r.note, flag)
	}
	t.Log(b.String())
}

func mustOpenHistory(t *testing.T) *sessionhistory.Store {
	t.Helper()
	st, err := sessionhistory.Open(filepath.Join(t.TempDir(), "sessionhistory", "timing.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}
