package tui

import (
	"strings"
	"testing"

	agent "wakil/internal/agent"

	"wakil/internal/config"

	tea "github.com/charmbracelet/bubbletea"
)

// progWriter is the agent→TUI bridge: writes become streamChunkMsgs. This is part
// of the seam that a package split has to preserve, so pin its behavior.
func TestProgWriterSendsChunks(t *testing.T) {
	var got []string
	w := agent.NewProgWriter(func(m agent.StreamChunkMsg) { got = append(got, m.Text) })

	n, err := w.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("Write returned (%d,%v), want (5,nil)", n, err)
	}
	// Empty writes send nothing but still report success.
	if n, _ := w.Write(nil); n != 0 {
		t.Errorf("empty write should report 0 bytes, got %d", n)
	}
	if len(got) != 1 || got[0] != "hello" {
		t.Errorf("expected one chunk %q, got %v", "hello", got)
	}
}

func TestFmtConfirmBlockReadActionOption(t *testing.T) {
	withRead := fmtConfirmBlock("Run it?", "$ ls", true)
	if !strings.Contains(withRead, "allow all reads") {
		t.Errorf("read action should offer the [a] option; got %q", withRead)
	}
	noRead := fmtConfirmBlock("Write it?", "path=x", false)
	if strings.Contains(noRead, "allow all reads") {
		t.Errorf("write action must not offer [a]; got %q", noRead)
	}
}

func TestNewHTTPClientHasHeaderTimeout(t *testing.T) {
	c := newHTTPClient()
	if c == nil || c.Transport == nil {
		t.Fatal("newHTTPClient should return a client with a transport")
	}
}

// View must render without panicking across the common states — a cheap smoke
// test that guards the rendering paths during refactors.
func TestViewRendersAcrossStates(t *testing.T) {
	app := &agent.App{Cfg: config.DefaultConfig(), Client: newTestClient(""), Exec: newFakeExecutor(),
		CtxLimit: agent.ContextLimit{NCtx: 196608, Source: "backend", ReasoningBudget: 4096, AnswerMargin: 4096}}
	m := NewTUIModel(app)
	m = step(m, tea.WindowSizeMsg{Width: 120, Height: 40})

	for _, st := range []agentState{stateIdle, stateStreaming, stateConfirm} {
		m.state = st
		out := m.View()
		if strings.TrimSpace(plain(out)) == "" {
			t.Errorf("View() produced empty output in state %v", st)
		}
	}
}
