package project

import (
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/sandbox/hisn"
)

// B2: the approval summary lists the project's rootfs and home overrides, so
// the user sees them before approving.
func TestSummaryListsRootfsAndHome(t *testing.T) {
	l := Loaded{Surface: hisn.Surface{Rootfs: "wakil-dev", Home: hisn.HomeTmpfs}}
	got := strings.Join(Summary(l), "\n")
	if !strings.Contains(got, `rootfs "wakil-dev"`) {
		t.Errorf("summary misses the rootfs: %q", got)
	}
	if !strings.Contains(got, `"tmpfs"`) {
		t.Errorf("summary misses the home override: %q", got)
	}
}
