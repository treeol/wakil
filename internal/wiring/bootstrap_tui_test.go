package wiring

import (
	"context"
	"sync"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
)

// TestBootstrapTUIFresh verifies BootstrapTUI with no resume ID: a manager,
// a live facade (session created), and a subscribed event pump that delivers
// turn events through the delivery callback.
func TestBootstrapTUIFresh(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.ExecMode = "direct"

	var mu sync.Mutex
	var delivered []event.Event
	deliver := func(ev event.Event) {
		mu.Lock()
		defer mu.Unlock()
		delivered = append(delivered, ev)
	}

	rt, cleanup, err := BootstrapTUI(cfg, fakeExec{}, "", deliver, BootstrapTUIOpts{})
	if err != nil {
		t.Fatalf("BootstrapTUI: %v", err)
	}
	defer cleanup()

	if rt.Facade == nil || rt.Manager == nil {
		t.Fatal("runtime missing facade or manager")
	}
	if rt.Principal.UserID == "" {
		t.Fatal("principal missing")
	}

	// Start the pump; submit a turn through the facade; expect delivery.
	rt.StartEventPump(context.Background())
	snap := rt.Facade.Snapshot()
	if _, err := rt.Facade.SubmitInput(context.Background(), rt.Principal, core.SubmitInputRequest{
		SessionID: snap.SessionID,
		Text:      "hello",
	}); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}

	waitUntil(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, ev := range delivered {
			if ev.Kind == event.KindTurnCompleted {
				return true
			}
		}
		return false
	})
}

// TestBootstrapTUIResumeMissing verifies a bad resume ID fails loudly and the
// cleanup path still runs (no leaked session).
func TestBootstrapTUIResumeMissing(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.ExecMode = "direct"
	t.Setenv("WAKIL_SESSIONS_DIR", t.TempDir())

	_, _, err := BootstrapTUI(cfg, fakeExec{}, "no-such-session", nil, BootstrapTUIOpts{})
	if err == nil {
		t.Fatal("resume of unknown session should fail")
	}
}

// TestBootstrapTUISubscribeLive verifies the manual-subscribe path main.go
// uses: BootstrapTUI with deliver=nil (prog.Send does not exist yet at
// construction), then SubscribeLive(prog.Send) once the program exists. A
// submitted turn must deliver its turn events through the callback — the
// regression test for the "stuck streaming" bug where a nil-deliver bootstrap
// left the facade with no subscription → no pump → turn events never reached
// the TUI.
func TestBootstrapTUISubscribeLive(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.ExecMode = "direct"

	rt, cleanup, err := BootstrapTUI(cfg, fakeExec{}, "", nil, BootstrapTUIOpts{})
	if err != nil {
		t.Fatalf("BootstrapTUI: %v", err)
	}
	defer cleanup()

	var mu sync.Mutex
	var delivered []event.Event
	if err := rt.SubscribeLive(context.Background(), func(ev event.Event) {
		mu.Lock()
		defer mu.Unlock()
		delivered = append(delivered, ev)
	}); err != nil {
		t.Fatalf("SubscribeLive: %v", err)
	}

	rt.StartEventPump(context.Background())
	snap := rt.Facade.Snapshot()
	if _, err := rt.Facade.SubmitInput(context.Background(), rt.Principal, core.SubmitInputRequest{
		SessionID: snap.SessionID,
		Text:      "hello",
	}); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}

	waitUntil(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, ev := range delivered {
			if ev.Kind == event.KindTurnCompleted {
				return true
			}
		}
		return false
	})
}
