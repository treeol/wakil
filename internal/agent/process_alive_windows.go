//go:build windows

package agent

// processAliveImpl on Windows always returns true — Windows lacks a
// kill(pid, 0) equivalent. Fail closed to avoid deleting live worktrees.
func processAliveImpl(pid int) bool {
	return true
}
