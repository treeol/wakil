package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/proxy"
)

// --- Phase 4: /compact gate in long-session mode + non-destructive no-op ---

// TestCompactGateRefusedInLongMode: bare /compact in long-session mode is
// refused with guidance; /compact force proceeds.
func TestCompactGateRefusedInLongMode(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	buildManyCollapsedTurns(t, app, 5)

	handled, _, cmd := HandleTUICommand("/compact", app)
	if !handled {
		t.Fatal("/compact should be handled")
	}
	c := cmd()
	note, ok := c.(SysNoteMsg)
	if !ok {
		t.Fatalf("/compact in long mode should return a SysNoteMsg, got %T", c)
	}
	if !strings.Contains(note.Text, "refused in long-session mode") {
		t.Errorf("note should explain the refusal: %s", note.Text)
	}
	if !strings.Contains(note.Text, "/compact force") {
		t.Errorf("note should mention the force syntax: %s", note.Text)
	}

	// Records must be intact after the refusal.
	n, _ := app.CollapsedCount()
	if n != 5 {
		t.Errorf("refused /compact must not touch records: %d remain, want 5", n)
	}
}

// TestCompactForceProceedsInLongMode: "/compact force" is not refused by the
// gate (the compaction itself then runs; with nothing summarizable it is a
// non-destructive no-op — see TestCompactForceNoopPreservesRecords).
func TestCompactForceProceedsInLongMode(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	buildManyCollapsedTurns(t, app, 5)

	handled, _, cmd := HandleTUICommand("/compact force", app)
	if !handled {
		t.Fatal("/compact force should be handled")
	}
	c := cmd()
	if note, isNote := c.(SysNoteMsg); isNote {
		if strings.Contains(note.Text, "refused") {
			t.Fatalf("/compact force must not be refused: %s", note.Text)
		}
	}
}

// TestCompactForceNoopPreservesRecords: forced /compact with NOTHING
// summarizable (keep boundary <= 0 on the projection) is a no-op that
// leaves the collapse records and Conv intact — the destructiveness guard
// reordered materialize behind the summarizability check.
func TestCompactForceNoopPreservesRecords(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	buildManyCollapsedTurns(t, app, 5)
	convBefore := len(app.Conv)

	// keepBytes larger than the whole projection → boundary <= 0 → no-op.
	app.Cfg.KeepBytes = 10_000_000
	app.Summarize = func(_ context.Context, _ string) (string, error) {
		return "[summary]", nil
	}

	did, err := app.Compact(context.Background(), app.summarizeFn(), true)
	if err != nil {
		t.Fatalf("Compact(force): %v", err)
	}
	if did {
		t.Error("nothing summarizable: Compact should return false")
	}
	n, _ := app.CollapsedCount()
	if n != 5 {
		t.Errorf("no-op forced compact must preserve records: %d remain, want 5", n)
	}
	if len(app.Conv) != convBefore {
		t.Errorf("no-op forced compact must not mutate Conv: %d → %d", convBefore, len(app.Conv))
	}
}

// TestCompactForceSummarizableStillWorks: when there IS something to
// summarize, forced compaction proceeds (gate + guard do not break the
// intended escape hatch).
func TestCompactForceSummarizableStillWorks(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	// One collapsed turn + one big uncollapsed tail to summarize.
	buildManyCollapsedTurns(t, app, 1)
	app.Conv = append(app.Conv, proxy.Message{Role: "user", Content: StrPtr(strings.Repeat("tail ", 500)), Turn: 2})
	app.Cfg.KeepBytes = 100 // tiny → large boundary
	app.Summarize = func(_ context.Context, _ string) (string, error) {
		return "[summary of older]", nil
	}

	did, err := app.Compact(context.Background(), app.summarizeFn(), true)
	if err != nil {
		t.Fatalf("Compact(force): %v", err)
	}
	if !did {
		t.Fatal("with a summarizable tail, forced compact should run")
	}
}
