package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/memory"
	"github.com/treeol/wakil/internal/proxy"
)

// --- Phase 1 correctness tests (plan v3, fixes 1.1-1.4) ---
//
// Fix 1.1: re-collapse index refresh failure must RETAIN the prior memory
// key on the record (the entry may be stale, but the pointer survives).
// Fix 1.2: partial→full upgrade always re-puts; on success the entry gains
// indexValueForTurn content and the LATEST archive reference.
// Fix 1.3: foreign-session ctx/ keys are refused; same-session qualified
// keys pass; bare keys are auto-qualified.
// Fix 1.4: per-operation failure reporting in the tool result.

// failedStore returns a *memory.Store whose writes will fail: the store is
// closed immediately after opening (SQLite writes on a closed handle error).
func failedStore(t *testing.T) *memory.Store {
	t.Helper()
	s := testStore(t)
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return s
}

// buildStashTurn builds an active turn-1 conversation with one completed tool
// round and the collapse_context boundary assistant message carrying call.
func buildStashTurn(t *testing.T, app *App, call proxy.ToolCall) {
	t.Helper()
	app.Conv = []proxy.Message{
		{Role: "user", Content: StrPtr("investigate the flow"), Turn: 1},
		tcMsg("tcA", "read_file", `{"path":"a.go"}`),
		trMsg("tcA", "read_file", bigContent),
		{Role: "assistant", ToolCalls: []proxy.ToolCall{call}, Turn: 1},
	}
	app.turnSeq = 1
	app.curTurn = 1
}

// TestRecollapseFailedIndexKeepsPriorKey (fix 1.1): a mid-turn collapse
// whose auto-index refresh fails must retain the key from the earlier
// collapse, and the tool result must report the staleness.
func TestRecollapseFailedIndexKeepsPriorKey(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	// A healthy store for the FIRST collapse, then swap in a failing one.
	app.MemoryStore = testStore(t)

	call1 := proxy.ToolCall{ID: "cc1", Type: "function", Function: proxy.FunctionCall{
		Name: "collapse_context", Arguments: `{"summary":"phase one done"}`}}
	buildStashTurn(t, app, call1)
	if res := app.handleCollapseContext(call1); strings.HasPrefix(res, "ERROR") {
		t.Fatalf("first collapse failed: %s", res)
	}
	app.stateMu.RLock()
	rec1 := app.collapsed[1]
	app.stateMu.RUnlock()
	if rec1.MemoryKey == "" {
		t.Fatal("first collapse should have indexed")
	}

	// Second collapse: the store is dead; the refresh fails.
	app.MemoryStore = failedStore(t)
	call2 := proxy.ToolCall{ID: "cc2", Type: "function", Function: proxy.FunctionCall{
		Name: "collapse_context", Arguments: `{"summary":"phase one and two done"}`}}
	app.appendConv(proxy.Message{Role: "assistant", ToolCalls: []proxy.ToolCall{call2}, Turn: 1})
	app.appendConv(trMsg("cc1", "collapse_context", "ok")) // boundary-adjacent filler
	app.appendConv(proxy.Message{Role: "user", Content: StrPtr("continue"), Turn: 1})
	res := app.handleCollapseContext(call2)
	if strings.HasPrefix(res, "ERROR") {
		t.Fatalf("second collapse failed: %s", res)
	}

	app.stateMu.RLock()
	rec2 := app.collapsed[1]
	app.stateMu.RUnlock()
	if rec2.MemoryKey != rec1.MemoryKey {
		t.Errorf("failed refresh must retain prior key: got %q, want %q", rec2.MemoryKey, rec1.MemoryKey)
	}
	if !rec2.IndexRefreshFailed {
		t.Error("IndexRefreshFailed should be flagged after a failed refresh")
	}
	if !strings.Contains(res, "may be stale") {
		t.Errorf("tool result must report the stale index: %s", res)
	}
}

