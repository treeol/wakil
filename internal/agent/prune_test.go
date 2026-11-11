package agent

import (
	"os"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
	wtools "github.com/treeol/wakil/internal/tools"
)

// Config validation is tested in internal/config (validateContextLimits).
var _ = config.DefaultConfig

// prune_test.go: per-round value pruning (prune.go). All tests run with
// context_prune on, except the off-mode golden test which proves byte-
// identical behaviour when the feature is off.

// pruneTestApp builds an App with pruning on in long-session mode.
func pruneTestApp() *App {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	_ = app.SetContextMode("long-session")
	app.Cfg.ContextPrune = "on"
	app.Cfg.ContextPruneAt = 0.70
	app.Cfg.ContextPruneTarget = 0.55
	app.Cfg.ContextPruneProtect = 3
	return app
}

// appendRound appends one tool round (assistant with tool_calls + results)
// stamped with turn, and returns the round's record id.
func appendRound(app *App, turn int, callID, name, args, result string) int {
	app.curTurn = turn
	app.appendConv(tcMsg(callID, name, args))
	app.appendConv(trMsg(callID, name, result))
	app.stateMu.RLock()
	max := 0
	for id := range app.pruneRounds {
		if id > max {
			max = id
		}
	}
	app.stateMu.RUnlock()
	return max
}

// ratePrevious appends an assistant text message rating the previous round.
func ratePrevious(app *App, turn int, text string) {
	app.curTurn = turn
	app.appendConv(proxy.Message{Role: "assistant", Content: StrPtr(text)})
}

// ratingOf returns the stored rating for round id.
func ratingOf(app *App, id int) RoundRating {
	app.stateMu.RLock()
	defer app.stateMu.RUnlock()
	return app.pruneRounds[id].Rating
}

// --- 1. Off mode: byte-identical golden ---

func TestPruneOffByteIdentical(t *testing.T) {
	// Master golden: with pruning off, a multi-round conversation — including
	// assistant text containing rating-tag literals — is stored verbatim.
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	_ = app.SetContextMode("long-session")
	app.Cfg.ContextPrune = "off"
	app.curTurn = 1
	app.appendConv(tcMsg("c1", "read_file", `{"path":"a.go"}`))
	app.appendConv(trMsg("c1", "read_file", bigContent))
	app.appendConv(proxy.Message{Role: "assistant", Content: StrPtr("done. [[drop]] was literal text here."), ToolCalls: []proxy.ToolCall{{
		ID: "c2", Type: "function", Function: proxy.FunctionCall{Name: "search_files", Arguments: `{"pattern":"x"}`},
	}}})
	app.appendConv(trMsg("c2", "search_files", bigContent))
	if got := DerefStr(app.Conv[2].Content); got != "done. [[drop]] was literal text here." {
		t.Errorf("off: content should be verbatim, got %q", got)
	}
	if len(app.pruneRounds) != 0 {
		t.Errorf("off: no round records expected, got %d", len(app.pruneRounds))
	}
	proj := app.contextProjection()
	if len(proj) != len(app.Conv) {
		t.Fatalf("off: projection length %d != Conv %d", len(proj), len(app.Conv))
	}
	for i := range proj {
		if proj[i].Role != app.Conv[i].Role || DerefStr(proj[i].Content) != DerefStr(app.Conv[i].Content) {
			t.Errorf("off: msg %d differs: %q vs %q", i, DerefStr(proj[i].Content), DerefStr(app.Conv[i].Content))
		}
	}
}

// --- 2. Tag parsing ---

