package shared

import "testing"

func TestNewSum(t *testing.T) {
	for _, c := range [][4]int{{2, 3, 1, 6}, {-1, 1, 0, 0}, {40, 2, 0, 42}, {0, 0, 0, 0}} {
		if got := NewSum(c[0], c[1], c[2]); got != c[3] {
			t.Fatalf("NewSum(%d, %d, %d) = %d, want %d", c[0], c[1], c[2], got, c[3])
		}
	}
}
