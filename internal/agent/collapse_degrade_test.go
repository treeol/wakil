package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/memory"
	"github.com/treeol/wakil/internal/proxy"
)

// --- Tier-2 degrade: old collapsed turns project to a pointer line ---

// buildManyCollapsedTurns builds n finished turns (1..n), each with a large W
// that has already been collapsed (records set directly, bypassing disk spill).
// Memory key uses archive basename; keep notes small.
func buildManyCollapsedTurns(t *testing.T, app *App, n int) {
	t.Helper()
	conv := make([]proxy.Message, 0, n*6)
	records := map[int]CollapseRecord{}
	for turn := 1; turn <= n; turn++ {
		msgs := oneCollapsedTurn(turn)
		for i := range msgs {
			msgs[i].Turn = turn
		}
		conv = append(conv, msgs...)
		archive := "/tmp/fake-archive-turn-" + itoa(turn)
		records[turn] = CollapseRecord{
			Turn:    turn,
			Archive: archive,
			WBytes:  5000,
			Note:    "[request " + itoa(turn) + " working set collapsed — 2 tool calls · 5000 chars]\nfull record: " + archive + "\n",
			At:      time.Now(),
		}
	}
	app.Conv = conv
	app.stateMu.Lock()
	app.collapsed = records
	app.stateMu.Unlock()
}

func itoa(n int) string {
	return fmt.Sprint(n)
}

// TestDegradeDisabledZeroTail keeps v1 behavior when ContextTailTurns is 0:
// collapsed turns stay at full U+A+note size regardless of age.
func TestDegradeDisabledZeroTail(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Cfg.ContextTailTurns = 0
	buildManyCollapsedTurns(t, app, 25)

	proj := app.contextProjection()
	// Each turn contributes U + A(+note) = 2 messages; none degrade.
	want := 25 * 2
	if len(proj) != want {
		t.Fatalf("tail=0: want %d msgs (v1 behavior), got %d", want, len(proj))
	}
}

// TestDegradeOldTurnsToPointers: with tail=20, turns 1..5 (25 total) degrade
// to one pointer message each; turns 6..25 stay at U+A.
func TestDegradeOldTurnsToPointers(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Cfg.ContextTailTurns = 20
	buildManyCollapsedTurns(t, app, 25)

	proj := app.contextProjection()
	want := 20*2 + 5 // tail at full size + 5 pointer lines
	if len(proj) != want {
		t.Fatalf("tail=20: want %d msgs, got %d", want, len(proj))
	}

	// Degraded turns appear as a single user-role pointer containing the
	// archive path — the recovery reference.
	pointers := 0
	for _, m := range proj {
		if strings.Contains(DerefStr(m.Content), "details archived") {
			pointers++
			if m.Role != "user" {
				t.Errorf("pointer line role = %q, want user", m.Role)
			}
		}
	}
	if pointers != 5 {
		t.Errorf("want 5 degraded pointer lines, got %d", pointers)
	}
	assertNoOrphanedToolCalls(t, proj)

	// Sent size stays roughly constant: measure that turns 1..5's projection
	// contribution is far below their full U+A+note size.
	projSize := 0
	for _, m := range proj {
		projSize += len(DerefStr(m.Content))
	}
	fullSize := 0
	for _, m := range app.Conv {
		fullSize += len(DerefStr(m.Content))
	}
	if projSize >= fullSize {
		t.Errorf("projected size %d should be well below full size %d", projSize, fullSize)
	}
}

// TestDegradeKeepExempt: a /keep turn never degrades even when old.
func TestDegradeKeepExempt(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Cfg.ContextTailTurns = 2
	buildManyCollapsedTurns(t, app, 6)
	// /keep turn 3 — remove its collapse record like AddKeep does.
	app.stateMu.Lock()
	delete(app.collapsed, 3)
	app.keep = map[int]bool{3: true}
	app.stateMu.Unlock()

	proj := app.contextProjection()
	fullTurn3 := 0
	pointerTurn3 := false
	for _, m := range proj {
		if m.Turn == 3 && strings.Contains(DerefStr(m.Content), "details archived") {
			pointerTurn3 = true
		}
		if m.Turn == 3 {
			fullTurn3++
		}
	}
	if pointerTurn3 {
		t.Error("keep-exempt turn 3 must not degrade to a pointer")
	}
	if fullTurn3 == 0 {
		t.Error("keep-exempt turn 3 should still be projected")
	}
}

