package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicWriteJSON_Success(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.json")

	type data struct {
		Name  string `json:"name"`
		Value int    `json:"value"`
	}
	in := data{Name: "test", Value: 42}

	if err := atomicWriteJSON(path, &in); err != nil {
		t.Fatalf("atomicWriteJSON: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var out data
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out != in {
		t.Errorf("round-trip mismatch: got %+v, want %+v", out, in)
	}
}

func TestAtomicWriteJSON_OverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.json")

	// Write initial content.
	if err := os.WriteFile(path, []byte(`{"old":true}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Overwrite with atomicWriteJSON.
	type data struct {
		New bool `json:"new"`
	}
	if err := atomicWriteJSON(path, &data{New: true}); err != nil {
		t.Fatalf("atomicWriteJSON: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, exists := m["old"]; exists {
		t.Error("old key should not exist after overwrite")
	}
	if _, exists := m["new"]; !exists {
		t.Error("new key should exist after overwrite")
	}
}

func TestAtomicWriteJSON_NoTempFileLeftBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.json")

	if err := atomicWriteJSON(path, &struct{ X int }{X: 1}); err != nil {
		t.Fatalf("atomicWriteJSON: %v", err)
	}

	// Verify no temp files remain in the directory.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "data.json" {
			t.Errorf("unexpected file left behind: %s", e.Name())
		}
	}
}

func TestAtomicWriteJSON_MarshalError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.json")

	// A channel can't be marshaled to JSON.
	err := atomicWriteJSON(path, make(chan int))
	if err == nil {
		t.Fatal("expected marshal error")
	}
	// Verify no file was created.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("target file should not exist after marshal error")
	}
}

func TestAtomicWriteJSON_FilePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.json")

	if err := atomicWriteJSON(path, &struct{ X int }{X: 1}); err != nil {
		t.Fatalf("atomicWriteJSON: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	perm := info.Mode().Perm()
	if perm != 0o644 {
		t.Errorf("file permissions = %o, want 0644 (os.CreateTemp defaults to 0600; explicit Chmod is required)", perm)
	}
}

func TestAtomicWriteJSON_DirDoesNotExist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nonexistent", "data.json")

	err := atomicWriteJSON(path, &struct{ X int }{X: 1})
	if err == nil {
		t.Fatal("expected error for non-existent directory")
	}
}

func TestResolveDataDir_EnvOverride(t *testing.T) {
	t.Setenv("WAKIL_TEST_DIR", "/custom/test/dir")
	got := resolveDataDir("WAKIL_TEST_DIR", "test")
	if got != "/custom/test/dir" {
		t.Errorf("resolveDataDir with env override = %q, want %q", got, "/custom/test/dir")
	}
}

func TestResolveDataDir_XDGDataHome(t *testing.T) {
	t.Setenv("WAKIL_TEST_DIR", "")
	t.Setenv("XDG_DATA_HOME", "/xdg/home")
	got := resolveDataDir("WAKIL_TEST_DIR", "sessions")
	if got != "/xdg/home/wakil/sessions" {
		t.Errorf("resolveDataDir with XDG_DATA_HOME = %q, want %q", got, "/xdg/home/wakil/sessions")
	}
}

func TestResolveDataDir_HomeFallback(t *testing.T) {
	t.Setenv("WAKIL_TEST_DIR", "")
	t.Setenv("XDG_DATA_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("os.UserHomeDir failed")
	}
	got := resolveDataDir("WAKIL_TEST_DIR", "repo-state")
	want := filepath.Join(home, ".local", "share", "wakil", "repo-state")
	if got != want {
		t.Errorf("resolveDataDir home fallback = %q, want %q", got, want)
	}
}

// TestResolveDataDir_MatchesSessionsDir verifies that sessionsDir() now
// delegates to resolveDataDir and produces the same result as the old inline
// resolution.
func TestResolveDataDir_MatchesSessionsDir(t *testing.T) {
	t.Setenv("WAKIL_SESSIONS_DIR", "")
	t.Setenv("XDG_DATA_HOME", "/xdg/test")
	got := sessionsDir()
	want := resolveDataDir("WAKIL_SESSIONS_DIR", "sessions")
	if got != want {
		t.Errorf("sessionsDir() = %q, want %q (resolveDataDir)", got, want)
	}
}

// TestResolveDataDir_MatchesRepoStateDir verifies that repoStateDir() now
// delegates to resolveDataDir.
func TestResolveDataDir_MatchesRepoStateDir(t *testing.T) {
	t.Setenv("WAKIL_REPO_STATE_DIR", "")
	t.Setenv("XDG_DATA_HOME", "/xdg/test")
	got := repoStateDir()
	want := resolveDataDir("WAKIL_REPO_STATE_DIR", "repo-state")
	if got != want {
		t.Errorf("repoStateDir() = %q, want %q (resolveDataDir)", got, want)
	}
}
