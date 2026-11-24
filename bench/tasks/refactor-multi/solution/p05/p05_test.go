package p05

import "testing"

// Visible test: new behaviour (K=5).
func TestTotalVisible(t *testing.T) {
	if got := Total(2, 3); got != 5+5 {
		t.Fatalf("Total(2, 3) = %d, want %d", got, 5+5)
	}
}