// --- Auto memory index ---

// TestAutoIndexWritesMemoryEntry: collapsing a finished turn in long-session
// mode with a live MemoryStore writes a ctx/<chatID>/… mid-tier entry and
// records the key on the CollapseRecord.
func TestAutoIndexWritesMemoryEntry(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.MemoryStore = testStore(t)
	app.Conv = oneCollapsedTurn(1)
	for i := range app.Conv {
		app.Conv[i].Turn = 1
	}
	app.turnSeq = 1

	app.collapseFinishedTurns()

	app.stateMu.RLock()
	rec, ok := app.collapsed[1]
	app.stateMu.RUnlock()
	if !ok {
		t.Fatal("turn 1 should have collapsed")
	}
	if rec.MemoryKey == "" {
		t.Fatal("CollapseRecord.MemoryKey should be set after auto-index")
	}
	if !strings.HasPrefix(rec.MemoryKey, "ctx/") {
		t.Errorf("memory key %q should be namespaced under ctx/", rec.MemoryKey)
	}
	ctx := context.Background()
	entry, err := app.MemoryStore.Get(ctx, rec.MemoryKey)
	if err != nil || entry == nil {
		t.Fatalf("memory_get on %q failed: %v", rec.MemoryKey, err)
	}
	if entry == nil {
		t.Fatal("entry should be non-nil")
	}
	if entry.Tier != memory.TierMid {
		t.Errorf("entry tier = %q, want mid", entry.Tier)
	}
	if !strings.Contains(entry.Value, rec.Archive) {
		t.Error("memory entry value should reference the archive path")
	}
}

// TestStashSurvivesUpgrade: a mid-turn stash must survive the partial→full
// upgrade at turn end (review #1/#2: the upgrade previously rebuilt the
// record with Summary only).
func TestStashSurvivesUpgrade(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.MemoryStore = testStore(t)

	stashCall := proxy.ToolCall{
		ID: "ccS", Type: "function",
		Function: proxy.FunctionCall{
			Name:      "collapse_context",
			Arguments: `{"summary":"phase one done","memory_key":"plan-arch","description":"architecture plan"}`,
		},
	}
	app.Conv = []proxy.Message{
		{Role: "user", Content: StrPtr("plan the work"), Turn: 1},
		tcMsg("tcA", "read_file", `{"path":"a.go"}`),
		trMsg("tcA", "read_file", bigContent),
		{Role: "assistant", ToolCalls: []proxy.ToolCall{stashCall}, Turn: 1},
	}
	app.turnSeq = 1
	app.curTurn = 1
	if res := app.handleCollapseContext(stashCall); strings.HasPrefix(res, "ERROR") {
		t.Fatalf("stash collapse failed: %s", res)
	}

	// Turn finishes with a clean answer.
	app.appendConv(proxy.Message{Role: "assistant", Content: StrPtr("the plan"), Turn: 1})

	app.collapseFinishedTurns()

	app.stateMu.RLock()
	rec, ok := app.collapsed[1]
	app.stateMu.RUnlock()
	if !ok || rec.Partial {
		t.Fatalf("turn 1 should have a FULL record after upgrade, got %+v (ok=%v)", rec, ok)
	}
	if rec.StashKey == "" || !strings.Contains(rec.StashKey, "plan-arch") {
		t.Errorf("stash key lost on upgrade: StashKey=%q", rec.StashKey)
	}
	if rec.MemoryKey == "" {
		t.Error("auto-index MemoryKey lost on upgrade")
	}
	// The pointer line (degrade view) carries the stash pointer too.
	if s := degradePointerLine(rec, nil); !strings.Contains(s, "plan-arch") {
		t.Errorf("degrade pointer should include the stash key, got: %s", s)
	}
}

