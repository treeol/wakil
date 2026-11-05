package tui

import "testing"

func TestFinishedStatusLabel(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"ok", ""},
		{"incomplete", ""},
		{"ok files 3/3", "✓ finished · files 3/3"},
		{"partial files 7/9", "◐ partial · files 7/9"},
		{"partial", "◐ partial"},
		{"rejected files 0/3", "✗ rejected · files 0/3"},
		{"refused", "✗ refused"},
		{"incomplete files 1/4", "incomplete · files 1/4"},
	}
	for _, c := range cases {
		if got := finishedStatusLabel(c.in); got != c.want {
			t.Errorf("finishedStatusLabel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
