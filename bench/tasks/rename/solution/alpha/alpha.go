// Package alpha uses the shared helper.
package alpha

import "frename/shared"

// Total returns the shared sum of a and b.
func Total(a, b int) int { return shared.NewName(a, b) }
