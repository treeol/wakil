package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestPickSplashLogo(t *testing.T) {
	cases := []struct {
		name      string
		w, h      int
		truecolor bool
		wantCols  int // 0 = fallback
	}{
		{"large pane", 120, 50, true, 64},
		{"width limited", 40, 50, true, 40},
		{"height limited", 120, 20, true, 40}, // 40 cols -> 20 braille rows; 41 would need 21
		{"smallest", 24, 20, true, 24},
		{"too narrow", 23, 50, true, 0},
		{"too short", 120, 11, true, 0},
		{"no truecolor", 120, 50, false, 0},
	}
	for _, c := range cases {
		got := pickSplashLogo(c.w, c.h, c.truecolor)
		if c.wantCols == 0 {
			if got != "" {
				t.Errorf("%s: want fallback, got logo", c.name)
			}
			continue
		}
		if got == "" {
			t.Errorf("%s: got fallback, want %d cols", c.name, c.wantCols)
			continue
		}
		lines := strings.Split(got, "\n")
		if len(lines) > c.h {
			t.Errorf("%s: rows = %d, exceeds pane height %d", c.name, len(lines), c.h)
		}
		for i, l := range lines {
			if w := ansi.StringWidth(l); w != c.wantCols {
				t.Errorf("%s: row %d width = %d, want %d", c.name, i, w, c.wantCols)
				break
			}
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
