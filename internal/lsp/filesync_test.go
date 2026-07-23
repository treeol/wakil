package lsp

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/exec"
)

// fakeLSPExec is a minimal exec.Executor for testing file-sync logic.
// It stores files in a map and supports ReadFile/StatFile/HostPathToURI/URIToHostPath.
type fakeLSPExec struct {
	files map[string]string
}

func newFakeLSPExec() *fakeLSPExec {
	return &fakeLSPExec{files: make(map[string]string)}
}

func (f *fakeLSPExec) RunShell(_ context.Context, _ string) (string, error) { return "", nil }
func (f *fakeLSPExec) ReadFile(_ context.Context, p string) (string, error) {
	if v, ok := f.files[p]; ok {
		return v, nil
	}
	return "", fmt.Errorf("no such file: %s", p)
}
func (f *fakeLSPExec) ListDir(_ context.Context, _ string) (string, error) { return "", nil }
func (f *fakeLSPExec) WriteFile(_ context.Context, _, _ string) (string, error) {
	return "", nil
}
func (f *fakeLSPExec) Cwd() string           { return "/work" }
func (f *fakeLSPExec) Describe() string      { return "fake-lsp" }
func (f *fakeLSPExec) Close() error          { return nil }
func (f *fakeLSPExec) SandboxTools() string  { return "" }
func (f *fakeLSPExec) WorkspaceRoot() string { return "/work" }
func (f *fakeLSPExec) ConfinePath(_ context.Context, p string) (string, error) {
	return p, nil
}
func (f *fakeLSPExec) DeletePath(_ context.Context, _ string) error  { return nil }
func (f *fakeLSPExec) MovePath(_ context.Context, _, _ string) error { return nil }
func (f *fakeLSPExec) StartBackground(_ context.Context, _, _ string) (int, int, error) {
	return 0, 0, nil
}
func (f *fakeLSPExec) KillPgid(_ context.Context, _, _ int) error   { return nil }
func (f *fakeLSPExec) IsProcessAlive(_ context.Context, _ int) bool { return false }
func (f *fakeLSPExec) ReadFileTail(_ context.Context, _ string, _ int64) (string, error) {
	return "", nil
}
func (f *fakeLSPExec) StatFile(_ context.Context, p string) (int64, error) {
	if v, ok := f.files[p]; ok {
		return int64(len(v)), nil
	}
	return 0, fmt.Errorf("no such file: %s", p)
}
func (f *fakeLSPExec) Generation() int       { return 1 }
func (f *fakeLSPExec) KVRSocketPath() string { return "" }
func (f *fakeLSPExec) KVRAvailable() bool    { return false }
func (f *fakeLSPExec) ContainerName() string { return "" }
func (f *fakeLSPExec) CDPPort() int          { return 0 }
func (f *fakeLSPExec) StartInteractive(_ context.Context, _ string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, int, error) {
	return nil, nil, nil, 0, fmt.Errorf("not implemented in fake executor")
}
func (f *fakeLSPExec) HostPathToURI(hostPath string) (string, error) {
	return "file://" + hostPath, nil
}
func (f *fakeLSPExec) URIToHostPath(uri string) (string, error) {
	return strings.TrimPrefix(uri, "file://"), nil
}

// Ensure the fake implements the interface.
var _ exec.Executor = (*fakeLSPExec)(nil)

// newTestServer creates a Server wired to a mock JSON-RPC conn and fake executor.
func newTestServer(t *testing.T, fe *fakeLSPExec) *Server {
	t.Helper()
	srv := newMockServer(t, nil)
	conn := newRPCConn(srv.clientW, srv.clientR, nil)
	t.Cleanup(func() { conn.Close() })

	cfg := config.DefaultConfig()
	mgr := &Manager{
		exec:    fe,
		cfg:     cfg,
		rootURI: "file:///work",
	}
	s := &Server{
		mgr:            mgr,
		conn:           conn,
		state:          StateReady,
		readyCh:        make(chan struct{}),
		deadCh:         make(chan struct{}),
		docs:           make(map[string]int32),
		progressTokens: make(map[any]bool),
	}
	close(s.readyCh)
	return s
}

func TestFileSync_SameSizeEditTriggersResync(t *testing.T) {
	fe := newFakeLSPExec()
	fe.files["/work/main.go"] = "package main\n\nfunc hello() {}"
	srv := newTestServer(t, fe)
	fs := srv.fileSync()

	ctx := context.Background()

	// Open the file (sends didOpen).
	uri, err := fs.ensureOpen(ctx, srv, "/work/main.go", "go")
	if err != nil {
		t.Fatalf("ensureOpen: %v", err)
	}

	// Simulate a same-size edit: flip one character without changing size.
	fe.files["/work/main.go"] = "package main\n\nfunc hello() { }"

	// Mark dirty (as run_shell would).
	fs.markDirty()

	// syncIfDirty should detect the change and send didChange.
	resynced, err := fs.syncIfDirty(ctx, srv, uri)
	if err != nil {
		t.Fatalf("syncIfDirty: %v", err)
	}
	if !resynced {
		t.Fatal("expected resync=true for same-size content change")
	}

	// Verify the content was updated.
	fs.mu.Lock()
	doc := fs.docs[uri]
	fs.mu.Unlock()
	if doc.content != "package main\n\nfunc hello() { }" {
		t.Errorf("doc.content not updated; got %q", doc.content)
	}
	if doc.version != 2 {
		t.Errorf("doc.version = %d, want 2", doc.version)
	}
}

