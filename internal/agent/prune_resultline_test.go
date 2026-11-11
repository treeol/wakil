package agent

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/proxy"
)

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
	app.Cfg.HardMaxBytes = 1 << 30 // far from any trigger: only the line is under test
	return app
}

const lineMarker = "done with this? context_mark it"

func TestPruneResultLinePresentAndStable(t *testing.T) {
	app := lineTestApp(t)
	appendRound(app, 1, "c1", "read_file", `{"path":"a.go"}`, strings.Repeat("a", 4000))
	appendRound(app, 1, "c2", "read_file", `{"path":"b.go"}`, strings.Repeat("b", 4000))
	first := app.requestMessages()
	got := toolResultOf(first, "c1")
	want := "[r1] " + strings.Repeat("a", 4000) + "\n[r1 · ~1000 tokens · done with this? context_mark it: stub with the facts you still need, or drop]"
	if got != want {
		t.Fatalf("r1 result:\n got tail %q\nwant tail %q", got[len(got)-130:], want[len(want)-130:])
	}
	// More rounds arrive: the line of r1 must not change, byte for byte.
	appendRound(app, 1, "c3", "read_file", `{"path":"c.go"}`, strings.Repeat("c", 3000))
	second := app.requestMessages()
	if toolResultOf(second, "c1") != got {
		t.Error("r1 projection changed between two request builds")
	}
	if toolResultOf(second, "c3") == "" || !strings.Contains(toolResultOf(second, "c3"), "[r3 · ~750 tokens") {
		t.Errorf("newest round lacks its line: %q", toolResultOf(second, "c3"))
	}
}

func TestPruneResultLineAbsentWhenPruneOff(t *testing.T) {
	app := lineTestApp(t)
	app.Cfg.ContextPrune = "off"
	app.curTurn = 1
	app.appendConv(tcMsg("c1", "read_file", `{"path":"a.go"}`))
	app.appendConv(trMsg("c1", "read_file", bigContent))
	for _, m := range app.requestMessages() {
		if strings.Contains(DerefStr(m.Content), "context_mark") || strings.HasPrefix(DerefStr(m.Content), "[r") {
			t.Errorf("prune off: message touched: %q", DerefStr(m.Content))
		}
	}
}

func TestPruneResultLineAbsentOnRatedPrunedProtectedTiny(t *testing.T) {
	app := lineTestApp(t)
	big := strings.Repeat("z", 4000)
	appendRound(app, 1, "kp", "read_file", `{"path":"keep.go"}`, big)  // r1 → keep
	appendRound(app, 1, "st", "read_file", `{"path":"stub.go"}`, big)  // r2 → stub
	appendRound(app, 1, "dr", "read_file", `{"path":"drop.go"}`, big)  // r3 → drop
	appendRound(app, 1, "tn", "read_file", `{"path":"tiny.go"}`, "ok") // r4 tiny
	appendRound(app, 1, "pn", "read_file", `{"path":"pin.go"}`, big)   // r5 pinned
	for i := range app.Conv {
		if app.Conv[i].ToolCallID == "pn" {
			app.Conv[i].Pinned = true
		}
	}
	appendRound(app, 2, "kt", "read_file", `{"path":"kt.go"}`, big) // r6, turn 2 under /keep
	appendRound(app, 3, "cl", "read_file", `{"path":"cl.go"}`, big) // r7, turn 3 collapsed
	appendRound(app, 4, "pr", "read_file", `{"path":"pr.go"}`, big) // r8 pruned
	appendRound(app, 4, "ok", "read_file", `{"path":"ok.go"}`, big) // r9 control: still prunable
	app.recordMarkRatings([]int{3}, []int{1}, map[int]string{2: "facts"})
	app.keep = map[int]bool{2: true}
	app.collapsed = map[int]CollapseRecord{3: {Turn: 3}}
	app.stateMu.Lock()
	rec := app.pruneRounds[8]
	rec.Pruned = true
	rec.Rating = RatingDrop
	rec.Archive = "/tmp/x"
	app.pruneRounds[8] = rec
	if app.prunedRounds == nil {
		app.prunedRounds = map[int]bool{}
	}
	app.prunedRounds[8] = true
	app.stateMu.Unlock()

	msgs := app.requestMessages()
	for _, id := range []string{"kp", "st", "dr", "tn", "pn", "kt", "pr"} {
		if r := toolResultOf(msgs, id); strings.Contains(r, lineMarker) {
			t.Errorf("%s carries the line: %q", id, r[max(0, len(r)-140):])
		}
	}
	if r := toolResultOf(msgs, "ok"); !strings.Contains(r, lineMarker) {
		t.Errorf("control round lost its line: %q", r[max(0, len(r)-140):])
	}
	// Collapsed-turn round: the projection elides it or leaves it without a line.
	if r := toolResultOf(msgs, "cl"); strings.Contains(r, lineMarker) {
		t.Errorf("collapsed round carries the line")
	}
}

