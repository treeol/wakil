package wiring

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/memory"
)

func stateSHA(t *testing.T, p string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

// C3 via the CLI: dry-run prints counts and writes nothing; --apply copies the
// attributable entry and leaves the legacy store byte-identical; usage errors.
func TestStateMigrateCLI(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("WAKIL_SESSIONS_DIR", t.TempDir())
	t.Setenv("WAKIL_WORKSPACE_KEY", "")
	proj := t.TempDir()
	legacyWS := t.TempDir()
	t.Setenv("WAKIL_WORKSPACE_PATH", legacyWS)
	// A test-only config file: the user's real config is never read.
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgFile, []byte(`{"base_url":"http://127.0.0.1:1","sandbox":{"backend":"direct"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAKIL_CONFIG", cfgFile)

	// Legacy store at the key the CLI derives from WAKIL_WORKSPACE_PATH.
	cfg := config.DefaultConfig()
	cfg.ExecMode = "direct"
	cfg.WorkDir = proj
	legacyDB := agent.LegacyMemoryDBPath(cfg)
	ctx := context.Background()
	lw, err := memory.Open(legacyDB, legacyWS)
	if err != nil {
		t.Fatal(err)
	}
	// Anchored under the known project (the launch dir), so attributable.
	if _, err := lw.PutActive(ctx, "cli/note", "copy me", "note", "durable", "t", "s", 0, nil, []string{filepath.Join(proj, "f.go")}, ""); err != nil {
		t.Fatal(err)
	}
	lw.Close()
	before := stateSHA(t, legacyDB)

	oldwd, _ := os.Getwd()
	if err := os.Chdir(proj); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldwd) })

	var out, errw bytes.Buffer
	if code := runStateMigrate([]string{"--from-legacy"}, &out, &errw); code != 0 {
		t.Fatalf("dry-run exit %d: %s", code, errw.String())
	}
	if !strings.Contains(out.String(), "dry-run") || !strings.Contains(out.String(), "1 to copy") {
		t.Fatalf("dry-run output:\n%s", out.String())
	}
	if _, err := os.Stat(agent.MemoryDBPath(proj)); !os.IsNotExist(err) {
		t.Fatal("dry-run wrote a project store")
	}

	out.Reset()
	if code := runStateMigrate([]string{"--from-legacy", "--apply"}, &out, &errw); code != 0 {
		t.Fatalf("apply exit %d: %s", code, errw.String())
	}
	if !strings.Contains(out.String(), "copied 1 entries") {
		t.Fatalf("apply output:\n%s", out.String())
	}
	ps, err := memory.Open(agent.MemoryDBPath(proj), proj)
	if err != nil {
		t.Fatal(err)
	}
	if e, err := ps.Get(ctx, "cli/note"); err != nil || e.Value != "copy me" {
		t.Fatalf("not copied: %v", err)
	}
	ps.Close()
	if stateSHA(t, legacyDB) != before {
		t.Fatal("apply modified the legacy store")
	}

	if code := runStateMigrate(nil, &out, &errw); code != 2 {
		t.Fatalf("missing --from-legacy must exit 2, got %d", code)
	}
	if code := runStateMigrate([]string{"--from-legacy", "--bogus"}, &out, &errw); code != 2 {
		t.Fatalf("unknown flag must exit 2, got %d", code)
	}
}
