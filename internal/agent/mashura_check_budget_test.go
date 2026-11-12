package agent

import "testing"

// Check reasoning default is low (no max_tokens) and answer floor is 4096:
// mashuraReasoningFor returns Effort low with MaxTokens 0 for check, and
// dispatch floor 4096 applies (mashuraMinAnswerTokens returns 0).
func TestMashuraCheckReasoningLowAndFloor4096(t *testing.T) {
	app, _ := mashuraTestApp(t, "f", "p", "s")
	rs := app.mashuraReasoningFor("mashura__check")
	if rs == nil {
		t.Fatal("mashuraReasoningFor(check) = nil, want low")
	}
	if rs.Effort != "low" {
		t.Errorf("check reasoning effort = %q, want low", rs.Effort)
	}
	if rs.MaxTokens != 0 {
		t.Errorf("check reasoning max_tokens = %d, want 0 (derived from effort)", rs.MaxTokens)
	}
	for _, n := range []string{"mashura__check", "mashura__review"} {
		if got := app.mashuraMinAnswerTokens(n); got != 0 {
			t.Errorf("mashuraMinAnswerTokens(%s) = %d, want 0 (dispatch 4096 floor)", n, got)
		}
	}
	// MaxTokens cap for check remains 1024.
	if got := app.mashuraMaxTokensFor("mashura__check"); got != 1024 {
		t.Errorf("mashuraMaxTokensFor(check) = %d, want 1024 cap", got)
	}
}
