package agent

// grants.go: session-scoped tool approval grants.
//
// A grant auto-approves a specific read-only built-in tool for the rest of
// the session without prompting. Grants are session-scoped (not persisted)
// and stored as an immutable *grantsSnapshot in atomic.Value for concurrent
// safety.
//
// Security model: grant-eligible tools are the deterministic, built-in
// read-only tools plus the per-session write/shell tools below. Deletes,
// moves, and external-backend egress are never grant-eligible and always
// prompt. A grant for run_shell/run_background never covers a command that
// IsDestructiveShell classifies as destructive — those still prompt (see
// GrantEligibleForCall). Grants never bypass hard safety gates (SuspendAuto
// carve-outs for destructive/external remain enforced on every path).
//
// The grantEligibleTools map mirrors sessionclient.grantEligibleTools — the
// TUI uses the sessionclient copy (it cannot import agent). Parity is
// enforced by TestGrantEligibilityParity in grants_test.go.

// grantEligibleTools is the set of tools that can be granted.
// MUST match sessionclient.grantEligibleTools (parity test enforced).
var grantEligibleTools = map[string]bool{
	"read_file":      true,
	"read_file_full": true,
	"search_files":   true,
	"find_files":     true,
	"list_dir":       true,
	"lsp_definition": true,
	"lsp_references": true,
	"lsp_hover":      true,
	"lsp_symbols":    true,
	// Write and shell tools grantable per-session. run_shell and
	// run_background are gated per call by GrantEligibleForCall (destructive
	// commands are never covered).
	"write_file":        true,
	"write_binary_file": true,
	"edit_file":         true,
	"run_shell":         true,
	"run_background":    true,
}

// IsGrantEligible reports whether a tool name is eligible for session grants.
// For run_shell and run_background this is necessary but not sufficient —
// use GrantEligibleForCall to also exclude destructive commands.
func IsGrantEligible(toolName string) bool {
	return grantEligibleTools[toolName]
}

// GrantEligibleForCall reports whether this specific call may be covered by
// a session grant. It uses the same classification that suspends /auto
// (IsDestructiveShell via ShellCmdFromDetail), not a second one: a grant for
// run_shell or run_background never covers a destructive command. All other
// grant-eligible tools are covered unconditionally.
func GrantEligibleForCall(toolName, detail string) bool {
	if !IsGrantEligible(toolName) {
		return false
	}
	switch toolName {
	case "run_shell", "run_background":
		return !IsDestructiveShell(ShellCmdFromDetail(detail))
	default:
		return true
	}
}

// Grant represents a session-scoped approval for a specific tool.
type Grant struct {
	Tool string // the tool name this grant covers
}

// grantsSnapshot is the immutable snapshot stored in atomic.Value as a pointer
// (atomic.Value.CompareAndSwap requires comparable values, and a struct
// containing a slice is not comparable — so we store *grantsSnapshot).
type grantsSnapshot struct {
	grants []Grant
}

// Grants returns a defensive copy of the current session grants.
// Safe from any goroutine.
func (a *App) Grants() []Grant {
	v := a.grants.Load()
	if v == nil {
		return nil
	}
	s, ok := v.(*grantsSnapshot)
	if !ok || s == nil {
		return nil
	}
	return append([]Grant(nil), s.grants...)
}

// AddGrant adds a session-scoped grant for the given tool. The tool must be
// grant-eligible (IsGrantEligible); ineligible tools are silently rejected.
// Copy-on-write: loads the current snapshot pointer, builds a new one,
// CAS-retries on concurrent modification.
func (a *App) AddGrant(tool string) {
	if !IsGrantEligible(tool) {
		return
	}
	for {
		raw := a.grants.Load()
		var oldGrants []Grant
		if raw != nil {
			old := raw.(*grantsSnapshot)
			if old != nil {
				oldGrants = old.grants
			}
		}
		// Check if already granted (dedup).
		for _, g := range oldGrants {
			if g.Tool == tool {
				return // already granted
			}
		}
		next := &grantsSnapshot{
			grants: append(append([]Grant(nil), oldGrants...), Grant{Tool: tool}),
		}
		if raw == nil {
			if a.grants.CompareAndSwap(nil, next) {
				return
			}
			continue // another goroutine stored first — retry
		}
		if a.grants.CompareAndSwap(raw, next) {
			return
		}
		// CAS failed — retry with reloaded snapshot.
	}
}

// HasGrant reports whether a session-scoped grant covers tool. Safe from
// any goroutine.
func (a *App) HasGrant(tool string) bool {
	for _, g := range a.Grants() {
		if g.Tool == tool {
			return true
		}
	}
	return false
}

// ClearGrants removes all session-scoped grants.
func (a *App) ClearGrants() {
	a.grants.Store(&grantsSnapshot{})
}
