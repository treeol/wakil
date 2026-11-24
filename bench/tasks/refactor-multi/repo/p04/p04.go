// Package p04 uses the shared helper.
package p04

import "frefactor/shared"

// Total returns the shared sum of a, b and 4.
func Total(a, b int) int { return shared.OldAdd(a, b) }
