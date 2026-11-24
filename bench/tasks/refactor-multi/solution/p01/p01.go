// Package p01 uses the shared helper.
package p01

import "frefactor/shared"

// Total returns the shared sum of a, b and 1.
func Total(a, b int) int { return shared.NewSum(a, b, 1) }
