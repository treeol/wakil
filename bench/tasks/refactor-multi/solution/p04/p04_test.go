package p04

import "testing"

// Visible test: new behaviour (K=4).
func TestTotalVisible(t *testing.T) {
	if got := Total(2, 3); got != 5+4 {
		t.Fatalf("Total(2, 3) = %d, want %d", got, 5+4)
	}
}
