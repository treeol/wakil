//go:build linux

package hisn

import (
	"syscall"
	"testing"
)

func TestSupervisorAttrHasPdeathsig(t *testing.T) {
	if a := supervisorSysProcAttr(); a == nil || a.Pdeathsig != syscall.SIGTERM {
		t.Fatalf("supervisor SysProcAttr = %+v, want Pdeathsig=SIGTERM", a)
	}
}
