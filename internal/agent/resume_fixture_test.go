package agent

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/proxy"
)

// resumeFixture describes a generated sessions directory. Everything is
// written under a temp dir (WAKIL_SESSIONS_DIR); the real data dir is never
// read or written.
type resumeFixture struct {
	Dir       string
	Workspace string
	Count     int
	Bytes     int64
}

// genResumeFixture writes n sessions of mixed, realistic size: mostly short
// (20 turns), every third medium (120 turns), every tenth long (600 turns),
// each turn a typed user message plus a tool result of toolLines lines. A
// system preamble opens every transcript. scale shrinks the tool results
// (1 = realistic ≈ 2 KB per result). Sessions are saved with WriteSession, the
// real save path, so the index (when present) is maintained exactly as in use.
func genResumeFixture(tb testing.TB, n, scale int) resumeFixture {
	tb.Helper()
	dir := tb.TempDir()
	tb.Setenv("WAKIL_SESSIONS_DIR", dir)
	ws := tb.TempDir()
	lines := 100 / scale
	if lines < 1 {
		lines = 1
	}
	now := time.Now()
	for i := 0; i < n; i++ {
		turns := 20
		switch {
		case i%10 == 0:
			turns = 600
		case i%3 == 0:
			turns = 120
		}
		conv := []proxy.Message{{Role: "system", Content: StrPtr(strings.Repeat("preamble ", 800)), Pinned: true}}
		for j := 0; j < turns; j++ {
			conv = append(conv,
				proxy.Message{Role: "user", Content: StrPtr(fmt.Sprintf("session %d question %d about the thing", i, j)), Turn: j + 1},
				proxy.Message{Role: "tool", ToolCallID: "x", Name: "read_file", Content: StrPtr(strings.Repeat("line of file output\n", lines)), Turn: j + 1})
		}
		s := &Session{
			ChatID:    fmt.Sprintf("chat-%04d-aaaaaaaa", i),
			Model:     "ilm",
			Workspace: ws,
			Created:   now.Add(-time.Duration(i+1) * time.Hour),
			Updated:   now.Add(-time.Duration(i) * time.Minute),
			Conv:      conv,
		}
		if err := WriteSession(s); err != nil {
			tb.Fatalf("WriteSession: %v", err)
		}
	}
	return resumeFixture{Dir: dir, Workspace: ws, Count: n}
}
