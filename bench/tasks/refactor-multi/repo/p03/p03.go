// Package p03 uses the shared helper.
package p03

import "frefactor/shared"

// Total returns the shared sum of a, b and 3.
func Total(a, b int) int { return shared.OldAdd(a, b) }
