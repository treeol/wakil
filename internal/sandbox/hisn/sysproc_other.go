//go:build !linux

package hisn

import "syscall"

// hisn only runs on Linux; there is no parent-death signal elsewhere, so the
// supervisor relies on Close/Stop and orphan reconcile. (Compile-only support.)
func supervisorSysProcAttr() *syscall.SysProcAttr { return nil }
