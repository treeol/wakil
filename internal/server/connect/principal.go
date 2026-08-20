package connect

import (
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
)

// localPrincipal returns the fixed server-side principal for P2 local mode.
// The principal is NEVER read from client-supplied request data (doc §6.2).
// In P2 the Unix socket (0600) is the security boundary; P4 adds SO_PEERCRED.
func localPrincipal() core.Principal {
	return core.EmbeddedPrincipal()
}

// Ensure event is used (EmbeddedTenantID comes from event.EmbeddedTenantID via
// EmbeddedPrincipal which references it internally).
var _ event.TenantID = event.EmbeddedTenantID
