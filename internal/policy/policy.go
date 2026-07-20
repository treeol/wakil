// Package policy implements a declarative consent policy engine for wakil's
// confirmation gate. It evaluates tool-call context against an ordered list of
// rules and returns one of three decisions: allow, ask, or deny.
//
// The evaluator is pure: it takes a structured EvalInput and returns a Result.
// It does not import internal/agent — the agent package builds EvalInput from
// the Confirmer args and derivable data (destructive classification, shell
// command extraction).
//
// Policy storage on App uses atomic.Value (same pattern as ConsentSnapshot)
// so that the policy can be swapped safely if a future /profile TUI command
// or mid-session switch is added. The MVP ships --policy/--profile flags for
// headless mode; TUI /profile is a follow-up.
package policy

// Decision is the outcome of a policy evaluation.
type Decision string

const (
	// Allow means the tool call may proceed without a confirmation prompt.
	// In TUI mode this behaves like AutoApprove — but SuspendAuto carve-outs
	// (external_backend egress, destructive shell) still fire on top, so
	// "allow" never bypasses hard safety gates.
	Allow Decision = "allow"

	// Ask means the tool call requires human confirmation. In headless mode
	// this is treated as a decline (no human present to answer the prompt).
	Ask Decision = "ask"

	// Deny means the tool call is blocked by policy. The reason string is
	// surfaced to the model (as a tool result) and/or the user (as a
	// declined reason in the transcript).
	Deny Decision = "deny"
)

// EvalInput is the structured context passed to the policy evaluator. Only
// fields that can be reliably populated from the existing Confirmer signature
// are included. Fields that need structured input not yet available (path_glob,
// memory_tier, subagent) are deferred to future cards.
type EvalInput struct {
	ToolName        string // the tool being called (e.g. "run_shell", "write_file")
	ReadAction      bool   // true if the tool is read-only (read_file, search_files, etc.)
	Destructive     bool   // true if the call is destructive (IsDestructiveShell for shell, or delete/move)
	Command         string // extracted shell command (for run_shell/run_background); empty for non-shell
	ExternalBackend bool   // true if the tool is external_backend (egress gate)
}

// Result is the outcome of evaluating a policy against an EvalInput.
type Result struct {
	Decision Decision
	Reason   string // human-readable explanation; empty for allow
	RuleName string // name of the matching rule; empty for default
}

// Rule is one policy rule. A rule matches if ALL non-zero match fields match
// the EvalInput. The first matching rule wins; if no rule matches, the
// policy's Default decision applies.
type Rule struct {
	Name            string `json:"name,omitempty"`
	Tool            string `json:"tool,omitempty"`             // exact match on tool name
	ReadAction      *bool  `json:"read_action,omitempty"`      // match on read-only flag
	Destructive     *bool  `json:"destructive,omitempty"`      // match on destructive flag
	CommandRegex    string `json:"command_regex,omitempty"`    // regex match on shell command
	ExternalBackend *bool  `json:"external_backend,omitempty"` // match on external_backend flag
	Decision        string `json:"decision"`                   // "allow", "ask", or "deny"
	Reason          string `json:"reason,omitempty"`           // explanation for deny/ask
}

// Policy is a complete consent policy: an ordered list of rules plus a default
// decision for calls that match no rule.
type Policy struct {
	Name    string `json:"name,omitempty"`
	Default string `json:"default"` // "allow", "ask", or "deny" — used when no rule matches
	Rules   []Rule `json:"rules"`
}

// Evaluate checks the policy against the input and returns the result.
// Rules are evaluated in order; the first matching rule wins.
// If no rule matches, the Default decision is returned.
func (p *Policy) Evaluate(input EvalInput) Result {
	for _, rule := range p.Rules {
		if ruleMatches(rule, input) {
			return Result{
				Decision: Decision(rule.Decision),
				Reason:   rule.Reason,
				RuleName: rule.Name,
			}
		}
	}
	return Result{
		Decision: Decision(p.Default),
		Reason:   "no matching rule — default decision",
		RuleName: "default",
	}
}

// ruleMatches reports whether all non-zero match fields in the rule match the
// input. A field is "non-zero" if it is non-empty (strings) or non-nil (bools).
// All specified fields must match (AND semantics).
func ruleMatches(rule Rule, input EvalInput) bool {
	if rule.Tool != "" && rule.Tool != input.ToolName {
		return false
	}
	if rule.ReadAction != nil && *rule.ReadAction != input.ReadAction {
		return false
	}
	if rule.Destructive != nil && *rule.Destructive != input.Destructive {
		return false
	}
	if rule.ExternalBackend != nil && *rule.ExternalBackend != input.ExternalBackend {
		return false
	}
	if rule.CommandRegex != "" {
		if input.Command == "" {
			return false // regex specified but no command to match
		}
		matched, err := regexCache.match(rule.CommandRegex, input.Command)
		if err != nil || !matched {
			return false
		}
	}
	return true
}