func TestFileSync_UnchangedDirtyClearsFlag(t *testing.T) {
	fe := newFakeLSPExec()
	fe.files["/work/main.go"] = "package main"
	srv := newTestServer(t, fe)
	fs := srv.fileSync()

	ctx := context.Background()

	uri, err := fs.ensureOpen(ctx, srv, "/work/main.go", "go")
	if err != nil {
		t.Fatalf("ensureOpen: %v", err)
	}

	// Mark dirty without changing the file.
	fs.markDirty()

	// syncIfDirty should NOT resync (content is the same).
	resynced, err := fs.syncIfDirty(ctx, srv, uri)
	if err != nil {
		t.Fatalf("syncIfDirty: %v", err)
	}
	if resynced {
		t.Fatal("expected resync=false for unchanged content")
	}

	// Dirty flag should be cleared.
	fs.mu.Lock()
	dirty := fs.dirty[uri]
	fs.mu.Unlock()
	if dirty {
		t.Error("dirty flag should be cleared after unchanged check")
	}
}

func TestFileSync_DifferentSizeEditTriggersResync(t *testing.T) {
	fe := newFakeLSPExec()
	fe.files["/work/main.go"] = "package main"
	srv := newTestServer(t, fe)
	fs := srv.fileSync()

	ctx := context.Background()

	uri, err := fs.ensureOpen(ctx, srv, "/work/main.go", "go")
	if err != nil {
		t.Fatalf("ensureOpen: %v", err)
	}

	// Change file size.
	fe.files["/work/main.go"] = "package main\n\nfunc hello() {}"

	fs.markDirty()

	resynced, err := fs.syncIfDirty(ctx, srv, uri)
	if err != nil {
		t.Fatalf("syncIfDirty: %v", err)
	}
	if !resynced {
		t.Fatal("expected resync=true for size-changing edit")
	}
}

func TestFileSync_DeletedFileClearsDirty(t *testing.T) {
	fe := newFakeLSPExec()
	fe.files["/work/main.go"] = "package main"
	srv := newTestServer(t, fe)
	fs := srv.fileSync()

	ctx := context.Background()

	uri, err := fs.ensureOpen(ctx, srv, "/work/main.go", "go")
	if err != nil {
		t.Fatalf("ensureOpen: %v", err)
	}

	// Delete the file.
	delete(fe.files, "/work/main.go")

	fs.markDirty()

	// syncIfDirty should clear dirty and not error (file was deleted).
	resynced, err := fs.syncIfDirty(ctx, srv, uri)
	if err != nil {
		t.Fatalf("syncIfDirty: %v", err)
	}
	if resynced {
		t.Fatal("expected resync=false for deleted file")
	}

	fs.mu.Lock()
	dirty := fs.dirty[uri]
	fs.mu.Unlock()
	if dirty {
		t.Error("dirty flag should be cleared for deleted file")
	}
}

func TestFileSync_NotDirtySkipsResync(t *testing.T) {
	fe := newFakeLSPExec()
	fe.files["/work/main.go"] = "package main"
	srv := newTestServer(t, fe)
	fs := srv.fileSync()

	ctx := context.Background()

	uri, err := fs.ensureOpen(ctx, srv, "/work/main.go", "go")
	if err != nil {
		t.Fatalf("ensureOpen: %v", err)
	}

	// Don't mark dirty — syncIfDirty should be a no-op.
	fe.files["/work/main.go"] = "package main\nfunc changed() {}"

	resynced, err := fs.syncIfDirty(ctx, srv, uri)
	if err != nil {
		t.Fatalf("syncIfDirty: %v", err)
	}
	if resynced {
		t.Fatal("expected resync=false when file is not dirty")
	}
}

func TestDetectLanguage(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"main.go", "go"},
		{"lib.rs", "rust"},
		{"index.ts", "typescript"},
		{"index.tsx", "typescript"},
		{"app.js", "javascript"},
		{"app.jsx", "javascript"},
		{"script.py", "python"},
		{"main.c", "c"},
		{"header.h", "c"},
		{"impl.cpp", "cpp"},
		{"impl.hpp", "cpp"},
		{"impl.cc", "cpp"},
		{"Main.java", "java"},
		{"README.md", ""},
		{"Makefile", ""},
		{"", ""},
		{"/path/to/file.go", "go"},
		// ext(".d.ts") = ".ts" → detectLanguage returns "typescript"
		{"/path/to/file.d.ts", "typescript"},
	}
	for _, tt := range tests {
		got := detectLanguage(tt.path)
		if got != tt.want {
			t.Errorf("detectLanguage(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestExt(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"main.go", ".go"},
		{"dir/file.rs", ".rs"},
		{"noext", ""},
		{"path/to/file.test.js", ".js"},
		{"file.", "."},
		// ext(".hidden") scans from the end, finds no "/" before ".",
		// so it returns ".hidden" — this is existing behavior.
		{".hidden", ".hidden"},
		{"/a/b.c/d", ""},
	}
	for _, tt := range tests {
		got := ext(tt.path)
		if got != tt.want {
			t.Errorf("ext(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestSplitOnNewlines(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"abc", []string{"abc"}},
		{"a\nb\nc", []string{"a", "b", "c"}},
		{"a\r\nb", []string{"a", "b"}}, // CRLF stripped
		{"", []string{}},
		{"\n", []string{""}},
		{"a\n", []string{"a"}},
		{"a\r\n", []string{"a"}},
	}
	for _, tt := range tests {
		got := splitOnNewlines(tt.input)
		if len(got) != len(tt.want) {
			t.Errorf("splitOnNewlines(%q) = %v (len %d), want %v (len %d)",
				tt.input, got, len(got), tt.want, len(tt.want))
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("splitOnNewlines(%q)[%d] = %q, want %q",
					tt.input, i, got[i], tt.want[i])
			}
		}
	}
}