func TestPruneResultLineNeverOnContextMarkResult(t *testing.T) {
	app := lineTestApp(t)
	appendRound(app, 1, "c1", "read_file", `{"path":"a.go"}`, strings.Repeat("a", 4000))
	app.appendConv(tcMsg("m1", "context_mark", `{"stub":{"r1":"facts"}}`))
	app.appendConv(trMsg("m1", "context_mark", strings.Repeat("m", 3000)))
	if r := toolResultOf(app.requestMessages(), "m1"); r != strings.Repeat("m", 3000) {
		t.Errorf("context_mark result touched: %q", r[:min(60, len(r))])
	}
}

func TestPruneResultLineWriteRoundWording(t *testing.T) {
	app := lineTestApp(t)
	appendRound(app, 1, "w1", "edit_file", `{"path":"a.go","old_string":"x","new_string":"y"}`, strings.Repeat("w", 4000))
	r := toolResultOf(app.requestMessages(), "w1")
	if !strings.HasSuffix(r, "[r1 · ~1000 tokens · done with this? context_mark it: stub with the facts you still need]") {
		t.Errorf("write round line: %q", r[len(r)-120:])
	}
	if strings.Contains(r, "or drop") {
		t.Error("write round offers drop")
	}
}

func TestPruneResultLineOncePerMultiResultRound(t *testing.T) {
	app := lineTestApp(t)
	app.curTurn = 1
	app.appendConv(proxy.Message{Role: "assistant", ToolCalls: []proxy.ToolCall{
		{ID: "p1", Type: "function", Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"a"}`}},
		{ID: "p2", Type: "function", Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"b"}`}},
	}})
	app.appendConv(trMsg("p1", "read_file", strings.Repeat("a", 3000)))
	app.appendConv(trMsg("p2", "read_file", strings.Repeat("b", 3000)))
	msgs := app.requestMessages()
	if strings.Contains(toolResultOf(msgs, "p1"), lineMarker) {
		t.Error("first result of a multi-result round carries the line")
	}
	if !strings.Contains(toolResultOf(msgs, "p2"), "[r1 · ~1500 tokens") {
		t.Errorf("last result lacks the line: %q", toolResultOf(msgs, "p2")[3000:])
	}
	if n := strings.Count(toolResultOf(msgs, "p1")+toolResultOf(msgs, "p2"), lineMarker); n != 1 {
		t.Errorf("line count = %d, want 1", n)
	}
}

func TestPruneResultLineNeverStoredOrArchived(t *testing.T) {
	app := lineTestApp(t)
	appendRound(app, 1, "c1", "read_file", `{"path":"a.go"}`, strings.Repeat("a", 4000))
	_ = app.requestMessages()
	for _, m := range app.Conv {
		if strings.Contains(DerefStr(m.Content), "context_mark it") || strings.HasPrefix(DerefStr(m.Content), "[r1]") {
			t.Errorf("a.Conv carries projection text: %q", DerefStr(m.Content)[:min(80, len(DerefStr(m.Content)))])
		}
	}
	// The archive is rendered from a.Conv, never from the projection.
	app.stateMu.RLock()
	rec := app.pruneRounds[1]
	app.stateMu.RUnlock()
	arch := renderRoundArchive(app.Conv, rec)
	if strings.Contains(arch, "context_mark it") || strings.Contains(arch, "[r1]") {
		t.Error("archive text carries projection text")
	}
	if !strings.Contains(arch, strings.Repeat("a", 100)) {
		t.Error("archive lost the original result")
	}
}

