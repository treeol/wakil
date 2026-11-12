package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/proxy"
)

// Tests for the reread lock (stop the drop-and-reread loop) and for
// drop_reason (a large drop needs a reason).

func markCall(id, args string) proxy.ToolCall {
	return proxy.ToolCall{ID: id, Type: "function", Function: proxy.FunctionCall{Name: "context_mark", Arguments: args}}
}

// lockRound builds the canonical drop -> prune -> reread sequence and
// returns the reread round's id.
func lockRound(t *testing.T, app *App, path, mark string) int {
	t.Helper()
	appendRound(app, 1, "c1", "read_file", fmt.Sprintf(`{"path":%q}`, path), strings.Repeat("a", 6000)) // r1
	appendRound(app, 1, "c2", "read_file", `{"path":"other.go"}`, strings.Repeat("b", 6000))            // r2
	// Drop r1 with a reason so the large-drop check passes.
	args := `{"drop":["r1"],"drop_reason":{"r1":"not needed any more, fully read and done"}}`
	if mark == "stub" {
		args = `{"stub":{"r1":"` + strings.Repeat("a", 300) + ` crypto.go facts, file:line per file, long enough for the check, padding padding padding padding padding padding padding padding"}}`
	}
	if got := app.handleContextMark(context.Background(), markCall("m1", args)); got == "" {
		t.Fatal("context_mark returned nothing")
	}
	// A later real round releases the pending mark.
	appendRound(app, 1, "c3", "read_file", `{"path":"zzz.go"}`, strings.Repeat("z", 6000)) // r3
	markPrunedDirectlyIfNeeded(app, 1)
	// Reread the path.
	reread := appendRound(app, 1, "c4", "read_file", fmt.Sprintf(`{"path":%q}`, path), strings.Repeat("c", 6000)) // r4
	return reread
}

func markPrunedDirectlyIfNeeded(app *App, id int) {
	app.stateMu.RLock()
	rec, ok := app.pruneRounds[id]
	app.stateMu.RUnlock()
	if !ok || rec.Pruned {
		return
	}
	markPrunedDirectly(app, id, rec.Rating)
}

func TestDropThenRereadThenDropIsRefused(t *testing.T) {
	app := lineTestApp(t)
	reread := lockRound(t, app, "crypto.go", "drop")
	got := toolResultOf(app.requestMessages(), "c4")
	want := "[reread · you dropped this file in r1 and are reading it again. It now stays in context and can no longer be marked]"
	if !strings.Contains(got, want) {
		t.Errorf("reread line missing: %q", got[max(0, len(got)-260):])
	}
	// The locked round cannot be dropped or stubbed again.
	out := app.handleContextMark(context.Background(), markCall("m2", `{"drop":["r4"]}`))
	if !strings.Contains(out, "rejected r4") || !strings.Contains(out, "It stays in context") {
		t.Errorf("drop of locked round not refused: %q", out)
	}
	if ratingOf(app, reread) != RatingNone {
		t.Errorf("locked round rated %q, want unrated", ratingOf(app, reread))
	}
	out = app.handleContextMark(context.Background(), markCall("m3", `{"stub":{"r4":"`+strings.Repeat("x", 300)+`"}}`))
	if !strings.Contains(out, "rejected r4") {
		t.Errorf("stub of locked round not refused: %q", out)
	}
	if ratingOf(app, reread) != RatingNone {
		t.Errorf("locked round rated %q after stub attempt, want unrated", ratingOf(app, reread))
	}
	// Keep still works.
	out = app.handleContextMark(context.Background(), markCall("m4", `{"keep":["r4"]}`))
	if !strings.Contains(out, "keep r4") {
		t.Errorf("keep of locked round refused: %q", out)
	}
	if ratingOf(app, reread) != RatingKeep {
		t.Errorf("keep not applied: %q", ratingOf(app, reread))
	}
	// The locked round never appears in a backlog line.
	appendRound(app, 1, "c5", "read_file", `{"path":"n1.go"}`, strings.Repeat("d", 6000))
	appendRound(app, 1, "c6", "read_file", `{"path":"n2.go"}`, strings.Repeat("e", 6000))
	appendRound(app, 1, "c7", "read_file", `{"path":"n3.go"}`, strings.Repeat("f", 6000))
	appendRound(app, 1, "c8", "read_file", `{"path":"n4.go"}`, strings.Repeat("g", 6000))
	if got := toolResultOf(app.requestMessages(), "c8"); strings.Contains(got, fmt.Sprintf("r%d (", reread)) {
		t.Errorf("locked round listed in backlog: %q", got[max(0, len(got)-300):])
	}
}

