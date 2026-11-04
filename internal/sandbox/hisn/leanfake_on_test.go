//go:build leanfake

package hisn

// leanBuild: this test binary is the lean one (no agent/TUI dependencies), so
// it can serve as the fake hisn itself. See fakeBinary.
const leanBuild = true
