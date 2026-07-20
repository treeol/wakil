package policy

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sync"
)

// ── Regex cache ────────────────────────────────────────────────────────────

// regexCache caches compiled regex patterns by source string. Patterns are
// compiled once on first use and reused. Go's regexp package uses RE2, so
// there is no catastrophic backtracking risk from user-supplied patterns.
type regexCacheType struct {
	mu    sync.RWMutex
	cache map[string]*regexp.Regexp
}

var regexCache = &regexCacheType{cache: make(map[string]*regexp.Regexp)}

func (rc *regexCacheType) match(pattern, input string) (bool, error) {
	re, err := rc.compile(pattern)
	if err != nil {
		return false, err
	}
	return re.MatchString(input), nil
}

func (rc *regexCacheType) compile(pattern string) (*regexp.Regexp, error) {
	rc.mu.RLock()
	re, ok := rc.cache[pattern]
	rc.mu.RUnlock()
	if ok {
		return re, nil
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	// Double-check after acquiring write lock.
	if re, ok := rc.cache[pattern]; ok {
		return re, nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid regex %q: %w", pattern, err)
	}
	rc.cache[pattern] = re
	return re, nil
}

// ── Loading ────────────────────────────────────────────────────────────────

// LoadFile reads a policy from a JSON file path.
func LoadFile(path string) (*Policy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading policy file %s: %w", path, err)
	}
	return Load(data)
}

// Load parses a policy from JSON bytes.
func Load(data []byte) (*Policy, error) {
	var p Policy
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parsing policy JSON: %w", err)
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// Validate checks that the policy is well-formed: the Default decision is
// valid, every rule has a valid Decision, and all regex patterns compile.
func (p *Policy) Validate() error {
	if p.Default != string(Allow) && p.Default != string(Ask) && p.Default != string(Deny) {
		return fmt.Errorf("policy default must be \"allow\", \"ask\", or \"deny\", got %q", p.Default)
	}
	for i, rule := range p.Rules {
		if rule.Decision != string(Allow) && rule.Decision != string(Ask) && rule.Decision != string(Deny) {
			return fmt.Errorf("rule %d (%s): decision must be \"allow\", \"ask\", or \"deny\", got %q",
				i, rule.Name, rule.Decision)
		}
		if rule.CommandRegex != "" {
			if _, err := regexCache.compile(rule.CommandRegex); err != nil {
				return fmt.Errorf("rule %d (%s): invalid command_regex: %w", i, rule.Name, err)
			}
		}
	}
	return nil
}

// ── Built-in profiles ──────────────────────────────────────────────────────

// Profile returns a named built-in profile, or nil if the name is unknown.
// Built-in profiles are data (not code): they use the same JSON-shaped Policy
// as user-supplied policy files.
func Profile(name string) *Policy {
	switch name {
	case "read-only":
		return &Policy{
			Name:    "read-only",
			Default: string(Ask),
			Rules: []Rule{
				{Name: "deny-destructive", Destructive: boolPtr(true), Decision: string(Deny), Reason: "destructive actions blocked in read-only profile"},
				{Name: "allow-reads", ReadAction: boolPtr(true), Decision: string(Allow), Reason: "read-only tools auto-approved"},
				{Name: "allow-readonly-shell", Tool: "run_shell", Destructive: boolPtr(false), CommandRegex: `^(ls|cat|grep|rg|find|git status|git log|git diff|head|tail|wc|jq)\b`, Decision: string(Allow), Reason: "read-only shell commands"},
			},
		}
	case "auto-safe":
		return &Policy{
			Name:    "auto-safe",
			Default: string(Allow),
			Rules: []Rule{
				{Name: "ask-destructive", Destructive: boolPtr(true), Decision: string(Ask), Reason: "destructive actions require confirmation"},
				{Name: "deny-external", ExternalBackend: boolPtr(true), Decision: string(Deny), Reason: "external backends blocked in auto-safe profile"},
			},
		}
	case "auto-destructive":
		return &Policy{
			Name:    "auto-destructive",
			Default: string(Allow),
			Rules: []Rule{
				{Name: "deny-external", ExternalBackend: boolPtr(true), Decision: string(Deny), Reason: "external backends always require explicit approval"},
			},
		}
	case "ci":
		return &Policy{
			Name:    "ci",
			Default: string(Allow),
			Rules: []Rule{
				{Name: "deny-external", ExternalBackend: boolPtr(true), Decision: string(Deny), Reason: "external backends blocked in CI profile"},
			},
		}
	}
	return nil
}

// ProfileNames returns the names of all built-in profiles.
func ProfileNames() []string {
	return []string{"read-only", "auto-safe", "auto-destructive", "ci"}
}

func boolPtr(v bool) *bool { return &v }
