package exec

import (
	"context"
	"strings"
	"testing"
)

// DirectExecutor write/read roundtrip — relative paths resolve against project root.
func TestDirectExecutorRoundtrip(t *testing.T) {
	dir := t.TempDir()
	ex, err := NewDirectExecutor(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()

	if _, err := ex.WriteFile("foo.txt", "hi"); err != nil {
		t.Fatal(err)
	}
	got, err := ex.ReadFile("foo.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got != "hi" {
		t.Errorf("read back %q, want %q", got, "hi")
	}
}

// TestCwdNotPersisted: in-command cd affects only that command; the next
// command starts from the project root again.
func TestCwdNotPersisted(t *testing.T) {
	dir := t.TempDir()
	ex, err := NewDirectExecutor(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()

	ctx := context.Background()
	if _, err := ex.RunShell(ctx, "mkdir sub"); err != nil {
		t.Fatal(err)
	}

	// Within one command, cd chains work.
	out1, err := ex.RunShell(ctx, "cd sub && pwd")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(strings.TrimSpace(out1), "/sub") {
		t.Errorf("in-command cd should reflect in pwd; got %q", out1)
	}

	// Next command resets to project root.
	out2, err := ex.RunShell(ctx, "pwd")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out2) != dir {
		t.Errorf("cwd should reset to project root between commands; got %q, want %q", out2, dir)
	}
	if ex.Cwd() != dir {
		t.Errorf("Cwd() should always return project root; got %q", ex.Cwd())
	}
}

// TestRelativeWriteAfterCdCommand: a write_file with a relative path in a turn
// after a cd-containing run_shell still lands at the project root.
func TestRelativeWriteAfterCdCommand(t *testing.T) {
	dir := t.TempDir()
	ex, err := NewDirectExecutor(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()

	ctx := context.Background()
	// Simulate a cd command in a prior turn.
	if _, err := ex.RunShell(ctx, "mkdir sub && cd sub"); err != nil {
		t.Fatal(err)
	}
	// Relative write_file should resolve against project root, not /sub.
	if _, err := ex.WriteFile("out.txt", "content"); err != nil {
		t.Fatal(err)
	}
	// File must be at project root, not inside sub/.
	content, err := ex.ReadFile("out.txt")
	if err != nil {
		t.Fatalf("expected out.txt at project root: %v", err)
	}
	if content != "content" {
		t.Errorf("wrong content: %q", content)
	}
}
