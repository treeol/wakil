package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/memory"
)

// D4: the notice appears once per project. It names the legacy count and the
// migrate command, and the flag file makes the second call silent.
func TestLegacyNoticeOncePerProject(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("WAKIL_WORKSPACE_KEY", "")
	ctx := context.Background()
	proj := t.TempDir()
	legacyWS := t.TempDir()
	t.Setenv("WAKIL_WORKSPACE_PATH", legacyWS)
	cfg := config.DefaultConfig()
	cfg.ExecMode = "direct"
	cfg.WorkDir = proj

	db := LegacyMemoryDBPath(cfg)
	lw, err := memory.Open(db, legacyWS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lw.PutActive(ctx, "old/one", "v", "note", "durable", "t", "s", 0, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	lw.Close()

	first := LegacyNotice(cfg)
	if !strings.Contains(first, "1 legacy entries readable") || !strings.Contains(first, "wakil state migrate --from-legacy") {
		t.Fatalf("first-start notice: %q", first)
	}
	if second := LegacyNotice(cfg); second != "" {
		t.Fatalf("notice must print once per project, second call: %q", second)
	}
	flag := filepath.Join(filepath.Dir(MemoryDBPath(ProjectDir(cfg))), legacyNoticeFlag)
	if _, err := os.Stat(flag); err != nil {
		t.Fatal("flag file not written")
	}
}

// D4: no notice without a legacy store, and none when the legacy path is the
// project itself (nothing legacy to point at).
func TestLegacyNoticeSilentWithoutLegacy(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("WAKIL_WORKSPACE_KEY", "")
	proj := t.TempDir()
	t.Setenv("WAKIL_WORKSPACE_PATH", "")
	cfg := config.DefaultConfig()
	cfg.ExecMode = "direct"
	cfg.WorkDir = proj
	if line := LegacyNotice(cfg); line != "" {
		t.Fatalf("no legacy store: notice %q", line)
	}
	// Legacy path equals the project: same store, nothing to migrate from.
	t.Setenv("WAKIL_WORKSPACE_PATH", proj)
	if line := LegacyNotice(cfg); line != "" {
		t.Fatalf("legacy == project: notice %q", line)
	}
}

// C4: the report names the legacy key and its entry count (read-only).
func TestWorkspaceKeysReportShowsLegacy(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("WAKIL_WORKSPACE_KEY", "")
	ctx := context.Background()
	proj := t.TempDir()
	legacyWS := t.TempDir()
	t.Setenv("WAKIL_WORKSPACE_PATH", legacyWS)
	cfg := config.DefaultConfig()
	cfg.ExecMode = "direct"
	cfg.WorkDir = proj

	// No legacy store yet: the report says none, and creates nothing.
	out := ResolveWorkspaceKeys(cfg).WithLegacy(cfg).Format()
	if !strings.Contains(out, "none on this host") {
		t.Fatalf("absent legacy store must say so:\n%s", out)
	}
	if _, err := os.Stat(LegacyMemoryDBPath(cfg)); !os.IsNotExist(err) {
		t.Fatal("report created the legacy store")
	}

	lw, err := memory.Open(LegacyMemoryDBPath(cfg), legacyWS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lw.PutActive(ctx, "a/b", "v", "note", "durable", "t", "s", 0, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	lw.Close()
	out = ResolveWorkspaceKeys(cfg).WithLegacy(cfg).Format()
	if !strings.Contains(out, "1 active entries") || !strings.Contains(out, "legacy store:") {
		t.Fatalf("report must show the legacy count:\n%s", out)
	}
}

// 0.5.0 review: the legacy key is the old WorkspacePath() derivation, independent
// of the project. When it equals the project's own key, the report says none; it
// never shows the project store as legacy.
func TestLegacyKeyIsOldDerivationAndNoneWhenEqual(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("WAKIL_WORKSPACE_KEY", "")
	proj := t.TempDir()
	legacyWS := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.ExecMode = "direct"
	cfg.WorkDir = proj

	// Old derivation: WAKIL_WORKSPACE_PATH wins; it is not the project.
	t.Setenv("WAKIL_WORKSPACE_PATH", legacyWS)
	k := ResolveWorkspaceKeys(cfg).WithLegacy(cfg)
	if want := WorkspaceKey(legacyWS)[:16]; k.LegacyKey != want {
		t.Fatalf("legacy key %s, want old derivation %s", k.LegacyKey, want)
	}
	if k.LegacyKey == k.MemoryKey {
		t.Fatal("legacy must differ from the project key here")
	}

	// Legacy workspace equal to the project (the chat's cwd): report none, not a
	// legacy store. The project is the cwd now, so chdir there.
	oldwd, _ := os.Getwd()
	if err := os.Chdir(proj); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	t.Setenv("WAKIL_WORKSPACE_PATH", proj)
	k = ResolveWorkspaceKeys(cfg).WithLegacy(cfg)
	out := k.Format()
	if !strings.Contains(out, "legacy store:       none") {
		t.Fatalf("equal legacy/project must report none:\n%s", out)
	}
	if strings.Contains(out, "read-only:") {
		t.Fatalf("project store must not be reported as legacy:\n%s", out)
	}
}

// 0.5.0 review: the WARNING about plan gc divergence is gone (keys agree by
// construction); the WAKIL_WORKSPACE_PATH line remains.
func TestNoDivergenceWarningAndEnvLineKept(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("WAKIL_WORKSPACE_KEY", "")
	t.Setenv("WAKIL_WORKSPACE_PATH", t.TempDir())
	cfg := config.DefaultConfig()
	cfg.ExecMode = "direct"
	cfg.WorkDir = t.TempDir()
	k := ResolveWorkspaceKeys(cfg)
	if strings.Contains(k.Format(), "WARNING") {
		t.Fatal("divergence WARNING must not be printed")
	}
	if !strings.Contains(k.EnvOverrideLine(), "IGNORED") {
		t.Fatalf("env override notice missing: %q", k.EnvOverrideLine())
	}
}

// Host finding (0.5.0 review): an explicit --workdir is labelled as the flag
// (not as config host_workdir), and plan gc's project is that same directory.
func TestWorkDirFlagSourceAndGCAgree(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("WAKIL_WORKSPACE_KEY", "")
	proj := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.ExecMode = "docker"
	cfg.HostWorkDir = t.TempDir() // a stale host_workdir must not win
	cfg.WorkDir = proj
	cfg.WorkDirFlag = true

	k := ResolveWorkspaceKeys(cfg)
	if k.ChatPathSource != SourceWorkDirFlag {
		t.Fatalf("source = %q, want %q", k.ChatPathSource, SourceWorkDirFlag)
	}
	if k.ChatPath != canonicalWorkspace(proj) {
		t.Fatalf("chat path %q, want the flag dir", k.ChatPath)
	}
	if k.GCPath != canonicalWorkspace(proj) || k.GCMemoryKey != k.MemoryKey {
		t.Fatalf("plan gc must resolve the flag dir: gc=%q key=%s chat=%s", k.GCPath, k.GCMemoryKey, k.MemoryKey)
	}
	if !strings.Contains(k.Format(), "plan gc from here:  "+canonicalWorkspace(proj)+"   [source: --workdir flag]") {
		t.Fatalf("report must show the flag as the gc source:\n%s", k.Format())
	}
}
