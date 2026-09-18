package exec

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDockerWorktreeDescribe covers Describe() and dockerSuffix() — both are
// pure string formatters that only read struct fields, no Docker calls.
func TestDockerWorktreeDescribe(t *testing.T) {
	// Minimal parent — Describe/dockerSuffix only use fields on w itself.
	parent := &DockerExecutor{image: "wakil-dev"}

	w := &dockerWorktreeExecutor{
		DockerExecutor: parent,
		wtRoot:         "/tmp/wakil-wt-abc",
	}
	desc := w.Describe()
	if !strings.Contains(desc, "docker-wt[") {
		t.Errorf("Describe should contain 'docker-wt['; got %q", desc)
	}
	if !strings.Contains(desc, "wakil-dev") {
		t.Errorf("Describe should contain image name; got %q", desc)
	}
	if !strings.Contains(desc, "/tmp/wakil-wt-abc") {
		t.Errorf("Describe should contain wtRoot; got %q", desc)
	}

	// With all flags set — verify suffix accumulation.
	w.dockerSock = true
	w.signing = true
	w.kvrAvailable = true
	w.seccompProfilePath = "/tmp/seccomp.json"
	desc2 := w.Describe()
	for _, want := range []string{"+docker", "+sign", "+kvr", "+iouring"} {
		if !strings.Contains(desc2, want) {
			t.Errorf("Describe with all flags should contain %s; got %q", want, desc2)
		}
	}

	// kvrAvailable=false but stagingMount set → "+kvr(off)"
	w.kvrAvailable = false
	w.stagingMount = "/staging"
	desc3 := w.Describe()
	if !strings.Contains(desc3, "+kvr(off)") {
		t.Errorf("Describe should contain +kvr(off); got %q", desc3)
	}
	// Should NOT contain "+kvr" without "(off)"
	if strings.Contains(desc3, "+kvr") && !strings.Contains(desc3, "+kvr(off)") {
		t.Errorf("Describe should not have bare +kvr when kvr off; got %q", desc3)
	}

	// No flags at all → empty suffix (fresh parent so no shared-state bleed)
	w2 := &dockerWorktreeExecutor{
		DockerExecutor: &DockerExecutor{image: "wakil-dev"},
		wtRoot:         "/tmp/wt",
	}
	desc4 := w2.Describe()
	if strings.Contains(desc4, "+") {
		t.Errorf("Describe with no flags should have no +suffix; got %q", desc4)
	}
}

// TestDockerWorktreeClose verifies Close is a no-op returning nil.
func TestDockerWorktreeClose(t *testing.T) {
	w := &dockerWorktreeExecutor{
		DockerExecutor: &DockerExecutor{},
		wtRoot:         "/tmp/wt",
	}
	if err := w.Close(); err != nil {
		t.Errorf("Close() should return nil; got %v", err)
	}
}

// TestDockerWorktreeGetters covers Cwd and WorkspaceRoot.
func TestDockerWorktreeGetters(t *testing.T) {
	w := &dockerWorktreeExecutor{
		DockerExecutor: &DockerExecutor{},
		wtRoot:         "/tmp/wakil-wt-xyz",
	}
	if got := w.Cwd(); got != "/tmp/wakil-wt-xyz" {
		t.Errorf("Cwd() = %q, want /tmp/wakil-wt-xyz", got)
	}
	if got := w.WorkspaceRoot(); got != "/tmp/wakil-wt-xyz" {
		t.Errorf("WorkspaceRoot() = %q, want /tmp/wakil-wt-xyz", got)
	}
}

// TestDockerWorktreeHostPathErrors: HostPathToURI and URIToHostPath return
// errors (no host mapping for container worktrees).
func TestDockerWorktreeHostPathErrors(t *testing.T) {
	w := &dockerWorktreeExecutor{
		DockerExecutor: &DockerExecutor{},
		wtRoot:         "/tmp/wt",
	}
	if _, err := w.HostPathToURI("/some/path"); err == nil {
		t.Error("HostPathToURI should return error for worktree executor")
	}
	if _, err := w.URIToHostPath("file:///some/path"); err == nil {
		t.Error("URIToHostPath should return error for worktree executor")
	}
}

