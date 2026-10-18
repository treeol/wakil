package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/memory"
	"github.com/treeol/wakil/internal/proxy"
	wtools "github.com/treeol/wakil/internal/tools"
)

// --- Phase 3: recall & recovery verification ---
//
// Exercises the ACTUAL recall paths end-to-end: memory_search (FTS5) finds
// the intended entry within top-k, memory_get retrieves it, and the
// archive reference reads back the expected archived content via the real
// spill files. Fixture corpus: multiple collapsed turns with distinct
// topics + distractors.

// recallFixture builds a session with three really-collapsed turns about
// distinct topics, returning the collapse records.
func recallFixture(t *testing.T, app *App) map[int]CollapseRecord {
	t.Helper()
	topics := []struct {
		u, w, a string
	}{
		{"investigate the database connection pool leak", "POOL WORKING SET " + strings.Repeat("p", 2400), "the leak was in connectionClose returning the conn twice"},
		{"add gzip compression to the http middleware stack", "GZIP WORKING SET " + strings.Repeat("g", 2400), "compression now applied via middleware.go WrapHandler"},
		{"figure out why the tui flickers on resize", "TUI WORKING SET " + strings.Repeat("t", 2400), "flicker caused by double paint in viewport.go draw()"},
	}
	records := map[int]CollapseRecord{}
	for turn, topic := range topics {
		tn := turn + 1
		app.Conv = append(app.Conv,
			proxy.Message{Role: "user", Content: StrPtr(topic.u), Turn: tn},
			tcMsg("tc"+itoa(tn)+"a", "read_file", `{"path":"x.go"}`),
			trMsg("tc"+itoa(tn)+"a", "read_file", topic.w),
			proxy.Message{Role: "assistant", Content: StrPtr(topic.a), Turn: tn},
		)
		app.turnSeq = tn
	}
	app.collapseFinishedTurns()
	app.stateMu.RLock()
	defer app.stateMu.RUnlock()
	for tn := 1; tn <= 3; tn++ {
		rec, ok := app.collapsed[tn]
		if !ok {
			t.Fatalf("turn %d should have collapsed in the fixture", tn)
		}
		records[tn] = rec
	}
	return records
}

// TestRecallSearchFindsTurnByUserQuery: an intent query phrased from the
// user request surfaces the intended turn's index entry in the top results.
func TestRecallSearchFindsTurnByUserQuery(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.MemoryStore = testStore(t)
	records := recallFixture(t, app)

	results, err := app.MemoryStore.Search(context.Background(), "connection pool leak", "", false)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("search returned no results")
	}
	if results[0].Key != records[1].MemoryKey {
		t.Errorf("top result = %q, want turn-1 entry %q", results[0].Key, records[1].MemoryKey)
	}
}