func TestPruneTagParsing(t *testing.T) {
	cases := []struct {
		in     string
		rating RoundRating
		stub   string
		found  bool
	}{
		{"[[keep]]", RatingKeep, "", true},
		{"[[drop]]", RatingDrop, "", true},
		{"[[stub: key finding]]", RatingStub, "key finding", true},
		{"text before [[keep]] and after", RatingKeep, "", true},
		{"[[drop]] then [[keep]]", RatingKeep, "", true}, // last wins
		{"[[keep]] then [[drop]]", RatingDrop, "", true}, // last wins
		{"[[stub: first]] then [[keep]]", RatingKeep, "", true},
		{"no tag at all", RatingNone, "", false},
		{"[[bogus]]", RatingNone, "", false},    // invalid → keep
		{"[[stub:]]", RatingNone, "", false},    // empty stub → keep
		{"[[stub:   ]]", RatingNone, "", false}, // whitespace stub → keep
		{"a [[drop b]]", RatingNone, "", false}, // malformed → keep
		// A "[[" inside a stub line: the first "]]" closes the stub, so the
		// inner "[[keep]]" is taken as the stub's tail text up to its own
		// close — the inner tag is NOT a separate rating (left-to-right first
		// match). The full winning stub is "a [[keep]] b"? No: [^\]] stops
		// at the inner "]", so the stub text is "a [[keep]" minus the inner
		// close — a malformed model stub line. The rating is still stub
		// (the outer tag won), and the stored stub text is everything to the
		// first "]]". This documents that stub lines must not contain the
		// tag syntax (the prompt says: one line of remembered facts).
		{"[[stub: a [[keep]] b]]", RatingStub, "a [[keep", true},
	}
	for _, c := range cases {
		rating, stub, found := parsePruneTag(c.in)
		if rating != c.rating || stub != c.stub || found != c.found {
			t.Errorf("parsePruneTag(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.in, rating, stub, found, c.rating, c.stub, c.found)
		}
	}
	// Stub length cap.
	long := "[[stub: " + strings.Repeat("z", 500) + "]]"
	_, stub, _ := parsePruneTag(long)
	if len([]rune(stub)) > stubTextMaxChars {
		t.Errorf("stub not capped: %d runes, want ≤ %d", len([]rune(stub)), stubTextMaxChars)
	}
}

// --- 3. Stripping: tags never reach Conv or the wire ---

func TestPruneTagsStrippedFromConv(t *testing.T) {
	app := pruneTestApp()
	appendRound(app, 1, "c1", "read_file", `{"path":"a.go"}`, bigContent)
	ratePrevious(app, 1, "[[drop]]\nworked.")
	if ratingOf(app, 1) != RatingDrop {
		t.Fatalf("ratingOf(r1) = %q, want drop", ratingOf(app, 1))
	}
	for i, m := range app.Conv {
		if c := DerefStr(m.Content); strings.Contains(c, "[[") {
			t.Errorf("Conv[%d] still contains a tag: %q", i, c)
		}
	}
	// Wire: projection must not contain tags either.
	for i, m := range app.contextProjection() {
		if c := DerefStr(m.Content); strings.Contains(c, "[[") {
			t.Errorf("projection[%d] contains a tag: %q", i, c)
		}
	}
}

// --- 4. Prune order & target; protect; keep/unrated ---

func TestPruneOrderAndTarget(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	_ = app.SetContextMode("standard") // pruning composes regardless of collapse mode
	app.Cfg.ContextPrune = "on"
	app.Cfg.ContextPruneAt = 0.70
	app.Cfg.ContextPruneTarget = 0.55
	app.Cfg.ContextPruneProtect = 3
	// Make hardMax the active threshold.
	app.Cfg.HardMaxBytes = 10000
	app.Cfg.CompactAt = 7000
	app.Cfg.KeepBytes = 5000
	app.Cfg.MaxChars = 0
	app.Cfg.CompactAtFrac = 0
	app.Cfg.KeepBytesFrac = 0
	app.Cfg.HardMaxFrac = 0
	tmp := t.TempDir()
	os.Setenv("XDG_DATA_HOME", tmp)
	t.Cleanup(func() { os.Unsetenv("XDG_DATA_HOME") })

	big := strings.Repeat("y", 4000)
	// r1 stub, r2 drop, r3 keep, r4 unrated, r5..r7 protect buffer (recent).
	appendRound(app, 1, "c1", "read_file", `{"path":"x"}`, big) // r1
	ratePrevious(app, 1, "[[stub: s1]]")                        // r1 = stub
	appendRound(app, 1, "c2", "read_file", `{"path":"x"}`, big) // r2
	ratePrevious(app, 1, "[[drop]]")                            // r2 = drop
	appendRound(app, 1, "c3", "read_file", `{"path":"x"}`, big) // r3
	ratePrevious(app, 1, "[[keep]]")                            // r3 = keep
	appendRound(app, 1, "c4", "read_file", `{"path":"x"}`, big) // r4 unrated
	appendRound(app, 1, "c5", "read_file", `{"path":"x"}`, big) // r5
	appendRound(app, 1, "c6", "read_file", `{"path":"x"}`, big) // r6
	appendRound(app, 1, "c7", "read_file", `{"path":"x"}`, big) // r7

	// Size: 7 rounds × 4000 + chats ≈ 28k > at=7000, target=5500. Pruning
	// drops (r2) first, then stubs (r1), until size ≤ 5500 or exhausted.
	app.pruneCheck()

	proj := app.contextProjection()
	// r2 tool result must be stubbed, r1 stubbed after r2 (order), r3/r4 intact.
	var r1stubbed, r2stubbed, r3intact, r4intact bool
	for _, m := range proj {
		c := DerefStr(m.Content)
		if m.ToolCallID == "c2" && strings.Contains(c, "[r2 pruned (drop):") {
			r2stubbed = true
		}
		if m.ToolCallID == "c1" && strings.Contains(c, "[r1 pruned: s1") {
			r1stubbed = true
		}
		if m.ToolCallID == "c3" && c == big {
			r3intact = true
		}
		if m.ToolCallID == "c4" && c == big {
			r4intact = true
		}
	}
	if !r2stubbed {
		t.Error("r2 should be pruned as drop")
	}
	if !r1stubbed {
		t.Error("r1 should be pruned as stub after r2")
	}
	if !r3intact {
		t.Error("r3 (keep) must not be pruned")
	}
	if !r4intact {
		t.Error("r4 (unrated) must not be pruned")
	}
	// Pairing after pruning.
	assertNoOrphanedToolCalls(t, proj)
	assertPairing(t, proj)
}

func TestPruneNeverProtectOrKeep(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	_ = app.SetContextMode("standard")
	app.Cfg.ContextPrune = "on"
	app.Cfg.ContextPruneAt = 0.70
	app.Cfg.ContextPruneTarget = 0.55
	app.Cfg.ContextPruneProtect = 3
	app.Cfg.HardMaxBytes = 10000
	app.Cfg.CompactAt = 7000
	app.Cfg.KeepBytes = 5000
	app.Cfg.MaxChars = 0
	app.Cfg.CompactAtFrac = 0
	app.Cfg.KeepBytesFrac = 0
	app.Cfg.HardMaxFrac = 0
	tmp := t.TempDir()
	os.Setenv("XDG_DATA_HOME", tmp)
	t.Cleanup(func() { os.Unsetenv("XDG_DATA_HOME") })

	big := strings.Repeat("y", 4000)
	appendRound(app, 1, "c1", "read_file", `{"path":"x"}`, big)
	ratePrevious(app, 1, "[[drop]]")
	appendRound(app, 2, "c2", "read_file", `{"path":"x"}`, big)
	appendRound(app, 3, "c3", "read_file", `{"path":"x"}`, big)
	appendRound(app, 4, "c4", "read_file", `{"path":"x"}`, big)
	// /keep the first turn.
	app.stateMu.Lock()
	app.keep = map[int]bool{1: true}
	app.stateMu.Unlock()
	app.pruneCheck()
	// Everything either /keep-exempt or protected → nothing pruned.
	for _, m := range app.contextProjection() {
		if strings.Contains(DerefStr(m.Content), "pruned") {
			t.Errorf("/keep or protected round was pruned: %q", DerefStr(m.Content))
		}
	}
}

// --- 5. Write rounds rated drop become stubs ---

func TestPruneWriteRoundDemoted(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	_ = app.SetContextMode("standard")
	app.Cfg.ContextPrune = "on"
	app.Cfg.ContextPruneAt = 0.70
	app.Cfg.ContextPruneTarget = 0.55
	app.Cfg.ContextPruneProtect = 3
	app.Cfg.HardMaxBytes = 10000
	app.Cfg.CompactAt = 7000
	app.Cfg.KeepBytes = 5000
	app.Cfg.MaxChars = 0
	app.Cfg.CompactAtFrac = 0
	app.Cfg.KeepBytesFrac = 0
	app.Cfg.HardMaxFrac = 0
	tmp := t.TempDir()
	os.Setenv("XDG_DATA_HOME", tmp)
	t.Cleanup(func() { os.Unsetenv("XDG_DATA_HOME") })

	big := strings.Repeat("y", 4000)
	appendRound(app, 1, "c1", "edit_file", `{"path":"f.go"}`, big)
	ratePrevious(app, 1, "[[drop]]")
	appendRound(app, 1, "c2", "read_file", `{"path":"x"}`, big)
	appendRound(app, 1, "c3", "read_file", `{"path":"x"}`, big)
	appendRound(app, 1, "c4", "read_file", `{"path":"x"}`, big)
	app.pruneCheck()
	for _, m := range app.contextProjection() {
		if m.ToolCallID == "c1" {
			c := DerefStr(m.Content)
			if strings.Contains(c, "(drop)") {
				t.Errorf("write round must not be dropped, only stubbed: %q", c)
			}
			if !strings.Contains(c, "write round pruned") {
				t.Errorf("write round should carry the write-pruned stub, got %q", c)
			}
		}
	}
}

// --- 6. Pairing invariant ---

func assertPairing(t *testing.T, proj []proxy.Message) {
	t.Helper()
	calls := map[string]int{}
	results := map[string]int{}
	for _, m := range proj {
		for _, tc := range m.ToolCalls {
			calls[tc.ID]++
		}
		if m.Role == "tool" {
			results[m.ToolCallID]++
		}
	}
	for id, n := range calls {
		if results[id] != n {
			t.Errorf("tool_call %q: %d calls but %d results", id, n, results[id])
		}
	}
	for id, n := range results {
		if calls[id] != n {
			t.Errorf("tool result %q: %d results but %d calls", id, n, calls[id])
		}
	}
}

// --- 7. Archive content ---

func TestPruneArchiveContent(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	_ = app.SetContextMode("standard")
	app.Cfg.ContextPrune = "on"
	app.Cfg.ContextPruneAt = 0.70
	app.Cfg.ContextPruneTarget = 0.55
	app.Cfg.ContextPruneProtect = 3
	app.Cfg.HardMaxBytes = 10000
	app.Cfg.CompactAt = 7000
	app.Cfg.KeepBytes = 5000
	app.Cfg.MaxChars = 0
	app.Cfg.CompactAtFrac = 0
	app.Cfg.KeepBytesFrac = 0
	app.Cfg.HardMaxFrac = 0
	tmp := t.TempDir()
	os.Setenv("XDG_DATA_HOME", tmp)
	t.Cleanup(func() { os.Unsetenv("XDG_DATA_HOME") })

	big := strings.Repeat("y", 4000)
	appendRound(app, 1, "c1", "read_file", `{"path":"target.go"}`, big)
	ratePrevious(app, 1, "[[drop]]")
	appendRound(app, 1, "c2", "read_file", `{"path":"x"}`, big)
	appendRound(app, 1, "c3", "read_file", `{"path":"x"}`, big)
	appendRound(app, 1, "c4", "read_file", `{"path":"x"}`, big)
	app.pruneCheck()
	app.stateMu.RLock()
	rec := app.pruneRounds[1]
	app.stateMu.RUnlock()
	if rec.Archive == "" {
		t.Fatal("round 1 was not archived (prune may not have run)")
	}
	content, err := wtools.ReadHostCacheFile(rec.Archive)
	if err != nil {
		t.Fatalf("archive unreadable: %v", err)
	}
	if !strings.Contains(string(content), big) {
		t.Error("archive does not contain the original result")
	}
	if !strings.Contains(string(content), "c1") {
		t.Error("archive does not contain the tool_call id")
	}
}

// --- 8. Archive failure ---

func TestPruneArchiveFailure(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	_ = app.SetContextMode("standard")
	app.Cfg.ContextPrune = "on"
	app.Cfg.ContextPruneAt = 0.70
	app.Cfg.ContextPruneTarget = 0.55
	app.Cfg.ContextPruneProtect = 3
	app.Cfg.HardMaxBytes = 10000
	app.Cfg.CompactAt = 7000
	app.Cfg.KeepBytes = 5000
	app.Cfg.MaxChars = 0
	app.Cfg.CompactAtFrac = 0
	app.Cfg.KeepBytesFrac = 0
	app.Cfg.HardMaxFrac = 0
	// Force SpillToCache to fail: empty chatID → toolCacheDir returns "".
	app.Client.ChatID = ""
	big := strings.Repeat("y", 4000)
	appendRound(app, 1, "c1", "read_file", `{"path":"x"}`, big)
	ratePrevious(app, 1, "[[drop]]")
	appendRound(app, 1, "c2", "read_file", `{"path":"x"}`, big)
	appendRound(app, 1, "c3", "read_file", `{"path":"x"}`, big)
	appendRound(app, 1, "c4", "read_file", `{"path":"x"}`, big)
	app.pruneCheck()
	app.stateMu.RLock()
	rec, ok := app.pruneRounds[1]
	app.stateMu.RUnlock()
	if ok && rec.Pruned {
		t.Error("round must not be pruned when archive write fails")
	}
	// The warning flag is set (once per session).
	app.stateMu.RLock()
	failed := app.pruneArchiveFailed
	app.stateMu.RUnlock()
	if !failed {
		t.Error("pruneArchiveFailed should be set after an archive failure")
	}
	// Original content still in Conv and projection.
	for _, m := range app.contextProjection() {
		if m.ToolCallID == "c1" && DerefStr(m.Content) != big {
			t.Errorf("failed-archive round should stay intact, got %q", DerefStr(m.Content))
		}
	}
}

// --- 9. Resume ---

func TestPruneResume(t *testing.T) {
	app := pruneTestApp()
	tmp := t.TempDir()
	os.Setenv("XDG_DATA_HOME", tmp)
	t.Cleanup(func() { os.Unsetenv("XDG_DATA_HOME") })
	app.Cfg.ContextPruneAt = 0.70
	app.Cfg.ContextPruneTarget = 0.55
	app.Cfg.ContextPruneProtect = 3
	app.Cfg.HardMaxBytes = 10000
	app.Cfg.CompactAt = 7000
	app.Cfg.KeepBytes = 5000
	app.Cfg.MaxChars = 0
	app.Cfg.CompactAtFrac = 0
	app.Cfg.KeepBytesFrac = 0
	app.Cfg.HardMaxFrac = 0
	big := strings.Repeat("y", 4000)
	appendRound(app, 1, "c1", "read_file", `{"path":"x"}`, big)
	ratePrevious(app, 1, "[[drop]]")
	appendRound(app, 1, "c2", "read_file", `{"path":"x"}`, big)
	appendRound(app, 1, "c3", "read_file", `{"path":"x"}`, big)
	appendRound(app, 1, "c4", "read_file", `{"path":"x"}`, big)

	s := &Session{Conv: append([]proxy.Message(nil), app.Conv...)}
	s.PruneRecords = app.pruneRecordsSlice()
	s.PruneBytesSaved = app.pruneBytesSaved
	app2 := pruneTestApp()
	app2.Conv = append([]proxy.Message(nil), app.Conv...)
	app2.RestoreTurnState(s)
	if ratingOf(app2, 1) != RatingDrop {
		t.Errorf("resumed ratingOf(r1) = %q, want drop", ratingOf(app2, 1))
	}
	if app2.roundSeq != 4 {
		t.Errorf("resumed roundSeq = %d, want 4", app2.roundSeq)
	}
	// Pruned state: force a prune then resume and check the projection stays stubbed.
	app.pruneCheck()
	s2 := &Session{Conv: append([]proxy.Message(nil), app.Conv...)}
	s2.PruneRecords = app.pruneRecordsSlice()
	s2.PruneBytesSaved = app.pruneBytesSaved
	app3 := pruneTestApp()
	app3.Conv = append([]proxy.Message(nil), app.Conv...)
	app3.RestoreTurnState(s2)
	for _, m := range app3.contextProjection() {
		if m.ToolCallID == "c1" && !strings.Contains(DerefStr(m.Content), "pruned") {
			t.Error("resumed pruned round should stay stubbed")
		}
	}
}

// --- 10. Rewind ---

func TestPruneRewind(t *testing.T) {
	app := pruneTestApp()
	appendRound(app, 1, "c1", "read_file", `{"path":"x"}`, "a")
	appendRound(app, 1, "c2", "read_file", `{"path":"x"}`, "b")
	// Truncate Conv (simulate rewind to first round).
	app.Conv = app.Conv[:2]
	app.pruneCollapseOnRewind()
	app.stateMu.RLock()
	_, ok1 := app.pruneRounds[1]
	_, ok2 := app.pruneRounds[2]
	app.stateMu.RUnlock()
	if !ok1 {
		t.Error("round 1 record should survive rewind")
	}
	if ok2 {
		t.Error("round 2 record should be deleted by rewind")
	}
}

// --- 11. Config validation ---

func TestPruneConfigDefaults(t *testing.T) {
	cfg := config.DefaultConfig()
	if cfg.ContextPrune != "off" {
		t.Errorf("DefaultConfig ContextPrune = %q, want off", cfg.ContextPrune)
	}
	if cfg.ContextPruneAt != 0.70 || cfg.ContextPruneTarget != 0.55 || cfg.ContextPruneProtect != 3 {
		t.Errorf("defaults = (%g, %g, %d), want (0.70, 0.55, 3)",
			cfg.ContextPruneAt, cfg.ContextPruneTarget, cfg.ContextPruneProtect)
	}
}

// --- 12. Pinned rounds are never pruned ---

func TestPrunePinnedNeverPruned(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	_ = app.SetContextMode("standard")
	app.Cfg.ContextPrune = "on"
	app.Cfg.ContextPruneAt = 0.70
	app.Cfg.ContextPruneTarget = 0.55
	app.Cfg.ContextPruneProtect = 3
	app.Cfg.HardMaxBytes = 10000
	app.Cfg.CompactAt = 7000
	app.Cfg.KeepBytes = 5000
	app.Cfg.MaxChars = 0
	app.Cfg.CompactAtFrac = 0
	app.Cfg.KeepBytesFrac = 0
	app.Cfg.HardMaxFrac = 0
	tmp := t.TempDir()
	os.Setenv("XDG_DATA_HOME", tmp)
	t.Cleanup(func() { os.Unsetenv("XDG_DATA_HOME") })

	big := strings.Repeat("y", 4000)
	appendRound(app, 1, "c1", "dispatch_subagent", `{}`, big)
	ratePrevious(app, 1, "[[drop]]")
	appendRound(app, 1, "c2", "read_file", `{"path":"x"}`, big)
	appendRound(app, 1, "c3", "read_file", `{"path":"x"}`, big)
	appendRound(app, 1, "c4", "read_file", `{"path":"x"}`, big)
	// Mark the first round's tool result Pinned (as finalizeToolResult does).
	for i := range app.Conv {
		if app.Conv[i].Role == "tool" && app.Conv[i].ToolCallID == "c1" {
			app.Conv[i].Pinned = true
		}
	}
	app.pruneCheck()
	for _, m := range app.contextProjection() {
		if m.ToolCallID == "c1" && DerefStr(m.Content) != big {
			t.Errorf("Pinned result must not be stubbed, got %q", DerefStr(m.Content))
		}
		if strings.Contains(DerefStr(m.Content), "pruned") && m.ToolCallID == "c1" {
			t.Errorf("Pinned round was pruned: %q", DerefStr(m.Content))
		}
	}
}

// --- 13. Archive renders multi-call round once ---

func TestPruneArchiveMultiCallOnce(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	_ = app.SetContextMode("standard")
	app.Cfg.ContextPrune = "on"
	app.Cfg.ContextPruneAt = 0.70
	app.Cfg.ContextPruneTarget = 0.55
	app.Cfg.ContextPruneProtect = 3
	app.Cfg.HardMaxBytes = 10000
	app.Cfg.CompactAt = 7000
	app.Cfg.KeepBytes = 5000
	app.Cfg.MaxChars = 0
	app.Cfg.CompactAtFrac = 0
	app.Cfg.KeepBytesFrac = 0
	app.Cfg.HardMaxFrac = 0
	tmp := t.TempDir()
	os.Setenv("XDG_DATA_HOME", tmp)
	t.Cleanup(func() { os.Unsetenv("XDG_DATA_HOME") })

	big := strings.Repeat("y", 4000)
	app.curTurn = 1
	twoCalls := proxy.Message{
		Role: "assistant",
		ToolCalls: []proxy.ToolCall{
			{ID: "c1", Type: "function", Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"a"}`}},
			{ID: "c2", Type: "function", Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"b"}`}},
		},
	}
	app.appendConv(twoCalls)
	app.appendConv(trMsg("c1", "read_file", big))
	app.appendConv(trMsg("c2", "read_file", big))
	ratePrevious(app, 1, "[[drop]]")
	appendRound(app, 1, "c3", "read_file", `{"path":"x"}`, big)
	appendRound(app, 1, "c4", "read_file", `{"path":"x"}`, big)
	appendRound(app, 1, "c5", "read_file", `{"path":"x"}`, big)
	app.pruneCheck()
	app.stateMu.RLock()
	rec := app.pruneRounds[1]
	app.stateMu.RUnlock()
	if rec.Archive == "" {
		t.Fatal("round 1 was not archived")
	}
	arch, err := wtools.ReadHostCacheFile(rec.Archive)
	if err != nil {
		t.Fatalf("archive unreadable: %v", err)
	}
	if strings.Count(string(arch), "read_file") < 2 {
		t.Errorf("archive should contain both calls; got:\n%s", string(arch))
	}
	// Assistant header rendered once (both calls inside one block, not
	// duplicated).
	if strings.Count(string(arch), "--- assistant ---") != 1 {
		t.Errorf("assistant message rendered %d times, want 1; archive:\n%s", strings.Count(string(arch), "--- assistant ---"), string(arch))
	}
}

