package wiring

import (
	"context"
	"fmt"
	"time"

	"github.com/treeol/wakil/internal/exec"
	"github.com/treeol/wakil/internal/sandbox/hisn"
)

// plannedKVR is the socket of an in-sandbox staging store the executor plans
// to run ("" = none; docker's store is reported by KVRSocketPath instead).
func plannedKVR(exe exec.Executor) string {
	if p, ok := exe.(exec.KVRPlanner); ok {
		return p.PlannedKVRSocket()
	}
	return ""
}

// inSandboxKVRGuard is the staging client's path guard for an in-sandbox
// store: before each connect it makes sure the session (and with it the
// service) is running, requires the service to have answered, and vets the
// socket. Any failure refuses the dial with the reason.
func inSandboxKVRGuard(exe exec.Executor, planned string) func() error {
	return func() error {
		if en, ok := exe.(exec.SessionEnsurer); ok {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			if err := en.EnsureSession(ctx); err != nil {
				return fmt.Errorf("sandbox session: %w", err)
			}
		}
		if exe.KVRSocketPath() != planned {
			return fmt.Errorf("the staging service is not running in this sandbox session (see /sandbox)")
		}
		return hisn.GuardKVRSocket(exe.WorkspaceRoot(), planned)
	}
}
