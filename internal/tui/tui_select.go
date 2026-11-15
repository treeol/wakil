package tui

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/treeol/wakil/internal/config"

	tea "github.com/charmbracelet/bubbletea"
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
//
// State machine:
//   - pressed: a left-press was captured inside the pane. The anchor is recorded
//     but NO highlight is rendered — styling is preserved. This prevents a plain
//     click from replacing the styled viewport with the ANSI-stripped plainLines.
//   - active: the highlight is rendered (reverse video on the selected range).
//     Set only when the mouse actually moves to a different cell (real drag).
//   - dragging: the mouse moved since press (distinguishes drag from click for
//     release routing).
type selection struct {
	pressed   bool // left-press captured in the pane; anchor recorded, no highlight yet
	active    bool // selection highlight is rendered
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
		msg.Button == tea.MouseButtonLeft && !m.sel.active && !m.sel.pressed &&
		msg.Y == m.height-1 {
		x := msg.X
		switch {
		case x < tabMainW:
			// Main tab clicked.
			m.subCur = -1
			m.followBottom = true
			m.vp.GotoBottom()
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
						// A sub-tab renders its own bottom-anchored content, but the
						// main viewport's follow latch resets so returning to main
						// shows the latest (not a stale scrolled-up position).
						m.followBottom = true
					}
					return m, true, nil
				}
			}
		}
	}

	// Sub-tab guard: when a sub-tab renders the conversation-pane area, the
	// in-app selection coordinates would map into the HIDDEN main-pane
	// content (mouseToContent reads the main viewport's plain mirrors) —
	// dragging visible sub-tab output must not copy hidden main-pane text
	// (Mashūra review finding). Tab-bar clicks are handled above; everything
	// else left-button in the pane area is swallowed while a sub-tab is
	// active. Wheel events already fell through earlier for scrolling. Any
	// stale selection highlight from the main pane is cleared so it can't
	// linger over sub-tab output.
	if m.subCur >= 0 {
		if m.sel.active || m.sel.pressed {
			wasActive := m.sel.active
			m.sel = selection{}
			if wasActive {
				// Restore the styled viewport — the selection highlight
				// replaced it with the ANSI-stripped plainLines overlay.
				m.refreshViewport()
			}
		}
		return m, true, nil
	}

	switch msg.Action {
	case tea.MouseActionPress:
		if msg.Button != tea.MouseButtonLeft {
			return m, false, nil
		}
		// Clear flash and reflow BEFORE computing the anchor so the coordinate
		// maps against the post-reflow geometry (review finding: the old code
		// computed row/col before reflow, which could shift the anchor if the
		// status zone height changed).
		before := m.effectiveStatusRows()
		m.flash = ""
		m = m.reflowIfStatusHeightChanged(before)
		// If a previous selection's highlight is visible, clear it now and
		// restore styling before computing the anchor — the anchor must map
		// against the post-clear geometry/YOffset.
		if m.sel.active {
			m.sel = selection{}
			m.refreshViewport()
		}
		row, col, in := m.mouseToContent(msg.X, msg.Y)
		if !in {
			// Press outside the conversation pane clears any pending press
			// state but doesn't need a viewport refresh (no highlight was
			// rendered for a pending-only state). A prior active highlight
			// was already cleared above.
			m.sel = selection{}
			return m, false, nil
		}
		// Record the anchor but do NOT render a highlight yet. This prevents
		// a plain click from replacing styled content with the ANSI-stripped
		// plainLines. The highlight activates only on the first real drag
		// (motion to a different cell).
		m.sel = selection{pressed: true, anchorRow: row, anchorCol: col, headRow: row, headCol: col}
		return m, true, nil

	case tea.MouseActionMotion:
		if !m.sel.pressed {
			return m, false, nil
		}
		// Cell-motion events only arrive while a button is held (the program
		// uses WithMouseCellMotion, not WithMouseAllMotion), so this is a drag.
		row, col := m.clampToContent(msg.X, msg.Y)
		// Don't activate until the pointer moves to a different cell — terminal
		// jitter or a same-cell motion shouldn't turn a click into a selection.
		if !m.sel.active && row == m.sel.anchorRow && col == m.sel.anchorCol {
			return m, true, nil
		}
		m.sel.dragging = true
		m.sel.active = true
		m.sel.headRow, m.sel.headCol = row, col
		m.renderSelection()
		return m, true, nil

	case tea.MouseActionRelease:
		if !m.sel.pressed {
			return m, false, nil
		}
		if !m.sel.dragging {
			// A plain click (no drag) — clear the pending press state and
			// consume the event without touching the viewport content.
			m.sel = selection{}
			return m, true, nil
		}
		text := m.selectedText()
		// Keep the highlight visible so the user sees what was copied.
		m.renderSelection()
		// Clear pressed/dragging but keep active so the highlight persists.
		m.sel.pressed = false
		m.sel.dragging = false
		return m, true, m.copyToClipboard(text)
	}
	return m, false, nil
}

