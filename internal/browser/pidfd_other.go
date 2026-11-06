//go:build !linux

package browser

import (
	"errors"
	"syscall"
)

// No pidfd off Linux: ownership falls back to the recorded start time and the
// marker check, and the signal goes through os.Process.
func pidfdOpen(int) int { return -1 }

func pidfdSignal(int, syscall.Signal) error { return errors.New("pidfd not supported") }

func pidfdClose(int) {}
