package agent

import "testing"

// M2 (0.4.4): with no saved repo-state both Apply functions still apply the
// --think flag — /new and /resume, and the daemon's restore RPCs, share them.
func TestRepoStateApplyNilStateAppliesThink(t *testing.T) {
	for name, apply := range map[string]func(*App, *RepoState) RestoreRepoStateResult{
		"new":    RestoreRepoStateApply,
		"resume": RestoreRepoStateResumeApply,
	} {
		t.Run(name, func(t *testing.T) {
			app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
			app.Cfg.ThinkExplicit, app.Cfg.ThinkEffort = true, "medium"
			res := apply(app, nil)
			if app.ReasoningEffort != "medium" || res.Note == "" {
				t.Errorf("effort=%q note=%q, want medium + a note", app.ReasoningEffort, res.Note)
			}

			app2 := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
			if res := apply(app2, nil); res != (RestoreRepoStateResult{}) || app2.ReasoningEffort != "" {
				t.Errorf("without --think a nil state must change nothing: %+v effort=%q", res, app2.ReasoningEffort)
			}

			app3 := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
			app3.Cfg.ThinkExplicit, app3.Cfg.ThinkEffort = true, "off"
			apply(app3, nil)
			if app3.ReasoningEffort != "none" {
				t.Errorf("--think off must set effort none, got %q", app3.ReasoningEffort)
			}
		})
	}
}
