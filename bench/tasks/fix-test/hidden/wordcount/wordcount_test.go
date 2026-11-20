package wordcount

import "testing"

// The bug: Count is case-sensitive, but frequency counting only makes
// sense case-insensitively ("The" and "the" are the same word).
func TestCountCaseInsensitive(t *testing.T) {
	got := Count("The the THE cat Cat")
	want := map[string]int{"the": 3, "cat": 2}
	if len(got) != len(want) {
		t.Fatalf("Count = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("Count[%q] = %d, want %d (full: %v)", k, got[k], v, got)
		}
	}
}