func TestPruneResultLineDisappearsAfterMark(t *testing.T) {
	app := lineTestApp(t)
	appendRound(app, 1, "c1", "read_file", `{"path":"a.go"}`, strings.Repeat("a", 4000))
	appendRound(app, 1, "c2", "read_file", `{"path":"b.go"}`, strings.Repeat("b", 4000))
	if !strings.Contains(toolResultOf(app.requestMessages(), "c1"), lineMarker) {
		t.Fatal("precondition: r1 should carry the line")
	}
	app.recordMarkRatings(nil, []int{1}, nil) // keep: the model decided, line goes away
	if strings.Contains(toolResultOf(app.requestMessages(), "c1"), lineMarker) {
		t.Error("rated round still carries the line")
	}
}

func TestPruneSystemSection(t *testing.T) {
	app := lineTestApp(t)
	pre := app.buildPreamble("today")
	i := strings.Index(pre, "## Context pruning: mark rounds as you go")
	if i < 0 {
		t.Fatal("section missing with prune on")
	}
	sec := app.pruneSystemSection()
	for _, bad := range []string{"optional \"prev\" property rating", "later pass will rate", "\"drop\" \"(no longer", "If you omit"} {
		if strings.Contains(pre, bad) {
			t.Errorf("preamble still contains %q", bad)
		}
	}
	if !strings.HasPrefix(strings.SplitN(sec, "\n", 2)[1], "Call context_mark") {
		t.Errorf("section does not lead with context_mark: %q", sec)
	}
	for _, s := range []string{"[rN]", "re-sent in full on every step", "as you go", "names, paths, values and line numbers", "archive path"} {
		if !strings.Contains(sec, s) {
			t.Errorf("section lacks %q", s)
		}
	}
	if strings.Contains(sec, "later pass") {
		t.Error("rater sentence present with rater off")
	}
	app.Cfg.ContextPruneRater = "main"
	if !strings.Contains(app.pruneSystemSection(), "later pass") {
		t.Error("rater sentence missing with rater on")
	}
	// Off: not in the preamble at all.
	app.Cfg.ContextPrune = "off"
	if strings.Contains(app.buildPreamble("today"), "Context pruning") {
		t.Error("section present with prune off")
	}
	if !strings.Contains(contextMarkDescription, "Unmarked results are re-sent in full on every step") ||
		strings.Contains(contextMarkDescription, "left as they are") {
		t.Errorf("tool description: %s", contextMarkDescription)
	}
}

func TestPruneToggleRefreshesPreamble(t *testing.T) {
	app := lineTestApp(t)
	app.InjectDate = true
	app.Cfg.ContextPrune = "off"
	app.ensurePreamble()
	if strings.Contains(DerefStr(app.Conv[0].Content), "Context pruning") {
		t.Fatal("section present while off")
	}
	app.stateMu.Lock()
	app.setPruneOverride("on")
	app.invalidatePreambleForPruneLocked()
	app.stateMu.Unlock()
	app.ensurePreamble()
	if !strings.Contains(DerefStr(app.Conv[0].Content), "Context pruning") {
		t.Error("section missing after /prune on")
	}
	n := 0
	for _, m := range app.Conv {
		if m.Role == "system" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("system messages = %d, want 1", n)
	}
}

