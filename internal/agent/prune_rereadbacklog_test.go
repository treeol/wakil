package agent

import (
	"os"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/proxy"
)

// Tests for the two shapes of tool-result line under pruning (Changes 1+2):
// the backlog line on the newest round's last result, and the reread line
// on a round that rereads something an earlier model-marked pruned round
// had read. Both are computed once, when the round is recorded, and stored
// on the round record — the projection only appends them.

// toolResultOf returns the projected content of the tool message for callID.
func toolResultOf(msgs []proxy.Message, callID string) string {
	for _, m := range msgs {
		if m.Role == "tool" && m.ToolCallID == callID {
			return DerefStr(m.Content)
		}
	}
	return "<missing>"
}

func lineTestApp(t *testing.T) *App {
	t.Helper()
	os.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Cleanup(func() { os.Unsetenv("XDG_DATA_HOME") })
	app := pruneTestApp()
	app.Cfg.HardMaxBytes = 1 << 30 // far from any trigger: only the lines are under test
	return app
}

const backlogMarker = "[context · unmarked for 3+ steps:"

func TestBacklogLineAbsentWhenFresh(t *testing.T) {
	app := lineTestApp(t)
	appendRound(app, 1, "c1", "read_file", `{"path":"a.go"}`, strings.Repeat("a", 4000))
	appendRound(app, 1, "c2", "read_file", `{"path":"b.go"}`, strings.Repeat("b", 4000))
	msgs := app.requestMessages()
	for _, id := range []string{"c1", "c2"} {
		if r := toolResultOf(msgs, id); strings.Contains(r, backlogMarker) {
			t.Errorf("%s carries a backlog line while all rounds are fresh: %q", id, r[max(0, len(r)-160):])
		}
	}
}

func TestBacklogLineListsStaleRoundsOnNewest(t *testing.T) {
	app := lineTestApp(t)
	appendRound(app, 1, "c1", "read_file", `{"path":"a.go"}`, strings.Repeat("a", 4000))
	appendRound(app, 1, "c2", "read_file", `{"path":"b.go"}`, strings.Repeat("b", 6000))
	appendRound(app, 1, "c3", "read_file", `{"path":"c.go"}`, strings.Repeat("c", 2000))
	appendRound(app, 1, "c4", "read_file", `{"path":"d.go"}`, strings.Repeat("d", 4000))
	appendRound(app, 1, "c5", "read_file", `{"path":"e.go"}`, strings.Repeat("e", 4000))
	msgs := app.requestMessages()
	// r5 is recorded 4 rounds after r1 and 3 after r2; both are >=3 real
	// rounds older, r3 is 2 older and must not be listed.
	got := toolResultOf(msgs, "c5")
	want := "[r5] " + strings.Repeat("e", 4000) +
		"\n[context · unmarked for 3+ steps: r2 (~1k tokens), r1 (~1k tokens) · if you are done with one, context_mark it: stub (only your note remains: facts with file:line) or drop. Keep what you will still cite]"
	if got != want {
		t.Fatalf("r5 result:\n got tail %q\nwant tail %q", got[max(0, len(got)-220):], want[max(0, len(want)-220):])
	}
	// Only the newest round carries the line.
	if r := toolResultOf(msgs, "c1"); strings.Contains(r, backlogMarker) {
		t.Error("older round gained a backlog line")
	}
	// Byte-identical across two request builds.
	second := app.requestMessages()
	if toolResultOf(second, "c5") != got {
		t.Error("backlog line changed between two request builds")
	}
}

func TestBacklogLineIgnoresMarkedRounds(t *testing.T) {
	app := lineTestApp(t)
	appendRound(app, 1, "c1", "read_file", `{"path":"a.go"}`, strings.Repeat("a", 4000))
	appendRound(app, 1, "c2", "read_file", `{"path":"b.go"}`, strings.Repeat("b", 4000))
	appendRound(app, 1, "c3", "read_file", `{"path":"c.go"}`, strings.Repeat("c", 4000))
	appendRound(app, 1, "c4", "read_file", `{"path":"d.go"}`, strings.Repeat("d", 4000))
	app.recordMarkRatings(nil, []int{1}, nil, nil, 0) // keep: no longer unmarked
	appendRound(app, 1, "c5", "read_file", `{"path":"e.go"}`, strings.Repeat("e", 4000))
	got := toolResultOf(app.requestMessages(), "c5")
	if strings.Contains(got, "r1 (") {
		t.Errorf("marked round listed in backlog: %q", got[max(0, len(got)-220):])
	}
	if !strings.Contains(got, backlogMarker) {
		t.Errorf("unmarked stale round missing from backlog: %q", got[max(0, len(got)-220):])
	}
}

