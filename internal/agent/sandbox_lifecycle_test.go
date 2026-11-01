package agent

import (
	"context"
	"io"
	"sync"
	"testing"

	"github.com/treeol/wakil/internal/proxy"
)

// lifecycleExec is a fakeExecutor that also implements the optional
// chat-binding and reset capabilities, recording how they are driven.
type lifecycleExec struct {
	*fakeExecutor
	mu     sync.Mutex
	binds  []string
	resets int
}

func (l *lifecycleExec) BindChat(_ context.Context, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.binds = append(l.binds, id)
	return nil
}

func (l *lifecycleExec) ResetSandbox(context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.resets++
	return nil
}

func newLifecycleApp(t *testing.T) (*App, *lifecycleExec) {
	t.Helper()
	le := &lifecycleExec{fakeExecutor: newFakeExecutor()}
	a := newTestApp("http://unused", le, func(_, _, _ string, _ bool) bool { return true })
	a.Out = io.Discard
	return a, le
}

func TestSetChatIDBindsExecutor(t *testing.T) {
	a, le := newLifecycleApp(t)
	a.SetChatID("chat-1")
	a.SetChatID("chat-2")
	if got := a.Client.ChatID; got != "chat-2" {
		t.Errorf("Client.ChatID = %q", got)
	}
	if len(le.binds) != 2 || le.binds[0] != "chat-1" || le.binds[1] != "chat-2" {
		t.Errorf("binds = %v", le.binds)
	}
}

func TestSetChatIDIgnoredForSubagentsAndEmpty(t *testing.T) {
	a, le := newLifecycleApp(t)
	a.SetChatID("")
	a.IsSubagent = true
	a.SetChatID("sub-chat")
	if len(le.binds) != 0 {
		t.Errorf("subagents (and empty ids) must never bind the shared session: %v", le.binds)
	}
}

func TestInstallSessionRebinds(t *testing.T) {
	a, le := newLifecycleApp(t)
	a.InstallSession(&Session{ChatID: "resumed-1", Conv: []proxy.Message{}})
	if len(le.binds) != 1 || le.binds[0] != "resumed-1" {
		t.Errorf("/resume must rebind the executor to the resumed chat: %v", le.binds)
	}
}

func TestNewConversationTransitionRebinds(t *testing.T) {
	a, le := newLifecycleApp(t)
	a.NewConversationTransition("fresh-1")
	if len(le.binds) != 1 || le.binds[0] != "fresh-1" {
		t.Errorf("/new must rebind the executor: %v", le.binds)
	}
}

func TestRewindResetsSandbox(t *testing.T) {
	a, le := newLifecycleApp(t)
	a.Conv = []proxy.Message{{Role: "user", Content: StrPtr("hi")}}
	if !a.startCheckpoint() {
		t.Fatal("checkpoint not created")
	}
	a.cpMu.Lock()
	a.cpActive = false
	a.cpMu.Unlock()
	res := a.rewind(1)
	if len(res.Errors) != 0 {
		t.Fatalf("rewind errors: %v", res.Errors)
	}
	if le.resets != 1 {
		t.Errorf("resets = %d, want 1 (/rewind restarts the sandbox session)", le.resets)
	}
}

func TestFailedRewindDoesNotResetSandbox(t *testing.T) {
	a, le := newLifecycleApp(t)
	if res := a.rewind(5); len(res.Errors) == 0 {
		t.Fatal("expected an invalid-rewind error")
	}
	if le.resets != 0 {
		t.Errorf("a refused rewind must not restart the sandbox (resets=%d)", le.resets)
	}
}