// TestUpgradeRefreshesIndexValue (fix 1.2): after partial→full upgrade the
// memory entry must contain the finished-turn index value (user text) and
// reference the FINISHED archive, not the stale partial one.
func TestUpgradeRefreshesIndexValue(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.MemoryStore = testStore(t)

	stashCall := proxy.ToolCall{ID: "ccP", Type: "function", Function: proxy.FunctionCall{
		Name: "collapse_context", Arguments: `{"summary":"phase one done"}`}}
	buildStashTurn(t, app, stashCall)
	if res := app.handleCollapseContext(stashCall); strings.HasPrefix(res, "ERROR") {
		t.Fatalf("partial collapse failed: %s", res)
	}
	app.stateMu.RLock()
	partRec := app.collapsed[1]
	app.stateMu.RUnlock()
	if !partRec.Partial {
		t.Fatal("precondition: record should be partial")
	}

	// Turn finishes with a clean answer; upgrade must refresh the index.
	app.appendConv(proxy.Message{Role: "assistant", Content: StrPtr("all done — the answer"), Turn: 1})
	app.collapseFinishedTurns()

	app.stateMu.RLock()
	fullRec := app.collapsed[1]
	app.stateMu.RUnlock()
	if fullRec.Partial {
		t.Fatal("record should be full after upgrade")
	}
	if fullRec.MemoryKey != partRec.MemoryKey {
		t.Fatalf("upgrade should refresh the SAME key: %q vs %q", fullRec.MemoryKey, partRec.MemoryKey)
	}
	entry, err := app.MemoryStore.Get(context.Background(), fullRec.MemoryKey)
	if err != nil || entry == nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(entry.Value, "investigate the flow") {
		t.Errorf("upgraded entry should carry indexValueForTurn content (user text), got: %s", entry.Value)
	}
	if !strings.Contains(entry.Value, fullRec.Archive) {
		t.Errorf("upgraded entry must reference the FINISHED archive %q, got: %s", fullRec.Archive, entry.Value)
	}
	if strings.Contains(entry.Value, partRec.Archive) && partRec.Archive != fullRec.Archive {
		t.Errorf("upgraded entry must NOT reference the stale partial archive %q", partRec.Archive)
	}
}

// TestUpgradeFailedRefreshKeepsKey (fix 1.2 failure path): if the re-put at
// upgrade fails, the prior key is retained and flagged, not dropped.
func TestUpgradeFailedRefreshKeepsKey(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.MemoryStore = testStore(t)

	stashCall := proxy.ToolCall{ID: "ccP", Type: "function", Function: proxy.FunctionCall{
		Name: "collapse_context", Arguments: `{"summary":"phase one done"}`}}
	buildStashTurn(t, app, stashCall)
	if res := app.handleCollapseContext(stashCall); strings.HasPrefix(res, "ERROR") {
		t.Fatalf("partial collapse failed: %s", res)
	}
	app.stateMu.RLock()
	partKey := app.collapsed[1].MemoryKey
	app.stateMu.RUnlock()
	if partKey == "" {
		t.Fatal("precondition: partial record should be indexed")
	}

	app.MemoryStore = failedStore(t)
	app.appendConv(proxy.Message{Role: "assistant", Content: StrPtr("done"), Turn: 1})
	app.collapseFinishedTurns()

	app.stateMu.RLock()
	rec := app.collapsed[1]
	app.stateMu.RUnlock()
	if rec.MemoryKey != partKey {
		t.Errorf("failed upgrade refresh must retain prior key: got %q, want %q", rec.MemoryKey, partKey)
	}
	if !rec.IndexRefreshFailed {
		t.Error("IndexRefreshFailed should be flagged after a failed upgrade refresh")
	}
}

