package tui

import (
	"sync"

	"github.com/treeol/wakil/internal/timeline"
)

var timelineOnceMu sync.Mutex
var timelineSeen = map[string]bool{}

// timelineOnce records the instant name the first time key is seen in this
// process (for example the first frame), and never again. It also captures the
// perf start-time marks (first_frame / prompt_ready) the same way, so the
// WAKIL_TUI_PERF summary can report both from process start.
func timelineOnce(key, name string) {
	timelineOnceMu.Lock()
	if timelineSeen[key] {
		timelineOnceMu.Unlock()
		return
	}
	timelineSeen[key] = true
	timelineOnceMu.Unlock()
	timeline.Event(name)
	// Perf start-time marks are captured at the same instants the timeline
	// records, so the summary cannot drift: "first-frame" (the real model's
	// first window size — prompt ready) and "first-frame-boot" (the start-up
	// view's first frame).
	switch key {
	case "first-frame":
		perfMarkPromptReady()
	case "first-frame-boot":
		perfMarkFirstFrame()
	}
}