func TestStubThenRereadThenStubIsRefused(t *testing.T) {
	app := lineTestApp(t)
	reread := lockRound(t, app, "crypto.go", "stub")
	got := toolResultOf(app.requestMessages(), "c4")
	if !strings.Contains(got, "you stubbed this file in r1 and are reading it again") {
		t.Errorf("stubbed reread line missing: %q", got[max(0, len(got)-260):])
	}
	out := app.handleContextMark(context.Background(), markCall("m2", `{"drop":["r4"]}`))
	if !strings.Contains(out, "rejected r4") {
		t.Errorf("drop of stub-locked round not refused: %q", out)
	}
	if ratingOf(app, reread) != RatingNone {
		t.Errorf("locked round rated %q, want unrated", ratingOf(app, reread))
	}
}

func TestMixedCallOneLockedOneFree(t *testing.T) {
	app := lineTestApp(t)
	lockRound(t, app, "crypto.go", "drop")
	// r4 is locked; r3 (zzz.go) is free.
	out := app.handleContextMark(context.Background(), markCall("m2", `{"drop":["r4","r3"],"drop_reason":{"r3":"searched and nothing needed"}}`))
	if !strings.Contains(out, "r4") || !strings.Contains(out, "rejected r4") {
		t.Errorf("locked r4 not refused in mixed call: %q", out)
	}
	// Other rounds in the same call are processed normally.
	if ratingOf(app, 3) != RatingDrop {
		t.Errorf("free round in mixed call not dropped: %q", ratingOf(app, 3))
	}
	if ratingOf(app, 4) != RatingNone {
		t.Errorf("locked round in mixed call rated %q", ratingOf(app, 4))
	}
}

func TestNoLockAfterWakilPrune(t *testing.T) {
	app := lineTestApp(t)
	appendRound(app, 1, "c1", "read_file", `{"path":"crypto.go"}`, strings.Repeat("a", 6000))
	appendRound(app, 1, "c2", "read_file", `{"path":"other.go"}`, strings.Repeat("b", 6000))
	markPrunedDirectly(app, 1, RatingSuperseded)
	appendRound(app, 1, "c3", "read_file", `{"path":"crypto.go"}`, strings.Repeat("c", 6000))
	if got := toolResultOf(app.requestMessages(), "c3"); strings.Contains(got, "[reread") {
		t.Errorf("reread line after superseded prune: %q", got[max(0, len(got)-200):])
	}
	app.stateMu.RLock()
	locked := len(app.pruneLocked)
	app.stateMu.RUnlock()
	if locked != 0 {
		t.Errorf("superseded reread locked %d paths, want 0", locked)
	}
	out := app.handleContextMark(context.Background(), markCall("m1", `{"drop":["r3"],"drop_reason":{"r3":"reread after superseded, fully done"}}`))
	if strings.Contains(out, "rejected") {
		t.Errorf("drop of a superseded-reread round refused: %q", out)
	}
}