// TestForeignSessionStashRefused (fix 1.3): a qualified key under ANOTHER
// session's ctx/ namespace is refused, not written.
func TestForeignSessionStashRefused(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.MemoryStore = testStore(t)

	foreign := "ctx/other-session/plan-arch"
	call := proxy.ToolCall{ID: "ccF", Type: "function", Function: proxy.FunctionCall{
		Name: "collapse_context", Arguments: `{"summary":"s","memory_key":"` + foreign + `"}`}}
	buildStashTurn(t, app, call)
	res := app.handleCollapseContext(call)
	if strings.HasPrefix(res, "ERROR") {
		t.Fatalf("collapse should proceed without the stash: %s", res)
	}
	if _, err := app.MemoryStore.Get(context.Background(), foreign); err == nil {
		t.Error("foreign-session stash must not be written")
	}
	app.stateMu.RLock()
	rec := app.collapsed[1]
	app.stateMu.RUnlock()
	if rec.StashKey != "" {
		t.Errorf("foreign stash must be refused, got %q", rec.StashKey)
	}
	if !strings.Contains(res, "refused") {
		t.Errorf("tool result should note the refusal: %s", res)
	}
}

// TestSameSessionQualifiedStashAccepted (fix 1.3): an explicitly qualified
// same-session key is accepted verbatim.
func TestSameSessionQualifiedStashAccepted(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.MemoryStore = testStore(t)

	key := "ctx/" + app.chatID() + "/plan-arch"
	call := proxy.ToolCall{ID: "ccS", Type: "function", Function: proxy.FunctionCall{
		Name: "collapse_context", Arguments: `{"summary":"s","memory_key":"` + key + `"}`}}
	buildStashTurn(t, app, call)
	if res := app.handleCollapseContext(call); strings.HasPrefix(res, "ERROR") {
		t.Fatalf("collapse failed: %s", res)
	}
	if _, err := app.MemoryStore.Get(context.Background(), key); err != nil {
		t.Errorf("same-session qualified stash should be written: %v", err)
	}
}

// TestBareStashAutoQualified (fix 1.3): a bare key is auto-qualified under
// ctx/<this-chatID>/.
func TestBareStashAutoQualified(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.MemoryStore = testStore(t)

	call := proxy.ToolCall{ID: "ccB", Type: "function", Function: proxy.FunctionCall{
		Name: "collapse_context", Arguments: `{"summary":"s","memory_key":"my-findings"}`}}
	buildStashTurn(t, app, call)
	if res := app.handleCollapseContext(call); strings.HasPrefix(res, "ERROR") {
		t.Fatalf("collapse failed: %s", res)
	}
	app.stateMu.RLock()
	rec := app.collapsed[1]
	app.stateMu.RUnlock()
	want := "ctx/" + app.chatID() + "/my-findings"
	if rec.StashKey != want {
		t.Errorf("bare key should be auto-qualified to %q, got %q", want, rec.StashKey)
	}
}

// TestRefusedStashReplacementKeepsPrior (fix 1.4): a second collapse that
// requests a NEW memory_key which is refused must retain the earlier stash
// pointer and say so in the result.
func TestRefusedStashReplacementKeepsPrior(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.MemoryStore = testStore(t)

	call1 := proxy.ToolCall{ID: "cc1", Type: "function", Function: proxy.FunctionCall{
		Name: "collapse_context", Arguments: `{"summary":"phase one","memory_key":"good-key"}`}}
	buildStashTurn(t, app, call1)
	if res := app.handleCollapseContext(call1); strings.HasPrefix(res, "ERROR") {
		t.Fatalf("first collapse failed: %s", res)
	}
	app.stateMu.RLock()
	firstStash := app.collapsed[1].StashKey
	app.stateMu.RUnlock()
	if firstStash == "" {
		t.Fatal("precondition: first stash should exist")
	}

	// Second collapse requests a foreign key → refused → keep the first.
	call2 := proxy.ToolCall{ID: "cc2", Type: "function", Function: proxy.FunctionCall{
		Name: "collapse_context", Arguments: `{"summary":"phase two","memory_key":"ctx/other/x"}`}}
	app.appendConv(trMsg("cc1", "collapse_context", "ok"))
	app.appendConv(proxy.Message{Role: "assistant", ToolCalls: []proxy.ToolCall{call2}, Turn: 1})
	res := app.handleCollapseContext(call2)
	if strings.HasPrefix(res, "ERROR") {
		t.Fatalf("second collapse failed: %s", res)
	}
	app.stateMu.RLock()
	rec := app.collapsed[1]
	app.stateMu.RUnlock()
	if rec.StashKey != firstStash {
		t.Errorf("refused replacement must retain the earlier stash: got %q, want %q", rec.StashKey, firstStash)
	}
	if !rec.StashRefused {
		t.Error("StashRefused should be flagged for a refused replacement")
	}
	if !rec.StashKeptFromPrior {
		t.Error("StashKeptFromPrior should be flagged")
	}
	if !strings.Contains(res, "retained earlier stash") {
		t.Errorf("tool result should report the retention: %s", res)
	}
}

