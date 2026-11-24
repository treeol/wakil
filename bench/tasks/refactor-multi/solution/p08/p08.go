// Package p08 uses the shared helper.
package p08

import "frefactor/shared"

// Total returns the shared sum of a, b and 8.
func Total(a, b int) int { return shared.NewSum(a, b, 8) }
