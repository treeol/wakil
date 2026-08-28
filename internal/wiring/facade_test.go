package wiring

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionclient"
	"github.com/treeol/wakil/internal/core/sessionhost"
)

// TestEventPumpDeliversEvents verifies that the event pump reads events from
// a subscription and delivers them to the callback.
func TestEventPumpDeliversEvents(t *testing.T) {
	// Create a host with a simple turn that produces text.
	turn := func(ctx context.Context, in sessionhost.TurnInput) (string, error) {
		return "hello", nil
	}
	host := sessionhost.New(turn)
	defer host.Close(context.Background())

	principal := core.Principal{
		TenantID: event.EmbeddedTenantID,
		UserID:   event.EmbeddedUserID,
		Role:     core.RoleOwner,
	}

	ws, _ := event.NewWorkspaceID("wsp_test")
	sess, err := host.CreateSession(context.Background(), principal, core.CreateSessionRequest{Workspace: ws})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Subscribe before submitting so we see all events.
	sub, err := host.Subscribe(context.Background(), principal, sess.ID, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	var mu sync.Mutex
	var delivered []event.Event
	deliver := func(ev event.Event) {
		mu.Lock()
		delivered = append(delivered, ev)
		mu.Unlock()
	}

	pump := NewEventPump(sub, host, principal, sess.ID, 0, deliver)
	ctx, cancel := context.WithCancel(context.Background())
	go pump.Run(ctx)

	// Submit a turn to generate events.
	_, err = host.SubmitInput(context.Background(), principal, core.SubmitInputRequest{
		SessionID: sess.ID,
		Text:      "test",
	})
	if err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}

	// Wait for events to arrive.
	time.Sleep(100 * time.Millisecond)

	pump.Stop()
	cancel()
	<-pump.Done()

	mu.Lock()
	defer mu.Unlock()
	if len(delivered) == 0 {
		t.Fatal("no events delivered")
	}
	// Expect at least session_created, user_message_committed, turn_started,
	// message_committed, turn_completed.
	kinds := make(map[event.Kind]bool)
	for _, ev := range delivered {
		kinds[ev.Kind] = true
	}
	for _, want := range []event.Kind{
		event.KindSessionCreated,
		event.KindUserMessageCommitted,
		event.KindTurnStarted,
		event.KindMessageCommitted,
		event.KindTurnCompleted,
	} {
		if !kinds[want] {
			t.Errorf("missing event kind %s", want)
		}
	}
}

// TestEventPumpStopIsIdempotent verifies that Stop can be called multiple times
// safely.
func TestEventPumpStopIsIdempotent(t *testing.T) {
	turn := func(ctx context.Context, in sessionhost.TurnInput) (string, error) {
		return "", nil
	}
	host := sessionhost.New(turn)
	defer host.Close(context.Background())

	principal := core.Principal{
		TenantID: event.EmbeddedTenantID,
		UserID:   event.EmbeddedUserID,
		Role:     core.RoleOwner,
	}

	ws, _ := event.NewWorkspaceID("wsp_test")
	sess, _ := host.CreateSession(context.Background(), principal, core.CreateSessionRequest{Workspace: ws})
	sub, _ := host.Subscribe(context.Background(), principal, sess.ID, 0)

	pump := NewEventPump(sub, host, principal, sess.ID, 0, func(event.Event) {})
	pump.Stop()
	pump.Stop() // idempotent
	pump.Stop() // still idempotent
}

// TestEventPumpCtxCancel verifies that the pump exits when the context is
// cancelled.
func TestEventPumpCtxCancel(t *testing.T) {
	turn := func(ctx context.Context, in sessionhost.TurnInput) (string, error) {
		return "", nil
	}
	host := sessionhost.New(turn)
	defer host.Close(context.Background())

	principal := core.Principal{
		TenantID: event.EmbeddedTenantID,
		UserID:   event.EmbeddedUserID,
		Role:     core.RoleOwner,
	}

	ws, _ := event.NewWorkspaceID("wsp_test")
	sess, _ := host.CreateSession(context.Background(), principal, core.CreateSessionRequest{Workspace: ws})
	sub, _ := host.Subscribe(context.Background(), principal, sess.ID, 0)

	pump := NewEventPump(sub, host, principal, sess.ID, 0, func(event.Event) {})
	ctx, cancel := context.WithCancel(context.Background())
	go pump.Run(ctx)

	cancel()
	select {
	case <-pump.Done():
	case <-time.After(time.Second):
		t.Fatal("pump did not exit after ctx cancel")
	}
}

// TestFacadeSatisfiesInterface verifies compile-time that wiringFacade
// satisfies sessionclient.Facade.
func TestFacadeSatisfiesInterface(t *testing.T) {
	var _ sessionclient.Facade = (*wiringFacade)(nil)
	var _ sessionclient.ConversationManager = (*conversationManager)(nil)
}
