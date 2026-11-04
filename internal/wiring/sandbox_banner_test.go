package wiring

import (
	"testing"

	"github.com/treeol/wakil/internal/exec"
	"github.com/treeol/wakil/internal/sandbox/fake"
)

func TestSandboxBanner(t *testing.T) {
	be := exec.NewBackendExecutor(fake.New("/ws", "/ws"), exec.BackendOptions{Label: "hisn"})
	if got, want := SandboxBanner(be), "sandbox: backend=hisn · risk=none"; got != want {
		t.Errorf("banner = %q, want %q", got, want)
	}
	d, err := exec.NewDirectExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got := SandboxBanner(d); got != "" {
		t.Errorf("an executor with no status adds no banner (the config banner already names it): %q", got)
	}
}
