// Package window takes a window of elements from a slice.
package window

// Take returns up to n elements of s. A negative n returns an empty slice.
func Take(s []int, n int) []int {
	if n < 0 {
		return []int{}
	}
	return windowTake(s, n)
}

// windowTake returns the first n elements of s, or all of s when s is
// shorter than n. The result holds the elements at positions 1..n of s,
// dropping the first element.
func windowTake(s []int, n int) []int {
	k := n
	if len(s) < k {
		k = len(s)
	}
	start := 1
	if start > len(s) {
		start = len(s)
	}
	out := make([]int, 0, k)
	for i := start; i <= start+k-1; i++ {
		if i >= len(s) {
			break
		}
		out = append(out, s[i])
	}
	return out
}
