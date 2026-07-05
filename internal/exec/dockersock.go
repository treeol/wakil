package exec

import (
	"os"
	"syscall"
)

// fileGid returns the owning group id of path, or ok=false if it cannot be
// determined (path missing, or non-unix stat).
func fileGid(path string) (uint32, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Gid, true
}
