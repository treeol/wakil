package agent

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/proxy"
)

func TestReconcilePurgesDeletedSessions(t *testing.T) {
	t.Setenv("WAKIL_SESSIONS_DIR", t.TempDir())
	ws := "/tmp/ws-g"
	app := newSessionHistoryApp(t, ws)

	s := Session{ChatID: "sess-gone", Workspace: ws, Conv: []proxy.Message{
		{Role: "user", Content: strPtr("io_uring configuration secrets")},
	}}
	writeSessionFile(t, &s)
	if err := app.indexSession(context.Background(), s, false); err != nil {
		t.Fatal(err)
	}

	// Delete the source file, then reconcile — the index row must be purged.
	if err := os.Remove(sessionPath(s.ChatID)); err != nil {
		t.Fatal(err)
	}
	if err := app.reconcileHistory(context.Background(), ws); err != nil {
		t.Fatal(err)
	}

	res, err := app.SessionHistory.Search(context.Background(), "io_uring", ws, "current", 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 0 {
		t.Fatalf("deleted session not purged by reconcile: %+v", res)
	}
}

func TestReconcileKeepsMalformedFileRow(t *testing.T) {
	t.Setenv("WAKIL_SESSIONS_DIR", t.TempDir())
	ws := "/tmp/ws-h"
	app := newSessionHistoryApp(t, ws)

	s := Session{ChatID: "sess-mal", Workspace: ws, Conv: []proxy.Message{
		{Role: "user", Content: strPtr("io_uring configuration")},
	}}
	writeSessionFile(t, &s)
	if err := app.indexSession(context.Background(), s, false); err != nil {
		t.Fatal(err)
	}

	// Corrupt the file (malformed JSON). ListSessions will skip it silently, so
	// reconcile must NOT purge the row (the file still exists on disk).
	if err := os.WriteFile(sessionPath(s.ChatID), []byte("{ not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := app.reconcileHistory(context.Background(), ws); err != nil {
		t.Fatal(err)
	}

	res, err := app.SessionHistory.Search(context.Background(), "io_uring", ws, "current", 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("malformed (still-present) file must keep its index row, got %d", len(res))
	}
}

func TestReconcilePreservesGeneratedSummary(t *testing.T) {
	t.Setenv("WAKIL_SESSIONS_DIR", t.TempDir())
	ws := "/tmp/ws-i"
	app := newSessionHistoryApp(t, ws)

	// A session with many turns (above threshold) and a generated summary.
	s := Session{ChatID: "sess-sum", Workspace: ws, Conv: []proxy.Message{}}
	for i := 0; i < 5; i++ {
		s.Conv = append(s.Conv,
			proxy.Message{Role: "user", Content: strPtr("q" + string(rune('a'+i)))},
			proxy.Message{Role: "assistant", Content: strPtr("a" + string(rune('a'+i)))},
		)
	}
	// Set a fixed summarizer so finalize writes a generated summary.
	app.Summarize = func(_ context.Context, _ string) (string, error) {
		return "GENERATED DISTILLED SUMMARY", nil
	}
	app.finalizeSessionHistory(context.Background(), s)

	// Verify the generated summary is in the index.
	summary, gen, err := app.SessionHistory.GetSummary(context.Background(), s.ChatID)
	if err != nil || !gen {
		t.Fatalf("expected a generated summary (gen=%v, err=%v)", gen, err)
	}
	if !contains(summary, "GENERATED DISTILLED SUMMARY") {
		t.Fatalf("unexpected summary: %q", summary)
	}

	// Re-ingest the same session (as reconcile would after a content change).
	// sessionToIndexInput alone would produce no summary (no compaction summary
	// in this transcript), so WITHOUT preservation the generated summary would
	// be lost. With preserveGenerated=true it must survive the generated flag.
	s.Updated = s.Updated.Add(time.Minute) // simulate a content change
	if err := app.indexSession(context.Background(), s, true); err != nil {
		t.Fatal(err)
	}
	summary2, gen2, err := app.SessionHistory.GetSummary(context.Background(), s.ChatID)
	if err != nil {
		t.Fatal(err)
	}
	if !gen2 {
		t.Fatalf("generated summary was clobbered by re-ingest (gen=%v)", gen2)
	}
	if !contains(summary2, "GENERATED DISTILLED SUMMARY") {
		t.Fatalf("generated summary text was clobbered, got %q", summary2)
	}
}

func TestReconcileIndexesNewSession(t *testing.T) {
	t.Setenv("WAKIL_SESSIONS_DIR", t.TempDir())
	ws := "/tmp/ws-j"
	app := newSessionHistoryApp(t, ws)

	// Session file on disk but NOT yet indexed; reconcile must backfill it.
	s := Session{ChatID: "sess-new", Workspace: ws, Conv: []proxy.Message{
		{Role: "user", Content: strPtr("how to set up io_uring support")},
	}}
	writeSessionFile(t, &s)

	if err := app.reconcileHistory(context.Background(), ws); err != nil {
		t.Fatal(err)
	}

	res, err := app.SessionHistory.Search(context.Background(), "io_uring", ws, "current", 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("reconcile did not backfill new session, got %d results", len(res))
	}
}

func TestReconcileSkipsUnchanged(t *testing.T) {
	t.Setenv("WAKIL_SESSIONS_DIR", t.TempDir())
	ws := "/tmp/ws-k"
	app := newSessionHistoryApp(t, ws)

	s := Session{ChatID: "sess-unchanged", Workspace: ws, Conv: []proxy.Message{
		{Role: "user", Content: strPtr("some io_uring text")},
	}}
	writeSessionFile(t, &s)
	if err := app.indexSession(context.Background(), s, false); err != nil {
		t.Fatal(err)
	}

	// Second reconcile with identical hash must not error / churn.
	if err := app.reconcileHistory(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	// Row still present and searchable.
	res, err := app.SessionHistory.Search(context.Background(), "io_uring", ws, "current", 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("unchanged session lost after second reconcile: %d results", len(res))
	}
}

func TestRememberSearchCrossWorkspaceIsolation(t *testing.T) {
	// Workspace A has a matching session; workspace B must never return it.
	t.Setenv("WAKIL_SESSIONS_DIR", t.TempDir())
	wsB := "/tmp/ws-iso"
	app := newSessionHistoryApp(t, wsB)

	// Index a session in a DIFFERENT workspace directly into the store. Its
	// content carries a unique token that must never leak into workspace B.
	secretToken := "topsecret-xyz-sandbox-config"
	other := Session{ChatID: "sess-other", Workspace: "/tmp/ws-other", Conv: []proxy.Message{
		{Role: "user", Content: strPtr("io_uring " + secretToken)},
	}}
	if err := app.indexSession(context.Background(), other, false); err != nil {
		t.Fatal(err)
	}

	// Remember search in workspace B must NOT surface the workspace-other session.
	out, err := app.RememberSearch(context.Background(), "io_uring")
	if err != nil {
		t.Fatal(err)
	}
	if contains(out, secretToken) {
		t.Errorf("cross-workspace leakage via bridge: %s", out)
	}
}
