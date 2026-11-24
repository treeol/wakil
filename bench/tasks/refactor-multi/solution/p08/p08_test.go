package p08

import "testing"

// Visible test: new behaviour (K=8).
func TestTotalVisible(t *testing.T) {
	if got := Total(2, 3); got != 5+8 {
		t.Fatalf("Total(2, 3) = %d, want %d", got, 5+8)
	}
}
