package tui

import (
	"sync"

	"github.com/treeol/wakil/internal/timeline"
)

var timelineOnceMu sync.Mutex
var timelineSeen = map[string]bool{}

// timelineOnce records the instant name the first time key is seen in this
// process (for example the first frame), and never again.
func timelineOnce(key, name string) {
	timelineOnceMu.Lock()
	if timelineSeen[key] {
		timelineOnceMu.Unlock()
		return
	}
	timelineSeen[key] = true
	timelineOnceMu.Unlock()
	timeline.Event(name)
}
