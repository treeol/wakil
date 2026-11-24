// Package p06 uses the shared helper.
package p06

import "frefactor/shared"

// Total returns the shared sum of a, b and 6.
func Total(a, b int) int { return shared.OldAdd(a, b) }