func TestMixedContextMarkMessageRegistersRound(t *testing.T) {
	app := lineTestApp(t)
	app.curTurn = 1
	appendRound(app, 1, "c1", "read_file", `{"path":"a.go"}`, strings.Repeat("a", 4000))
	app.appendConv(proxy.Message{Role: "assistant", ToolCalls: []proxy.ToolCall{
		{ID: "mx", Type: "function", Function: proxy.FunctionCall{Name: "context_mark", Arguments: `{"drop":["r1"]}`}},
		{ID: "rf", Type: "function", Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"b.go"}`}},
	}})
	app.appendConv(trMsg("mx", "context_mark", "marked: drop r1"))
	app.appendConv(trMsg("rf", "read_file", strings.Repeat("b", 4000)))
	app.stateMu.RLock()
	rec, ok := app.pruneRounds[2]
	app.stateMu.RUnlock()
	if !ok {
		t.Fatal("mixed message registered no round")
	}
	if len(rec.CallIDs) != 1 || rec.CallIDs[0] != "rf" || rec.ToolName != "read_file" {
		t.Errorf("round r2 = %+v, want only the read_file call", rec)
	}
	msgs := app.requestMessages()
	if !strings.HasPrefix(toolResultOf(msgs, "rf"), "[r2] ") {
		t.Errorf("read_file result lacks [r2]: %q", toolResultOf(msgs, "rf")[:20])
	}
	if strings.HasPrefix(toolResultOf(msgs, "mx"), "[r") {
		t.Error("context_mark result got a round id")
	}
}

func TestOnlyContextMarkMessageRegistersNoRound(t *testing.T) {
	app := lineTestApp(t)
	appendRound(app, 1, "c1", "read_file", `{"path":"a.go"}`, strings.Repeat("a", 4000))
	before := len(app.pruneRounds)
	app.appendConv(proxy.Message{Role: "assistant", ToolCalls: []proxy.ToolCall{
		{ID: "m1", Type: "function", Function: proxy.FunctionCall{Name: "context_mark", Arguments: `{"drop":["r1"]}`}},
		{ID: "m2", Type: "function", Function: proxy.FunctionCall{Name: "context_mark", Arguments: `{"keep":["r1"]}`}},
	}})
	if len(app.pruneRounds) != before || app.roundSeq != before {
		t.Errorf("only-context_mark message registered a round: %d → %d (seq %d)", before, len(app.pruneRounds), app.roundSeq)
	}
}

func TestPruneResultLineWaitsForAllResultsOfRound(t *testing.T) {
	app := lineTestApp(t)
	app.curTurn = 1
	app.appendConv(proxy.Message{Role: "assistant", ToolCalls: []proxy.ToolCall{
		{ID: "p1", Type: "function", Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"a"}`}},
		{ID: "p2", Type: "function", Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"b"}`}},
	}})
	app.appendConv(trMsg("p1", "read_file", strings.Repeat("a", 3000)))
	partial := app.requestMessages()
	if strings.Contains(toolResultOf(partial, "p1"), lineMarker) {
		t.Error("partial round carries the line")
	}
	app.appendConv(trMsg("p2", "read_file", strings.Repeat("b", 3000)))
	full := app.requestMessages()
	// The earlier result's bytes did not change once the round completed.
	if toolResultOf(full, "p1") != toolResultOf(partial, "p1") {
		t.Error("first result changed when the round completed")
	}
	if !strings.Contains(toolResultOf(full, "p2"), "[r1 · ~1500 tokens") {
		t.Errorf("complete round lacks the line: %q", toolResultOf(full, "p2")[3000:])
	}
}

func TestMixedContextMarkCannotMarkItsOwnRound(t *testing.T) {
	app := lineTestApp(t)
	app.curTurn = 1
	appendRound(app, 1, "c1", "read_file", `{"path":"a.go"}`, strings.Repeat("a", 4000))
	call := proxy.ToolCall{ID: "mx", Type: "function", Function: proxy.FunctionCall{Name: "context_mark", Arguments: `{"drop":["r2","r1"]}`}}
	app.appendConv(proxy.Message{Role: "assistant", ToolCalls: []proxy.ToolCall{
		call,
		{ID: "rf", Type: "function", Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"b.go"}`}},
	}})
	res := app.handleContextMark(context.Background(), call)
	if !strings.Contains(res, "r2") || !strings.Contains(res, "current round") {
		t.Errorf("own round r2 not rejected: %q", res)
	}
	if ratingOf(app, 1) != RatingDrop {
		t.Errorf("r1 = %q, want drop", ratingOf(app, 1))
	}
	if ratingOf(app, 2) != RatingNone {
		t.Errorf("r2 = %q, want none", ratingOf(app, 2))
	}
}