// --- 14. Rating does not fall through to older rounds ---

func TestPruneRatingDoesNotFallThrough(t *testing.T) {
	app := pruneTestApp()
	appendRound(app, 1, "c1", "read_file", `{"path":"x"}`, "a")
	appendRound(app, 1, "c2", "read_file", `{"path":"x"}`, "b")
	ratePrevious(app, 1, "[[drop]]") // rates r2
	ratePrevious(app, 1, "[[drop]]") // r2 already rated — must NOT rate r1
	if ratingOf(app, 2) != RatingDrop {
		t.Errorf("ratingOf(r2) = %q, want drop", ratingOf(app, 2))
	}
	if ratingOf(app, 1) != RatingNone {
		t.Errorf("ratingOf(r1) = %q, want unrated (no fallthrough)", ratingOf(app, 1))
	}
}

// --- 15. Non-tool messages with a matching ToolCallID are untouched ---

func TestPruneProjectionRoleCheck(t *testing.T) {
	app := pruneTestApp()
	appendRound(app, 1, "c1", "read_file", `{"path":"x"}`, bigContent)
	ratePrevious(app, 1, "[[drop]]")
	appendRound(app, 1, "c2", "read_file", `{"path":"x"}`, bigContent)
	appendRound(app, 1, "c3", "read_file", `{"path":"x"}`, bigContent)
	appendRound(app, 1, "c4", "read_file", `{"path":"x"}`, bigContent)
	app.Cfg.ContextPruneAt = 0.0001
	app.Cfg.ContextPruneTarget = 0.00001
	app.Cfg.HardMaxBytes = 100000
	app.Cfg.CompactAt = 70000
	app.Cfg.KeepBytes = 50000
	app.pruneCheck()
	// A non-tool message smuggling the id must not be rewritten.
	app.appendConv(proxy.Message{Role: "user", ToolCallID: "c1", Content: StrPtr("smuggled")})
	for _, m := range app.contextProjection() {
		if m.Role == "user" && DerefStr(m.Content) == "smuggled" {
			continue // intact
		}
		if m.Role == "user" && m.ToolCallID == "c1" {
			t.Errorf("non-tool message with matching ToolCallID was rewritten: %q", DerefStr(m.Content))
		}
	}
}

