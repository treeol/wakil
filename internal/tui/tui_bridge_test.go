package tui

import (
	"strings"
	"testing"

	agent "github.com/treeol/wakil/internal/agent"

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
	withRead := fmtConfirmBlock("Run it?", "$ ls", true, false)
	if !strings.Contains(withRead, "allow all reads") {
		t.Errorf("read action should offer the [a] option; got %q", withRead)
	}
	noRead := fmtConfirmBlock("Write it?", "path=x", false, false)
	if strings.Contains(noRead, "allow all reads") {
		t.Errorf("write action must not offer [a]; got %q", noRead)
	}
}

// Every grant-eligible prompt shows [g] in its text — including read-only
// prompts that accept it today without showing it.
func TestFmtConfirmBlockGrantOption(t *testing.T) {
	for _, c := range []struct {
		name       string
		headline   string
		detail     string
		readAction bool
	}{
		{"read", "Run it?", "$ ls", true},
		{"write", "Write it?", "path=x", false},
	} {
		got := fmtConfirmBlock(c.headline, c.detail, c.readAction, true)
		if !strings.Contains(got, "[g] grant for this session") {
			t.Errorf("%s: grant-eligible prompt must show [g]; got %q", c.name, got)
		}
		ineligible := fmtConfirmBlock(c.headline, c.detail, c.readAction, false)
		if strings.Contains(ineligible, "[g]") {
			t.Errorf("%s: ineligible prompt must not show [g]; got %q", c.name, ineligible)
		}
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
	f := &fakeFacade{sid: "sess_tui_test", chatID: "chat123",
		info: wiringTestInfo(196608)}
	m := newWiringModel(f)
	m = step(m, tea.WindowSizeMsg{Width: 120, Height: 40})

	for _, st := range []agentState{stateIdle, stateStreaming, stateConfirm} {
		m.state = st
		out := m.View()
		if strings.TrimSpace(plain(out)) == "" {
			t.Errorf("View() produced empty output in state %v", st)
		}
	}
}
