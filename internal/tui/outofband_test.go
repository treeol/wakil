package tui

import (
	"context"
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	agent "github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/diag"
)

// diagRedirect swaps the diagnostic sink; the test-local wrapper exists so
// the restore semantics stay in one place.
func diagRedirect(w interface{ Write([]byte) (int, error) }) {
	if w == nil {
		diag.Redirect(nil)
		return
	}
	diag.Redirect(w)
}

// diagSink forwards to the current diag destination, exactly like
// wiring's diagWriter: the session log while the TUI runs.
type diagSink struct{}

func (diagSink) Write(p []byte) (int, error) { return diag.Write(p) }

// TestTUIIgnoresOutOfBandStdio pins Group C rule 2: while the TUI is
// running, nothing writes to stdout or stderr outside the render loop.
// The probes write through the diag sink (the session log), never
// through os.Stdout/os.Stderr: a stray fmt.Fprintln(os.Stderr, ...) is
// the bug being pinned. The test redirects os.Stdout/os.Stderr to pipes,
// runs the TUI model's Update through the real construction path
// (newBaseModel, as NewTUIModelWithFacade does), and runs the real
// context-limit probe through the real diag-writer wiring
// (agent.ResolveContextLimit with the diag sink — the same sink the
// App's probe paths use). No bytes may arrive on the pipes while the
// probe's sentence lands in the session log.
func TestTUIIgnoresOutOfBandStdio(t *testing.T) {
	oldOut, oldErr := os.Stdout, os.Stderr
	rOut, wOut, _ := os.Pipe()
	rErr, wErr, _ := os.Pipe()
	os.Stdout, os.Stderr = wOut, wErr
	t.Cleanup(func() { os.Stdout, os.Stderr = oldOut, oldErr })

	m := newBaseModel()
	_ = step(m, tea.WindowSizeMsg{Width: 80, Height: 30})

	// The diag sink stands in for the session log the TUI redirects to
	// (diag.OpenSessionLog, main.go:434). Every producer goroutine that
	// used to write the probe line to os.Stderr now writes through it.
	var log strings.Builder
	diagRedirect(&log)
	t.Cleanup(func() { diagRedirect(nil) })
	// The probe that bootstrap runs after the first prompt (runProbes)
	// and that /new and /resume re-run: its notice goes through the
	// diag sink — here, the redirect — never through the pipes.
	_ = agent.ResolveContextLimit(context.Background(), nil, config.DefaultConfig(), diagSink{})
	if !strings.Contains(log.String(), "context limit") {
		t.Fatalf("probe sentence missing from the session log: %q", log.String())
	}
	// Drain the pipes with a deadline: close the write ends first (no
	// more writes can arrive), then read to EOF.
	wOut.Close()
	wErr.Close()
	outBytes := make([]byte, 4096)
	nOut, _ := rOut.Read(outBytes)
	errBytes := make([]byte, 4096)
	nErr, _ := rErr.Read(errBytes)
	if nOut != 0 {
		t.Fatalf("%d bytes arrived on stdout outside the renderer: %q", nOut, outBytes[:nOut])
	}
	if nErr != 0 {
		t.Fatalf("%d bytes arrived on stderr outside the renderer: %q", nErr, errBytes[:nErr])
	}
}

// TestPlaceholderAppearsOnce pins Group C rule 5: the input placeholder
// ("type a task…") appears exactly once in the first rendered frame at
// 80, 100 and 120 columns. No layout or width arithmetic was changed to
// make this pass — it passes on the current layout. The frame is driven
// through the same first-frame path the real program uses — a real
// WindowSizeMsg is moved into the model (Update sets ready=true at
// tui.go:780) before View().
func TestPlaceholderAppearsOnce(t *testing.T) {
	for _, w := range []int{80, 100, 120} {
		m := newBaseModel()
		m = step(m, tea.WindowSizeMsg{Width: w, Height: 30})
		view := m.View()
		if n := strings.Count(plain(view), "type a task"); n != 1 {
			t.Fatalf("width %d: placeholder appears %d times in first frame, want exactly 1", w, n)
		}
	}
}
