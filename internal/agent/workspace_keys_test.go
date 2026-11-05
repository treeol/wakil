package agent

// M1 / D3 (0.4.4): the workspace identity report. It only reports; the values
// must equal what the stores actually use (no derivation change).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
)

// directCfg is a direct-mode config rooted at dir.
func directCfg(dir string) config.Config {
	c := config.DefaultConfig()
	c.ExecMode = "direct"
	c.WorkDir = dir
	return c
}

// Frozen golden keys from the 0.4.4 cleanup plan: /mnt/wakil.
func TestResolveWorkspaceKeys_GoldenAndMatchesStores(t *testing.T) {
	t.Setenv("WAKIL_WORKSPACE_KEY", "")
	t.Setenv("WAKIL_WORKSPACE_PATH", "")
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("WAKIL_REPO_STATE_DIR", "")

	k := ResolveWorkspaceKeys(directCfg("/mnt/wakil"))
	if k.ChatPath != "/mnt/wakil" {
		t.Fatalf("ChatPath = %q", k.ChatPath)
	}
	if k.MemoryKey != "f3fa3243c6fc07e5" {
		t.Errorf("memory key = %q, want the frozen f3fa3243c6fc07e5", k.MemoryKey)
	}
	if want := "f3fa3243c6fc07e588a42f82ee6ea59ba4d2c27cb028afbbbba79a0283a11ffb"; k.FullKey != want {
		t.Errorf("full key = %q, want %q", k.FullKey, want)
	}
	// The reported paths ARE the paths the stores use — not a parallel computation.
	if k.MemoryDBPath != MemoryDBPath("/mnt/wakil") || !strings.HasPrefix(k.MemoryDBPath, data) {
		t.Errorf("memory db = %q, store uses %q", k.MemoryDBPath, MemoryDBPath("/mnt/wakil"))
	}
	if k.SessionHostDBPath != SessionHostDBPath("/mnt/wakil") {
		t.Errorf("sessionhost db = %q, store uses %q", k.SessionHostDBPath, SessionHostDBPath("/mnt/wakil"))
	}
	if k.SessionHostID != "wsp_f3fa3243c6fc07e5" || k.SessionHostKey != "f3fa3243c6fc07e5" {
		t.Errorf("sessionhost id/key = %q / %q", k.SessionHostID, k.SessionHostKey)
	}
	if k.ChatPathSource == SourceEnvPath {
		t.Errorf("source = %q without WAKIL_WORKSPACE_PATH", k.ChatPathSource)
	}
	// The report must not create anything.
	if entries, _ := os.ReadDir(data); len(entries) != 0 {
		t.Errorf("--print-workspace-key must not create files, found %d entries under the data dir", len(entries))
	}
}

// With WAKIL_WORKSPACE_PATH unset and the chat started in the cwd, chat and
// plan gc agree (the 0.4.3 guarantee) — also through a symlinked cwd.
func TestResolveWorkspaceKeys_CwdAgreesWithGC_Symlink(t *testing.T) {
	t.Setenv("WAKIL_WORKSPACE_KEY", "")
	t.Setenv("WAKIL_WORKSPACE_PATH", "")
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	old, _ := os.Getwd()
	if err := os.Chdir(link); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	k := ResolveWorkspaceKeys(directCfg(link))
	if k.Diverges {
		t.Errorf("chat and plan gc must agree for the same cwd: chat %s gc %s", k.MemoryKey, k.GCMemoryKey)
	}
	if k.ChatPathSource != SourceCwd {
		t.Errorf("source = %q, want %q", k.ChatPathSource, SourceCwd)
	}
	if got, _ := filepath.EvalSymlinks(real); k.GCPath != got {
		t.Errorf("GCPath = %q, want canonical %q", k.GCPath, got)
	}
	if k.EnvOverrideLine() != "" {
		t.Errorf("no startup line without WAKIL_WORKSPACE_PATH, got %q", k.EnvOverrideLine())
	}
}

