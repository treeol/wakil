package tui

import (
	"os"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// SGR escapes for the selection highlight. Emitted literally (rather than via
// lipgloss) so the highlight renders regardless of the detected color profile.
const (
	sgrReverse = "\x1b[7m"
	sgrReset   = "\x1b[0m"
)

// selection tracks a mouse drag over the conversation pane. Coordinates are in
// content space: row is an index into the viewport's full (scrolled) content,
// col is a 0-based visual column. anchor is where the drag began; head is the
// moving end.
type selection struct {
	active    bool // a selection exists (rendered highlighted)
	dragging  bool // the mouse moved since press (distinguishes drag from click)
	anchorRow int
	anchorCol int
	headRow   int
	headCol   int
}

// ordered returns the selection bounds normalized so (sr,sc) <= (er,ec).
func (s selection) ordered() (sr, sc, er, ec int) {
	sr, sc, er, ec = s.anchorRow, s.anchorCol, s.headRow, s.headCol
	if er < sr || (er == sr && ec < sc) {
		sr, sc, er, ec = er, ec, sr, sc
	}
	return
}

// handleMouse processes a mouse event. It returns the updated model, whether the
// event was consumed (and so must not be forwarded to the viewport/textarea),
// and an optional command (the clipboard copy on drag-release).
//
// Only the left button drives selection; wheel events fall through so the
// viewport keeps scrolling.
func (m tuiModel) handleMouse(msg tea.MouseMsg) (tuiModel, bool, tea.Cmd) {
	if tea.MouseEvent(msg).IsWheel() {
		return m, false, nil
	}

	// Tab bar click (bottom row) when sub tabs exist.
	// Layout: main tab = [0, tabMainW), visible sub slot = [subTabSlotStart(slot), +tabSubW).
	// × close button = last 2 visual chars of each sub tab slot.
	// The tab bar is the last section in View(), so its Y is m.height-1.
	// This holds by construction: sizes() computes topOuterH =
	// m.height - inputOuterH - completionHeight() - tabH, so the sections
	// always sum to exactly m.height.
	if len(m.subTabs) > 0 && msg.Action == tea.MouseActionRelease &&
		msg.Button == tea.MouseButtonLeft && !m.sel.dragging &&
		msg.Y == m.height-1 {
		x := msg.X
		switch {
		case x < tabMainW:
			// Main tab clicked.
			m.subCur = -1
			return m, true, nil
		default:
			// Find which visible sub tab was clicked (windowed to terminal width,
			// same mapping renderMainTabBar uses).
			start, count := m.visibleSubTabs()
			for slot := 0; slot < count; slot++ {
				k := start + slot
				lo := m.subTabSlotStart(slot)
				hi := lo + tabSubW
				if x >= lo && x < hi {
					if x >= hi-2 && (m.subTabs[k].done || m.subTabs[k].finished) {
						// × on a finished tab — close it.
						m.subTabs = append(m.subTabs[:k], m.subTabs[k+1:]...)
						if m.subCur >= k {
							m.subCur--
						}
						if m.subCur < -1 {
							m.subCur = -1
						}
						// When the last tab is removed the tab bar disappears,
						// reclaiming one row. Reflow so the viewport height is
						// correct; without this the display underflows by one row.
						if len(m.subTabs) == 0 {
							m = m.reflow()
						}
					} else {
						// Switch to this tab (running tabs ignore the × area).
						m.subCur = k
					}
					return m, true, nil
				}
			}
		}
	}

	switch msg.Action {
	case tea.MouseActionPress:
		if msg.Button != tea.MouseButtonLeft {
			return m, false, nil
		}
		row, col, in := m.mouseToContent(msg.X, msg.Y)
		if !in {
			// Press outside the conversation pane clears any selection but is
			// otherwise not ours to consume.
			if m.sel.active {
				m.sel = selection{}
				m.refreshViewport()
			}
			return m, false, nil
		}
		before := m.statusRows()
		m.flash = ""
		m = m.reflowIfStatusHeightChanged(before)
		m.sel = selection{active: true, anchorRow: row, anchorCol: col, headRow: row, headCol: col}
		m.renderSelection()
		return m, true, nil

	case tea.MouseActionMotion:
		if !m.sel.active {
			return m, false, nil
		}
		// Cell-motion events only arrive while a button is held, so this is a
		// drag. Clamp to the pane so dragging past an edge extends to it.
		m.sel.dragging = true
		m.sel.headRow, m.sel.headCol = m.clampToContent(msg.X, msg.Y)
		m.renderSelection()
		return m, true, nil

	case tea.MouseActionRelease:
		if !m.sel.active {
			return m, false, nil
		}
		if !m.sel.dragging {
			// A plain click (no drag) just clears the selection.
			m.sel = selection{}
			m.refreshViewport()
			return m, true, nil
		}
		text := m.selectedText()
		// Keep the highlight visible so the user sees what was copied.
		m.renderSelection()
		return m, true, copyToClipboard(text)
	}
	return m, false, nil
}

// renderSelection re-overlays the highlight on the existing plainLines without
// rebuilding the styled transcript, and pins the scroll offset so a drag never
// jumps the view. Used for the fast press/motion/release path; refreshViewport
// handles the cases where the underlying content actually changed.
func (m *tuiModel) renderSelection() {
	off := m.vp.YOffset
	m.vp.SetContent(m.highlightedContent())
	m.vp.SetYOffset(off)
}

// mouseToContent maps a screen cell to content coordinates, reporting whether it
// landed inside the conversation pane's inner area and on a content line.
func (m tuiModel) mouseToContent(x, y int) (row, col int, in bool) {
	vpW, vpH, _ := m.sizes()
	// The conv pane is the topmost section, so it occupies rows 1..vpH
	// (row 0 is the top border, rows 1..vpH are content, row vpH+1 is the
	// bottom border) without any top offset.
	if x < 1 || x > vpW || y < 1 || y > vpH {
		return 0, 0, false
	}
	row = (y - 1) - m.bottomPad(vpH) + m.vp.YOffset
	// Clicks in the blank padding above short content are not on any line.
	if row < 0 || row >= m.vp.TotalLineCount() {
		return 0, 0, false
	}
	return row, x - 1, true
}

// clampToContent is mouseToContent without the bounds check: the cell is clamped
// into the pane so a drag beyond an edge selects up to that edge.
func (m tuiModel) clampToContent(x, y int) (row, col int) {
	vpW, vpH, _ := m.sizes()
	if x < 1 {
		x = 1
	} else if x > vpW {
		x = vpW
	}
	if y < 1 {
		y = 1
	} else if y > vpH {
		y = vpH
	}
	row = (y - 1) - m.bottomPad(vpH) + m.vp.YOffset
	// Clamp to the valid content range [0, TotalLineCount-1].
	total := m.vp.TotalLineCount()
	if row < 0 {
		row = 0
	} else if row >= total {
		row = total - 1
	}
	if row < 0 {
		row = 0 // TotalLineCount()==0 guard
	}
	return row, x - 1
}

// sliceRange returns the inclusive rune range [a,z] of the selection on content
// line i, clamped to the line. ok is false when there is nothing to select.
func (m tuiModel) sliceRange(i, sr, sc, er, ec, lineLen int) (a, z int, ok bool) {
	a, z = 0, lineLen-1
	if i == sr {
		a = sc
	}
	if i == er {
		z = ec
	}
	if a < 0 {
		a = 0
	}
	if z > lineLen-1 {
		z = lineLen - 1
	}
	if lineLen == 0 || a > z {
		return 0, 0, false
	}
	return a, z, true
}

// selectedText extracts the plain text covered by the selection, trimming the
// trailing padding spaces glamour adds to each line.
func (m tuiModel) selectedText() string {
	sr, sc, er, ec := m.sel.ordered()
	var b strings.Builder
	for i := sr; i <= er && i < len(m.plainLines); i++ {
		runes := []rune(m.plainLines[i])
		seg := ""
		if a, z, ok := m.sliceRange(i, sr, sc, er, ec, len(runes)); ok {
			seg = string(runes[a : z+1])
		}
		b.WriteString(strings.TrimRight(seg, " "))
		if i != er {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// highlightedContent rebuilds the viewport content from the plain mirror with
// the selected range drawn in reverse video. Colors are dropped while a
// selection is active; they return when it clears.
func (m tuiModel) highlightedContent() string {
	sr, sc, er, ec := m.sel.ordered()
	out := make([]string, len(m.plainLines))
	for i, line := range m.plainLines {
		if i < sr || i > er {
			out[i] = line
			continue
		}
		runes := []rune(line)
		a, z, ok := m.sliceRange(i, sr, sc, er, ec, len(runes))
		if !ok {
			out[i] = line
			continue
		}
		out[i] = string(runes[:a]) + sgrReverse + string(runes[a:z+1]) + sgrReset + string(runes[z+1:])
	}
	return strings.Join(out, "\n")
}

// clipboardCmds lists the native clipboard writers we try, in preference order.
// The first one found on PATH wins. wl-copy (Wayland) and the X11 tools forward
// stdin to the system clipboard; pbcopy is macOS; clip.exe covers WSL.
var clipboardCmds = [][]string{
	{"wl-copy"},
	{"xclip", "-selection", "clipboard"},
	{"xsel", "--clipboard", "--input"},
	{"pbcopy"},
	{"clip.exe"},
}

// nativeClipboardWriter returns the first available clipboard command, or nil.
func nativeClipboardWriter() []string {
	for _, c := range clipboardCmds {
		if p, err := exec.LookPath(c[0]); err == nil {
			return append([]string{p}, c[1:]...)
		}
	}
	return nil
}

// copyToClipboard writes the selection to the system clipboard. It prefers a
// native clipboard command (wl-copy/xclip/xsel/pbcopy/clip.exe) since many
// terminals silently ignore OSC 52 clipboard writes, and falls back to OSC 52
// when no command is available (e.g. a bare remote shell over SSH).
func copyToClipboard(text string) tea.Cmd {
	return func() tea.Msg {
		if strings.TrimSpace(text) == "" {
			return nil
		}
		if argv := nativeClipboardWriter(); argv != nil {
			cmd := exec.Command(argv[0], argv[1:]...)
			cmd.Stdin = strings.NewReader(text)
			// Leave Stdout/Stderr nil → /dev/null, so a daemonizing writer
			// (wl-copy forks to serve the selection) can't scribble on the TUI.
			if err := cmd.Run(); err == nil {
				return copiedMsg{n: len([]rune(text))}
			}
		}
		// Fallback: OSC 52 is self-contained and cursor-neutral, so writing it
		// straight to stdout alongside Bubble Tea's renderer is safe.
		_, _ = os.Stdout.WriteString(ansi.SetSystemClipboard(text))
		return copiedMsg{n: len([]rune(text))}
	}
}

// bottomPad returns the number of blank rows prepended at the top of the
// conversation pane when content is shorter than the viewport. This happens
// because bottomAlignViewport moves the viewport's top-aligned padding to the
// top so that short content sits flush against the input box. Mouse
// coordinate mapping must skip these blank rows to hit the right content line.
func (m tuiModel) bottomPad(vpH int) int {
	n := m.vp.TotalLineCount()
	if n >= vpH {
		return 0
	}
	return vpH - n
}
