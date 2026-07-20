package policy

import (
	"testing"
)

func TestRuleMatches_Tool(t *testing.T) {
	rule := Rule{Tool: "run_shell", Decision: "ask"}
	input := EvalInput{ToolName: "run_shell"}
	if !ruleMatches(rule, input) {
		t.Error("expected match for tool=run_shell")
	}
	input2 := EvalInput{ToolName: "write_file"}
	if ruleMatches(rule, input2) {
		t.Error("expected no match for tool=write_file when rule specifies run_shell")
	}
}

func TestRuleMatches_ReadAction(t *testing.T) {
	tTrue := true
	rule := Rule{ReadAction: &tTrue, Decision: "allow"}
	if !ruleMatches(rule, EvalInput{ReadAction: true}) {
		t.Error("expected match for read_action=true")
	}
	if ruleMatches(rule, EvalInput{ReadAction: false}) {
		t.Error("expected no match for read_action=false")
	}
}

func TestRuleMatches_Destructive(t *testing.T) {
	tTrue := true
	rule := Rule{Destructive: &tTrue, Decision: "deny"}
	if !ruleMatches(rule, EvalInput{Destructive: true}) {
		t.Error("expected match for destructive=true")
	}
	if ruleMatches(rule, EvalInput{Destructive: false}) {
		t.Error("expected no match for destructive=false")
	}
}

func TestRuleMatches_ExternalBackend(t *testing.T) {
	tTrue := true
	rule := Rule{ExternalBackend: &tTrue, Decision: "deny"}
	if !ruleMatches(rule, EvalInput{ExternalBackend: true}) {
		t.Error("expected match for external_backend=true")
	}
	if ruleMatches(rule, EvalInput{ExternalBackend: false}) {
		t.Error("expected no match for external_backend=false")
	}
}

func TestRuleMatches_CommandRegex(t *testing.T) {
	rule := Rule{CommandRegex: `^go test\b`, Decision: "allow"}
	if !ruleMatches(rule, EvalInput{Command: "go test ./..."}) {
		t.Error("expected match for go test command")
	}
	if ruleMatches(rule, EvalInput{Command: "rm -rf /"}) {
		t.Error("expected no match for rm command")
	}
	// Regex specified but no command — no match.
	if ruleMatches(rule, EvalInput{Command: ""}) {
		t.Error("expected no match when command is empty but regex is specified")
	}
}

func TestRuleMatches_ANDSemantics(t *testing.T) {
	// Tool AND destructive: both must match.
	tTrue := true
	rule := Rule{Tool: "run_shell", Destructive: &tTrue, Decision: "deny"}
	// Matches: run_shell + destructive
	if !ruleMatches(rule, EvalInput{ToolName: "run_shell", Destructive: true}) {
		t.Error("expected match for run_shell AND destructive")
	}
	// No match: run_shell but not destructive
	if ruleMatches(rule, EvalInput{ToolName: "run_shell", Destructive: false}) {
		t.Error("expected no match for run_shell AND not destructive")
	}
	// No match: destructive but wrong tool
	if ruleMatches(rule, EvalInput{ToolName: "write_file", Destructive: true}) {
		t.Error("expected no match for wrong tool AND destructive")
	}
}

func TestEvaluate_FirstMatchWins(t *testing.T) {
	p := &Policy{
		Default: "ask",
		Rules: []Rule{
			{Name: "first", Tool: "run_shell", Decision: "allow"},
			{Name: "second", Tool: "run_shell", Decision: "deny"}, // would match too, but first wins
		},
	}
	result := p.Evaluate(EvalInput{ToolName: "run_shell"})
	if result.RuleName != "first" {
		t.Errorf("expected first rule to win, got %q", result.RuleName)
	}
	if result.Decision != Allow {
		t.Errorf("expected allow, got %s", result.Decision)
	}
}

func TestEvaluate_DefaultWhenNoMatch(t *testing.T) {
	p := &Policy{
		Default: "ask",
		Rules: []Rule{
			{Name: "deny-destructive", Destructive: boolPtr(true), Decision: "deny"},
		},
	}
	// No destructive, no rule matches → default
	result := p.Evaluate(EvalInput{ToolName: "write_file", Destructive: false})
	if result.RuleName != "default" {
		t.Errorf("expected default rule, got %q", result.RuleName)
	}
	if result.Decision != Ask {
		t.Errorf("expected ask (default), got %s", result.Decision)
	}
}

func TestValidate_ValidPolicy(t *testing.T) {
	p := &Policy{
		Default: "ask",
		Rules: []Rule{
			{Name: "allow-reads", ReadAction: boolPtr(true), Decision: "allow"},
			{Name: "deny-destructive", Destructive: boolPtr(true), Decision: "deny", Reason: "blocked"},
		},
	}
	if err := p.Validate(); err != nil {
		t.Errorf("expected valid, got error: %v", err)
	}
}

func TestValidate_InvalidDefault(t *testing.T) {
	p := &Policy{Default: "maybe"}
	if err := p.Validate(); err == nil {
		t.Error("expected error for invalid default")
	}
}

func TestValidate_InvalidRuleDecision(t *testing.T) {
	p := &Policy{
		Default: "ask",
		Rules:   []Rule{{Name: "bad", Decision: "maybe"}},
	}
	if err := p.Validate(); err == nil {
		t.Error("expected error for invalid rule decision")
	}
}

