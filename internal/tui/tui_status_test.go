package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// plain strips ANSI codes so we can check segment content as plain text.

// statusSegTexts returns the plain-text identity segments (no ctx gauge —
// that needs an app), so tests can assert ordering without styling noise.
func statusSegTexts(in statusLineInput) []string {
	segs := statusSegments(in)
	out := make([]string, len(segs))
	for i, s := range segs {
		out[i] = plain(s)
	}
	return out
}

func TestStatusSegmentsIdleInitial(t *testing.T) {
	segs := statusSegTexts(statusLineInput{state: stateIdle, hadTurn: false})
	if len(segs) != 1 || segs[0] != "•" {
		t.Errorf("fresh idle = dot only; got: %v", segs)
	}
}

func TestStatusSegmentsIdleAfterTurn(t *testing.T) {
	segs := statusSegTexts(statusLineInput{state: stateIdle, hadTurn: true})
	if len(segs) != 1 || segs[0] != "• awaiting input" {
		t.Errorf("dot should glue to 'awaiting input' when AUTO is off; got: %v", segs)
	}
}

func TestStatusSegmentsStreaming(t *testing.T) {
	segs := statusSegTexts(statusLineInput{state: stateStreaming, tps: 45})
	if segs[0] != "• streaming" {
		t.Errorf("head = %q, want '• streaming'", segs[0])
	}
	if len(segs) != 2 || segs[1] != "45 t/s" {
		t.Errorf("t/s must be its own segment; got: %v", segs)
	}
}

func TestStatusSegmentsReasoning(t *testing.T) {
	segs := statusSegTexts(statusLineInput{state: stateStreaming, reasoning: true})
	if segs[0] != "• reasoning" {
		t.Errorf("reasoning head = %q, want '• reasoning'", segs[0])
	}
}

func TestStatusSegmentsConfirm(t *testing.T) {
	segs := statusSegTexts(statusLineInput{state: stateConfirm})
	if segs[0] != "• confirming" {
		t.Errorf("confirm head = %q, want '• confirming'", segs[0])
	}
}

func TestStatusSegmentsAutoGlued(t *testing.T) {
	// Dot glued to AUTO by a plain space; the state label glued on behind it —
	// fixed slots 1+2, one head segment.
	segs := statusSegTexts(statusLineInput{state: stateIdle, autoApprove: true, hadTurn: true})
	if len(segs) != 1 || segs[0] != "• AUTO awaiting input" {
		t.Errorf("head must be 'dot AUTO state' glued in fixed order; got: %v", segs)
	}
}

func TestStatusSegmentsAutoDestructive(t *testing.T) {
	segs := statusSegTexts(statusLineInput{state: stateIdle, autoApprove: true, allowDestructive: true, hadTurn: true})
	if !strings.Contains(segs[0], "AUTO!") {
		t.Errorf("AUTO! must appear when allowDestructive; got: %v", segs)
	}
	segs = statusSegTexts(statusLineInput{state: stateIdle, allowDestructive: true, hadTurn: true})
	if strings.Contains(segs[0], "AUTO") {
		t.Errorf("no AUTO segment without autoApprove; got: %v", segs)
	}
}

func TestStatusSegmentsWorkflowLabel(t *testing.T) {
	segs := statusSegTexts(statusLineInput{state: stateStreaming, workflowLabel: "implement 3/6"})
	if len(segs) != 2 || segs[1] != "plan implement 3/6" {
		t.Errorf("workflow label must be 'plan <label>' segment; got: %v", segs)
	}
}

func TestStatusSegmentsVolatileRight(t *testing.T) {
	// Volatile segments (t/s, flash) must sit rightmost so they never shift
	// the stable identity segments under the user's eyes.
	segs := statusSegTexts(statusLineInput{
		state: stateStreaming, model: "moonshotai/kimi-k3", tps: 45, flash: "copied 12",
	})
	mi, ti, fi := -1, -1, -1
	for i, seg := range segs {
		switch {
		case strings.Contains(seg, "kimi"):
			mi = i
		case strings.Contains(seg, "t/s"):
			ti = i
		case strings.Contains(seg, "copied"):
			fi = i
		}
	}
	if mi < 0 || ti < 0 || fi < 0 {
		t.Fatalf("missing segments; got: %v", segs)
	}
	if !(mi < ti && ti < fi) {
		t.Errorf("want model < t/s < flash (volatile right); got: %v", segs)
	}
}

func TestStatusSegmentsDotAlwaysPresent(t *testing.T) {
	for _, state := range []agentState{stateIdle, stateStreaming, stateConfirm, stateCompacting} {
		segs := statusSegTexts(statusLineInput{state: state})
		if !strings.HasPrefix(segs[0], "•") {
			t.Errorf("dot missing in state %d: %v", state, segs)
		}
	}
}

// --- Backend segment (P29) ---

