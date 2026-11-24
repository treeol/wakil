package p02

import "testing"

// Visible test: new behaviour (K=2).
func TestTotalVisible(t *testing.T) {
	if got := Total(2, 3); got != 5+2 {
		t.Fatalf("Total(2, 3) = %d, want %d", got, 5+2)
	}
}
