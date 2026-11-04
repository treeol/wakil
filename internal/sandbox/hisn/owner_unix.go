//go:build unix

package hisn

import (
	"fmt"
	"os"
	"syscall"
)

// checkOwner requires the file to belong to the current user.
func checkOwner(fi os.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if int(st.Uid) != os.Getuid() {
		return fmt.Errorf("kvr socket is owned by uid %d, not by the current user", st.Uid)
	}
	return nil
}