func TestStatusSegmentsBackendOmittedWhenDefault(t *testing.T) {
	segs := statusSegTexts(statusLineInput{
		state: stateIdle, hadTurn: true, backendUsed: "local", backendDefault: "local",
	})
	for _, s := range segs {
		if strings.Contains(s, "local") {
			t.Errorf("default backend should be omitted; got %v", segs)
		}
	}
}

func TestStatusSegmentsBackendShownWhenNonDefault(t *testing.T) {
	segs := statusSegTexts(statusLineInput{
		state: stateIdle, hadTurn: true, backendUsed: "openrouter", backendDefault: "local",
	})
	found := false
	for _, s := range segs {
		if s == "openrouter" {
			found = true
		}
	}
	if !found {
		t.Errorf("non-default backend should appear; got %v", segs)
	}
}

func TestStatusSegmentsBackendOverrideMarker(t *testing.T) {
	segs := statusSegTexts(statusLineInput{
		state: stateIdle, hadTurn: true,
		backendUsed: "openrouter", backendRequested: "together", backendDefault: "",
	})
	found := false
	for _, s := range segs {
		if s == "openrouter!" {
			found = true
		}
	}
	if !found {
		t.Errorf("override marker expected; got %v", segs)
	}
}

// --- flowSegments (status line packing) ---

func TestFlowSegmentsOneLineWhenFits(t *testing.T) {
	rows := flowSegments([]string{"• AUTO", "model", "ctx ⣿ 0k"}, 120)
	if len(rows) != 1 {
		t.Fatalf("should fit on one row; got %d: %v", len(rows), rows)
	}
	if plain(rows[0]) != "• AUTO · model · ctx ⣿ 0k" {
		t.Errorf("joined with separators; got %q", plain(rows[0]))
	}
}

func TestFlowSegmentsWrapsToTwo(t *testing.T) {
	// Each segment ~10 wide; w=25 fits two per row → 2 rows for 3 segments.
	rows := flowSegments([]string{"aaaaaaaaaa", "bbbbbbbbbb", "cccccccccc"}, 25)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows; got %d: %v", len(rows), rows)
	}
	if plain(rows[0]) != "aaaaaaaaaa · bbbbbbbbbb" {
		t.Errorf("row 1 packs until full; got %q", plain(rows[0]))
	}
	if plain(rows[1]) != "cccccccccc" {
		t.Errorf("overflow segment moves wholesale to row 2; got %q", plain(rows[1]))
	}
}

func TestFlowSegmentsNeverExceedsTwoRows(t *testing.T) {
	var segs []string
	for i := 0; i < 12; i++ {
		segs = append(segs, strings.Repeat(string(rune('a'+i)), 10))
	}
	rows := flowSegments(segs, 30)
	if len(rows) != 2 {
		t.Fatalf("status zone caps at 2 rows; got %d", len(rows))
	}
	// Dropped segments leave an ellipsis on the last row.
	if !strings.Contains(plain(rows[1]), "…") {
		t.Errorf("dropped segments should leave an ellipsis; got %q", plain(rows[1]))
	}
	for i, r := range rows {
		if w := lipgloss.Width(plain(r)); w > 30 {
			t.Errorf("row %d exceeds width: %d > 30 (%q)", i, w, plain(r))
		}
	}
}

func TestFlowSegmentsOversizedSingleSegment(t *testing.T) {
	rows := flowSegments([]string{strings.Repeat("x", 50)}, 20)
	if len(rows) != 1 {
		t.Fatalf("one row; got %d", len(rows))
	}
	if w := lipgloss.Width(plain(rows[0])); w > 20 {
		t.Errorf("oversized segment must be truncated in place: %d cols", w)
	}
}

func TestFlowSegmentsDropsRightmostSuffixNotMiddle(t *testing.T) {
	// Row 2 fits "small2" but NOT "wide…": the wide segment and everything
	// after it must be dropped — a later smaller segment must never leapfrog
	// a dropped middle one.
	segs := []string{"aaa", "bbb", "small2", "widewidewidewide", "ccc"}
	rows := flowSegments(segs, 16) // row1: "aaa · bbb · small2"=18>16 → row1 "aaa · bbb", row2 "small2" then wide drops
	if len(rows) != 2 {
		t.Fatalf("want 2 rows; got %d: %v", len(rows), rows)
	}
	r2 := plain(rows[1])
	if strings.Contains(r2, "ccc") {
		t.Errorf("later segment must not survive a dropped middle one; row2=%q", r2)
	}
	if strings.Contains(r2, "widewide") {
		t.Errorf("dropped wide segment should not appear; row2=%q", r2)
	}
	if !strings.Contains(r2, "small2") {
		t.Errorf("fitting segment keeps its place; row2=%q", r2)
	}
	if !strings.Contains(r2, "…") {
		t.Errorf("ellipsis marks the drop; row2=%q", r2)
	}
}

