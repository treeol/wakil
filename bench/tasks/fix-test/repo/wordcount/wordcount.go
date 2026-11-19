// Package wordcount counts how often each word occurs in text.
package wordcount

import "strings"

// Count returns a frequency map of the words in s. Words are split on
// whitespace; the map keys are the words as they appear.
func Count(s string) map[string]int {
	counts := map[string]int{}
	for _, w := range strings.Fields(s) {
		counts[w]++
	}
	return counts
}
