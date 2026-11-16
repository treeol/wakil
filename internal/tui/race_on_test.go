//go:build race

package tui

// raceEnabled reports whether the test binary was built with the race
// detector, which slows the TUI's string-width and wrap work by roughly 9x.
const raceEnabled = true