// TestAlreadyCollapsedIgnoresNewMemoryKey (fix 1.4): the same-batch
// early-return notes that a newly supplied memory_key is ignored.
func TestAlreadyCollapsedIgnoresNewMemoryKey(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.MemoryStore = testStore(t)

	call1 := proxy.ToolCall{ID: "cc1", Type: "function", Function: proxy.FunctionCall{
		Name: "collapse_context", Arguments: `{"summary":"phase one","memory_key":"first-key"}`}}
	buildStashTurn(t, app, call1)
	if res := app.handleCollapseContext(call1); strings.HasPrefix(res, "ERROR") {
		t.Fatalf("first collapse failed: %s", res)
	}

	// SAME boundary (same tool batch) with a new memory_key: no new boundary
	// assistant message appended → findBoundaryAssistantIdx resolves the same
	// index → early return must note the ignored key.
	res := app.handleCollapseContext(call1)
	if !strings.Contains(res, "already collapsed") {
		t.Fatalf("expected already-collapsed early return, got: %s", res)
	}
	if !strings.Contains(res, "ignored for an already-collapsed prefix") {
		t.Errorf("result should note the ignored memory_key: %s", res)
	}
}

// TestNilStoreCaveatInResult (fix 1.4): with no memory store at all, the
// tool result must tell the model recall is archive-read only.
func TestNilStoreCaveatInResult(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	// MemoryStore deliberately nil.

	call := proxy.ToolCall{ID: "ccN", Type: "function", Function: proxy.FunctionCall{
		Name: "collapse_context", Arguments: `{"summary":"s","memory_key":"wont-exist"}`}}
	buildStashTurn(t, app, call)
	res := app.handleCollapseContext(call)
	if strings.HasPrefix(res, "ERROR") {
		t.Fatalf("collapse failed: %s", res)
	}
	if !strings.Contains(res, "no memory store") {
		t.Errorf("result should state recall is archive-read only: %s", res)
	}
}

