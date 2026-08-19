// Package id generates the typed, prefixed domain identifiers defined in
// internal/core/event (foundation doc §4.1).
//
// The event package types the ID strings and validates their prefix grammar,
// so the domain vocabulary is fixed before any store exists. This package
// generates production IDs with time-sortable UUIDv7 bodies, split out so the
// event package stays usable with cheap test IDs like "ses_test" while every
// production ID is a real UUIDv7.
//
// Bodies use the canonical dashed UUIDv7 form (e.g. "0194d6e0-8f6c-7b3a-9c4d-
// 1a2b3c4d5e6f"). The first field carries the 48-bit millisecond timestamp, so
// string ordering of IDs is roughly time-ordering and B-tree indexes on the
// TEXT primary keys stay compact (doc §4.1).
package id

import (
	"crypto/rand"
	"fmt"
	"io"

	"github.com/google/uuid"

	"github.com/treeol/wakil/internal/core/event"
)

// Generator produces prefixed UUIDv7 domain IDs. A Generator from New is safe
// for concurrent use (crypto/rand.Reader is, and the uuid library serializes
// timestamp assignment internally). A Generator from NewFromReader is only as
// concurrency-safe as the reader it is given.
type Generator struct {
	r io.Reader
}

// New returns a Generator backed by crypto/rand.Reader.
func New() *Generator { return &Generator{r: rand.Reader} }

// NewFromReader returns a Generator using r as its entropy source. Intended
// for deterministic tests; production code should use New.
func NewFromReader(r io.Reader) *Generator { return &Generator{r: r} }

// raw builds a "<prefix><uuidv7>" string from the generator's entropy source.
func (g *Generator) raw(prefix string) (string, error) {
	if g == nil || g.r == nil {
		return "", fmt.Errorf("id: nil generator or entropy source")
	}
	u, err := uuid.NewV7FromReader(g.r)
	if err != nil {
		return "", fmt.Errorf("id: generate uuidv7: %w", err)
	}
	return prefix + u.String(), nil
}

// TenantID returns a fresh "tnt_<uuidv7>" tenant id.
func (g *Generator) TenantID() (event.TenantID, error) {
	raw, err := g.raw("tnt_")
	if err != nil {
		return "", err
	}
	return event.NewTenantID(raw)
}

// UserID returns a fresh "usr_<uuidv7>" user id.
func (g *Generator) UserID() (event.UserID, error) {
	raw, err := g.raw("usr_")
	if err != nil {
		return "", err
	}
	return event.NewUserID(raw)
}

// WorkspaceID returns a fresh "wsp_<uuidv7>" workspace id.
func (g *Generator) WorkspaceID() (event.WorkspaceID, error) {
	raw, err := g.raw("wsp_")
	if err != nil {
		return "", err
	}
	return event.NewWorkspaceID(raw)
}

// SessionID returns a fresh "ses_<uuidv7>" session id.
func (g *Generator) SessionID() (event.SessionID, error) {
	raw, err := g.raw("ses_")
	if err != nil {
		return "", err
	}
	return event.NewSessionID(raw)
}

// TurnID returns a fresh "trn_<uuidv7>" turn id.
func (g *Generator) TurnID() (event.TurnID, error) {
	raw, err := g.raw("trn_")
	if err != nil {
		return "", err
	}
	return event.NewTurnID(raw)
}

// ToolCallID returns a fresh "tcl_<uuidv7>" tool-call id.
func (g *Generator) ToolCallID() (event.ToolCallID, error) {
	raw, err := g.raw("tcl_")
	if err != nil {
		return "", err
	}
	return event.NewToolCallID(raw)
}

// ApprovalID returns a fresh "apr_<uuidv7>" approval id.
func (g *Generator) ApprovalID() (event.ApprovalID, error) {
	raw, err := g.raw("apr_")
	if err != nil {
		return "", err
	}
	return event.NewApprovalID(raw)
}

// SubagentID returns a fresh "sub_<uuidv7>" subagent id.
func (g *Generator) SubagentID() (event.SubagentID, error) {
	raw, err := g.raw("sub_")
	if err != nil {
		return "", err
	}
	return event.NewSubagentID(raw)
}

// defaultGenerator backs the package-level New*ID helpers with crypto/rand.
var defaultGenerator = New()

// NewTenantID returns a fresh prefixed UUIDv7 tenant id (see Generator.TenantID).
func NewTenantID() (event.TenantID, error) { return defaultGenerator.TenantID() }

// NewUserID returns a fresh prefixed UUIDv7 user id.
func NewUserID() (event.UserID, error) { return defaultGenerator.UserID() }

// NewWorkspaceID returns a fresh prefixed UUIDv7 workspace id.
func NewWorkspaceID() (event.WorkspaceID, error) { return defaultGenerator.WorkspaceID() }

// NewSessionID returns a fresh prefixed UUIDv7 session id.
func NewSessionID() (event.SessionID, error) { return defaultGenerator.SessionID() }

// NewTurnID returns a fresh prefixed UUIDv7 turn id.
func NewTurnID() (event.TurnID, error) { return defaultGenerator.TurnID() }

// NewToolCallID returns a fresh prefixed UUIDv7 tool-call id.
func NewToolCallID() (event.ToolCallID, error) { return defaultGenerator.ToolCallID() }

// NewApprovalID returns a fresh prefixed UUIDv7 approval id.
func NewApprovalID() (event.ApprovalID, error) { return defaultGenerator.ApprovalID() }

// NewSubagentID returns a fresh prefixed UUIDv7 subagent id.
func NewSubagentID() (event.SubagentID, error) { return defaultGenerator.SubagentID() }