// --- 16. Off-mode wire bytes (golden) ---

func TestPruneOffWireBytes(t *testing.T) {
	// With prune off, wire bytes of a multi-round conversation are exactly
	// the master wire bytes: Content verbatim, tags never stripped.
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	_ = app.SetContextMode("long-session")
	app.Cfg.ContextPrune = "off"
	app.curTurn = 1
	app.appendConv(tcMsg("c1", "read_file", `{"path":"a.go"}`))
	app.appendConv(trMsg("c1", "read_file", bigContent))
	app.appendConv(proxy.Message{Role: "assistant", Content: StrPtr("[[keep]] hello [[drop]]")})
	app.appendConv(tcMsg("c2", "search_files", `{"pattern":"x"}`))
	app.appendConv(trMsg("c2", "search_files", bigContent))
	app.appendConv(proxy.Message{Role: "assistant", Content: StrPtr("[[stub: s]] done.")})
	proj := app.contextProjection()
	var got []string
	for _, m := range proj {
		got = append(got, m.Role+"|"+DerefStr(m.Content))
	}
	want := []string{
		"assistant|",
		"tool|" + bigContent,
		"assistant|[[keep]] hello [[drop]]",
		"assistant|",
		"tool|" + bigContent,
		"assistant|[[stub: s]] done.",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("wire bytes differ.\ngot:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if len(app.pruneRounds) != 0 {
		t.Errorf("off: %d round records, want 0", len(app.pruneRounds))
	}
	if dp := DerefStr(app.Conv[2].Content); dp != "[[keep]] hello [[drop]]" {
		t.Errorf("off: stored content should be verbatim, got %q", dp)
	}
	if dp := DerefStr(app.Conv[5].Content); dp != "[[stub: s]] done." {
		t.Errorf("off: stored content should be verbatim, got %q", dp)
	}
}