// TestDegradeBoundary: n == tail → no pointers; n == tail+1 → exactly one
// pointer, and it's turn 1 (review #8).
func TestDegradeBoundary(t *testing.T) {
	for _, tc := range []struct{ n, tail, wantPointers int }{
		{6, 6, 0},
		{7, 6, 1},
	} {
		app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
		app.SetContextMode("long-session")
		app.Cfg.ContextTailTurns = tc.tail
		buildManyCollapsedTurns(t, app, tc.n)
		proj := app.contextProjection()
		pointers := 0
		for _, m := range proj {
			if strings.Contains(DerefStr(m.Content), "details archived") {
				pointers++
			}
		}
		if pointers != tc.wantPointers {
			t.Errorf("n=%d tail=%d: pointers = %d, want %d", tc.n, tc.tail, pointers, tc.wantPointers)
		}
		if tc.wantPointers == 1 {
			for _, m := range proj {
				if strings.Contains(DerefStr(m.Content), "details archived") && m.Turn != 1 {
					t.Errorf("n=7 tail=6: the degraded turn should be 1, got turn %d", m.Turn)
				}
			}
		}
	}
}

// TestStashNamespaceRefused: a memory_key outside ctx/ is refused, not
// silently written (review #24).
func TestStashNamespaceRefused(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.MemoryStore = testStore(t)
	app.Conv = []proxy.Message{
		{Role: "user", Content: StrPtr("x"), Turn: 1},
		tcMsg("tcA", "read_file", `{"path":"a.go"}`),
		trMsg("tcA", "read_file", bigContent),
	}
	app.turnSeq = 1
	app.curTurn = 1
	// Boundary assistant message carrying the call.
	call := proxy.ToolCall{ID: "ccN", Type: "function", Function: proxy.FunctionCall{
		Name: "collapse_context", Arguments: `{"summary":"s","memory_key":"project/evil-overwrite"}`}}
	app.appendConv(proxy.Message{Role: "assistant", ToolCalls: []proxy.ToolCall{call}, Turn: 1})

	if _, err := app.MemoryStore.Get(context.Background(), "project/evil-overwrite"); err == nil {
		t.Fatal("precondition: key should not exist")
	}
	res := app.handleCollapseContext(call)
	if strings.Contains(res, "ERROR") {
		t.Fatalf("collapse should proceed without the stash: %s", res)
	}
	app.stateMu.RLock()
	rec := app.collapsed[1]
	app.stateMu.RUnlock()
	if rec.StashKey != "" {
		t.Errorf("stash outside ctx/ must be refused, got %q", rec.StashKey)
	}
	if _, err := app.MemoryStore.Get(context.Background(), "project/evil-overwrite"); err == nil {
		t.Error("no entry may be written outside ctx/")
	}
}

// TestIndexValueCarriesContent: the auto-index value includes the user text
// and summary so memory_search can find turns by content (review #6).
func TestIndexValueCarriesContent(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.MemoryStore = testStore(t)
	app.Conv = oneCollapsedTurn(1)
	for i := range app.Conv {
		app.Conv[i].Turn = 1
	}
	app.turnSeq = 1
	app.collapseFinishedTurns()

	app.stateMu.RLock()
	rec := app.collapsed[1]
	app.stateMu.RUnlock()
	entry, err := app.MemoryStore.Get(context.Background(), rec.MemoryKey)
	if err != nil || entry == nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(entry.Value, "do something") {
		t.Errorf("index value should contain the user text, got: %s", entry.Value)
	}
	if !strings.Contains(entry.Value, rec.Archive) {
		t.Error("index value should contain the archive path")
	}
}

// TestAutoIndexFailureDoesNotBlockCollapse: when the memory store is nil, the
// collapse still happens; MemoryKey stays empty and no crash occurs.
func TestAutoIndexFailureDoesNotBlockCollapse(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	// MemoryStore deliberately nil (newTestApp default).
	app.Conv = oneCollapsedTurn(1)
	for i := range app.Conv {
		app.Conv[i].Turn = 1
	}
	app.turnSeq = 1

	app.collapseFinishedTurns()

	app.stateMu.RLock()
	rec, ok := app.collapsed[1]
	app.stateMu.RUnlock()
	if !ok {
		t.Fatal("collapse must still happen when the memory store is unavailable")
	}
	if rec.MemoryKey != "" {
		t.Error("MemoryKey should be empty when memory write is skipped")
	}
}

// --- fitConvToWindow measures the projection ---

