package event

import (
	"fmt"
	"strings"
)

// Typed, prefixed identifiers (foundation doc §4.1). The prefix makes a
// mis-routed identifier visibly wrong in logs and prevents the "WHERE id =
// <uuid of another table>" bug. The body is a UUIDv7 in production
// (time-sortable); generation lives in internal/core/id (chunk 3). This package
// types the strings and validates them so the domain vocabulary is fixed before
// any store exists.
//
// P0 grammar (deliberately loose — see checkID): prefix + non-empty body.
// Production bodies are UUIDv7; "local" is the only documented non-UUID body
// (the embedded principal). Strict UUIDv7 enforcement lands at generation
// (internal/core/id) and at wire/SQL ingress in P2/P1, not here — this package
// must remain usable with test IDs like ses_test. Constructors are validators,
// not generators.

// TenantID identifies a tenant. Prefix "tnt_".
type TenantID string

// UserID identifies a user. Prefix "usr_".
type UserID string

// WorkspaceID identifies a workspace (project root / working dir). Prefix "wsp_".
type WorkspaceID string

// SessionID identifies a session. Prefix "ses_". This is the durable domain
// identity; the proxy ChatID is an external backend correlation field (D4), not
// the domain ID.
type SessionID string

// TurnID identifies a turn. Prefix "trn_".
type TurnID string

// ToolCallID identifies a tool call. Prefix "tcl_".
type ToolCallID string

// ApprovalID identifies an approval request. Prefix "apr_".
type ApprovalID string

// SubagentID identifies a subagent dispatch. Prefix "sub_".
type SubagentID string

// idSpec is the validation contract for one ID type.
type idSpec struct {
	prefix string
	label  string
}

// idSpecs maps each ID kind to its required prefix. Keyed by a closed set of
// constant strings — never by caller-supplied input, so an unknown key is a
// programmer error and checkID panics (loud) rather than silently validating
// against an empty prefix.
var idSpecs = map[string]idSpec{
	"tenant":    {prefix: "tnt_", label: "tenant"},
	"user":      {prefix: "usr_", label: "user"},
	"workspace": {prefix: "wsp_", label: "workspace"},
	"session":   {prefix: "ses_", label: "session"},
	"turn":      {prefix: "trn_", label: "turn"},
	"toolcall":  {prefix: "tcl_", label: "tool call"},
	"approval":  {prefix: "apr_", label: "approval"},
	"subagent":  {prefix: "sub_", label: "subagent"},
}

// checkID validates that raw carries the required prefix for the given kind and
// has a non-empty body after it. It is the one choke point every New*ID
// constructor and every ID.Validate method funnels through.
//
// An unknown kind key is a programming error (the keys are literals internal to
// this package), so it panics instead of silently passing — the previous
// behavior where a typo produced a zero spec and strings.HasPrefix(raw, "")
// always matched.
func checkID(kind, raw string) error {
	spec, ok := idSpecs[kind]
	if !ok {
		panic(fmt.Sprintf("event: unknown id kind %q (programmer error)", kind))
	}
	if !strings.HasPrefix(raw, spec.prefix) {
		return fmt.Errorf("invalid %s id %q: missing %q prefix", spec.label, raw, spec.prefix)
	}
	if len(raw) == len(spec.prefix) {
		return fmt.Errorf("invalid %s id %q: empty body after prefix", spec.label, raw)
	}
	return nil
}

// Validate checks this ID's prefix and body. Called by Event.Validate on the
// envelope and by payload Validate methods on nested IDs, so a mis-routed ID is
// rejected at the boundary rather than persisted.
func (id TenantID) Validate() error    { return checkID("tenant", string(id)) }
func (id UserID) Validate() error      { return checkID("user", string(id)) }
func (id WorkspaceID) Validate() error { return checkID("workspace", string(id)) }
func (id SessionID) Validate() error   { return checkID("session", string(id)) }
func (id TurnID) Validate() error      { return checkID("turn", string(id)) }
func (id ToolCallID) Validate() error  { return checkID("toolcall", string(id)) }
func (id ApprovalID) Validate() error  { return checkID("approval", string(id)) }
func (id SubagentID) Validate() error  { return checkID("subagent", string(id)) }

// NewTenantID validates a tenant ID string. Returns (TenantID, error).
func NewTenantID(raw string) (TenantID, error) {
	if err := checkID("tenant", raw); err != nil {
		return "", err
	}
	return TenantID(raw), nil
}

// NewUserID validates a user ID string.
func NewUserID(raw string) (UserID, error) {
	if err := checkID("user", raw); err != nil {
		return "", err
	}
	return UserID(raw), nil
}

// NewWorkspaceID validates a workspace ID string.
func NewWorkspaceID(raw string) (WorkspaceID, error) {
	if err := checkID("workspace", raw); err != nil {
		return "", err
	}
	return WorkspaceID(raw), nil
}

// NewSessionID validates a session ID string.
func NewSessionID(raw string) (SessionID, error) {
	if err := checkID("session", raw); err != nil {
		return "", err
	}
	return SessionID(raw), nil
}

// NewTurnID validates a turn ID string.
func NewTurnID(raw string) (TurnID, error) {
	if err := checkID("turn", raw); err != nil {
		return "", err
	}
	return TurnID(raw), nil
}

// NewToolCallID validates a tool-call ID string.
func NewToolCallID(raw string) (ToolCallID, error) {
	if err := checkID("toolcall", raw); err != nil {
		return "", err
	}
	return ToolCallID(raw), nil
}

// NewApprovalID validates an approval ID string.
func NewApprovalID(raw string) (ApprovalID, error) {
	if err := checkID("approval", raw); err != nil {
		return "", err
	}
	return ApprovalID(raw), nil
}

// NewSubagentID validates a subagent ID string.
func NewSubagentID(raw string) (SubagentID, error) {
	if err := checkID("subagent", raw); err != nil {
		return "", err
	}
	return SubagentID(raw), nil
}

// EmbeddedTenantID is the constant tenant for embedded/local single-user mode
// (D4): the core never sees an anonymous call, so even the single-user path
// carries a real principal. Auth resolution replaces this in P4; the constant
// remains valid for embedded mode forever.
const EmbeddedTenantID TenantID = "tnt_local"

// EmbeddedUserID is the constant user for embedded/local single-user mode (D4).
const EmbeddedUserID UserID = "usr_local"
