package id

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/treeol/wakil/internal/core/event"
)

// TestGeneratedIDsAreValidPrefixedUUIDv7 checks the two invariants that
// matter at this layer: the prefix grammar (via event.*ID.Validate) and a
// real UUIDv7 body.
func TestGeneratedIDsAreValidPrefixedUUIDv7(t *testing.T) {
	g := New()

	sid, err := g.SessionID()
	if err != nil {
		t.Fatalf("SessionID: %v", err)
	}
	checkBody(t, "ses_", string(sid))

	tid, err := g.TenantID()
	if err != nil {
		t.Fatalf("TenantID: %v", err)
	}
	checkBody(t, "tnt_", string(tid))

	uid, err := g.UserID()
	if err != nil {
		t.Fatalf("UserID: %v", err)
	}
	checkBody(t, "usr_", string(uid))

	wid, err := g.WorkspaceID()
	if err != nil {
		t.Fatalf("WorkspaceID: %v", err)
	}
	checkBody(t, "wsp_", string(wid))

	trn, err := g.TurnID()
	if err != nil {
		t.Fatalf("TurnID: %v", err)
	}
	checkBody(t, "trn_", string(trn))

	tcl, err := g.ToolCallID()
	if err != nil {
		t.Fatalf("ToolCallID: %v", err)
	}
	checkBody(t, "tcl_", string(tcl))

	apr, err := g.ApprovalID()
	if err != nil {
		t.Fatalf("ApprovalID: %v", err)
	}
	checkBody(t, "apr_", string(apr))

	sub, err := g.SubagentID()
	if err != nil {
		t.Fatalf("SubagentID: %v", err)
	}
	checkBody(t, "sub_", string(sub))
}

// checkBody validates the prefix grammar and parses the body as a UUIDv7.
func checkBody(t *testing.T, prefix, raw string) {
	t.Helper()
	if !strings.HasPrefix(raw, prefix) {
		t.Fatalf("%q: missing prefix %q", raw, prefix)
	}
	body := strings.TrimPrefix(raw, prefix)
	u, err := uuid.Parse(body)
	if err != nil {
		t.Fatalf("%q: body is not a UUID: %v", raw, err)
	}
	if u.Version() != 7 {
		t.Fatalf("%q: body version is %d, want 7", raw, u.Version())
	}
}

func TestGeneratedIDsAreUnique(t *testing.T) {
	g := New()
	seen := map[event.SessionID]bool{}
	for i := 0; i < 1000; i++ {
		id, err := g.SessionID()
		if err != nil {
			t.Fatalf("SessionID: %v", err)
		}
		if seen[id] {
			t.Fatalf("duplicate session id %q", id)
		}
		seen[id] = true
	}
}

func TestPackageLevelHelpersUseDefaultGenerator(t *testing.T) {
	id, err := NewSessionID()
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	checkBody(t, "ses_", string(id))
}

// TestNilGeneratorFails makes the nil-safety path concrete.
func TestNilGeneratorFails(t *testing.T) {
	var g *Generator
	if _, err := g.SessionID(); err == nil {
		t.Fatal("nil generator should return an error")
	}
}
