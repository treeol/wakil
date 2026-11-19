// Package gamma uses the shared helper.
package gamma

import "frename/shared"

// Total returns the shared sum of a and b.
func Total(a, b int) int { return shared.NewName(a, b) }
