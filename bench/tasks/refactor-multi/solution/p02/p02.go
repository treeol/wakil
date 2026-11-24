// Package p02 uses the shared helper.
package p02

import "frefactor/shared"

// Total returns the shared sum of a, b and 2.
func Total(a, b int) int { return shared.NewSum(a, b, 2) }
