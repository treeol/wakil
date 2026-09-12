package exec

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestDirectExecutorGetters: all getter methods return expected values.
func TestDirectExecutorGetters(t *testing.T) {
	dir := t.TempDir()
	ex, err := NewDirectExecutor(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()

	if got := ex.Cwd(); got != dir {
		t.Errorf("Cwd() = %q, want %q", got, dir)
	}
	if got := ex.WorkspaceRoot(); got != dir {
		t.Errorf("WorkspaceRoot() = %q, want %q", got, dir)
	}
	if got := ex.Generation(); got != 1 {
		t.Errorf("Generation() = %d, want 1", got)
	}
	if got := ex.KVRSocketPath(); got != "" {
		t.Errorf("KVRSocketPath() = %q, want empty", got)
	}
	if got := ex.KVRAvailable(); got != false {
		t.Errorf("KVRAvailable() = %v, want false", got)
	}
	if got := ex.ContainerName(); got != "" {
		t.Errorf("ContainerName() = %q, want empty", got)
	}
	if got := ex.CDPPort(); got != 0 {
		t.Errorf("CDPPort() = %d, want 0", got)
	}
	desc := ex.Describe()
	if !strings.Contains(desc, "direct[") {
		t.Errorf("Describe() = %q, want to contain 'direct['", desc)
	}
	if err := ex.Close(); err != nil {
		t.Errorf("Close() = %v, want nil", err)
	}
}

// TestDirectExecutorListDir: ListDir returns entries with trailing / on dirs.
func TestDirectExecutorListDir(t *testing.T) {
	dir := t.TempDir()
	ex, err := NewDirectExecutor(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()

	ctx := context.Background()
	if _, err := ex.WriteFile(ctx, "file.txt", "content"); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.RunShell(ctx, "mkdir subdir"); err != nil {
		t.Fatal(err)
	}

	out, err := ex.ListDir(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "file.txt") {
		t.Errorf("ListDir should contain file.txt; got %q", out)
	}
	if !strings.Contains(out, "subdir/") {
		t.Errorf("ListDir should contain subdir/ with trailing slash; got %q", out)
	}
}

// TestDirectExecutorListDirSubdir: ListDir on a subdirectory.
func TestDirectExecutorListDirSubdir(t *testing.T) {
	dir := t.TempDir()
	ex, err := NewDirectExecutor(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()

	ctx := context.Background()
	if _, err := ex.RunShell(ctx, "mkdir -p a/b"); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.WriteFile(ctx, "a/b/deep.txt", "deep"); err != nil {
		t.Fatal(err)
	}

	out, err := ex.ListDir(ctx, "a/b")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "deep.txt") {
		t.Errorf("ListDir(a/b) should contain deep.txt; got %q", out)
	}
}

// TestDirectExecutorStatFileSize: StatFile returns file size.
func TestDirectExecutorStatFileSize(t *testing.T) {
	dir := t.TempDir()
	ex, err := NewDirectExecutor(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()

	ctx := context.Background()
	content := "hello world"
	if _, err := ex.WriteFile(ctx, "size.txt", content); err != nil {
		t.Fatal(err)
	}

	size, err := ex.StatFile(ctx, "size.txt")
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(content)) {
		t.Errorf("StatFile = %d, want %d", size, len(content))
	}
}

// TestDirectExecutorStatFileNotFound: StatFile on nonexistent file returns error.
func TestDirectExecutorStatFileNotFound(t *testing.T) {
	dir := t.TempDir()
	ex, err := NewDirectExecutor(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()

	_, err = ex.StatFile(context.Background(), "nonexistent.txt")
	if err == nil {
		t.Error("StatFile on nonexistent file should return error")
	}
}

// TestDirectExecutorReadFileNotFound: ReadFile on nonexistent file returns error.
func TestDirectExecutorReadFileNotFound(t *testing.T) {
	dir := t.TempDir()
	ex, err := NewDirectExecutor(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()

	_, err = ex.ReadFile(context.Background(), "nonexistent.txt")
	if err == nil {
		t.Error("ReadFile on nonexistent file should return error")
	}
}

// TestDirectExecutorWriteFileNested: WriteFile creates parent directories.
func TestDirectExecutorWriteFileNested(t *testing.T) {
	dir := t.TempDir()
	ex, err := NewDirectExecutor(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()

	ctx := context.Background()
	msg, err := ex.WriteFile(ctx, "a/b/c/deep.txt", "nested content")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "wrote") {
		t.Errorf("WriteFile message = %q, want to contain 'wrote'", msg)
	}

	got, err := ex.ReadFile(ctx, "a/b/c/deep.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got != "nested content" {
		t.Errorf("ReadFile = %q, want %q", got, "nested content")
	}
}

// TestDirectExecutorStartInteractive: StartInteractive returns working pipes.
func TestDirectExecutorStartInteractive(t *testing.T) {
	dir := t.TempDir()
	ex, err := NewDirectExecutor(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()

	ctx := context.Background()
	stdin, stdout, stderr, pid, err := ex.StartInteractive(ctx, "echo hello")
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()

	if pid <= 0 {
		t.Errorf("StartInteractive pid = %d, want > 0", pid)
	}
	if stdin == nil || stdout == nil || stderr == nil {
		t.Error("StartInteractive returned nil pipe")
	}
}

// TestShQuote: shQuote escapes single quotes.
func TestShQuote(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello", "'hello'"},
		{"it's", "'it'\\''s'"},
		{"", "''"},
		{"a'b'c", "'a'\\''b'\\''c'"},
	}
	for _, tt := range tests {
		if got := shQuote(tt.input); got != tt.want {
			t.Errorf("shQuote(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// TestRunFromRoot: runFromRoot wraps command with cd.
func TestRunFromRoot(t *testing.T) {
	got := runFromRoot("/my/project", "ls -la")
	if !strings.Contains(got, "cd") {
		t.Errorf("runFromRoot should contain cd; got %q", got)
	}
	if !strings.Contains(got, "/my/project") {
		t.Errorf("runFromRoot should contain the root path; got %q", got)
	}
	if !strings.Contains(got, "ls -la") {
		t.Errorf("runFromRoot should contain the command; got %q", got)
	}
}

// TestRandSuffix: randSuffix returns a non-empty hex string.
func TestRandSuffix(t *testing.T) {
	s1 := randSuffix(8)
	if s1 == "" {
		t.Error("randSuffix returned empty string")
	}
	// Two calls should return different values (with overwhelming probability).
	s2 := randSuffix(8)
	if s1 == s2 {
		t.Errorf("randSuffix returned same value twice: %q", s1)
	}
}

// TestDockerExecutorDescribe: Describe formats the executor description.
func TestDockerExecutorDescribe(t *testing.T) {
	d := &DockerExecutor{
		image:         "wakil-dev",
		hostMount:     "/home/user/project",
		workspaceRoot: "/workspace",
		dockerSock:    true,
		signing:       true,
		kvrAvailable:  true,
		stagingMount:  "/staging",
	}
	desc := d.Describe()
	if !strings.Contains(desc, "wakil-dev") {
		t.Errorf("Describe should contain image name; got %q", desc)
	}
	if !strings.Contains(desc, "+docker") {
		t.Errorf("Describe should contain +docker; got %q", desc)
	}
	if !strings.Contains(desc, "+sign") {
		t.Errorf("Describe should contain +sign; got %q", desc)
	}
	if !strings.Contains(desc, "+kvr") {
		t.Errorf("Describe should contain +kvr; got %q", desc)
	}
	if !strings.Contains(desc, "/home/user/project") {
		t.Errorf("Describe should contain hostMount; got %q", desc)
	}

	// Without hostMount.
	d2 := &DockerExecutor{image: "wakil-dev", workspaceRoot: "/workspace"}
	desc2 := d2.Describe()
	if strings.Contains(desc2, "→") {
		t.Errorf("Describe without hostMount should not contain →; got %q", desc2)
	}

	// With stagingMount but no kvrAvailable.
	d3 := &DockerExecutor{image: "wakil-dev", workspaceRoot: "/workspace", stagingMount: "/staging"}
	desc3 := d3.Describe()
	if !strings.Contains(desc3, "+kvr(off)") {
		t.Errorf("Describe with kvrAvailable=false and stagingMount should contain +kvr(off); got %q", desc3)
	}
}

// TestDockerExecutorGetters: all getter methods return expected values.
func TestDockerExecutorGetters(t *testing.T) {
	d := &DockerExecutor{
		workspaceRoot: "/workspace",
		container:     "wakil-session-123",
		generation:    5,
		kvrSocket:     "/run/kvr-staging/kvr.sock",
		kvrAvailable:  true,
		cdpPort:       32768,
		hostMount:     "/host/project",
	}
	if got := d.Cwd(); got != "/workspace" {
		t.Errorf("Cwd() = %q, want /workspace", got)
	}
	if got := d.WorkspaceRoot(); got != "/workspace" {
		t.Errorf("WorkspaceRoot() = %q, want /workspace", got)
	}
	if got := d.Generation(); got != 5 {
		t.Errorf("Generation() = %d, want 5", got)
	}
	if got := d.KVRSocketPath(); got != "/run/kvr-staging/kvr.sock" {
		t.Errorf("KVRSocketPath() = %q, want /run/kvr-staging/kvr.sock", got)
	}
	if got := d.KVRAvailable(); got != true {
		t.Errorf("KVRAvailable() = %v, want true", got)
	}
	if got := d.ContainerName(); got != "wakil-session-123" {
		t.Errorf("ContainerName() = %q, want wakil-session-123", got)
	}
	if got := d.CDPPort(); got != 32768 {
		t.Errorf("CDPPort() = %d, want 32768", got)
	}
}

// TestDockerExecutorHostPathToURI: host path → container URI translation.
func TestDockerExecutorHostPathToURI(t *testing.T) {
	d := &DockerExecutor{
		hostMount:     "/home/user/project",
		workspaceRoot: "/workspace",
	}

	// Absolute path under hostMount.
	uri, err := d.HostPathToURI("/home/user/project/src/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(uri, "file://") {
		t.Errorf("URI should start with file://; got %q", uri)
	}
	if !strings.Contains(uri, "/workspace/src/main.go") {
		t.Errorf("URI should contain /workspace/src/main.go; got %q", uri)
	}

	// Relative path (resolved against hostMount).
	uri2, err := d.HostPathToURI("src/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(uri2, "/workspace/src/main.go") {
		t.Errorf("URI for relative path should contain /workspace/src/main.go; got %q", uri2)
	}

	// Path outside hostMount should error.
	_, err = d.HostPathToURI("/etc/passwd")
	if err == nil {
		t.Error("HostPathToURI outside hostMount should return error")
	}
}

// TestDockerExecutorURIToHostPath: container URI → host path translation.
func TestDockerExecutorURIToHostPath(t *testing.T) {
	d := &DockerExecutor{
		hostMount:     "/home/user/project",
		workspaceRoot: "/workspace",
	}

	// URI under workspaceRoot.
	hostPath, err := d.URIToHostPath("file:///workspace/src/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if hostPath != "/home/user/project/src/main.go" {
		t.Errorf("URIToHostPath = %q, want /home/user/project/src/main.go", hostPath)
	}

	// URI outside workspaceRoot should error (e.g. GOROOT).
	_, err = d.URIToHostPath("file:///usr/local/go/src/fmt/print.go")
	if err == nil {
		t.Error("URIToHostPath outside workspaceRoot should return error")
	}
}

// TestDirectExecutorWriteFileBytes: WriteFileBytes writes raw bytes (binary-safe).
func TestDirectExecutorWriteFileBytes(t *testing.T) {
	dir := t.TempDir()
	ex, err := NewDirectExecutor(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()

	ctx := context.Background()
	data := []byte{0x00, 0x01, 0xFF, 0xFE, 'h', 'i'}
	msg, err := ex.WriteFileBytes(ctx, "bin.dat", data)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "wrote") {
		t.Errorf("WriteFileBytes message = %q, want to contain 'wrote'", msg)
	}
	got, err := ex.ReadFile(ctx, "bin.dat")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(data) {
		t.Errorf("ReadFile length = %d, want %d", len(got), len(data))
	}
}

// TestDirectExecutorConfinePath: ConfinePath resolves and confines paths to workspace.
func TestDirectExecutorConfinePath(t *testing.T) {
	dir := t.TempDir()
	ex, err := NewDirectExecutor(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()

	ctx := context.Background()
	// Write a file so it exists for symlink resolution.
	if _, err := ex.WriteFile(ctx, "real.txt", "content"); err != nil {
		t.Fatal(err)
	}
	// Relative path inside workspace → resolved absolute path.
	p, err := ex.ConfinePath(ctx, "real.txt")
	if err != nil {
		t.Fatalf("ConfinePath(real.txt): %v", err)
	}
	if !filepath.IsAbs(p) && !strings.Contains(p, "real.txt") {
		t.Errorf("ConfinePath = %q, expected absolute path with real.txt", p)
	}
	// Non-existent relative path inside workspace → should still resolve (no symlink).
	p2, err := ex.ConfinePath(ctx, "newdir/file.txt")
	if err != nil {
		t.Fatalf("ConfinePath(newdir/file.txt): %v", err)
	}
	if !strings.HasSuffix(p2, "newdir/file.txt") {
		t.Errorf("ConfinePath = %q, expected suffix newdir/file.txt", p2)
	}
	// Path outside workspace → error.
	_, err = ex.ConfinePath(ctx, "/etc/passwd")
	if err == nil {
		t.Error("ConfinePath(/etc/passwd) should error (outside workspace)")
	}
}

// TestDirectExecutorDeletePath: DeletePath removes files and errors on non-existent.
func TestDirectExecutorDeletePath(t *testing.T) {
	dir := t.TempDir()
	ex, err := NewDirectExecutor(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()

	ctx := context.Background()
	if _, err := ex.WriteFile(ctx, "to-delete.txt", "bye"); err != nil {
		t.Fatal(err)
	}
	if err := ex.DeletePath(ctx, filepath.Join(dir, "to-delete.txt")); err != nil {
		t.Fatalf("DeletePath: %v", err)
	}
	// Deleting again should return ErrFileNotFound.
	err = ex.DeletePath(ctx, filepath.Join(dir, "to-delete.txt"))
	if err == nil {
		t.Error("DeletePath on non-existent file should error")
	}
}

// TestDirectExecutorDeletePathNonEmptyDir: DeletePath on non-empty dir gives
// actionable error.
func TestDirectExecutorDeletePathNonEmptyDir(t *testing.T) {
	dir := t.TempDir()
	ex, err := NewDirectExecutor(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()

	ctx := context.Background()
	if _, err := ex.WriteFile(ctx, "dir/file.txt", "content"); err != nil {
		t.Fatal(err)
	}
	err = ex.DeletePath(ctx, filepath.Join(dir, "dir"))
	if err == nil {
		t.Fatal("DeletePath on non-empty dir should error")
	}
	if !strings.Contains(err.Error(), "not empty") {
		t.Errorf("DeletePath on non-empty dir should mention 'not empty'; got %v", err)
	}
}

// TestDirectExecutorMovePath: MovePath renames files and errors on existing dst.
func TestDirectExecutorMovePath(t *testing.T) {
	dir := t.TempDir()
	ex, err := NewDirectExecutor(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()

	ctx := context.Background()
	if _, err := ex.WriteFile(ctx, "src.txt", "content"); err != nil {
		t.Fatal(err)
	}
	srcFull := filepath.Join(dir, "src.txt")
	dstFull := filepath.Join(dir, "dst.txt")
	if err := ex.MovePath(ctx, srcFull, dstFull); err != nil {
		t.Fatalf("MovePath: %v", err)
	}
	// Verify src gone, dst exists.
	got, err := ex.ReadFile(ctx, "dst.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got != "content" {
		t.Errorf("ReadFile(dst) = %q, want 'content'", got)
	}
	// Moving to an existing dst should error.
	if _, err := ex.WriteFile(ctx, "src2.txt", "x"); err != nil {
		t.Fatal(err)
	}
	err = ex.MovePath(ctx, filepath.Join(dir, "src2.txt"), dstFull)
	if err == nil {
		t.Error("MovePath to existing dst should error")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("MovePath to existing dst should mention 'already exists'; got %v", err)
	}
}

// TestDirectExecutorSandboxTools: SandboxTools probes and caches tool availability.
func TestDirectExecutorSandboxTools(t *testing.T) {
	dir := t.TempDir()
	ex, err := NewDirectExecutor(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()

	// SandboxTools should return a non-empty string (at least the probe result).
	got := ex.SandboxTools()
	// On this system sh, cat, etc. should be present. But on minimal CI it may
	// be empty if ALL tools are absent — just check it doesn't panic and is
	// idempotent (cached by sync.Once).
	got2 := ex.SandboxTools()
	if got != got2 {
		t.Errorf("SandboxTools not idempotent: first=%q second=%q", got, got2)
	}
}

// TestExitStatusLine: ExitStatusLine renders correct strings for all cases.
func TestExitStatusLine(t *testing.T) {
	tests := []struct {
		code  int
		known bool
		want  string
	}{
		{0, true, "exited OK"},
		{1, true, "exited with code 1"},
		{42, true, "exited with code 42"},
		{0, false, "exit code unknown (completion marker missing or log unreadable)"},
	}
	for _, tt := range tests {
		got := ExitStatusLine(tt.code, tt.known)
		if got != tt.want {
			t.Errorf("ExitStatusLine(%d, %v) = %q, want %q", tt.code, tt.known, got, tt.want)
		}
	}
}