func TestBacklogLineFrozenWhenLaterMarked(t *testing.T) {
	app := lineTestApp(t)
	appendRound(app, 1, "c1", "read_file", `{"path":"a.go"}`, strings.Repeat("a", 4000))
	appendRound(app, 1, "c2", "read_file", `{"path":"b.go"}`, strings.Repeat("b", 4000))
	appendRound(app, 1, "c3", "read_file", `{"path":"c.go"}`, strings.Repeat("c", 4000))
	appendRound(app, 1, "c4", "read_file", `{"path":"d.go"}`, strings.Repeat("d", 4000))
	before := toolResultOf(app.requestMessages(), "c4")
	if !strings.Contains(before, "r1 (") {
		t.Fatalf("precondition: r4 backlog should name r1: %q", before[max(0, len(before)-220):])
	}
	app.recordMarkRatings(nil, []int{1}, nil, nil, 0)
	after := toolResultOf(app.requestMessages(), "c4")
	if after != before {
		t.Error("marking a listed round changed the already-written backlog line")
	}
}

func TestBacklogLineCapsAndMore(t *testing.T) {
	app := lineTestApp(t)
	for i := 0; i < 10; i++ {
		appendRound(app, 1, strings.Repeat("c", i+1)[:1]+string(rune('a'+i)), "read_file", `{"path":"`+string(rune('a'+i))+`.go"}`, strings.Repeat("x", 4000+1000*i))
	}
	// At r10 the stale rounds are r1..r7 (7 rounds, r8 is 2 older) — 6
	// listed, 1 more.
	got := toolResultOf(app.requestMessages(), toolResultLastID(app))
	if !strings.Contains(got, "+1 more") {
		t.Fatalf("expected '+1 more' (7 stale - 6 listed): %q", got[max(0, len(got)-260):])
	}
	if strings.Count(got, "(~") != 6 {
		t.Errorf("listed rounds = %d, want 6", strings.Count(got, "(~"))
	}
}

// toolResultLastID returns the call id of the last tool result in Conv.
func toolResultLastID(app *App) string {
	for i := len(app.Conv) - 1; i >= 0; i-- {
		if app.Conv[i].Role == "tool" {
			return app.Conv[i].ToolCallID
		}
	}
	return ""
}

func TestBacklogLineAbsentWhenPruneOff(t *testing.T) {
	app := lineTestApp(t)
	app.Cfg.ContextPrune = "off"
	for i := 0; i < 5; i++ {
		appendRound(app, 1, "c"+string(rune('a'+i)), "read_file", `{"path":"a.go"}`, strings.Repeat("a", 4000))
	}
	for _, m := range app.requestMessages() {
		if strings.Contains(DerefStr(m.Content), "context_mark it") || strings.HasPrefix(DerefStr(m.Content), "[r") {
			t.Errorf("prune off: message touched: %q", DerefStr(m.Content)[:min(80, len(DerefStr(m.Content)))])
		}
	}
}

// markPrunedDirectly prunes a round as the model-marked path does: rating
// drop/stub and Pruned, with the archive filled.
func markPrunedDirectly(app *App, id int, rating RoundRating) {
	app.stateMu.Lock()
	rec := app.pruneRounds[id]
	rec.Rating = rating
	rec.Pruned = true
	rec.Archive = "/tmp/round"
	if app.prunedRounds == nil {
		app.prunedRounds = map[int]bool{}
	}
	app.prunedRounds[id] = true
	app.pruneRounds[id] = rec
	app.stateMu.Unlock()
}

func TestRereadLineOnStubThenReread(t *testing.T) {
	app := lineTestApp(t)
	appendRound(app, 1, "c1", "read_file", `{"path":"crypto.go"}`, strings.Repeat("a", 4000)) // r1
	appendRound(app, 1, "c2", "read_file", `{"path":"other.go"}`, strings.Repeat("b", 4000))  // r2
	markPrunedDirectly(app, 1, RatingStub)
	appendRound(app, 1, "c3", "read_file", `{"path":"crypto.go"}`, strings.Repeat("c", 4000)) // r3 rereads
	got := toolResultOf(app.requestMessages(), "c3")
	want := "[r3] " + strings.Repeat("c", 4000) +
		"\n[reread · you stubbed this file in r1 and are reading it again. It now stays in context and can no longer be marked]"
	if got != want {
		t.Fatalf("reread result:\n got tail %q\nwant tail %q", got[max(0, len(got)-240):], want[max(0, len(want)-240):])
	}
	// And it is byte-identical on the next build.
	if toolResultOf(app.requestMessages(), "c3") != got {
		t.Error("reread line changed between two request builds")
	}
}

