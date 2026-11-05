package main

// D1 (0.4.4): the daemon, the remote TUI and the web console say so on start,
// once, and nothing else about their behaviour changes.

import (
	"strings"
	"testing"
)

func TestDeprecatedDaemonLineNamesEveryFeature(t *testing.T) {
	for _, want := range []string{"daemon", "--daemon", "web console", "deprecated", "will be removed in a future release"} {
		if !strings.Contains(deprecatedDaemonLine, want) {
			t.Errorf("deprecation line lacks %q: %s", want, deprecatedDaemonLine)
		}
	}
	if strings.Contains(deprecatedDaemonLine, "\n") {
		t.Error("the deprecation line must be one line")
	}
}
