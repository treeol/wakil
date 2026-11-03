package exec

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/treeol/wakil/internal/sandbox/fake"
)

func TestProtectedKind(t *testing.T) {
	root := "/w"
	cases := []struct {
		path string
		want int
	}{
		{"/w/.wakil/sandbox.json", 1},
		{".wakil/sandbox.json", 1},
		{"/w/.wakil/../.wakil/sandbox.json", 1},
		{"/w/.wakil", 2},
		{"/w/.wakil/kvr.sock", 0},
		{"/w/sub/.wakil/sandbox.json", 0},
		{"/other/.wakil/sandbox.json", 0},
		{"", 0},
	}
	for _, c := range cases {
		if got := protectedKind(root, c.path); got != c.want {
			t.Errorf("protectedKind(%q) = %d, want %d", c.path, got, c.want)
		}
	}
}

func TestDirectExecutorRefusesProtectedFile(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	e, err := NewDirectExecutor(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	file := filepath.Join(dir, ".wakil", "sandbox.json")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// A symlink to the file inside the workspace must not be a way around it.
	link := filepath.Join(dir, "innocent.json")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	check := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, ErrProtectedPath) {
			t.Errorf("%s: want ErrProtectedPath, got %v", what, err)
		}
	}
	_, err = e.WriteFile(ctx, file, "x")
	check("WriteFile abs", err)
	_, err = e.WriteFile(ctx, ".wakil/sandbox.json", "x")
	check("WriteFile rel", err)
	_, err = e.WriteFileBytes(ctx, file, []byte("x"))
	check("WriteFileBytes", err)
	_, err = e.WriteFile(ctx, link, "x")
	check("WriteFile via symlink", err)
	check("DeletePath", e.DeletePath(ctx, file))
	check("DeletePath dir", e.DeletePath(ctx, filepath.Dir(file)))
	check("MovePath src", e.MovePath(ctx, file, filepath.Join(dir, "gone.json")))
	check("MovePath dst", e.MovePath(ctx, filepath.Join(dir, "a.json"), file))
	check("MovePath dir", e.MovePath(ctx, filepath.Dir(file), filepath.Join(dir, ".wakil.bak")))
	if b, _ := os.ReadFile(file); string(b) != `{}` {
		t.Errorf("file was modified: %q", b)
	}
	// Neighbours stay writable, and the file stays readable.
	if _, err := e.WriteFile(ctx, filepath.Join(dir, ".wakil", "other.txt"), "ok"); err != nil {
		t.Errorf("unrelated file in .wakil must stay writable: %v", err)
	}
	if got, err := e.ReadFile(ctx, file); err != nil || got != `{}` {
		t.Errorf("read must stay allowed: %q %v", got, err)
	}
}

func TestBackendExecutorRefusesProtectedFile(t *testing.T) {
	fb := fake.New("/ws", "/ws")
	e := NewBackendExecutor(fb, BackendOptions{Label: "fake"})
	ctx := context.Background()
	f := "/ws/.wakil/sandbox.json"
	if _, err := e.WriteFile(ctx, f, "x"); !errors.Is(err, ErrProtectedPath) {
		t.Errorf("WriteFile: %v", err)
	}
	if _, err := e.WriteFileBytes(ctx, f, []byte("x")); !errors.Is(err, ErrProtectedPath) {
		t.Errorf("WriteFileBytes: %v", err)
	}
	if err := e.DeletePath(ctx, f); !errors.Is(err, ErrProtectedPath) {
		t.Errorf("DeletePath: %v", err)
	}
	if err := e.MovePath(ctx, "/ws/.wakil", "/ws/x"); !errors.Is(err, ErrProtectedPath) {
		t.Errorf("MovePath dir: %v", err)
	}
	if err := e.MovePath(ctx, "/ws/a", f); !errors.Is(err, ErrProtectedPath) {
		t.Errorf("MovePath dst: %v", err)
	}
}
