package wiring

import (
	"context"
	"time"

	"github.com/treeol/wakil/internal/exec"
)

// SandboxBanner is the one-line statement of the running sandbox:
// "sandbox: backend=hisn · risk=<label>". It is empty for executors that do
// not report a status (docker and direct state their backend in the config
// banner printed before the executor exists).
func SandboxBanner(exe exec.Executor) string {
	s, ok := exe.(exec.SandboxStatuser)
	if !ok {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return s.SandboxStatus(ctx).Banner()
}
