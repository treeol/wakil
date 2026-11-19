// Package beta uses the shared helper.
package beta

import "frename/shared"

// Total returns the shared sum of a and b.
func Total(a, b int) int { return shared.OldName(a, b) }
