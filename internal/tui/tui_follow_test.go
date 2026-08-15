package tui

import (
	"context"
	"strings"
	"testing"

	agent "github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"

	tea "github.com/charmbracelet/bubbletea"
)

// followModel builds a TUI model with a tall viewport and long committed content
// so scrolling is actually possible (maxYOffset > 0). Content is added through
// the real path (addItem) so refreshViewport's prefix rebuild sees it — a raw
// m.vp.SetContent would be overwritten by the next refresh.
func followModel(t *testing.T) tuiModel {
	t.Helper()
	app := &agent.App{Cfg: config.DefaultConfig(), Client: newTestClient(""), Exec: newFakeExecutor()}
	m := NewTUIModel(app)
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 40})
	// Enough committed content to fill and exceed the viewport.
	for i := 0; i < 40; i++ {
		m.addItem(iAsst, "line of content "+strings.Repeat("x", 20))
	}
	m.followBottom = true
	m.vp.GotoBottom()
	return m
}

func TestFollow_InitTrue(t *testing.T) {
	// Construct the model directly — NOT via followModel, which overwrites the
	// field. This asserts NewTUIModel's initial value specifically.
	app := &agent.App{Cfg: config.DefaultConfig(), Client: newTestClient(""), Exec: newFakeExecutor()}
	m := NewTUIModel(app)
	if !m.followBottom {
		t.Fatal("NewTUIModel should initialize followBottom to true")
	}
}

func TestFollow_StreamChunkKeepsPinned(t *testing.T) {
	m := followModel(t)
	m.followBottom = true
	// Simulate a stream chunk: new content appended, viewport refreshed.
	before := m.vp.YOffset
	m.streaming.WriteString("new content\n")
	m.refreshViewport()
	if !m.vp.AtBottom() {
		t.Errorf("following chunk should keep viewport at bottom (offset %d)", m.vp.YOffset)
	}
	if m.vp.YOffset <= before {
		t.Errorf("offset should have grown past %d, got %d", before, m.vp.YOffset)
	}
}

func TestFollow_ScrollUpDisengages(t *testing.T) {
	m := followModel(t)
	m.followBottom = true
	// Scroll up via the viewport's Up key.
	m, _ = m.updateViewport(tea.KeyMsg{Type: tea.KeyUp})
	if m.followBottom {
		t.Fatal("scrolling up should disengage follow")
	}
}

func TestFollow_ScrollUpThenChunkStaysPaused(t *testing.T) {
	m := followModel(t)
	m.followBottom = true
	m, _ = m.updateViewport(tea.KeyMsg{Type: tea.KeyUp})
	offsetAfterUp := m.vp.YOffset
	// A new chunk while paused must NOT move the offset.
	m.streaming.WriteString("another line\n")
	m.refreshViewport()
	if m.vp.YOffset != offsetAfterUp {
		t.Errorf("paused offset moved: %d -> %d (chunk should not move a scrolled-up view)", offsetAfterUp, m.vp.YOffset)
	}
}

func TestFollow_ScrollDownToBottomReengages(t *testing.T) {
	m := followModel(t)
	m.followBottom = true
	// Scroll up, then scroll back down to bottom via repeated Down keys.
	m, _ = m.updateViewport(tea.KeyMsg{Type: tea.KeyUp})
	if m.followBottom {
		t.Fatal("precondition: up should disengage")
	}
	for i := 0; i < 1000; i++ {
		m, _ = m.updateViewport(tea.KeyMsg{Type: tea.KeyDown})
		if m.followBottom {
			break
		}
	}
	if !m.followBottom {
		t.Error("reaching the bottom by Down should re-engage follow")
	}
	if !m.vp.AtBottom() {
		t.Errorf("after Down to bottom, viewport should be at bottom (offset %d, max %d)", m.vp.YOffset, m.vp.TotalLineCount())
	}
}

func TestFollow_DownAtBottomReengagesWithoutMovement(t *testing.T) {
	m := followModel(t)
	m.followBottom = false // user was paused
	m.vp.GotoBottom()      // but manually at bottom (offset may not change on Down)
	// A Down press at bottom produces no offset delta but is explicit follow intent.
	m, _ = m.updateViewport(tea.KeyMsg{Type: tea.KeyDown})
	if !m.followBottom {
		t.Error("Down while already at bottom should re-engage follow")
	}
}

func TestFollow_NewConversationResetsFollow(t *testing.T) {
	m := followModel(t)
	m.followBottom = false // simulate scrolled-up stale state
	m = step(m, agent.NewConvMsg{Note: "fresh"})
	if !m.followBottom {
		t.Error("NewConvMsg should reset followBottom to true")
	}
}