// TestStashWriteFailureKeepsPrior (review #1 high-priority): when a new
// memory_key is requested but the WRITE fails (dead store or nil store),
// the earlier partial stash pointer must be retained — not dropped.
func TestStashWriteFailureKeepsPrior(t *testing.T) {
	for _, tc := range []struct {
		name string
		// nilStore swaps MemoryStore to nil before the second collapse;
		// otherwise a closed (failing) store is installed.
		nilStore bool
	}{
		{name: "dead store", nilStore: false},
		{name: "nil store", nilStore: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
			app.SetContextMode("long-session")
			app.MemoryStore = testStore(t)

			call1 := proxy.ToolCall{ID: "cc1", Type: "function", Function: proxy.FunctionCall{
				Name: "collapse_context", Arguments: `{"summary":"phase one","memory_key":"first-key"}`}}
			buildStashTurn(t, app, call1)
			if res := app.handleCollapseContext(call1); strings.HasPrefix(res, "ERROR") {
				t.Fatalf("first collapse failed: %s", res)
			}
			app.stateMu.RLock()
			firstStash := app.collapsed[1].StashKey
			app.stateMu.RUnlock()
			if firstStash == "" {
				t.Fatal("precondition: first stash should exist")
			}

			if tc.nilStore {
				app.MemoryStore = nil
			} else {
				app.MemoryStore = failedStore(t)
			}
			call2 := proxy.ToolCall{ID: "cc2", Type: "function", Function: proxy.FunctionCall{
				Name: "collapse_context", Arguments: `{"summary":"phase two","memory_key":"second-key"}`}}
			app.appendConv(trMsg("cc1", "collapse_context", "ok"))
			app.appendConv(proxy.Message{Role: "assistant", ToolCalls: []proxy.ToolCall{call2}, Turn: 1})
			res := app.handleCollapseContext(call2)
			if strings.HasPrefix(res, "ERROR") {
				t.Fatalf("second collapse failed: %s", res)
			}

			app.stateMu.RLock()
			rec := app.collapsed[1]
			app.stateMu.RUnlock()
			if rec.StashKey != firstStash {
				t.Errorf("failed replacement must retain the earlier stash: got %q, want %q", rec.StashKey, firstStash)
			}
			if tc.nilStore {
				if !strings.Contains(res, "no memory store") {
					t.Errorf("nil store must be reported as such: %s", res)
				}
				app.stateMu.RLock()
				rec := app.collapsed[1]
				app.stateMu.RUnlock()
				if rec.StashRefused {
					t.Error("nil store is unavailability, not refusal: StashRefused must be false")
				}
				if rec.StashKeptFromPrior != true {
					t.Error("StashKeptFromPrior should be flagged for nil-store retention")
				}
			} else {
				if !rec.StashWriteFailed {
					t.Error("StashWriteFailed should be flagged")
				}
				if !rec.StashKeptFromPrior {
					t.Error("StashKeptFromPrior should be flagged")
				}
				if !strings.Contains(res, "retained earlier stash") {
					t.Errorf("tool result should report the retention: %s", res)
				}
			}
		})
	}
}

// TestSuccessfulStashReplacementClearsFlags: after a failed attempt that
// retained the prior pointer, a successful replacement resets the failure
// flags and points at the new key.
func TestSuccessfulStashReplacementClearsFlags(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.MemoryStore = testStore(t)

	call1 := proxy.ToolCall{ID: "cc1", Type: "function", Function: proxy.FunctionCall{
		Name: "collapse_context", Arguments: `{"summary":"phase one","memory_key":"first-key"}`}}
	buildStashTurn(t, app, call1)
	if res := app.handleCollapseContext(call1); strings.HasPrefix(res, "ERROR") {
		t.Fatalf("first collapse failed: %s", res)
	}

	// Failing store → second key refused-by-failure, prior retained.
	app.MemoryStore = failedStore(t)
	call2 := proxy.ToolCall{ID: "cc2", Type: "function", Function: proxy.FunctionCall{
		Name: "collapse_context", Arguments: `{"summary":"phase two","memory_key":"second-key"}`}}
	app.appendConv(trMsg("cc1", "collapse_context", "ok"))
	app.appendConv(proxy.Message{Role: "assistant", ToolCalls: []proxy.ToolCall{call2}, Turn: 1})
	if res := app.handleCollapseContext(call2); strings.HasPrefix(res, "ERROR") {
		t.Fatalf("second collapse failed: %s", res)
	}
	app.stateMu.RLock()
	rec2 := app.collapsed[1]
	app.stateMu.RUnlock()
	if !rec2.StashKeptFromPrior {
		t.Fatal("precondition: prior stash should have been retained")
	}

	// Healthy store → third collapse with a valid new key succeeds and the
	// failure flags reset.
	app.MemoryStore = testStore(t)
	call3 := proxy.ToolCall{ID: "cc3", Type: "function", Function: proxy.FunctionCall{
		Name: "collapse_context", Arguments: `{"summary":"phase three","memory_key":"third-key"}`}}
	app.appendConv(trMsg("cc2", "collapse_context", "ok"))
	app.appendConv(proxy.Message{Role: "assistant", ToolCalls: []proxy.ToolCall{call3}, Turn: 1})
	if res := app.handleCollapseContext(call3); strings.HasPrefix(res, "ERROR") {
		t.Fatalf("third collapse failed: %s", res)
	}
	app.stateMu.RLock()
	rec3 := app.collapsed[1]
	app.stateMu.RUnlock()
	if !strings.Contains(rec3.StashKey, "third-key") {
		t.Errorf("successful replacement should point at the new key: %q", rec3.StashKey)
	}
	if rec3.StashKeptFromPrior || rec3.StashWriteFailed || rec3.StashRefused || rec3.IndexRefreshFailed {
		t.Errorf("success must clear failure flags: %+v", rec3)
	}
}