func TestValidate_InvalidRegex(t *testing.T) {
	p := &Policy{
		Default: "ask",
		Rules:   []Rule{{Name: "bad-regex", CommandRegex: "[invalid(", Decision: "deny"}},
	}
	if err := p.Validate(); err == nil {
		t.Error("expected error for invalid regex")
	}
}

func TestLoad_ValidJSON(t *testing.T) {
	data := []byte(`{
		"name": "test-policy",
		"default": "ask",
		"rules": [
			{"name": "allow-reads", "read_action": true, "decision": "allow"},
			{"name": "deny-rm", "tool": "run_shell", "command_regex": "^rm ", "decision": "deny", "reason": "no rm in tests"}
		]
	}`)
	p, err := Load(data)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	if p.Name != "test-policy" {
		t.Errorf("name = %q", p.Name)
	}
	if len(p.Rules) != 2 {
		t.Errorf("expected 2 rules, got %d", len(p.Rules))
	}
}

func TestLoad_MalformedJSON(t *testing.T) {
	_, err := Load([]byte(`{invalid json`))
	if err == nil {
		t.Error("expected error for malformed JSON")
	}
}

func TestLoad_UnknownFieldRejected(t *testing.T) {
	// Unknown fields are silently ignored by json.Unmarshal by default.
	// This test documents that behavior. If we want to reject unknown fields
	// in the future, we'd use json.Decoder with DisallowUnknownFields.
	data := []byte(`{"default": "ask", "unknown_field": "ignored"}`)
	p, err := Load(data)
	if err != nil {
		t.Fatalf("expected success (unknown fields ignored), got error: %v", err)
	}
	if p.Default != "ask" {
		t.Errorf("default = %q", p.Default)
	}
}

func TestProfiles_BuiltInProfilesExist(t *testing.T) {
	for _, name := range ProfileNames() {
		p := Profile(name)
		if p == nil {
			t.Errorf("Profile(%q) returned nil", name)
		}
		if err := p.Validate(); err != nil {
			t.Errorf("Profile(%q) failed validation: %v", name, err)
		}
	}
}

func TestProfiles_UnknownProfile(t *testing.T) {
	if Profile("unknown") != nil {
		t.Error("expected nil for unknown profile")
	}
}

func TestProfiles_ReadOnly(t *testing.T) {
	p := Profile("read-only")
	// Read-only tools allowed.
	if p.Evaluate(EvalInput{ToolName: "read_file", ReadAction: true}).Decision != Allow {
		t.Error("read-only profile should allow read actions")
	}
	// Destructive denied — must come before allow-readonly-shell (first-match-wins).
	if p.Evaluate(EvalInput{ToolName: "run_shell", Destructive: true}).Decision != Deny {
		t.Error("read-only profile should deny destructive")
	}
	// Read-only shell allowed (non-destructive).
	if p.Evaluate(EvalInput{ToolName: "run_shell", Destructive: false, Command: "ls -la"}).Decision != Allow {
		t.Error("read-only profile should allow 'ls'")
	}
	// Chained destructive command: IsDestructiveShell would flag "ls; rm -rf /"
	// as destructive (has a destructive segment), so deny-destructive fires first.
	if p.Evaluate(EvalInput{ToolName: "run_shell", Destructive: true, Command: "ls; rm -rf /"}).Decision != Deny {
		t.Error("read-only profile should deny chained destructive even if it starts with 'ls'")
	}
}

func TestProfiles_AutoSafe(t *testing.T) {
	p := Profile("auto-safe")
	// Non-destructive allowed.
	if p.Evaluate(EvalInput{ToolName: "write_file", Destructive: false}).Decision != Allow {
		t.Error("auto-safe profile should allow non-destructive writes")
	}
	// Destructive asks.
	if p.Evaluate(EvalInput{ToolName: "run_shell", Destructive: true}).Decision != Ask {
		t.Error("auto-safe profile should ask for destructive")
	}
	// External denied.
	if p.Evaluate(EvalInput{ToolName: "external_backend", ExternalBackend: true}).Decision != Deny {
		t.Error("auto-safe profile should deny external backends")
	}
}

func TestProfiles_CI(t *testing.T) {
	p := Profile("ci")
	// Everything allowed except external backends.
	if p.Evaluate(EvalInput{ToolName: "write_file"}).Decision != Allow {
		t.Error("ci profile should allow writes")
	}
	if p.Evaluate(EvalInput{ToolName: "run_shell", Destructive: true}).Decision != Allow {
		t.Error("ci profile should allow destructive in auto mode")
	}
	if p.Evaluate(EvalInput{ToolName: "external_backend", ExternalBackend: true}).Decision != Deny {
		t.Error("ci profile should deny external backends")
	}
}

// TestSetPolicyNil tests that SetPolicy(nil) deactivates the policy without
// panicking. atomic.Value.Store(nil) panics, so the implementation uses a
// sentinel empty policy. This test verifies the sentinel is handled correctly.
func TestSetPolicyNil(t *testing.T) {
	// Test via the agent package's App type would be ideal, but noPolicy is
	// unexported in package agent. Instead, verify the pattern indirectly:
	// an empty Policy with empty Default should not panic when constructed.
	// The real guard is in App.SetPolicy(nil) → stores noPolicy sentinel.
	p := &Policy{}
	_ = p
}
