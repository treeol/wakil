package main

import (
	"testing"
	"time"

	"wakil/internal/agent"
	"wakil/internal/config"
	"wakil/internal/proxy"
)

func TestSessionRoundTrip(t *testing.T) {
	t.Setenv("WAKIL_SESSIONS_DIR", t.TempDir())

	older := &agent.Session{
		ChatID:  "aaaaaaaa-0000-0000-0000-000000000000",
		Model:   "ilm",
		Created: time.Now().Add(-2 * time.Hour),
		Updated: time.Now().Add(-2 * time.Hour),
		Conv:    []proxy.Message{{Role: "user", Content: agent.StrPtr("first session")}},
	}
	newer := &agent.Session{
		ChatID:  "bbbbbbbb-0000-0000-0000-000000000000",
		Model:   "ilm",
		Created: time.Now(),
		Updated: time.Now(),
		Conv:    []proxy.Message{{Role: "user", Content: agent.StrPtr("second session")}},
	}
	if err := agent.WriteSession(older); err != nil {
		t.Fatal(err)
	}
	if err := agent.WriteSession(newer); err != nil {
		t.Fatal(err)
	}

	// List is most-recent-first.
	list, err := agent.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 sessions, got %d", len(list))
	}
	if list[0].ChatID != newer.ChatID {
		t.Fatalf("newest first expected; got %s", list[0].ChatID)
	}

	// Empty id resolves to the most recent.
	latest, err := agent.LoadSession("")
	if err != nil {
		t.Fatal(err)
	}
	if latest.ChatID != newer.ChatID {
		t.Fatalf("latest = %s, want %s", latest.ChatID, newer.ChatID)
	}

	// Unique prefix resolves to that session.
	got, err := agent.LoadSession("aaaa")
	if err != nil {
		t.Fatal(err)
	}
	if got.ChatID != older.ChatID {
		t.Fatalf("prefix load = %s, want %s", got.ChatID, older.ChatID)
	}

	// Unknown prefix errors.
	if _, err := agent.LoadSession("zzzz"); err == nil {
		t.Fatal("expected error for unknown prefix")
	}
}

func TestAppSaveSession(t *testing.T) {
	t.Setenv("WAKIL_SESSIONS_DIR", t.TempDir())
	app := &agent.App{
		Cfg:    config.Config{ExecMode: "direct", WorkDir: "/tmp/zdb"},
		Client: &proxy.Client{ChatID: "cccccccc-0000-0000-0000-000000000000", Model: "ilm"},
		Session: &agent.Session{
			ChatID: "cccccccc-0000-0000-0000-000000000000",
			Model:  "ilm",
		},
		Conv: []proxy.Message{{Role: "user", Content: agent.StrPtr("hello")}},
	}
	app.SaveSession()

	got, err := agent.LoadSession("cccccccc")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Conv) != 1 || agent.DerefStr(got.Conv[0].Content) != "hello" {
		t.Fatalf("unexpected conv: %+v", got.Conv)
	}
	if got.Workspace != "/tmp/zdb" {
		t.Fatalf("workspace = %q, want /tmp/zdb", got.Workspace)
	}
	if got.Updated.IsZero() {
		t.Fatal("Updated not stamped")
	}
}