func TestFlowSegmentsAnsiStyledSegments(t *testing.T) {
	// Colored segments: packing must be ANSI-aware (lipgloss.Width) and never
	// leak a partial escape sequence into the visible width math.
	red := lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Render("REDRED")
	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render("dimdim")
	rows := flowSegments([]string{red, dim, "plain"}, 14)
	for i, r := range rows {
		if w := lipgloss.Width(r); w > 14 {
			t.Errorf("row %d styled width %d > 14", i, w)
		}
	}
}

func TestFlowSegmentsPathologicalWidths(t *testing.T) {
	segs := []string{"• AUTO", "model", "ctx ⣿ 0k", "hist 1 2k"}
	for _, w := range []int{1, 2, 3, 5, 8} {
		rows := flowSegments(segs, w)
		if len(rows) == 0 || len(rows) > 2 {
			t.Errorf("w=%d: rows=%d, want 1..2", w, len(rows))
		}
		for i, r := range rows {
			if got := lipgloss.Width(r); got > w+1 { // +1 slack for the ellipsis cell
				t.Errorf("w=%d row %d = %d cols %q", w, i, got, plain(r))
			}
		}
	}
}

func TestFlowSegmentsSingleSegmentPerRowWhenForced(t *testing.T) {
	// Width fits exactly one segment per row: head on row 1, next on row 2,
	// the rest dropped with ellipsis.
	rows := flowSegments([]string{"aaaa", "bbbb", "cccc"}, 4)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows; got %d: %v", len(rows), rows)
	}
	if plain(rows[0]) != "aaaa" {
		t.Errorf("row1 = %q, want head only", plain(rows[0]))
	}
	// Row 2: "bbbb" doesn't fit with the drop ellipsis, so it truncates to
	// "bbb…" — the ellipsis cell is reserved out of the width budget.
	if !strings.Contains(plain(rows[1]), "bbb") || !strings.Contains(plain(rows[1]), "…") {
		t.Errorf("row2 = truncated second segment + ellipsis; got %q", plain(rows[1]))
	}
	if w := lipgloss.Width(plain(rows[1])); w > 4 {
		t.Errorf("row2 width %d exceeds 4", w)
	}
}

// --- statusLines integration (model → rows) ---

func TestStatusLinesSearchOwnsRow(t *testing.T) {
	m := layoutModel(100, 40)
	m.searchActive = true
	m.searchQuery = "git"
	m.inputHistory = []string{"git commit"}
	m.searchIdx = 0
	lines := m.statusLines()
	if len(lines) != 1 {
		t.Fatalf("search = 1 row; got %d", len(lines))
	}
	if !strings.Contains(plain(lines[0]), "reverse-i-search") {
		t.Errorf("search prompt should own the status row; got %q", plain(lines[0]))
	}
	if m.statusRows() != 1 {
		t.Errorf("statusRows() = %d during search, want 1", m.statusRows())
	}
}

func TestStatusRowsAgreesWithStatusLines(t *testing.T) {
	// The layout path must reserve exactly what the renderer produces across
	// widths and content mixes.
	for _, w := range []int{24, 40, 60, 80, 120, 200} {
		m := layoutModel(w, 40)
		m.app = nil // dot-only path
		if got, want := m.statusRows(), len(m.statusLines()); got != want {
			t.Errorf("w=%d nil-app: statusRows()=%d, len(statusLines())=%d", w, got, want)
		}
	}
}

// TestLeadingAnsiCodes checks the helper used by wrapAnsiLine.
func TestLeadingAnsiCodes(t *testing.T) {
	cases := []struct{ input, want string }{
		{"\x1b[2mtext", "\x1b[2m"},                 // dim
		{"\x1b[2m\x1b[33mtext", "\x1b[2m\x1b[33m"}, // stacked
		{"\x1b[0mtext", ""},                        // reset: nothing to carry
		{"plain text", ""},                         // no ANSI
		{"", ""},
	}
	for _, c := range cases {
		if got := leadingAnsiCodes(c.input); got != c.want {
			t.Errorf("leadingAnsiCodes(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

// TestWrapAnsiLinePreservesStyle verifies that a dim-wrapped long line keeps
// the dim style on the continuation segment (P23 bug fix).
func TestWrapAnsiLinePreservesStyle(t *testing.T) {
	// Construct a dim line that will wrap at width 20.
	longText := "· run_shell → this is a very long tool result line"
	dimLine := "\x1b[2m" + longText + "\x1b[0m"

	wrapped := wrapAnsiLine(dimLine, 20)
	lines := strings.Split(wrapped, "\n")
	if len(lines) < 2 {
		t.Fatal("expected wrapping to produce ≥2 lines")
	}
	// Each continuation line must start with the dim code.
	for i, l := range lines[1:] {
		if !strings.HasPrefix(l, "\x1b[2m") {
			t.Errorf("continuation line %d lost dim style: %q", i+1, l)
		}
	}
	// First segment must end with a reset so it is self-contained.
	if !strings.HasSuffix(lines[0], "\x1b[0m") {
		t.Errorf("first segment should close the dim code: %q", lines[0])
	}
}