// TestDockerWorktreeReadFileTailValidation: the maxBytes validation runs before
// any Docker call, so it's testable without a container.
func TestDockerWorktreeReadFileTailValidation(t *testing.T) {
	w := &dockerWorktreeExecutor{
		DockerExecutor: &DockerExecutor{},
		wtRoot:         "/tmp/wt",
	}
	_, err := w.ReadFileTail(context.Background(), "some.log", 0)
	if err == nil {
		t.Error("ReadFileTail with maxBytes=0 should return validation error")
	}
	_, err = w.ReadFileTail(context.Background(), "some.log", -1)
	if err == nil {
		t.Error("ReadFileTail with maxBytes=-1 should return validation error")
	}
}

// TestNewDockerWorktreeExecutor verifies the constructor returns the right type.
func TestNewDockerWorktreeExecutor(t *testing.T) {
	parent := &DockerExecutor{image: "wakil-dev"}
	ex := NewDockerWorktreeExecutor(parent, "/tmp/wt-123")
	w, ok := ex.(*dockerWorktreeExecutor)
	if !ok {
		t.Fatalf("expected *dockerWorktreeExecutor, got %T", ex)
	}
	if w.wtRoot != "/tmp/wt-123" {
		t.Errorf("wtRoot = %q, want /tmp/wt-123", w.wtRoot)
	}
	if w.DockerExecutor != parent {
		t.Error("parent DockerExecutor not set correctly")
	}
}

// TestCrossDeviceCopy copies a file from src to dst and removes src.
func TestCrossDeviceCopy(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")
	content := []byte("hello cross-device")

	if err := os.WriteFile(src, content, 0644); err != nil {
		t.Fatal(err)
	}

	if err := crossDeviceCopy(src, dst); err != nil {
		t.Fatalf("crossDeviceCopy: %v", err)
	}

	// Source should be removed.
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("source should be removed after copy; stat err=%v", err)
	}
	// Destination should have the content.
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello cross-device" {
		t.Errorf("content = %q, want %q", got, "hello cross-device")
	}
}

// TestCrossDeviceCopySrcNotExist verifies error when source doesn't exist.
func TestCrossDeviceCopySrcNotExist(t *testing.T) {
	dir := t.TempDir()
	err := crossDeviceCopy(filepath.Join(dir, "nope"), filepath.Join(dir, "dst"))
	if err == nil {
		t.Error("crossDeviceCopy should fail when source doesn't exist")
	}
}

// TestCrossDeviceCopyDstExists verifies error when dst already exists (O_EXCL).
func TestCrossDeviceCopyDstExists(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")
	os.WriteFile(src, []byte("src"), 0644)
	os.WriteFile(dst, []byte("existing"), 0644)

	err := crossDeviceCopy(src, dst)
	if err == nil {
		t.Error("crossDeviceCopy should fail when dst exists (O_EXCL)")
	}
}

// TestIsImageNotFound covers the pure string-matching helper.
func TestIsImageNotFound(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"not found locally", strErr("image wakil-dev not found locally"), true},
		{"daemon error", strErr("Cannot connect to the Docker daemon"), false},
		{"permission error", strErr("permission denied"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isImageNotFound(tt.err); got != tt.want {
				t.Errorf("isImageNotFound(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// strErr returns a simple error wrapping a string, for table tests.
type strErrVal string

func (e strErrVal) Error() string { return string(e) }
func strErr(s string) error       { return strErrVal(s) }

// TestExpandHome covers the expandHome helper for both ~ and non-~ paths.
func TestExpandHome(t *testing.T) {
	// Non-tilde path should pass through unchanged.
	if got := expandHome("/absolute/path"); got != "/absolute/path" {
		t.Errorf("expandHome(/absolute/path) = %q, want /absolute/path", got)
	}
	if got := expandHome("relative/path"); got != "relative/path" {
		t.Errorf("expandHome(relative/path) = %q, want relative/path", got)
	}

	// Tilde paths should expand.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("os.UserHomeDir not available")
	}
	got := expandHome("~/foo")
	if got != filepath.Join(home, "foo") {
		t.Errorf("expandHome(~/foo) = %q, want %q", got, filepath.Join(home, "foo"))
	}
	got2 := expandHome("~")
	if got2 != home {
		t.Errorf("expandHome(~) = %q, want %q", got2, home)
	}
}