func TestLockMatchesPathVariants(t *testing.T) {
	app := lineTestApp(t)
	variant := lockRound(t, app, "a.go", "drop")
	// Reread locked via ./a.go and an absolute path.
	appendRound(app, 1, "c5", "read_file", `{"path":"./a.go"}`, strings.Repeat("d", 6000))
	out := app.handleContextMark(context.Background(), markCall("m2", `{"drop":["r5"]}`))
	if !strings.Contains(out, "rejected r5") {
		t.Errorf("drop of ./-variant locked round not refused: %q", out)
	}
	_ = variant
	for _, p := range []string{"a.go", "./a.go", "/abs/a.go", "x/../a.go"} {
		app2 := lineTestApp(t)
		app2.Cfg.WorkDir = "/work"
		if _, ok := app2.rereadLocked(p); ok {
			t.Errorf("rereadLocked(%q) true before any lock", p)
		}
		_ = app2
	}
	if got := app.canonPath("a.go"); got != app.canonPath("./a.go") || got != app.canonPath("x/../a.go") {
		t.Errorf("canonPath variants differ: %q %q %q", app.canonPath("a.go"), app.canonPath("./a.go"), app.canonPath("x/../a.go"))
	}
	// An absolute path to the same file also locks.
	if _, ok := app.rereadLocked(app.canonPath("a.go")); !ok {
		t.Error("rereadLocked(a.go) lost")
	}
}

// The lock set survives persistence like the other prune state.
func TestLockSurvivesSaveAndRestore(t *testing.T) {
	app := lineTestApp(t)
	lockRound(t, app, "crypto.go", "drop")
	s := &Session{PruneRecords: app.pruneRecordsSlice(), PruneLocked: app.rereadLocksSlice()}
	app2 := lineTestApp(t)
	app2.restorePruneState(s)
	if _, ok := app2.rereadLocked("crypto.go"); !ok {
		t.Fatal("lock lost after restore")
	}
	out := app2.handleContextMark(context.Background(), markCall("m9", `{"drop":["r4"]}`))
	if !strings.Contains(out, "rejected r4") {
		t.Errorf("drop of restored locked round not refused: %q", out)
	}
}

// A large drop without a reason is refused; with a reason it is accepted and
// the reason shows in the stub line.
func TestLargeDropNeedsReason(t *testing.T) {
	app := lineTestApp(t)
	appendRound(app, 1, "c1", "read_file", `{"path":"big.go"}`, strings.Repeat("a", 22000)) // ~21 KB
	appendRound(app, 1, "c2", "read_file", `{"path":"o.go"}`, strings.Repeat("b", 6000))
	out := app.handleContextMark(context.Background(), markCall("m1", `{"drop":["r1"]}`))
	if !strings.Contains(out, "r1 not dropped:") || !strings.Contains(out, "Say in drop_reason") {
		t.Errorf("large drop without reason accepted or wrong message: %q", out)
	}
	if ratingOf(app, 1) != RatingNone {
		t.Errorf("large drop without reason rated %q", ratingOf(app, 1))
	}
	out = app.handleContextMark(context.Background(), markCall("m2", `{"drop":["r1"],"drop_reason":{"r1":"searched and nothing needed"}}`))
	if !strings.Contains(out, "drop r1") {
		t.Errorf("large drop with reason refused: %q", out)
	}
	if ratingOf(app, 1) != RatingDrop {
		t.Errorf("large drop with reason rated %q", ratingOf(app, 1))
	}
	// The reason is visible in the stub line.
	markPrunedDirectly(app, 1, RatingDrop)
	app.stateMu.Lock()
	rec := app.pruneRounds[1]
	rec.DropReason = "searched and nothing needed"
	rec.Archive = "/tmp/round"
	app.pruneRounds[1] = rec
	app.stateMu.Unlock()
	if got := toolResultOf(app.requestMessages(), "c1"); !strings.Contains(got, "[r1 pruned (drop: searched and nothing needed)") {
		t.Errorf("drop reason not visible in stub line: %q", got[max(0, len(got)-200):])
	}
}

func TestSmallDropNeedsNoReason(t *testing.T) {
	app := lineTestApp(t)
	appendRound(app, 1, "c1", "read_file", `{"path":"small.go"}`, strings.Repeat("a", 1000))
	appendRound(app, 1, "c2", "read_file", `{"path":"o.go"}`, strings.Repeat("b", 1000))
	out := app.handleContextMark(context.Background(), markCall("m1", `{"drop":["r1"]}`))
	if strings.Contains(out, "not dropped") || !strings.Contains(out, "drop r1") {
		t.Errorf("small drop without reason refused: %q", out)
	}
	if ratingOf(app, 1) != RatingDrop {
		t.Errorf("small drop rated %q", ratingOf(app, 1))
	}
}