// renderSelection re-overlays the highlight on the existing plainLines without
// rebuilding the styled transcript, and pins the scroll offset so a drag never
// jumps the view. Used for the fast press/motion/release path; refreshViewport
// handles the cases where the underlying content actually changed.
func (m *tuiModel) renderSelection() {
	off := m.vp.YOffset
	sr, sc, er, ec := m.sel.ordered()
	m.vp.SetContent(m.highlightedWindow(sr, sc, er, ec))
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

// selectedText extracts the plain text covered by the selection from the
// no-box plain mirror (plainLinesNoBox), which contains only conversation
// content without box border decoration. Selection coordinates are in
// plainLines space (with borders); rowToNoBox maps them to the no-box mirror.
func (m tuiModel) selectedText() string {
	sr, sc, er, ec := m.sel.ordered()
	// Map selection row indices from plainLines (with borders) to
	// plainLinesNoBox (without borders) by counting non-border rows.
	srNB := m.rowToNoBoxPair(sr)
	erNB := m.rowToNoBoxPair(er)
	if srNB >= len(m.plainLinesNoBox) {
		return ""
	}
	if erNB >= len(m.plainLinesNoBox) {
		erNB = len(m.plainLinesNoBox) - 1
	}

	// Determine whether the source lines have box borders by checking if
	// plainLines differs from plainLinesNoBox (i.e. boxes are rendered).
	// When boxes are present, col 0 in plainLines is the left border │,
	// so content starts at col 1. We subtract 1 from selection columns.
	// When no boxes, columns map directly.
	hasBorders := len(m.plainLines) != len(m.plainLinesNoBox)
	colAdj := 0
	if hasBorders {
		colAdj = 1 // shift left past the │ border
	}

	var parts []string
	for i := srNB; i <= erNB; i++ {
		runes := []rune(m.plainLinesNoBox[i])
		seg := ""
		adjSC := sc
		adjEC := ec
		if hasBorders {
			adjSC = sc - colAdj
			adjEC = ec - colAdj
			if adjSC < 0 {
				adjSC = 0
			}
			if adjEC < 0 {
				adjEC = 0
			}
		}
		// Preserve interior structure exactly: a full-width selection on a blank
		// interior line yields an empty segment (kept — blank lines are part of
		// the copied text), and trailing spaces within the selected range are kept
		// verbatim. Trailing layout padding is excluded because the no-box mirror
		// is built from ansi.Strip of UNPADDED content (refreshViewport's
		// plainSB path) — the mirror lines carry no renderer padding to leak.
		if a, z, ok := m.sliceRange(i, srNB, adjSC, erNB, adjEC, len(runes)); ok {
			seg = string(runes[a : z+1])
		}
		parts = append(parts, seg)
	}
	return strings.Join(parts, "\n")
}

// rowToNoBox maps a row index in plainLines (with box borders) to the
// corresponding index in plainLinesNoBox (without borders). For border rows,
// returns the index of the next content row (or past-end if the border is
// the last row). Callers must bounds-check the result against
// len(plainLinesNoBox) before use.
//
// Both mirrors are line-split views of the SAME wrapped content
// (refreshViewport, tui.go:2238-2239), so they have the same number of
// lines and the same indices. A real box border therefore has NO content
// row in the no-box mirror at the same index — the walk below counts a
// `plainLines` row as a border only when the corresponding no-box row is
// NOT plain content that could carry it. The pinch below asks "is this a
// border row" using isBoxBorderLine over the BORDERED space, which alone
// cannot tell a wrapped content row whose trimmed form starts and ends
// with │ (a row that happens to be box-drawn) from a real top/bottom
// border.
func (m tuiModel) rowToNoBox(row int) int {
	noBoxIdx := 0
	for i := 0; i < row && i < len(m.plainLines); i++ {
		if !isBoxBorderLine(strings.TrimSpace(m.plainLines[i])) {
			noBoxIdx++
		}
	}
	// Check if the target row itself is a border row.
	if row < len(m.plainLines) && isBoxBorderLine(strings.TrimSpace(m.plainLines[row])) {
		return noBoxIdx // map to the next content row (or past end)
	}
	return noBoxIdx
}

// rowToNoBoxPair maps a row in plainLines (with borders) through BOTH
// mirrors. It is a pinch-point guard (Part 3): a plainLines border-row
// is only a real box border when the corresponding no-box row does NOT
// already hold that content — since both mirrors are the same wrapped
// content, a no-box line at the same index that carries visible text
// proves the plainLines row is content (a soft-wrapped interior row),
// not a border. The walk below uses that test to bound the mapping and
// corrects the classic failure: a wrapped interior row that happens to
// start and end with │ being counted as a border is impossible once the
// no-box mirror is consulted.
//
// In practice a real border row in plainLines is paired with an EMPTY or
// border row in plainLinesNoBox (the no-box mirror strips boxes); a
// content row is paired with the same text. If they differ in length,
// the mirrors have drifted and the `plainLines` space is the only safe
// authority (old behaviour).
func (m tuiModel) rowToNoBoxPair(row int) int {
	if len(m.plainLines) != len(m.plainLinesNoBox) {
		return m.rowToNoBox(row) // drift: plainLines space is authoritative
	}
	// Evidence guard: if every paired border candidate is paired with a
	// content row, the "borders" are in fact content and the mapping must
	// NOT skip them. Count them; if there is at least one border row
	// whose no-box partner is empty/border-type, the walk stands.
	hasTrueBorder := false
	for i := range m.plainLines {
		if !isBoxBorderLine(strings.TrimSpace(m.plainLines[i])) {
			continue
		}
		nb := strings.TrimSpace(m.plainLinesNoBox[i])
		if nb == "" || isBoxBorderLine(nb) {
			hasTrueBorder = true
			break
		}
	}
	if !hasTrueBorder {
		// No true border anywhere: the walk must not have skipped.
		for i := 0; i < row && i < len(m.plainLines); i++ {
			// no skip
			_ = i
		}
		// Fall through to the corrected walk below, which counts a row
		// as border only when its no-box partner cannot be that content.
		noBoxIdx := 0
		for i := 0; i < row && i < len(m.plainLines); i++ {
			if !m.isTrueBorderPair(i) {
				noBoxIdx++
			}
		}
		if row < len(m.plainLines) && m.isTrueBorderPair(row) {
			return noBoxIdx
		}
		return noBoxIdx
	}
	return m.rowToNoBox(row)
}

// isTrueBorderPair reports whether row i of plainLines is a real box
// border: border-shaped in the bordered mirror AND NOT holding the same
// visible text in the no-box mirror.
func (m tuiModel) isTrueBorderPair(i int) bool {
	if i < 0 || i >= len(m.plainLines) {
		return false
	}
	if !isBoxBorderLine(strings.TrimSpace(m.plainLines[i])) {
		return false
	}
	nb := strings.TrimSpace(m.plainLinesNoBox[i])
	return nb == "" || isBoxBorderLine(nb)
}

// isBoxBorderLine returns true if the line is a top or bottom border row of a
// turn box (e.g. "╭───╮" or "╰───╯"). Only these rows exist in plainLines
// (with borders) but NOT in plainLinesNoBox (without borders), so rowToNoBox
// must skip exactly these and no others.
//
// The previous implementation matched any line consisting solely of
// box-drawing characters and spaces, which misidentified separator lines
// ("│ ─── │"), blank content lines ("│     │"), and any content line
// containing only box glyphs as border rows. This caused rowToNoBox to skip
// content rows, desynchronizing the plainLines → plainLinesNoBox mapping and
// making copied text come from the wrong lines.
func isBoxBorderLine(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	runes := []rune(s)
	if len(runes) < 2 {
		return false
	}
	first, last := runes[0], runes[len(runes)-1]
	// Rounded border corners (styleTurnBox uses lipgloss.RoundedBorder()).
	// Also accept square corners for robustness.
	return (first == '╭' && last == '╮') || (first == '╰' && last == '╯') ||
		(first == '┌' && last == '┐') || (first == '└' && last == '┘')
}

// highlightedContent rebuilds the viewport content from the plain mirror with
// the selected range drawn in reverse video. Colors are dropped while a
// selection is active; they return when it clears.
func (m tuiModel) highlightedContent() string {
	sr, sc, er, ec := m.sel.ordered()
	// Full-content highlight (no viewport window): used by tests that
	// assert on the whole highlight, and by refreshViewport's active path
	// before this windowed optimisation landed. The drag hot path uses
	// highlightedWindow via renderSelection/refreshViewport.
	lines := m.plainLines
	out := make([]string, len(lines))
	for i, line := range lines {
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

// highlightedWindow renders only the visible window of the highlight (plus
// the selection span), with row references copied unless the row is inside
// the selection (then one styled string is built). SetContent slices the
// result in View, so handing it the whole transcript on every drag step
// pays the full join for the whole session (Part 1 Drag2000: 53.6 ms,
// 20 MB, 59k allocs at 2000 messages).
func (m tuiModel) highlightedWindow(sr, sc, er, ec int) string {
	lines := m.plainLines
	if len(lines) == 0 {
		return ""
	}
	_, vpH, _ := m.sizes()
	lo := m.vp.YOffset
	if lo < 0 {
		lo = 0
	}
	hi := lo + vpH
	if hi > len(lines) {
		hi = len(lines)
	}
	var sb strings.Builder
	for i := lo; i < hi; i++ {
		if i > lo {
			sb.WriteByte('\n')
		}
		line := lines[i]
		if i < sr || i > er {
			sb.WriteString(line)
			continue
		}
		runes := []rune(line)
		a, z, ok := m.sliceRange(i, sr, sc, er, ec, len(runes))
		if !ok {
			sb.WriteString(line)
			continue
		}
		sb.WriteString(string(runes[:a]))
		sb.WriteString(sgrReverse)
		sb.WriteString(string(runes[a : z+1]))
		sb.WriteString(sgrReset)
		sb.WriteString(string(runes[z+1:]))
	}
	return sb.String()
}

// clipboardCmds lists the native clipboard writers we try, in preference
// order. wl-copy (Wayland) and the X11 tools forward stdin to the system
// clipboard; pbcopy is macOS; clip.exe covers WSL. copyToClipboard tries them
// in order until one SUCCEEDS — presence on PATH is not enough (review
// finding: xclip present without DISPLAY must not block xsel/wl-copy).
var clipboardCmds = [][]string{
	{"wl-copy"},
	{"xclip", "-selection", "clipboard"},
	{"xsel", "--clipboard", "--input"},
	{"pbcopy"},
	{"clip.exe"},
}

// clipboardTimeout bounds each native clipboard writer. A hanging selection
// owner must not leave the copy command in flight forever.
const clipboardTimeout = 2 * time.Second

// nativeCopyResult reports the outcome of attempting the native backends.
type nativeCopyResult struct {
	backend string   // name of the backend that succeeded (e.g. "wl-copy"), if any
	errs    []string // per-backend failure reasons (bounded), for diagnostics
}

// tryNativeClipboard runs each known backend in order until one succeeds.
// Bounded by clipboardTimeout per command via CommandContext; child stdout and
// stderr are left nil (/dev/null) so a chatty writer can't scribble on the
// TUI. Note CommandContext does not kill grandchildren the backend may fork
// (wl-copy daemonizes legitimately) — the timeout applies to the foreground
// process only, which is the behavior we want.
func tryNativeClipboard(text string) (nativeCopyResult, bool) {
	var res nativeCopyResult
	for _, c := range clipboardCmds {
		path, err := exec.LookPath(c[0])
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), clipboardTimeout)
		cmd := exec.CommandContext(ctx, path, c[1:]...)
		cmd.Stdin = strings.NewReader(text)
		err = cmd.Run()
		cancel()
		if err == nil {
			res.backend = c[0]
			return res, true
		}
		if len(res.errs) < 3 { // bounded diagnostics, not a full log
			res.errs = append(res.errs, fmt.Sprintf("%s: %v", c[0], err))
		}
	}
	return res, false
}

// copyToClipboard writes text to the system clipboard, honoring the model's
// configured clipboard mode:
//
//   - auto (default): native tools first (tried until one succeeds), then
//     OSC 52 — so SSH users still get a best-effort copy.
//   - native: native tools only; failure is reported honestly, no OSC 52.
//   - osc52: OSC 52 only; no local command is ever executed.
//
// The OSC 52 path returns the escape bytes in the msg instead of writing them
// to os.Stdout here — the renderer owns stdout in alt-screen mode, and a
// direct write from this goroutine would interleave with frame data and
// garble the display. Delivery is best-effort: the terminal may silently
// ignore the sequence, and the status hint says "sent, unverified".
func (m tuiModel) copyToClipboard(text string) tea.Cmd {
	mode := m.clipboardMode
	if mode == "" {
		mode = config.ClipboardModeAuto
	}
	// Mint the request id NOW, synchronously: copyToClipboard is called from
	// Update (single-threaded), so dispatch order == id order. Minting inside
	// the Cmd closure would let concurrent Cmd goroutines invert the order
	// (Mashūra A1): an older copy could mint a higher id and supersede a
	// newer one.
	id := nextCopyID()
	return func() tea.Msg {
		if strings.TrimSpace(text) == "" {
			return nil
		}
		var detail string
		if mode != config.ClipboardModeOSC52 {
			res, ok := tryNativeClipboard(text)
			if ok {
				return copiedMsg{n: len([]rune(text)), via: copyViaNative, outcome: copyOK, detail: res.backend, id: id}
			}
			if len(res.errs) > 0 {
				detail = strings.Join(res.errs, "; ")
			}
		}
		if mode == config.ClipboardModeNative {
			if detail == "" {
				detail = "no native clipboard tool available"
			}
			return copiedMsg{n: 0, via: copyViaNative, outcome: copyFailed, detail: detail, id: id}
		}
		esc := []byte(ansi.SetSystemClipboard(text))
		if mode == config.ClipboardModeAuto && detail != "" {
			// Auto mode fell through after real native failures — surface why.
			return copiedMsg{n: len([]rune(text)), via: copyViaOSC52, outcome: copySentUnverified, detail: "native failed (" + detail + ")", id: id, escape: esc}
		}
		return copiedMsg{n: len([]rune(text)), via: copyViaOSC52, outcome: copySentUnverified, detail: "", id: id, escape: esc}
	}
}

// copyIDGen is the process-wide copy-request generation counter. Monotonic per
// process; the handler rejects copiedMsg with id < the latest seen.
var copyIDGen atomic.Uint64

func nextCopyID() uint64 { return copyIDGen.Add(1) }

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
