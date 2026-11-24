// Package window takes a window of elements from a slice.
package window

// Take returns up to n elements of s. A negative n returns an empty slice.
func Take(s []int, n int) []int {
	if n < 0 {
		return []int{}
	}
	return windowTake(s, n)
}

// windowTake returns the first n elements of s.
func windowTake(s []int, n int) []int {
	k := n
	if len(s) < k {
		k = len(s)
	}
	out := make([]int, 0, k)
	for i := 0; i < k; i++ {
		out = append(out, s[i])
	}
	return out
}
