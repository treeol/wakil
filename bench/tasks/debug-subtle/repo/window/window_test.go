package window

import (
	"reflect"
	"testing"
)

// The symptom: a slice exactly as long as the window loses its last
// element.
func TestTakeExactSize(t *testing.T) {
	got := Take([]int{1, 2, 3}, 3)
	want := []int{1, 2, 3}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Take([1 2 3], 3) = %v, want %v", got, want)
	}
}