// TestFitConvToWindowMeasuresProjection: with a large raw Conv but a small
// projection (long-session mode), the downshift guard must NOT fire hard-max.
func TestFitConvToWindowMeasuresProjection(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	buildManyCollapsedTurns(t, app, 40)
	// Raw conv is 40 turns × ~5k = ~200k chars. Hard max below raw size but
	// above the projected size (~40 × ~100 B notes + small tail... set tail
	// high so nothing degrades; the projection is still ≪ raw).
	app.Cfg.ContextTailTurns = 100
	app.Cfg.HardMaxBytes = 100000 // < raw (~200k), > projected
	if raw := TranscriptSize(app.Conv); raw <= app.Cfg.HardMaxBytes {
		t.Fatalf("test fixture broken: raw conv size %d must exceed hardMax %d", raw, app.Cfg.HardMaxBytes)
	}
	if proj := TranscriptSize(app.contextProjection()); proj > app.Cfg.HardMaxBytes {
		t.Fatalf("test fixture broken: projected size %d must be under hardMax %d", proj, app.Cfg.HardMaxBytes)
	}

	app.fitConvToWindow(context.Background())

	if len(app.Conv) != 40*6 {
		t.Fatalf("hard-max must not have fired: conv len = %d, want %d", len(app.Conv), 40*6)
	}
}

// --- Compact soft path is a no-op in long-session mode ---

// TestCompactSoftNoopInLongSession: an over-threshold soft Compact call in
// long-session mode must NOT materialize collapsed turns (records survive,
// Conv untouched).
func TestCompactSoftNoopInLongSession(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	buildManyCollapsedTurns(t, app, 10)
	convBefore := len(app.Conv)

	// Force over-threshold: compactAt 0 with a non-forced call.
	app.Cfg.CompactAt = 1
	did, err := app.Compact(context.Background(), func(context.Context, string) (string, error) {
		return "summary", nil
	}, false)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if did {
		t.Error("soft Compact should be a no-op in long-session mode")
	}
	if len(app.Conv) != convBefore {
		t.Errorf("Conv was mutated: %d → %d", convBefore, len(app.Conv))
	}
	app.stateMu.RLock()
	n := len(app.collapsed)
	app.stateMu.RUnlock()
	if n != 10 {
		t.Errorf("collapse records destroyed: %d remain, want 10", n)
	}
}

// --- Model-named stash via collapse_context ---

// TestCollapseContextStash: collapse_context with memory_key + description
// writes the stash entry and stamps the record.
func TestCollapseContextStash(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.MemoryStore = testStore(t)

	// Build an active turn on iteration 2 with a completed tool round and the
	// collapse_context boundary assistant message (its ToolCalls carry tcStash,
	// which findBoundaryAssistantIdx resolves as the elide boundary).
	stashCall := proxy.ToolCall{
		ID:   "tcStash",
		Type: "function",
		Function: proxy.FunctionCall{
			Name:      "collapse_context",
			Arguments: `{"summary":"Auth uses JWT in middleware.go","memory_key":"auth-flow-findings","description":"How auth works after investigation"}`,
		},
	}
	app.Conv = []proxy.Message{
		{Role: "user", Content: StrPtr("investigate the auth flow"), Turn: 1},
		tcMsg("tcA", "read_file", `{"path":"auth.go"}`),
		trMsg("tcA", "read_file", bigContent),
		{Role: "assistant", ToolCalls: []proxy.ToolCall{stashCall}, Turn: 1},
	}
	for i := 1; i < len(app.Conv); i++ {
		app.Conv[i].Turn = 1
	}
	app.turnSeq = 1
	app.curTurn = 1

	res := app.handleCollapseContext(stashCall)
	if strings.HasPrefix(res, "ERROR") {
		t.Fatalf("collapse_context stash failed: %s", res)
	}

	app.stateMu.RLock()
	rec, ok := app.collapsed[1]
	app.stateMu.RUnlock()
	if !ok || !rec.Partial {
		t.Fatal("partial collapse record missing")
	}
	if rec.StashKey == "" {
		t.Fatal("StashKey should be set for a model-named stash")
	}
	if !strings.Contains(rec.StashKey, "auth-flow-findings") {
		t.Errorf("stash key %q should contain the model's key", rec.StashKey)
	}
	entry, err := app.MemoryStore.Get(context.Background(), rec.StashKey)
	if err != nil || entry == nil {
		t.Fatalf("stash entry missing: %v", err)
	}
	if !strings.Contains(entry.Value, "JWT") {
		t.Error("stash entry should carry the summary content")
	}
	// Checkpoint block surfaces the stash pointer.
	if !strings.Contains(buildPartialCheckpointBlock(rec), "memory: ") {
		t.Error("checkpoint block should include the stash pointer line")
	}
}
