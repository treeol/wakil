package behaviour

import (
	"testing"

	"frename/alpha"
	"frename/beta"
	"frename/delta"
	"frename/epsilon"
	"frename/gamma"
	"frename/shared"
	"frename/zeta"
)

// The rename must not change what the function does.
func TestBehaviour(t *testing.T) {
	for _, c := range [][3]int{{2, 3, 5}, {-1, 1, 0}, {40, 2, 42}} {
		if got := shared.NewName(c[0], c[1]); got != c[2] {
			t.Fatalf("shared.NewName(%d, %d) = %d, want %d", c[0], c[1], got, c[2])
		}
		for name, total := range map[string]func(a, b int) int{
			"alpha": alpha.Total, "beta": beta.Total, "gamma": gamma.Total,
			"delta": delta.Total, "epsilon": epsilon.Total, "zeta": zeta.Total,
		} {
			if got := total(c[0], c[1]); got != c[2] {
				t.Fatalf("%s.Total(%d, %d) = %d, want %d", name, c[0], c[1], got, c[2])
			}
		}
	}
}
