// Package p07 uses the shared helper.
package p07

import "frefactor/shared"

// Total returns the shared sum of a, b and 7.
func Total(a, b int) int { return shared.NewSum(a, b, 7) }
