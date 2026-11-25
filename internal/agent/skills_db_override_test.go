package agent

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/treeol/wakil/internal/memory"
)

// skills_db_override_test.go — tests for the WAKIL_SKILLS_DB env override
// (bench data isolation, plan §2). All tests use temp dirs only; none touch
// the user's real data dir.

// skillDBChecksum is the test helper for byte-comparing a store file.
func skillDBChecksum(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(b)
	return sum[:]
}

// unsetSkillEnv clears the env vars that influence SkillDBPath resolution.
func unsetSkillEnv(t *testing.T) {
	t.Helper()
	t.Setenv("WAKIL_SKILLS_DB", "")
	t.Setenv("WAKIL_REPO_STATE_DIR", "")
	t.Setenv("XDG_DATA_HOME", "")
}

func TestSkillDBPath_OverrideSet(t *testing.T) {
	unsetSkillEnv(t)
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "priv"), 0o700); err != nil {
		t.Fatalf("mkdir priv: %v", err)
	}
	override := filepath.Join(dir, "priv", "skills.db")
	t.Setenv("WAKIL_SKILLS_DB", override)

	got, err := SkillDBPath()
	if err != nil {
		t.Fatalf("SkillDBPath: %v", err)
	}
	if got != override {
		t.Errorf("SkillDBPath = %q, want override %q", got, override)
	}
}

func TestSkillDBPath_OverrideLeavesDefaultUntouched(t *testing.T) {
	xdgHome := t.TempDir()
	unsetSkillEnv(t)
	t.Setenv("XDG_DATA_HOME", xdgHome)

	defPath, err := SkillDBPath()
	if err != nil {
		t.Fatalf("SkillDBPath (default): %v", err)
	}
	// A fake "real" skills.db with known bytes.
	if err := os.MkdirAll(filepath.Dir(defPath), 0o700); err != nil {
		t.Fatalf("mkdir default dir: %v", err)
	}
	fake := []byte("fake-real-skills-db")
	if err := os.WriteFile(defPath, fake, 0o600); err != nil {
		t.Fatalf("write fake default: %v", err)
	}
	before := skillDBChecksum(t, defPath)

	overrideDir := t.TempDir()
	override := filepath.Join(overrideDir, "skills.db")
	t.Setenv("WAKIL_SKILLS_DB", override)

	got, err := SkillDBPath()
	if err != nil {
		t.Fatalf("SkillDBPath (override): %v", err)
	}
	if got != override {
		t.Fatalf("SkillDBPath = %q, want %q", got, override)
	}
	// Open + write a skill through the actual production path (as bootstrap
	// does: memory.Open(path, "") + NewSkillsProfile).
	store, err := memory.Open(got, "")
	if err != nil {
		t.Fatalf("memory.Open(override): %v", err)
	}
	sp := newSkillsProfile(store)
	ctx := context.Background()
	if _, err := sp.putActiveSkill(ctx, "bench-skill", "private skill", "main", "sess1", memory.TaintFalse, false, ""); err != nil {
		t.Fatalf("putActiveSkill: %v", err)
	}
	// The skill is readable back from the override store.
	skill, err := sp.getActiveSkill(ctx, "bench-skill")
	if err != nil {
		t.Fatalf("getActiveSkill: %v", err)
	}
	if skill.Value != "private skill" {
		t.Errorf("override skill value = %q, want written value", skill.Value)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close override store: %v", err)
	}

	after := skillDBChecksum(t, defPath)
	if string(before) != string(after) {
		t.Errorf("default skills.db changed while override was set")
	}
	// No SQLite sidecars appeared beside the default file either.
	defDir := filepath.Dir(defPath)
	entries, err := os.ReadDir(defDir)
	if err != nil {
		t.Fatalf("list default dir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(defPath) {
			t.Errorf("unexpected file beside default skills.db: %s", e.Name())
		}
	}
	// And the override DB really carries the skill (not the fake).
	if _, err := os.Stat(override); err != nil {
		t.Fatalf("override db stat: %v", err)
	}
}

func TestSkillDBPath_OverrideInvalid(t *testing.T) {
	base := t.TempDir()
	// A symlink inside base, and a directory used as a leaf candidate.
	linkTarget := filepath.Join(base, "target.db")
	if err := os.WriteFile(linkTarget, []byte("x"), 0o600); err != nil {
		t.Fatalf("write link target: %v", err)
	}
	link := filepath.Join(base, "link.db")
	if err := os.Symlink(linkTarget, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	dirLeaf := filepath.Join(base, "adir")
	if err := os.Mkdir(dirLeaf, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A symlinked parent (the leaf itself would be fine).
	symParent := filepath.Join(t.TempDir(), "pparent")
	if err := os.Symlink(base, symParent); err != nil {
		t.Fatalf("symlink parent: %v", err)
	}
	// A non-regular leaf (FIFO).
	fifo := filepath.Join(base, "fifo.db")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	// An unclean path that would reach a different parent than validated.
	// (Built with string concat: filepath.Join would clean it away.)
	unclean := base + "/subdir/../skills.db"

	cases := []struct {
		name  string
		value string
	}{
		{"relative", "relative/path.db"},
		{"missing-parent", filepath.Join(base, "nope", "skills.db")},
		{"symlink-leaf", link},
		{"dir-leaf", dirLeaf},
		{"symlink-parent", filepath.Join(symParent, "skills.db")},
		{"fifo-leaf", fifo},
		{"unclean", unclean},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			unsetSkillEnv(t)
			t.Setenv("WAKIL_SKILLS_DB", tc.value)
			p, err := SkillDBPath()
			if err == nil {
				t.Fatalf("SkillDBPath(%q) accepted, want error", tc.value)
			}
			if !strings.Contains(err.Error(), "WAKIL_SKILLS_DB") {
				t.Errorf("error %q does not name WAKIL_SKILLS_DB", err)
			}
			// The returned error must never have fallen back to the real
			// store: resolution failed, so no path was returned.
			if p != "" {
				t.Errorf("SkillDBPath on invalid value returned path %q, want empty", p)
			}
		})
	}
}

func TestSkillDBPath_UnsetUnresolvableHome(t *testing.T) {
	unsetSkillEnv(t)
	// Unset + empty home => ("", nil): today's best-effort fallback.
	t.Setenv("HOME", "")
	p, err := SkillDBPath()
	if err != nil {
		t.Fatalf("SkillDBPath with unset override and no home returned error: %v", err)
	}
	if p != "" {
		t.Errorf("SkillDBPath = %q, want empty with unresolvable home", p)
	}
}

func TestSkillDBPath_UnsetDefault(t *testing.T) {
	unsetSkillEnv(t)
	xdgHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdgHome)

	p, err := SkillDBPath()
	if err != nil {
		t.Fatalf("SkillDBPath: %v", err)
	}
	want := filepath.Join(xdgHome, "wakil", "skills", "skills.db")
	if p != want {
		t.Errorf("SkillDBPath = %q, want %q", p, want)
	}
}
