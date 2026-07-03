package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	agent "github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
)

// TestViewportBottomAlignShortContent verifies that short content is
// bottom-aligned (blank padding at top, not bottom) so the last line sits
// flush against the input box. Before the fix, the viewport's default Top
// alignment left a visible gap below short content — e.g. during google
// searches when only short tool-result items are in the viewport.
func TestViewportBottomAlignShortContent(t *testing.T) {
	const w, h = 120, 50
	app := &agent.App{Cfg: config.DefaultConfig(), Client: newTestClient(""), Exec: newFakeExecutor()}
	m := NewTUIModel(app)
	m = step(m, tea.WindowSizeMsg{Width: w, Height: h})

	// Simulate google-search state: short content (4 lines)
	m.vp.SetContent("▶ search for go 1.26\n────\n· google_search\n  ran: search")
	_, vpH, _ := m.sizes()

	// bottomAlignViewport moves trailing blanks to top
	raw := m.vp.View()
	fixed := bottomAlignViewport(raw, vpH)

	rawLines := strings.Split(raw, "\n")
	fixedLines := strings.Split(fixed, "\n")

	// Count trailing blanks in raw (should be many — the original gap)
	rawTrailing := 0
	for i := len(rawLines) - 1; i >= 0; i-- {
		if strings.TrimSpace(rawLines[i]) == "" {
			rawTrailing++
		} else {
			break
		}
	}

	// Count trailing blanks in fixed (should be 0 — content flush at bottom)
	fixedTrailing := 0
	for i := len(fixedLines) - 1; i >= 0; i-- {
		if strings.TrimSpace(fixedLines[i]) == "" {
			fixedTrailing++
		} else {
			break
		}
	}

	// Count leading blanks in fixed (should be the moved padding)
	fixedLeading := 0
	for i := 0; i < len(fixedLines); i++ {
		if strings.TrimSpace(fixedLines[i]) == "" {
			fixedLeading++
		} else {
			break
		}
	}

	if fixedTrailing != 0 {
		t.Errorf("after fix: trailing blank lines = %d, want 0 (gap should be gone)", fixedTrailing)
	}
	if fixedLeading != rawTrailing {
		t.Errorf("after fix: leading blanks = %d, want %d (moved from trailing)", fixedLeading, rawTrailing)
	}
	if lipgloss.Height(fixed) != vpH {
		t.Errorf("after fix: height = %d, want %d (height must not change)", lipgloss.Height(fixed), vpH)
	}

	// Verify the full View() still renders to exactly terminal height
	fullView := m.View()
	fullLines := strings.Count(fullView, "\n") + 1
	if fullLines != h {
		t.Errorf("full View() lines = %d, want %d (terminal height)", fullLines, h)
	}

	// The last viewport content line should be at the bottom of the conv pane.
	// Below it: 1 row (conv hidden bottom border) + inputOuterH (6) = 7 rows from terminal bottom.
	// So the last content line should be at h - 7 (0-indexed: h-8).
	fullViewLines := strings.Split(fullView, "\n")
	lastContentIdx := -1
	for i := len(fullViewLines) - 1; i >= 0; i-- {
		if strings.TrimSpace(fullViewLines[i]) != "" {
			lastContentIdx = i
			break
		}
	}
	// The last non-blank line in the full view should be the textarea content
	// (inside the input box). The last *viewport* content line is above the
	// input box. Check that there are only structural blanks (borders) between
	// the viewport content and the input box — not the 38-row gap.
	// Input box occupies the last inputOuterH(6) rows + 1 border = 7 rows from bottom.
	// Viewport content should end at h - 7 - 1 (border) = h - 8 (0-indexed).
	vpContentEnd := h - 8 // 0-indexed
	if lastContentIdx < vpContentEnd {
		t.Errorf("last content at line %d, but viewport content should extend to line %d (gap still present)",
			lastContentIdx, vpContentEnd)
	}
}

// TestViewportBottomAlignFullContent verifies that when content fills or
// exceeds the viewport, bottomAlignViewport is a no-op (no blank lines moved).
func TestViewportBottomAlignFullContent(t *testing.T) {
	const w, h = 120, 50
	app := &agent.App{Cfg: config.DefaultConfig(), Client: newTestClient(""), Exec: newFakeExecutor()}
	m := NewTUIModel(app)
	m = step(m, tea.WindowSizeMsg{Width: w, Height: h})

	// Fill the viewport with content longer than vpH
	longContent := strings.Repeat("line of content\n", 60)
	m.vp.SetContent(longContent)
	_, vpH, _ := m.sizes()

	raw := m.vp.View()
	fixed := bottomAlignViewport(raw, vpH)

	if raw != fixed {
		t.Errorf("full content should be unchanged, but output differs")
	}
}
