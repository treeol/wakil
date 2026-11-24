package p03

import "testing"

func TestTotalHidden(t *testing.T) {
	for _, c := range [][3]int{{2, 3, 5 + 3}, {-1, 1, 0 + 3}, {40, 2, 42 + 3}, {0, 0, 0 + 3}} {
		if got := Total(c[0], c[1]); got != c[2] {
			t.Fatalf("Total(%d, %d) = %d, want %d", c[0], c[1], got, c[2])
		}
	}
}
