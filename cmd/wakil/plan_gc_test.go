package main

import "testing"

func TestClassifyPlanEntry(t *testing.T) {
	live := map[string]bool{"live-chat": true}
	exists := func(id string) bool { return live[id] }
	cases := []struct {
		name, key, session string
		want               planGCVerdict
	}{
		{"dead plan/current", "plan/current", "gone", planGCDead},
		{"live plan/current", "plan/current", "live-chat", planGCAlive},
		{"no owner is kept, never deleted", "plan/current", "", planGCKeep},
		{"dead tainted plan keyed by chat id", "plan/tainted/gone/current", "other", planGCDead},
		{"live tainted plan", "plan/tainted/live-chat/current", "", planGCAlive},
		{"tainted key owner beats entry session", "plan/tainted/live-chat/current", "gone", planGCAlive},
		{"unrelated key skipped", "plan/other", "gone", planGCSkip},
		{"lookalike prefix skipped", "plan/currentfoo", "gone", planGCSkip},
		{"malformed tainted key skipped", "plan/tainted/x", "gone", planGCSkip},
		{"extra segment skipped", "plan/tainted/gone/current/x", "gone", planGCSkip},
	}
	for _, c := range cases {
		if got := classifyPlanEntry(c.key, c.session, exists); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