// TestRecallSearchFindsTurnByAnswerQuery: a query phrased from the ANSWER
// (not the request) still surfaces the entry — the enrichment closes the
// panel-flagged coverage gap.
func TestRecallSearchFindsTurnByAnswerQuery(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.MemoryStore = testStore(t)
	records := recallFixture(t, app)

	for _, q := range []string{"double paint viewport", "middleware WrapHandler"} {
		results, err := app.MemoryStore.Search(context.Background(), q, "", false)
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		found := false
		for _, r := range results {
			if r.Key == records[2].MemoryKey || r.Key == records[3].MemoryKey {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("answer query %q should surface the turn whose answer mentions it; got keys: %v", q, keysOf(results))
		}
	}
}

// TestRecallGetAndReadArchive: memory_get returns the entry, and the
// embedded archive reference reads back the expected working-set content
// via the real file.
func TestRecallGetAndReadArchive(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.MemoryStore = testStore(t)
	records := recallFixture(t, app)

	entry, err := app.MemoryStore.Get(context.Background(), records[2].MemoryKey)
	if err != nil || entry == nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(entry.Value, "gzip") {
		t.Errorf("entry value should carry the turn content, got: %s", entry.Value)
	}
	// The archive path embedded in the note must read back the working set.
	archive := records[2].Archive
	content, rerr := readArchiveForTest(t, app, archive)
	if rerr != nil {
		t.Fatalf("read archive: %v", rerr)
	}
	if !strings.Contains(content, "GZIP WORKING SET") {
		t.Errorf("archive should contain the working-set render, got: %.80s", content)
	}
}

// TestRecallUpgradedEntryPointsAtLatestArchive: after partial→full
// upgrade, the searchable entry references the FINISHED archive and its
// value contains user + answer content.
func TestRecallUpgradedEntryPointsAtLatestArchive(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.MemoryStore = testStore(t)

	// Mid-turn partial collapse first.
	call := proxy.ToolCall{ID: "ccP", Type: "function", Function: proxy.FunctionCall{
		Name: "collapse_context", Arguments: `{"summary":"phase one: pool basics"}`}}
	buildStashTurn(t, app, call)
	if res := app.handleCollapseContext(call); strings.HasPrefix(res, "ERROR") {
		t.Fatalf("partial collapse failed: %s", res)
	}
	app.stateMu.RLock()
	partArchive := app.collapsed[1].Archive
	partKey := app.collapsed[1].MemoryKey
	app.stateMu.RUnlock()

	// Finish the turn → upgrade.
	app.appendConv(proxy.Message{Role: "assistant", Content: StrPtr("the pool leak was fixed by dedup in release()"), Turn: 1})
	app.collapseFinishedTurns()

	app.stateMu.RLock()
	fullRec := app.collapsed[1]
	app.stateMu.RUnlock()

	entry, err := app.MemoryStore.Get(context.Background(), fullRec.MemoryKey)
	if err != nil || entry == nil {
		t.Fatalf("get: %v", err)
	}
	if fullRec.MemoryKey != partKey {
		t.Fatalf("upgrade should keep the same key: %q vs %q", fullRec.MemoryKey, partKey)
	}
	if fullRec.Archive == partArchive {
		t.Fatal("upgrade should point at the FINISHED archive")
	}
	if !strings.Contains(entry.Value, fullRec.Archive) {
		t.Error("entry must reference the finished archive")
	}
	if strings.Contains(entry.Value, "pool leak was fixed") {
		t.Log("answer enrichment present in upgraded entry")
	} else {
		t.Error("upgraded entry should carry the final answer (enrichment)")
	}
	// And search finds it by the answer phrasing.
	results, err := app.MemoryStore.Search(context.Background(), "dedup in release", "", false)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) == 0 || results[0].Key != fullRec.MemoryKey {
		t.Errorf("search should find the upgraded entry by answer content, top=%v", keysOf(results))
	}
}

// TestRecallStashGetByKey: a model-named stash is retrievable by its exact
// key with the distilled summary content.
func TestRecallStashGetByKey(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.MemoryStore = testStore(t)

	call := proxy.ToolCall{ID: "ccS", Type: "function", Function: proxy.FunctionCall{
		Name: "collapse_context", Arguments: `{"summary":"Auth flow: JWT in middleware, refresh tokens in auth.go","memory_key":"auth-recall"}`}}
	buildStashTurn(t, app, call)
	if res := app.handleCollapseContext(call); strings.HasPrefix(res, "ERROR") {
		t.Fatalf("stash collapse failed: %s", res)
	}

	entry, err := app.MemoryStore.Get(context.Background(), "ctx/"+app.chatID()+"/auth-recall")
	if err != nil || entry == nil {
		t.Fatalf("get stash by exact key: %v", err)
	}
	if !strings.Contains(entry.Value, "JWT") {
		t.Errorf("stash should carry the distilled content, got: %s", entry.Value)
	}
	if entry.Tier != memory.TierMid {
		t.Errorf("stash tier = %q, want mid", entry.Tier)
	}
}

// TestRecallDegradePointerCarriesReferences: the degraded pointer line
// (tier-2 projection) contains the archive path and memory key so a model
// seeing only pointers can still recall.
func TestRecallDegradePointerCarriesReferences(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.MemoryStore = testStore(t)
	records := recallFixture(t, app)

	line := degradePointerLine(records[1], nil)
	if !strings.Contains(line, records[1].Archive) {
		t.Error("pointer line must carry the archive path")
	}
	if !strings.Contains(line, records[1].MemoryKey) {
		t.Error("pointer line must carry the memory key")
	}
}

// keysOf extracts entry keys for failure messages.
func keysOf(entries []*memory.Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Key)
	}
	return out
}

// readArchiveForTest reads an archive spill file the way the model would.
func readArchiveForTest(t *testing.T, app *App, path string) (string, error) {
	t.Helper()
	content, err := readHostCacheForTest(path)
	if err != nil {
		return "", err
	}
	return content, nil
}

// readHostCacheForTest reads a toolcache spill file via the same confined
// reader the model's read_file uses.
func readHostCacheForTest(path string) (string, error) {
	return wtools.ReadHostCacheFile(path)
}