// Simulation shaped like the live run: 15 files, each read, dropped, read
// again and (tried to be) dropped again, three times round. With the lock the
// second drop is refused, so later rounds accumulate in context and the
// third read is the last one needed.
func TestRereadLockSimulation(t *testing.T) {
	app := lineTestApp(t)
	var reads, marks, refused int
	for cycle := 0; cycle < 3; cycle++ {
		for i := 1; i <= 15; i++ {
			path := fmt.Sprintf("file%02d.go", i)
			cid := fmt.Sprintf("cycle%d-%02d", cycle, i)
			rid := appendRound(app, 1, cid, "read_file", fmt.Sprintf(`{"path":%q}`, path), strings.Repeat("a", 6000))
			reads++
			// A later real round releases the pending mark, then the round
			// is pruned. The drop needs a drop_reason (large round). Once
			// the path is locked (cycle >= 1) the initial drop is itself
			// refused, which does not change the count: the lock already
			// keeps the round in context.
			hub := appendRound(app, 1, cid+"-hub", "read_file", `{"path":"hub.go"}`, strings.Repeat("h", 6000))
			reads++
			if out := app.handleContextMark(context.Background(), markCall(cid+"-drop0", fmt.Sprintf(`{"drop":["r%d"],"drop_reason":{"r%d":"read, fully reviewed, done"}}`, rid, rid))); !strings.Contains(out, "drop") {
				if cycle == 0 {
					t.Errorf("cycle %d file %d: initial drop not accepted: %q", cycle, i, out)
				}
			}
			marks++
			markPrunedDirectlyIfNeeded(app, rid)
			_ = hub
			// The reread round. Its first read locks the path (the first
			// model-marked prune of that path precedes it); the drop below
			// is refused, so the round stays in context.
			reread := appendRound(app, 1, cid+"-reread", "read_file", fmt.Sprintf(`{"path":%q}`, path), strings.Repeat("c", 6000))
			reads++
			out := app.handleContextMark(context.Background(), markCall(cid+"-mark", fmt.Sprintf(`{"drop":["r%d"],"drop_reason":{"r%d":"read again, fully reviewed"}}`, reread, reread)))
			marks++
			if strings.Contains(out, "rejected") {
				refused++
			} else if cycle == 1 {
				t.Errorf("cycle %d file %d: reread drop not refused: %q", cycle, i, out)
			}
		}
	}
	t.Logf("simulation with lock: reads=%d marks=%d refused=%d locked-paths=%d", reads, marks, refused, func() int {
		app.stateMu.RLock()
		defer app.stateMu.RUnlock()
		return len(app.pruneLocked)
	}())
	if refused == 0 {
		t.Error("no mark was refused; the lock never engaged")
	}
	// Cycle 0 locks 15 paths; cycles 1-2 first and second drops are both
	// refused (30 + 15 = 45 refusals, of which 30 are the second drop).
	if refused != 45 {
		t.Errorf("refused = %d, want 45 (15 locked paths x cycles 1-2)", refused)
	}
	if needs := app.pruneLocked; len(needs) != 15 {
		t.Errorf("locked paths = %d, want 15", len(needs))
	}
}

// Marshal round-trip of Session with locks and drop reasons.
func TestLockAndDropReasonPersist(t *testing.T) {
	app := lineTestApp(t)
	lockRound(t, app, "crypto.go", "drop")
	app.stateMu.Lock()
	rec := app.pruneRounds[1]
	rec.DropReason = "searched and nothing needed"
	app.pruneRounds[1] = rec
	app.stateMu.Unlock()
	s := &Session{PruneRecords: app.pruneRecordsSlice(), PruneLocked: app.rereadLocksSlice()}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var back Session
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.PruneLocked) != 1 || back.PruneRecords[0].DropReason == "" {
		t.Errorf("persisted lock/reason missing: %+v", back)
	}
}
