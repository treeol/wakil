package policy

import (
	"os"
	"path/filepath"
	"testing"
)

// policy_extra_test.go — tests for the gaps in policy/load.go: LoadFile (0%)
// and the regexCache.match error path (75%).

// ── LoadFile ───────────────────────────────────────────────────────────────

func TestLoadFile_ValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	data := `{
		"name": "file-policy",
		"default": "ask",
		"rules": [
			{"name": "deny-rm", "tool": "run_shell", "command_regex": "^rm ", "decision": "deny"}
		]
	}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile failed: %v", err)
	}
	if p.Name != "file-policy" {
		t.Errorf("name = %q, want file-policy", p.Name)
	}
	if len(p.Rules) != 1 {
		t.Errorf("expected 1 rule, got %d", len(p.Rules))
	}
}

func TestLoadFile_NonExistentFile(t *testing.T) {
	_, err := LoadFile("/nonexistent/path/policy.json")
	if err == nil {
		t.Error("expected error for non-existent file")
	}
}

func TestLoadFile_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte(`{invalid`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadFile(path)
	if err == nil {
		t.Error("expected error for invalid JSON in file")
	}
}

// ── regexCache.match error path ───────────────────────────────────────────

func TestRegexCache_MatchInvalidPattern(t *testing.T) {
	_, err := regexCache.match("[invalid(", "test input")
	if err == nil {
		t.Error("expected error for invalid regex pattern")
	}
}

func TestRegexCache_MatchValidPattern(t *testing.T) {
	matched, err := regexCache.match(`^go test\b`, "go test ./...")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !matched {
		t.Error("expected match for 'go test' pattern")
	}
}

func TestRegexCache_CompileCachesPattern(t *testing.T) {
	// Compile the same pattern twice — second call should hit cache.
	re1, err := regexCache.compile(`^test\d+$`)
	if err != nil {
		t.Fatalf("first compile failed: %v", err)
	}
	re2, err := regexCache.compile(`^test\d+$`)
	if err != nil {
		t.Fatalf("second compile failed: %v", err)
	}
	if re1 != re2 {
		t.Error("expected same *regexp.Regexp pointer from cache on second compile")
	}
}

// ── Load with validation failure ───────────────────────────────────────────

func TestLoad_ValidationFailure(t *testing.T) {
	// Valid JSON but invalid policy (bad default decision).
	data := []byte(`{"name": "bad", "default": "maybe", "rules": []}`)
	_, err := Load(data)
	if err == nil {
		t.Error("expected error for invalid default decision")
	}
}

func TestLoad_EmptyRules(t *testing.T) {
	data := []byte(`{"name": "empty", "default": "allow", "rules": []}`)
	p, err := Load(data)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	if len(p.Rules) != 0 {
		t.Errorf("expected 0 rules, got %d", len(p.Rules))
	}
	// With no rules, Evaluate should return the default.
	result := p.Evaluate(EvalInput{ToolName: "run_shell"})
	if result.Decision != Allow {
		t.Errorf("expected allow (default), got %s", result.Decision)
	}
}

// ── Profile: auto-destructive ──────────────────────────────────────────────

func TestProfiles_AutoDestructive(t *testing.T) {
	p := Profile("auto-destructive")
	if p == nil {
		t.Fatal("expected non-nil profile")
	}
	// Everything allowed except external backends.
	if p.Evaluate(EvalInput{ToolName: "write_file"}).Decision != Allow {
		t.Error("auto-destructive profile should allow writes")
	}
	if p.Evaluate(EvalInput{ToolName: "run_shell", Destructive: true}).Decision != Allow {
		t.Error("auto-destructive profile should allow destructive (no destructive rule)")
	}
	if p.Evaluate(EvalInput{ToolName: "external_backend", ExternalBackend: true}).Decision != Deny {
		t.Error("auto-destructive profile should deny external backends")
	}
}
