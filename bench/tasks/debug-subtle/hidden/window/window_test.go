package window

import (
	"reflect"
	"testing"
)

func check(t *testing.T, name string, got, want []int) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s = %v, want %v", name, got, want)
	}
}

// Hidden cases through the public function: empty, single, full,
// over-full and negative sizes.
func TestTakeCases(t *testing.T) {
	check(t, "Take(nil, 0)", Take(nil, 0), []int{})
	check(t, "Take(nil, 3)", Take(nil, 3), []int{})
	check(t, "Take([7], 1)", Take([]int{7}, 1), []int{7})
	check(t, "Take([1 2 3], 3)", Take([]int{1, 2, 3}, 3), []int{1, 2, 3})
	check(t, "Take([1 2 3 4], 2)", Take([]int{1, 2, 3, 4}, 2), []int{1, 2})
	check(t, "Take([1 2], 5)", Take([]int{1, 2}, 5), []int{1, 2})
	check(t, "Take([1 2 3], -1)", Take([]int{1, 2, 3}, -1), []int{})
	check(t, "Take([1 2 3], 0)", Take([]int{1, 2, 3}, 0), []int{})
}

// Hidden cases through the helper directly, so a caller-side workaround
// that patches Take without fixing the helper still fails.
func TestHelperCases(t *testing.T) {
	check(t, "windowTake([4 5 6], 3)", windowTake([]int{4, 5, 6}, 3), []int{4, 5, 6})
	check(t, "windowTake([4 5 6], 2)", windowTake([]int{4, 5, 6}, 2), []int{4, 5})
	check(t, "windowTake([4], 1)", windowTake([]int{4}, 1), []int{4})
	check(t, "windowTake(nil, 0)", windowTake(nil, 0), []int{})
	check(t, "windowTake([1 2], 9)", windowTake([]int{1, 2}, 9), []int{1, 2})
}