// TestSameBatchDistinctKeysIgnored: two distinct collapse_context calls in
// the same assistant batch (same boundary) — the second gets the
// already-collapsed note mentioning the ignored key.
func TestSameBatchDistinctKeysIgnored(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.MemoryStore = testStore(t)

	callA := proxy.ToolCall{ID: "ccA", Type: "function", Function: proxy.FunctionCall{
		Name: "collapse_context", Arguments: `{"summary":"phase one","memory_key":"key-a"}`}}
	buildStashTurn(t, app, callA)
	// Same batch: a second parallel collapse_context call in the SAME
	// assistant message (both resolved at the same boundary index).
	callB := proxy.ToolCall{ID: "ccB", Type: "function", Function: proxy.FunctionCall{
		Name: "collapse_context", Arguments: `{"summary":"phase one again","memory_key":"key-b"}`}}
	am := proxy.Message{Role: "assistant", ToolCalls: []proxy.ToolCall{callA, callB}, Turn: 1}
	app.Conv[len(app.Conv)-1] = am

	if res := app.handleCollapseContext(callA); strings.HasPrefix(res, "ERROR") {
		t.Fatalf("first collapse failed: %s", res)
	}

	res := app.handleCollapseContext(callB)
	if !strings.Contains(res, "already collapsed") {
		t.Fatalf("expected already-collapsed early return, got: %s", res)
	}
	if !strings.Contains(res, "ignored for an already-collapsed prefix") {
		t.Errorf("result should note the ignored memory_key: %s", res)
	}
	app.stateMu.RLock()
	rec := app.collapsed[1]
	app.stateMu.RUnlock()
	if !strings.Contains(rec.StashKey, "key-a") {
		t.Errorf("second same-batch key must not replace the first: %q", rec.StashKey)
	}
	if _, err := app.MemoryStore.Get(context.Background(), "ctx/"+app.chatID()+"/key-b"); err == nil {
		t.Error("key-b must not have been written")
	}
}

// TestMalformedStashKeysRefused: empty suffix, dot, dot-dot, and
// path-traversal-looking suffixes are refused without writing.
func TestMalformedStashKeysRefused(t *testing.T) {
	for _, bad := range []string{"ctx/PLACEHOLDER/", "ctx/PLACEHOLDER/.", "ctx/PLACEHOLDER/..", "ctx/PLACEHOLDER/a/b", ".", "..", `a\\b`} {
		t.Run(bad, func(t *testing.T) {
			app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
			app.SetContextMode("long-session")
			app.MemoryStore = testStore(t)
			key := strings.ReplaceAll(bad, "PLACEHOLDER", app.chatID())
			call := proxy.ToolCall{ID: "ccM", Type: "function", Function: proxy.FunctionCall{
				Name: "collapse_context", Arguments: `{"summary":"s","memory_key":"` + key + `"}`}}
			buildStashTurn(t, app, call)
			res := app.handleCollapseContext(call)
			if strings.HasPrefix(res, "ERROR") {
				t.Fatalf("collapse should proceed: %s", res)
			}
			app.stateMu.RLock()
			rec := app.collapsed[1]
			app.stateMu.RUnlock()
			if !rec.Partial {
				t.Fatal("collapse record missing")
			}
			if rec.StashKey != "" {
				t.Errorf("malformed key %q must not produce a stash pointer", key)
			}
			if !rec.StashRefused {
				t.Errorf("malformed key %q should set StashRefused", key)
			}
			if !strings.Contains(res, "refused") {
				t.Errorf("result should report refusal: %s", res)
			}
			if _, err := app.MemoryStore.Get(context.Background(), key); err == nil {
				t.Errorf("malformed key %q must not be written", key)
			}
		})
	}
}

