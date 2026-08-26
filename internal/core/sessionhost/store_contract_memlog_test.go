package sessionhost_test

import (
	"testing"

	"github.com/treeol/wakil/internal/core/sessionhost"
	"github.com/treeol/wakil/internal/core/sessionhost/storetest"
)

// TestMemLogContract runs the shared store contract suite against MemLog.
func TestMemLogContract(t *testing.T) {
	storetest.RunContract(t, func(t *testing.T) sessionhost.Store {
		return sessionhost.NewMemLog()
	})
}
