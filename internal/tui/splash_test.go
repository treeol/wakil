package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// The logo has ONE size: 44 columns x 20 rows, whatever the pane offers.
func TestPickSplashLogo(t *testing.T) {
	const wantCols, wantRows = 44, 20
	if splashCols != wantCols || splashRows != wantRows {
		t.Fatalf("logo size = %dx%d, want %dx%d", splashCols, splashRows, wantCols, wantRows)
	}
	cases := []struct {
		name      string
		w, h      int
		truecolor bool
		want      bool // logo drawn (else fallback)
	}{
		{"large pane", 200, 80, true, true},
		{"exact fit", 44, 20, true, true},
		{"too narrow by one", 43, 50, true, false},
		{"too short by one", 120, 19, true, false},
		{"no truecolor", 120, 50, false, false},
	}
	for _, c := range cases {
		got := pickSplashLogo(c.w, c.h, c.truecolor)
		if !c.want {
			if got != "" {
				t.Errorf("%s: want fallback, got logo", c.name)
			}
			continue
		}
		lines := strings.Split(got, "\n")
		if len(lines) != wantRows {
			t.Errorf("%s: rows = %d, want %d", c.name, len(lines), wantRows)
		}
		for i, l := range lines {
			if w := ansi.StringWidth(l); w != wantCols {
				t.Errorf("%s: row %d width = %d, want %d", c.name, i, w, wantCols)
				break
			}
		}
	}
}

// A pane too narrow for the logo shows the name as plain text, not a figlet
// wordmark that would not fit either.
func TestRenderSplashNarrowShowsPlainName(t *testing.T) {
	forceTruecolor(t)
	out := ansi.Strip(renderSplash(30, 30))
	if !strings.Contains(out, "wakīl") {
		t.Fatalf("narrow splash lacks the plain name:\n%s", out)
	}
	if strings.Contains(out, "_|_|_|") || strings.ContainsRune(out, '\u28ff') {
		t.Fatalf("narrow splash drew art:\n%s", out)
	}
	for _, l := range strings.Split(out, "\n") {
		if w := ansi.StringWidth(l); w > 30 {
			t.Fatalf("line wider than the pane (%d > 30): %q", w, l)
		}
	}
}

// The logo is transparent around the artwork: the corner cells must be blank
// with no background colour, so the terminal's own background shows through.
func TestSplashLogoTransparentCorners(t *testing.T) {
	got := pickSplashLogo(120, 50, true)
	if strings.Contains(got, "\x1b[48;2;") && !strings.Contains(got, "▀") {
		t.Error("unexpected background colour without half blocks")
	}
	lines := strings.Split(got, "\n")
	for _, i := range []int{0, len(lines) - 1} {
		plain := ansi.Strip(lines[i])
		if !strings.HasPrefix(plain, " ") || !strings.HasSuffix(plain, " ") {
			t.Errorf("row %d corners are drawn, want transparent: %q", i, plain)
		}
	}
	if !strings.Contains(got, "\x1b[38;2;") {
		t.Error("no truecolor foreground in output: logo is blank")
	}
}

func TestRenderSplashFallbackHasWordmark(t *testing.T) {
	// Test profile is not truecolor, so this exercises the figlet fallback.
	out := ansi.Strip(renderSplash(80, 30))
	if !strings.Contains(out, "_|_|_|") {
		t.Fatalf("splash missing figlet wordmark fallback:\n%s", out)
	}
}
