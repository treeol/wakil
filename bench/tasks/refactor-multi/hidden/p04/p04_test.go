package p04

import "testing"

func TestTotalHidden(t *testing.T) {
	for _, c := range [][3]int{{2, 3, 5 + 4}, {-1, 1, 0 + 4}, {40, 2, 42 + 4}, {0, 0, 0 + 4}} {
		if got := Total(c[0], c[1]); got != c[2] {
			t.Fatalf("Total(%d, %d) = %d, want %d", c[0], c[1], got, c[2])
		}
	}
}
