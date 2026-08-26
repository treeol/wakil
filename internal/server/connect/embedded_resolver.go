package connect

import (
	"context"

	"github.com/treeol/wakil/internal/core"
)

// embeddedResolver is a test-only resolver that always returns the embedded
// principal. It bypasses peer-credential resolution and is fail-open.
//
// It lives in this non-test file because test files in cmd/wakil (package
// main) need to reference it, and Go's internal package visibility rules
// prevent _test.go files from being imported by other packages. The comment
// and naming convention make the test-only intent explicit.
//
// Production code MUST NOT use this resolver. The daemon's newDaemonServer
// always uses auth.LocalResolver (fail-closed).
type embeddedResolver struct{}

// NewEmbeddedResolver creates a principal resolver for tests that always
// returns the embedded principal, bypassing peer-credential resolution.
//
// Test-only. Never use in production daemon code.
func NewEmbeddedResolver() principalResolver {
	return embeddedResolver{}
}

func (embeddedResolver) Resolve(ctx context.Context) (core.Principal, error) {
	return core.EmbeddedPrincipal(), nil
}
