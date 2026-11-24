package p04

import "testing"

// Visible test: old behaviour.
func TestTotalVisible(t *testing.T) {
	if got := Total(2, 3); got != 5 {
		t.Fatalf("Total(2, 3) = %d, want 5", got)
	}
}
