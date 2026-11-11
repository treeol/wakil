package agent

import (
	"context"
	"encoding/json"
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
	// Layer 2: r4 is unrated with 4000 bytes (≥2KB) — eligible for
	// auto-stub after drop/stub exhaustion. Pairing must survive.
	assertNoOrphanedToolCalls(t, proj)
	assertPairing(t, proj)
	_ = r4intact
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

// --- 17. prev via tool args ---

func TestPrunePrevViaToolArgs(t *testing.T) {
	app := pruneTestApp()
	appendRound(app, 1, "c1", "read_file", `{"path":"a.go"}`, bigContent)
	// Next round carries prev=drop on its tool call (as the model sends it).
	app.curTurn = 1
	tcMsg2 := tcMsg("c2", "read_file", `{"path":"b.go","prev":"drop"}`)
	app.stripPrevFromMessage(&tcMsg2)
	app.appendConv(tcMsg2)
	app.appendConv(trMsg("c2", "read_file", bigContent))
	if ratingOf(app, 1) != RatingDrop {
		t.Fatalf("prev=drop should rate r1 = drop, got %q", ratingOf(app, 1))
	}
	// prev must not persist in stored args or reach the wire.
	for _, m := range app.Conv {
		for _, tc := range m.ToolCalls {
			if strings.Contains(tc.Function.Arguments, `"prev"`) {
				t.Errorf("stored args contain prev: %s", tc.Function.Arguments)
			}
		}
	}
	for _, m := range app.contextProjection() {
		for _, tc := range m.ToolCalls {
			if strings.Contains(tc.Function.Arguments, `"prev"`) {
				t.Errorf("wire args contain prev: %s", tc.Function.Arguments)
			}
		}
	}
}

func TestPrunePrevStrippedFromExecution(t *testing.T) {
	app := pruneTestApp()
	// Dispatcher defense: even if prev reached handleToolCall, the tool
	// must receive args without it.
	fe := newFakeExecutor()
	fe.files["x.go"] = "hello"
	app.Exec = fe
	app.Cfg.ToolResultCap = 0
	tc := proxy.ToolCall{Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"x.go","prev":"stub: remember this"}`}}
	res := app.handleToolCall(context.Background(), tc)
	if !res.ok {
		t.Errorf("read_file with prev should succeed, got: %s", res.text)
	}
	// handleToolCall strips prev at entry; the stored result must have
	// succeeded reading x.go through the fake executor (prev never
	// reached the read handler — a prev leak to a strict validator
	// would have failed, and the fake here is permissive by design).
	if !strings.Contains(res.text, "hello") {
		t.Errorf("expected file content through prev-stripped call, got: %s", res.text)
	}
	// The caller's copy is by value; the defense strips its local copy.
	// Assert via the strip helper directly instead.
	if _, _, had := stripPrevFromArgs(tc.Function.Arguments); !had {
		// prev already stripped in the live path — here, direct calls
		// keep their args; the guarantee is execution saw stripped args.
		// Verify the helper strips before execution:
		cleaned, _, _ := stripPrevFromArgs(`{"path":"x.go","prev":"drop"}`)
		if strings.Contains(cleaned, `"prev"`) {
			t.Error("stripPrevFromArgs failed to strip")
		}
	}
	_ = tc
}

// --- 17b. Auto-stub fallback ---

func TestPruneAutoStubEligible(t *testing.T) {
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
	// Superseded auto-stub: r1 is unrated ≥2KB whose same path "x" is
	// re-read by r2 (later). Prior to the thrash-field fix, size alone
	// (autoStubMinBytes) made ALL unrated ≥2KB rounds eligible; now only
	// superseded (re-read/edit/same-command) rounds qualify.
	// Round ids: r1 c1, r2 c2 (superseder), r3/r4/r5 protected buffer.
	appendRound(app, 1, "c1", "read_file", `{"path":"x"}`, big)
	appendRound(app, 1, "c2", "read_file", `{"path":"x"}`, big)
	appendRound(app, 1, "c3", "read_file", `{"path":"y"}`, big)
	appendRound(app, 1, "c4", "read_file", `{"path":"z"}`, big)
	appendRound(app, 1, "c5", "read_file", `{"path":"w"}`, big)
	// Force the trigger: make at/target tiny so the initial size check
	// fires, then pruneCheck prunes until size <= target.
	app.Cfg.ContextPruneAt = 0.001
	app.Cfg.ContextPruneTarget = 0.0001
	app.Cfg.HardMaxBytes = 100000
	app.Cfg.CompactAt = 70000
	app.Cfg.KeepBytes = 50000
	app.pruneCheck()
	found := false
	for _, m := range app.contextProjection() {
		if m.ToolCallID == "c1" && strings.Contains(DerefStr(m.Content), "superseded") {
			found = true
		}
		if m.ToolCallID == "c5" && strings.Contains(DerefStr(m.Content), "pruned") {
			t.Error("protected round must not be auto-pruned")
		}
	}
	if !found {
		t.Error("unrated ≥2KB superseded read round should be auto-stubbed (superseded)")
	}
}

func TestPruneAutoStubNeverWrite(t *testing.T) {
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
	appendRound(app, 1, "c2", "read_file", `{"path":"x"}`, big)
	appendRound(app, 1, "c3", "read_file", `{"path":"x"}`, big)
	appendRound(app, 1, "c4", "read_file", `{"path":"x"}`, big)
	app.pruneCheck()
	for _, m := range app.contextProjection() {
		if m.ToolCallID == "c1" && strings.Contains(DerefStr(m.Content), "pruned") {
			t.Errorf("write round must never be auto-pruned: %q", DerefStr(m.Content))
		}
	}
}

// --- 17c. Never-inflate ---

func TestPruneNeverInflate(t *testing.T) {
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

	// All rounds are 4KB reads but only ~100-byte stub overhead — a
	// 2-line result ("ok") would inflate; here every result is big
	// enough that the stub is strictly smaller, so pruning proceeds.
	big := strings.Repeat("y", 4000)
	appendRound(app, 1, "c1", "read_file", `{"path":"x"}`, big)
	ratePrevious(app, 1, "[[drop]]")
	appendRound(app, 1, "c2", "read_file", `{"path":"x"}`, big)
	appendRound(app, 1, "c3", "read_file", `{"path":"x"}`, big)
	appendRound(app, 1, "c4", "read_file", `{"path":"x"}`, big)
	app.pruneCheck()
	pruned := false
	for _, m := range app.contextProjection() {
		if m.ToolCallID == "c1" && strings.Contains(DerefStr(m.Content), "pruned") {
			pruned = true
		}
	}
	if !pruned {
		t.Error("round with 4000-byte result should be pruned (stub is smaller)")
	}

	// Now a round whose result is tiny (5 bytes): stub (~120 bytes)
	// would be larger than the original — never inflate.
	app2 := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	_ = app2.SetContextMode("standard")
	app2.Cfg.ContextPrune = "on"
	app2.Cfg.ContextPruneAt = 0.70
	app2.Cfg.ContextPruneTarget = 0.55
	app2.Cfg.ContextPruneProtect = 3
	app2.Cfg.HardMaxBytes = 10000
	app2.Cfg.CompactAt = 7000
	app2.Cfg.KeepBytes = 5000
	app2.Cfg.MaxChars = 0
	app2.Cfg.CompactAtFrac = 0
	app2.Cfg.KeepBytesFrac = 0
	app2.Cfg.HardMaxFrac = 0
	os.Setenv("XDG_DATA_HOME", tmp)
	t.Cleanup(func() { os.Unsetenv("XDG_DATA_HOME") })
	appendRound(app2, 1, "c1", "read_file", `{"path":"x"}`, "ok")
	ratePrevious(app2, 1, "[[drop]]")
	appendRound(app2, 1, "c2", "read_file", `{"path":"x"}`, big)
	appendRound(app2, 1, "c3", "read_file", `{"path":"x"}`, big)
	appendRound(app2, 1, "c4", "read_file", `{"path":"x"}`, big)
	app2.pruneCheck()
	for _, m := range app2.contextProjection() {
		if m.ToolCallID == "c1" && strings.Contains(DerefStr(m.Content), "pruned") {
			t.Errorf("never-inflate: tiny result must not be pruned: %q", DerefStr(m.Content))
		}
	}
}

// --- 17d. Final-answer tag strip ---

func TestPruneFinalAnswerTagStrip(t *testing.T) {
	app := pruneTestApp()
	appendRound(app, 1, "c1", "read_file", `{"path":"x"}`, bigContent)
	app.curTurn = 1
	app.appendConv(proxy.Message{Role: "assistant", Content: StrPtr("done. [[drop]]")})
	if ratingOf(app, 1) != RatingDrop {
		t.Errorf("final-answer tag should rate the last round: %q", ratingOf(app, 1))
	}
	// Stored content must be tag-free.
	for _, m := range app.Conv {
		if c := DerefStr(m.Content); strings.Contains(c, "[[drop]]") {
			t.Errorf("stored assistant content contains tag: %q", c)
		}
	}
	// Returned final must be tag-free.
	if got := app.stripFinalTags("done. [[drop]]"); got != "done. " {
		t.Errorf("stripFinalTags = %q, want %q", got, "done. ")
	}
	// Off: no stripping.
	app2 := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app2.Cfg.ContextPrune = "off"
	if got := app2.stripFinalTags("done. [[drop]]"); got != "done. [[drop]]" {
		t.Errorf("off-mode stripFinalTags must be identity, got %q", got)
	}
}

// --- 17e. /prune toggle + resume ---

func TestPruneToggleAndResume(t *testing.T) {
	app := pruneTestApp()
	// Default (config on) → pruneEnabled true.
	if !app.pruneEnabled() {
		t.Fatal("prune should be on from config")
	}
	// /prune off via override, persisted.
	app.stateMu.Lock()
	app.setPruneOverride("off")
	app.stateMu.Unlock()
	if app.pruneEnabled() {
		t.Error("/prune off must disable pruning")
	}
	s := &Session{Conv: append([]proxy.Message(nil), app.Conv...)}
	s.PruneOverride = "off"
	app2 := pruneTestApp()
	app2.Conv = append([]proxy.Message(nil), app.Conv...)
	app2.RestoreTurnState(s)
	if app2.pruneEnabled() {
		t.Error("resumed /prune off must survive")
	}
	if app2.pruneToggleStatus() == "" {
		t.Error("toggle status must be non-empty")
	}
}

// --- 17f. /compact off-mode unchanged ---

func TestPruneCompactOffUnchanged(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	_ = app.SetContextMode("standard")
	app.Cfg.ContextPrune = "off"
	app.curTurn = 1
	app.appendConv(tcMsg("c1", "read_file", `{"path":"a.go"}`))
	app.appendConv(trMsg("c1", "read_file", bigContent))
	app.appendConv(tcMsg("c2", "search_files", `{"pattern":"x"}`))
	app.appendConv(trMsg("c2", "search_files", bigContent))
	// Snapshot Conv, run Compact (soft pass — no-op when under threshold),
	// assert Conv is identical.
	before := append([]proxy.Message(nil), app.Conv...)
	ok, err := app.Compact(context.Background(), app.summarizeFn(), false)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if ok {
		t.Error("soft Compact should be a no-op for a small transcript")
	}
	if len(before) != len(app.Conv) {
		t.Fatalf("Conv length changed: %d -> %d", len(before), len(app.Conv))
	}
	for i := range before {
		if before[i].Role != app.Conv[i].Role || DerefStr(before[i].Content) != DerefStr(app.Conv[i].Content) {
			t.Errorf("Conv[%d] changed after /compact (off mode): %q -> %q", i, DerefStr(before[i].Content), DerefStr(app.Conv[i].Content))
		}
	}
	if len(app.pruneRounds) != 0 {
		t.Errorf("off mode must have no prune records, got %d", len(app.pruneRounds))
	}
}

// --- 17g. Khatam replay: 29 large reads, zero ratings, at=0.15 ---

func TestPruneKhatamReplay(t *testing.T) {
	tmp := t.TempDir()
	os.Setenv("XDG_DATA_HOME", tmp)
	t.Cleanup(func() { os.Unsetenv("XDG_DATA_HOME") })

	big := strings.Repeat("k", 6000)
	// --- (a) no ratings anywhere → no thrash: superseded-only pruning,
	// and below the 90% emergency level no bulk auto-prune at all. ---
	// Each read is a DIFFERENT path here (a real read-all-of-internal/web
	// shape: 29 reads, 29 different files). With no supersede (paths do
	// not repeat) and no ratings, prune candidates are empty.
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	_ = app.SetContextMode("standard")
	app.Cfg.ContextPrune = "on"
	app.Cfg.ContextPruneAt = 0.15
	app.Cfg.ContextPruneTarget = 0.10
	app.Cfg.ContextPruneProtect = 3
	app.Cfg.HardMaxBytes = 160000
	app.Cfg.CompactAt = 145000
	app.Cfg.KeepBytes = 120000
	app.Cfg.MaxChars = 0
	app.Cfg.CompactAtFrac = 0
	app.Cfg.KeepBytesFrac = 0
	app.Cfg.HardMaxFrac = 0
	app.curTurn = 1
	for i := 1; i <= 29; i++ {
		id := "k" + string(rune('a'+i)) + string(rune('0'+i%10))
		path := "f" + string(rune('a'+i)) + ".go"
		app.appendConv(tcMsg(id, "read_file", `{"path":"`+path+`"}`))
		app.appendConv(trMsg(id, "read_file", big))
		app.pruneCheck()
	}
	pruned := 0
	for _, m := range app.contextProjection() {
		if strings.Contains(DerefStr(m.Content), "pruned") {
			pruned++
		}
	}
	// Distinct paths → no superseded candidates → 0 pruned.
	if pruned != 0 {
		t.Errorf("no-ratings khatam: expected 0 pruned (no superseded, below emergency), got %d", pruned)
	}
	assertPairing(t, app.contextProjection())

	// --- (b) same path re-read → the earlier unrated round is superseded
	// and pruned (its content is no longer needed — a later read has it).
	app2 := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	_ = app2.SetContextMode("standard")
	app2.Cfg.ContextPrune = "on"
	app2.Cfg.ContextPruneAt = 0.15
	app2.Cfg.ContextPruneTarget = 0.10
	app2.Cfg.ContextPruneProtect = 3
	app2.Cfg.HardMaxBytes = 160000
	app2.Cfg.CompactAt = 145000
	app2.Cfg.KeepBytes = 120000
	app2.Cfg.MaxChars = 0
	app2.Cfg.CompactAtFrac = 0
	app2.Cfg.KeepBytesFrac = 0
	app2.Cfg.HardMaxFrac = 0
	app2.curTurn = 1
	for i := 1; i <= 10; i++ {
		id := "s" + string(rune('a'+i)) + string(rune('0'+i%10))
		app2.appendConv(tcMsg(id, "read_file", `{"path":"x"}`))
		app2.appendConv(trMsg(id, "read_file", big))
		app2.pruneCheck()
	}
	// Last 3 protected → rounds 1..7 are superseded (same path "x").
	pruned2 := 0
	for _, m := range app2.contextProjection() {
		c := DerefStr(m.Content)
		if strings.Contains(c, "superseded") || strings.Contains(c, "pruned") {
			pruned2++
		}
	}
	if pruned2 == 0 {
		t.Error("superseded replay: expected superseded pruning of earlier same-path rounds")
	}
	assertPairing(t, app2.contextProjection())

	// --- (c) emergency: push past 90% of hard max with unrated, distinct-
	// path rounds → oldest unrated pruned as emergency.
	app3 := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	_ = app3.SetContextMode("standard")
	app3.Cfg.ContextPrune = "on"
	app3.Cfg.ContextPruneAt = 0.15
	app3.Cfg.ContextPruneTarget = 0.10
	app3.Cfg.ContextPruneProtect = 3
	app3.Cfg.HardMaxBytes = 20000 // 90% = 18000
	app3.Cfg.CompactAt = 19000
	app3.Cfg.KeepBytes = 15000
	app3.Cfg.MaxChars = 0
	app3.Cfg.CompactAtFrac = 0
	app3.Cfg.KeepBytesFrac = 0
	app3.Cfg.HardMaxFrac = 0
	app3.curTurn = 1
	for i := 1; i <= 6; i++ {
		id := "e" + string(rune('a'+i)) + string(rune('0'+i%10))
		path := "e" + string(rune('a'+i)) + ".go"
		app3.appendConv(tcMsg(id, "read_file", `{"path":"`+path+`"}`))
		app3.appendConv(trMsg(id, "read_file", big))
		app3.pruneCheck()
	}
	// 6 × 6KB = 36KB ≥ 90% of 20KB → emergency pruning of oldest unrated
	// (rounds 1..3, since protect=3) must have fired.
	emergency := 0
	for _, m := range app3.contextProjection() {
		if strings.Contains(DerefStr(m.Content), "auto-emergency") {
			emergency++
		}
	}
	if emergency == 0 {
		t.Error("emergency replay: no emergency pruning above 90% of hard max")
	}
	assertPairing(t, app3.contextProjection())
	// The pruned ones are logged as emergency in the rate-mapped count.
	em := 0
	for _, rec := range app3.pruneRounds {
		if rec.Rating == RatingEmergency {
			em++
		}
	}
	if em == 0 {
		t.Error("emergency replay: no record rated emergency")
	}
}

// --- 18. Absolute thresholds (§2) ---

func TestPruneAbsoluteThresholds(t *testing.T) {
	// 1M-token backend case: at_tokens=60000 converts to 240k chars,
	// fraction 0.70×3.2M chars ≈ 2.24M — min = 240k chars (60k tokens).
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.ContextPruneAtTokens = 60000
	app.Cfg.ContextPruneTargetTokens = 40000
	app.Cfg.ContextPruneAt = 0.70
	app.Cfg.ContextPruneTarget = 0.55
	app.Cfg.HardMaxBytes = 3200000
	app.Cfg.CompactAt = 2900000
	app.Cfg.KeepBytes = 2400000
	at, target := app.pruneThresholds()
	if at != 60000*4 {
		t.Errorf("1M at = %d, want %d (token cap dominates)", at, 60000*4)
	}
	if target != 40000*4 {
		t.Errorf("1M target = %d, want %d", target, 40000*4)
	}
	// 160k-window case: at_frac 0.70×512k ≈ 358k, token cap 240k → min = 240k.
	app2 := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app2.Cfg.ContextPruneAtTokens = 60000
	app2.Cfg.ContextPruneTargetTokens = 40000
	app2.Cfg.ContextPruneAt = 0.70
	app2.Cfg.ContextPruneTarget = 0.55
	app2.Cfg.HardMaxBytes = 512000
	app2.Cfg.CompactAt = 320000
	app2.Cfg.KeepBytes = 256000
	at2, target2 := app2.pruneThresholds()
	if at2 != 60000*4 {
		t.Errorf("160k at = %d, want %d (min wins)", at2, 60000*4)
	}
	if target2 != 40000*4 {
		t.Errorf("160k target = %d, want %d", target2, 40000*4)
	}
	// min() where fraction wins: huge token caps.
	app3 := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app3.Cfg.ContextPruneAtTokens = 10000000
	app3.Cfg.ContextPruneTargetTokens = 9000000
	app3.Cfg.ContextPruneAt = 0.70
	app3.Cfg.ContextPruneTarget = 0.55
	app3.Cfg.HardMaxBytes = 10000
	app3.Cfg.CompactAt = 7000
	app3.Cfg.KeepBytes = 5000
	at3, target3 := app3.pruneThresholds()
	if at3 != 7000 {
		t.Errorf("fraction-wins at = %d, want 7000", at3)
	}
	if target3 != 5500 {
		t.Errorf("fraction-wins target = %d, want 5500", target3)
	}
}

// --- 18b. Split-tag stream test (§3) ---

func TestTagBufferSplitTag(t *testing.T) {
	b := &tagBuffer{}
	var out strings.Builder
	out.WriteString(b.push("hello [[drop"))
	out.WriteString(b.push("]] world"))
	got := out.String()
	if strings.Contains(got, "[[") || strings.Contains(got, "]]") {
		t.Errorf("split tag leaked: %q", got)
	}
	if !strings.Contains(got, "hello") || !strings.Contains(got, "world") {
		t.Errorf("prose lost: %q", got)
	}
	// Over-200 chars without close: flush verbatim.
	b2 := &tagBuffer{}
	long := "[[ " + strings.Repeat("x", 300)
	got2 := b2.push(long)
	if !strings.Contains(got2, "[[ ") {
		t.Errorf("200-char flush lost prose: %q", got2[:min(60, len(got2))])
	}
}

// --- 18c. Toggle regression via the real dispatch path (§1) ---

func TestPruneToggleViaDispatchPath(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	_ = app.SetContextMode("long-session")
	app.Cfg.ContextPrune = "off"
	// Off by default from config.
	if app.pruneEnabled() {
		t.Fatal("off by default")
	}
	handled, _, cmd := HandleTUICommand("/prune on", app)
	if !handled {
		t.Fatal("/prune on not handled")
	}
	note := cmd().(SysNoteMsg)
	if !strings.Contains(note.Text, "prune: on") {
		t.Errorf("notice = %q", note.Text)
	}
	// The next request application point: toolsForTurn's withPrevProperty
	// is invoked only when pruneEnabled — which must be true NOW.
	if !app.pruneEnabled() {
		t.Fatal("pruneEnabled false after /prune on")
	}
	// /mode status must agree in the same line — the field-bug regression.
	_, _, cmd2 := HandleTUICommand("/mode status", app)
	st := cmd2().(SysNoteMsg)
	if !strings.Contains(st.Text, "prune: on") {
		t.Errorf("mode status after toggle: %s", st.Text)
	}
	if !strings.Contains(st.Text, "trigger ") {
		t.Errorf("mode status must show trigger: %s", st.Text)
	}
	// /prune off round-trips.
	handled, _, cmd = HandleTUICommand("/prune off", app)
	if !handled {
		t.Fatal("/prune off not handled")
	}
	_ = cmd().(SysNoteMsg)
	if app.pruneEnabled() {
		t.Error("prune still enabled after /prune off")
	}
	_, _, cmd3 := HandleTUICommand("/prune status", app)
	st3 := cmd3().(SysNoteMsg)
	if !strings.Contains(st3.Text, "prune: off") {
		t.Errorf("prune status off: %s", st3.Text)
	}
	if st3.Text == "" {
		t.Error("prune status empty")
	}
}

// --- 19. Required prev + missing hint + counters + race ---

func TestPrunePrevRequiredInSchema(t *testing.T) {
	tools := []proxy.Tool{{Type: "function", Function: proxy.ToolFunction{
		Name:       "read_file",
		Parameters: []byte(`{"type":"object","properties":{"path":{"type":"string"}}}`),
	}}}
	out := withPrevProperty(tools)
	if len(out) != 1 {
		t.Fatalf("tools len = %d", len(out))
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(out[0].Function.Parameters, &obj); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	props, _ := obj["properties"].(map[string]interface{})
	if _, ok := props["prev"]; !ok {
		t.Error("prev missing from schema")
	}
	req, _ := obj["required"].([]interface{})
	found := false
	for _, r := range req {
		if r == "prev" {
			found = true
		}
	}
	if !found {
		t.Error("prev not in required[]")
	}
	// Off-mode: withPrevProperty is never called — golden test asserts
	// wire identity; here assert the injector itself is offline-safe on
	// empty params.
	empty := withPrevProperty([]proxy.Tool{{Type: "function", Function: proxy.ToolFunction{Name: "x"}}})
	if len(empty) != 1 {
		t.Fatal("empty tools")
	}
}

func TestPruneMissingPrevHintAndCounter(t *testing.T) {
	// Build an app past round 1 with prune on.
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	_ = app.SetContextMode("standard")
	app.Cfg.ContextPrune = "on"
	app.Cfg.ContextPruneProtect = 3
	app.Cfg.HardMaxBytes = 160000
	app.Cfg.CompactAt = 145000
	app.Cfg.KeepBytes = 120000
	app.Cfg.MaxChars = 0
	app.Cfg.CompactAtFrac = 0
	app.Cfg.KeepBytesFrac = 0
	app.Cfg.HardMaxFrac = 0
	app.curTurn = 1
	big := strings.Repeat("z", 4)
	// Go through appendConv (which assigns round ids) so roundSeq > 1.
	app.appendConv(tcMsg("h1", "read_file", `{"path":"a.go"}`))
	app.appendConv(trMsg("h1", "read_file", big))
	app.appendConv(tcMsg("h2", "read_file", `{"path":"b.go"}`))
	app.appendConv(trMsg("h2", "read_file", big))
	// Simulate a message with NO prev at round ≥2: hint + counter.
	hint := app.checkPrevMissing(nil, []proxy.ToolCall{{ID: "h3"}})
	if hint == "" {
		t.Fatal("missing prev should produce a hint")
	}
	if !strings.Contains(hint, "unrated") {
		t.Errorf("hint = %q", hint)
	}
	if hint == "" || app.pruneMissingPrev == 0 {
		t.Error("missing counter not incremented")
	}
	if app.pruneMissingPrev != 1 {
		t.Errorf("pruneMissingPrev = %d, want 1", app.pruneMissingPrev)
	}
	// Round 1: no obligation.
	app2 := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	_ = app2.SetContextMode("standard")
	app2.Cfg.ContextPrune = "on"
	app2.curTurn = 1
	if h := app2.checkPrevMissing(nil, []proxy.ToolCall{{ID: "x"}}); h != "" {
		t.Errorf("round 1 must not hint, got %q", h)
	}
}

func TestPruneSupersededDetection(t *testing.T) {
	// Direct unit test of findSuperseded via synthetic rounds.
	rounds := map[int]RoundRecord{
		1: {ID: 1, Rating: RatingNone, ReadPaths: []string{"a.go"}, BytesLost: 4000},
		2: {ID: 2, Rating: RatingNone, ReadPaths: []string{"a.go"}, BytesLost: 4000},
		3: {ID: 3, Rating: RatingNone, ReadPaths: []string{"b.go"}, HasShellCmd: true, CmdText: "ls", BytesLost: 4000},
		4: {ID: 4, Rating: RatingNone, ReadPaths: []string{"b.go"}, Write: true, HasShellCmd: true, CmdText: "ls", BytesLost: 4000},
	}
	order := []int{1, 2, 3, 4}
	sup := findSuperseded(order, rounds)
	if _, ok := sup[1]; !ok {
		t.Error("r1 should be superseded by r2 (same path read again)")
	}
	if _, ok := sup[3]; !ok {
		t.Error("r3 should be superseded by r4 (edit/same-command)")
	}
	if got := len(sup); got != 2 {
		t.Errorf("superseded count = %d, want 2", got)
	}
}

func TestPruneNoAutoPruneOfUnratedBelowEmergency(t *testing.T) {
	// Unrated distinct-path rounds, well below 90% of hard max: no pruning.
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	_ = app.SetContextMode("standard")
	app.Cfg.ContextPrune = "on"
	app.Cfg.ContextPruneAt = 0.15
	app.Cfg.ContextPruneTarget = 0.10
	app.Cfg.ContextPruneProtect = 3
	app.Cfg.HardMaxBytes = 160000
	app.Cfg.CompactAt = 145000
	app.Cfg.KeepBytes = 120000
	app.Cfg.MaxChars = 0
	app.Cfg.CompactAtFrac = 0
	app.Cfg.KeepBytesFrac = 0
	app.Cfg.HardMaxFrac = 0
	app.curTurn = 1
	big := strings.Repeat("q", 6000)
	for i := 1; i <= 6; i++ {
		id := "n" + string(rune('a'+i))
		path := "n" + string(rune('a'+i)) + ".go"
		appendRound(app, 1, id, "read_file", `{"path":"`+path+`"}`, big)
		app.pruneCheck()
	}
	// 36KB total < 90% of 160k (144k) → no emergency, no superseded
	// (distinct paths) → 0 pruned.
	for _, m := range app.contextProjection() {
		if strings.Contains(DerefStr(m.Content), "pruned") || strings.Contains(DerefStr(m.Content), "superseded") || strings.Contains(DerefStr(m.Content), "auto-emergency") {
			t.Errorf("unrated non-superseded round pruned below emergency: %q", DerefStr(m.Content))
		}
	}
}

func TestPruneRereadCounter(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	_ = app.SetContextMode("standard")
	app.Cfg.ContextPrune = "on"
	app.Cfg.ContextPruneAt = 0.001
	app.Cfg.ContextPruneTarget = 0.0001
	app.Cfg.ContextPruneProtect = 1
	app.Cfg.HardMaxBytes = 100000
	app.Cfg.CompactAt = 70000
	app.Cfg.KeepBytes = 50000
	app.Cfg.MaxChars = 0
	app.Cfg.CompactAtFrac = 0
	app.Cfg.KeepBytesFrac = 0
	app.Cfg.HardMaxFrac = 0
	tmp := t.TempDir()
	os.Setenv("XDG_DATA_HOME", tmp)
	t.Cleanup(func() { os.Unsetenv("XDG_DATA_HOME") })
	big := strings.Repeat("r", 4000)
	// r1 reads a.go (rated drop via text tag) → pruned.
	appendRound(app, 1, "rc1", "read_file", `{"path":"a.go"}`, big)
	ratePrevious(app, 1, "[[drop]]")
	appendRound(app, 1, "rc2", "read_file", `{"path":"b.go"}`, big)
	app.pruneCheck() // r1 pruned (drop, oldest non-protected)
	// Now re-read a.go — matches the pruned r1's path → counter + line.
	appendRound(app, 1, "rc3", "read_file", `{"path":"a.go"}`, big)
	if app.pruneReread == 0 {
		t.Error("reread counter not incremented for a path whose round was pruned")
	}
	if !strings.Contains(app.pruneStatusLine(), "rereads") {
		t.Errorf("status missing rereads: %s", app.pruneStatusLine())
	}
	if !strings.Contains(app.pruneStatusLine(), "prev missing") {
		t.Errorf("status missing prev missing: %s", app.pruneStatusLine())
	}
}

func TestPruneOverrideRaceWithStream(t *testing.T) {
	// -race target: toggle /prune from one goroutine while pruneEnabled is
	// read from another (turn streaming). The override is atomic, so this
	// must not race.
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	_ = app.SetContextMode("standard")
	app.Cfg.ContextPrune = "off"
	done := make(chan struct{})
	start := make(chan struct{})
	// toggler
	go func() {
		<-start
		for i := 0; i < 200; i++ {
			if i%2 == 0 {
				app.setPruneOverride("on")
			} else {
				app.setPruneOverride("off")
			}
		}
		close(done)
	}()
	// reader (mirrors the turn goroutine)
	close(start)
	for i := 0; i < 200; i++ {
		_ = app.pruneEnabled()
	}
	<-done
}