// TestForeignMemoryUntouchedAfterRefusal: pre-existing memory outside the
// session namespace remains byte-for-byte unchanged after a refused stash.
func TestForeignMemoryUntouchedAfterRefusal(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.MemoryStore = testStore(t)

	const foreignKey = "project/important-notes"
	if _, err := app.MemoryStore.PutActive(context.Background(), foreignKey,
		"ORIGINAL VALUE", "note", memory.TierDurable, "main", "other-session", 0, nil, nil, ""); err != nil {
		t.Fatalf("seed: %v", err)
	}

	call := proxy.ToolCall{ID: "ccX", Type: "function", Function: proxy.FunctionCall{
		Name: "collapse_context", Arguments: `{"summary":"s","memory_key":"` + foreignKey + `"}`}}
	buildStashTurn(t, app, call)
	if res := app.handleCollapseContext(call); strings.HasPrefix(res, "ERROR") {
		t.Fatalf("collapse should proceed: %s", res)
	}
	entry, err := app.MemoryStore.Get(context.Background(), foreignKey)
	if err != nil || entry == nil {
		t.Fatalf("foreign entry must survive: %v", err)
	}
	if entry.Value != "ORIGINAL VALUE" {
		t.Errorf("foreign entry was modified: %q", entry.Value)
	}
}

// TestUpgradeCarriesStashAttemptFlags (review #2): partial→full upgrade
// performs no new stash attempt, so the stash-outcome flags of the LAST
// attempt must survive the upgrade (no false "clean" state).
func TestUpgradeCarriesStashAttemptFlags(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.MemoryStore = testStore(t)

	// First collapse: successful stash.
	call1 := proxy.ToolCall{ID: "cc1", Type: "function", Function: proxy.FunctionCall{
		Name: "collapse_context", Arguments: `{"summary":"phase one","memory_key":"first-key"}`}}
	buildStashTurn(t, app, call1)
	if res := app.handleCollapseContext(call1); strings.HasPrefix(res, "ERROR") {
		t.Fatalf("first collapse failed: %s", res)
	}

	// Second collapse: refused key → prior stash retained, flags set.
	call2 := proxy.ToolCall{ID: "cc2", Type: "function", Function: proxy.FunctionCall{
		Name: "collapse_context", Arguments: `{"summary":"phase two","memory_key":"ctx/other/x"}`}}
	app.appendConv(trMsg("cc1", "collapse_context", "ok"))
	app.appendConv(proxy.Message{Role: "assistant", ToolCalls: []proxy.ToolCall{call2}, Turn: 1})
	if res := app.handleCollapseContext(call2); strings.HasPrefix(res, "ERROR") {
		t.Fatalf("second collapse failed: %s", res)
	}
	app.stateMu.RLock()
	part := app.collapsed[1]
	app.stateMu.RUnlock()
	if !part.StashRefused || !part.StashKeptFromPrior {
		t.Fatalf("precondition: flags should be set after refusal, got %+v", part)
	}

	// Turn finishes → upgrade must carry the flags.
	app.appendConv(proxy.Message{Role: "assistant", Content: StrPtr("done"), Turn: 1})
	app.collapseFinishedTurns()

	app.stateMu.RLock()
	full := app.collapsed[1]
	app.stateMu.RUnlock()
	if full.Partial {
		t.Fatal("record should be full after upgrade")
	}
	if !full.StashRefused {
		t.Error("upgrade must carry StashRefused (last attempt outcome)")
	}
	if !full.StashKeptFromPrior {
		t.Error("upgrade must carry StashKeptFromPrior")
	}
	if full.StashKey != part.StashKey {
		t.Errorf("upgrade must keep the stash pointer: %q vs %q", full.StashKey, part.StashKey)
	}
}