func TestFollow_ContentShrinkClampsOffset(t *testing.T) {
	m := followModel(t)
	m.followBottom = false
	// Pin a large offset (clamped to the current max).
	m.vp.SetYOffset(1000)
	big := m.vp.YOffset
	if big == 0 {
		t.Fatal("precondition: content should be tall enough to scroll")
	}
	// Shrink the actual transcript (not a raw SetContent, which refreshViewport
	// would overwrite from *m.items), then refresh.
	*m.items = []convItem{{kind: iAsst, text: "short\ncontent"}}
	m.prefixDirty = true
	m.refreshViewport()
	// The preserved offset must be clamped to the new (smaller) geometry: never
	// negative, and never past the bottom (PastBottom = offset > maxYOffset).
	if m.vp.YOffset < 0 {
		t.Errorf("offset should never be negative, got %d", m.vp.YOffset)
	}
	if m.vp.PastBottom() {
		t.Errorf("offset %d is past the bottom (max %d) after shrink",
			m.vp.YOffset, max(0, m.vp.TotalLineCount()-m.vp.Height))
	}
}

func TestFollow_UnrelatedMessageDoesNotChangeLatch(t *testing.T) {
	m := followModel(t)
	m.followBottom = false
	// A non-scroll key (e.g. a rune) forwarded to the viewport must not flip it.
	m, _ = m.updateViewport(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if m.followBottom {
		t.Error("a non-scroll message must not re-engage follow")
	}
}

// TestFollow_PageAndWheelThroughUpdate exercises the full Update routing for
// page keys and mouse wheel, which reach updateViewport via the trailing
// forward (wheel) or the textarea/viewport forward (page keys).
func TestFollow_PageAndWheelThroughUpdate(t *testing.T) {
	// Page Up disengages.
	m := followModel(t)
	m.followBottom = true
	m = step(m, tea.KeyMsg{Type: tea.KeyPgUp})
	if m.followBottom {
		t.Error("Page Up should disengage follow through full Update routing")
	}

	// Page Down back to bottom re-engages.
	for i := 0; i < 200 && !m.followBottom; i++ {
		m = step(m, tea.KeyMsg{Type: tea.KeyPgDown})
	}
	if !m.followBottom {
		t.Error("Page Down to bottom should re-engage follow through full Update routing")
	}

	// Wheel up disengages.
	m = followModel(t)
	m.followBottom = true
	m = step(m, tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp})
	if m.followBottom {
		t.Error("wheel up should disengage follow through full Update routing")
	}

	// Wheel down at bottom re-engages.
	m = followModel(t)
	m.followBottom = false
	m.vp.GotoBottom()
	m = step(m, tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})
	if !m.followBottom {
		t.Error("wheel down at bottom should re-engage follow through full Update routing")
	}
}

// TestFollow_DownPartwayStaysPaused verifies a downward scroll that does NOT
// reach the bottom leaves follow disengaged.
func TestFollow_DownPartwayStaysPaused(t *testing.T) {
	m := followModel(t)
	m.followBottom = true
	// Scroll up several lines to leave room above bottom.
	for i := 0; i < 5; i++ {
		m, _ = m.updateViewport(tea.KeyMsg{Type: tea.KeyUp})
	}
	if m.followBottom {
		t.Fatal("precondition: up should disengage")
	}
	// One down press (1 line) from partway up must NOT re-engage.
	m, _ = m.updateViewport(tea.KeyMsg{Type: tea.KeyDown})
	if m.followBottom {
		t.Error("a single Down that doesn't reach bottom must not re-engage follow")
	}
}

// TestFollow_TurnStartReengages verifies startTurn centralizes the re-engage.
func TestFollow_TurnStartReengages(t *testing.T) {
	m := followModel(t)
	m.followBottom = false // scrolled up
	m, _ = m.startTurn(func(ctx context.Context) tea.Cmd { return nil })
	if !m.followBottom {
		t.Error("startTurn should re-engage followBottom")
	}
}

// TestFollow_SelectionActiveContentChange verifies the offset stays valid
// while a selection is active and content changes (shrink or growth).
func TestFollow_SelectionActiveContentChange(t *testing.T) {
	m := followModel(t)
	m.followBottom = false
	m.sel = selection{active: true, anchorRow: 0, headRow: 0}
	m.vp.SetYOffset(500)
	big := m.vp.YOffset
	if big == 0 {
		t.Fatal("precondition: content should scroll")
	}
	// Shrink content while selection active; offset must clamp to valid range.
	*m.items = []convItem{{kind: iAsst, text: "short"}}
	m.prefixDirty = true
	m.refreshViewport()
	if m.vp.YOffset < 0 || m.vp.PastBottom() {
		t.Errorf("selection-active shrink left invalid offset %d (height %d, total %d)",
			m.vp.YOffset, m.vp.Height, m.vp.TotalLineCount())
	}
}
