// Package p05 uses the shared helper.
package p05

import "frefactor/shared"

// Total returns the shared sum of a, b and 5.
func Total(a, b int) int { return shared.OldAdd(a, b) }