func TestRereadLineDropWording(t *testing.T) {
	app := lineTestApp(t)
	appendRound(app, 1, "c1", "read_file", `{"path":"crypto.go"}`, strings.Repeat("a", 4000))
	appendRound(app, 1, "c2", "read_file", `{"path":"other.go"}`, strings.Repeat("b", 4000))
	markPrunedDirectly(app, 1, RatingDrop)
	appendRound(app, 1, "c3", "read_file", `{"path":"crypto.go"}`, strings.Repeat("c", 4000))
	got := toolResultOf(app.requestMessages(), "c3")
	if !strings.Contains(got, "you dropped this file in r1 and are reading it again") {
		t.Errorf("drop reread line: %q", got[max(0, len(got)-240):])
	}
}

func TestRereadLineAbsentAfterWakilPrune(t *testing.T) {
	app := lineTestApp(t)
	for _, rating := range []RoundRating{RatingSuperseded, RatingEmergency} {
		app.stateMu.Lock()
		app.pruneRounds = nil
		app.prunedRounds = nil
		app.roundSeq = 0
		app.stateMu.Unlock()
		appendRound(app, 1, "c1", "read_file", `{"path":"crypto.go"}`, strings.Repeat("a", 4000))
		appendRound(app, 1, "c2", "read_file", `{"path":"other.go"}`, strings.Repeat("b", 4000))
		markPrunedDirectly(app, 1, rating)
		appendRound(app, 1, "c3", "read_file", `{"path":"crypto.go"}`, strings.Repeat("c", 4000))
		if got := toolResultOf(app.requestMessages(), "c3"); strings.Contains(got, "[reread") {
			t.Errorf("reread line after %s prune: %q", rating, got[max(0, len(got)-200):])
		}
	}
}

func TestRereadLinePrecludesBacklogLine(t *testing.T) {
	app := lineTestApp(t)
	appendRound(app, 1, "c1", "read_file", `{"path":"a.go"}`, strings.Repeat("a", 4000))
	appendRound(app, 1, "c2", "read_file", `{"path":"b.go"}`, strings.Repeat("b", 4000))
	appendRound(app, 1, "c3", "read_file", `{"path":"c.go"}`, strings.Repeat("c", 4000))
	markPrunedDirectly(app, 1, RatingStub)
	appendRound(app, 1, "c4", "read_file", `{"path":"a.go"}`, strings.Repeat("d", 4000))
	got := toolResultOf(app.requestMessages(), "c4")
	if !strings.Contains(got, "[reread") {
		t.Fatalf("reread line missing: %q", got[max(0, len(got)-240):])
	}
	if strings.Contains(got, backlogMarker) {
		t.Errorf("reread result also carries the backlog line: %q", got[max(0, len(got)-260):])
	}
}

func TestRereadLineAbsentWhenPruneOff(t *testing.T) {
	app := lineTestApp(t)
	app.Cfg.ContextPrune = "off"
	app.curTurn = 1
	app.appendConv(tcMsg("c1", "read_file", `{"path":"a.go"}`))
	app.appendConv(trMsg("c1", "read_file", strings.Repeat("a", 4000)))
	for _, m := range app.requestMessages() {
		if strings.Contains(DerefStr(m.Content), "[reread") {
			t.Error("reread line with prune off")
		}
	}
}

func TestRereadAndBacklogNotStoredOrArchived(t *testing.T) {
	app := lineTestApp(t)
	appendRound(app, 1, "c1", "read_file", `{"path":"crypto.go"}`, strings.Repeat("a", 4000))
	appendRound(app, 1, "c2", "read_file", `{"path":"other.go"}`, strings.Repeat("b", 4000))
	markPrunedDirectly(app, 1, RatingStub)
	appendRound(app, 1, "c3", "read_file", `{"path":"crypto.go"}`, strings.Repeat("c", 4000))
	appendRound(app, 1, "c4", "read_file", `{"path":"x.go"}`, strings.Repeat("d", 4000))
	_ = app.requestMessages()
	for _, m := range app.Conv {
		if c := DerefStr(m.Content); strings.Contains(c, "[reread") || strings.Contains(c, "[context · unmarked") {
			t.Errorf("a.Conv carries a projection line: %q", c[:min(80, len(c))])
		}
	}
	app.stateMu.RLock()
	rec := app.pruneRounds[3]
	app.stateMu.RUnlock()
	arch := renderRoundArchive(app.Conv, rec)
	if strings.Contains(arch, "[reread") || strings.Contains(arch, "[context · unmarked") {
		t.Error("archive text carries a projection line")
	}
}
