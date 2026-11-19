// Package wordcount counts how often each word occurs in text.
package wordcount

import "strings"

// Count returns a frequency map of the words in s. Words are split on
// whitespace and counted case-insensitively: the map keys are lower-case.
func Count(s string) map[string]int {
	counts := map[string]int{}
	for _, w := range strings.Fields(strings.ToLower(s)) {
		counts[w]++
	}
	return counts
}