// D3: WAKIL_WORKSPACE_PATH set → source says so, the divergence from plan gc is
// reported, and the one startup line names the path, the keys and the override.
func TestResolveWorkspaceKeys_EnvPathOverrides(t *testing.T) {
	t.Setenv("WAKIL_WORKSPACE_KEY", "")
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	old, _ := os.Getwd()
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	t.Setenv("WAKIL_WORKSPACE_PATH", "/mnt/wakil")

	k := ResolveWorkspaceKeys(directCfg(cwd))
	if k.ChatPath != "/mnt/wakil" || k.ChatPathSource != SourceEnvPath {
		t.Fatalf("path/source = %q / %q", k.ChatPath, k.ChatPathSource)
	}
	if k.MemoryKey != "f3fa3243c6fc07e5" {
		t.Errorf("memory key = %q", k.MemoryKey)
	}
	if !k.Diverges || k.GCMemoryKey == k.MemoryKey || k.GCPath != cwd {
		t.Errorf("must report divergence from plan gc (gc %q key %q, diverges=%v)", k.GCPath, k.GCMemoryKey, k.Diverges)
	}
	line := k.EnvOverrideLine()
	for _, want := range []string{"WAKIL_WORKSPACE_PATH=/mnt/wakil", "overrides the working directory", "memory key f3fa3243c6fc07e5", "sessionhost key f3fa3243c6fc07e5", "plan gc"} {
		if !strings.Contains(line, want) {
			t.Errorf("startup line lacks %q: %s", want, line)
		}
	}
	if strings.Count(line, "\n") != 0 {
		t.Errorf("startup line must be one line: %q", line)
	}
	out := k.Format()
	for _, want := range []string{"[source: WAKIL_WORKSPACE_PATH]", "[source: cwd]", "WARNING:", "memory store key:", "sessionhost id:"} {
		if !strings.Contains(out, want) {
			t.Errorf("Format lacks %q:\n%s", want, out)
		}
	}
}

func TestResolveWorkspaceKeys_KeyOverrideAndEmpty(t *testing.T) {
	t.Setenv("WAKIL_WORKSPACE_PATH", "")
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("WAKIL_WORKSPACE_KEY", strings.Repeat("b", 64))
	k := ResolveWorkspaceKeys(directCfg("/mnt/wakil"))
	if !k.KeyOverride || k.MemoryKey != strings.Repeat("b", 16) {
		t.Errorf("override: %+v", k)
	}
	if !strings.Contains(k.Format(), "WAKIL_WORKSPACE_KEY (verbatim override)") {
		t.Errorf("Format must name the key override:\n%s", k.Format())
	}

	t.Setenv("WAKIL_WORKSPACE_KEY", "")
	k = ResolveWorkspaceKeys(directCfg("")) // direct mode, no workdir: what a chat sees too
	if k.ChatPath != "" || k.MemoryKey != "" {
		t.Errorf("empty workspace must yield no keys: %+v", k)
	}
	if !strings.Contains(k.Format(), "NOTE: no workspace path") {
		t.Errorf("empty case must be explained:\n%s", k.Format())
	}
}

// /sandbox shows the WAKIL_WORKSPACE_PATH line (D3).
func TestSandboxCommandShowsWorkspaceOverride(t *testing.T) {
	t.Setenv("WAKIL_WORKSPACE_KEY", "")
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg = directCfg(t.TempDir())

	t.Setenv("WAKIL_WORKSPACE_PATH", "")
	if got := app.renderSandboxState(); strings.Contains(got, "WAKIL_WORKSPACE_PATH") {
		t.Errorf("no override line expected without the variable: %s", got)
	}
	t.Setenv("WAKIL_WORKSPACE_PATH", "/mnt/wakil")
	got := app.renderSandboxState()
	if !strings.Contains(got, "WAKIL_WORKSPACE_PATH=/mnt/wakil") || !strings.Contains(got, "overrides the working directory") {
		t.Errorf("/sandbox must show the override: %s", got)
	}
}
