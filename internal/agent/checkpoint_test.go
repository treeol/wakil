package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/exec"
)

// checkpointTestApp creates an App with a DirectExecutor rooted at a temp dir,
// configured for checkpoint testing.
func checkpointTestApp(t *testing.T) (*App, string) {
	t.Helper()
	dir := t.TempDir()
	exe, err := exec.NewDirectExecutor(dir)
	if err != nil {
		t.Fatalf("NewDirectExecutor: %v", err)
	}
	app := &App{
		Exec:  exe,
		Out:   os.Stderr,
	}
	return app, dir
}

func TestCheckpoint_CaptureAndRewind(t *testing.T) {
	app, dir := checkpointTestApp(t)
	ctx := context.Background()

	// Write a file.
	p := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(p, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Start a checkpoint (simulates turn start).
	app.startCheckpoint()

	// Capture the file before mutation.
	app.captureForCheckpoint(ctx, p)

	// Mutate the file.
	if _, err := app.Exec.WriteFile(ctx, p, "modified"); err != nil {
		t.Fatal(err)
	}

	// End the turn (simulates SendOutcome completion).
	app.endCheckpoint()

	// Verify mutation.
	got, _ := app.Exec.ReadFile(ctx, p)
	if got != "modified" {
		t.Fatalf("after mutation: got %q, want %q", got, "modified")
	}

	// Rewind 1 turn.
	result := app.rewind(1)
	if len(result.RestoredPaths) != 1 {
		t.Fatalf("expected 1 restored path, got %d: %+v", len(result.RestoredPaths), result)
	}

	// Verify file restored to original.
	got, _ = app.Exec.ReadFile(ctx, p)
	if got != "original" {
		t.Fatalf("after rewind: got %q, want %q", got, "original")
	}
}

func TestCheckpoint_CreateAndDelete(t *testing.T) {
	app, dir := checkpointTestApp(t)
	ctx := context.Background()

	// File does NOT exist at checkpoint time.
	p := filepath.Join(dir, "new.txt")

	// Start checkpoint.
	app.startCheckpoint()

	// Capture (file doesn't exist → Exists=false).
	app.captureForCheckpoint(ctx, p)

	// Create the file.
	if _, err := app.Exec.WriteFile(ctx, p, "new content"); err != nil {
		t.Fatal(err)
	}

	// End the turn.
	app.endCheckpoint()

	// Verify file exists.
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("file should exist: %v", err)
	}

	// Rewind.
	result := app.rewind(1)
	if len(result.DeletedPaths) != 1 {
		t.Fatalf("expected 1 deleted path, got %d: %+v", len(result.DeletedPaths), result)
	}

	// Verify file is gone.
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("file should not exist after rewind: %v", err)
	}
}

func TestCheckpoint_MultipleTurnsRewind(t *testing.T) {
	app, dir := checkpointTestApp(t)
	ctx := context.Background()

	// Create two files.
	p1 := filepath.Join(dir, "a.txt")
	p2 := filepath.Join(dir, "b.txt")
	os.WriteFile(p1, []byte("a-original"), 0o644)
	os.WriteFile(p2, []byte("b-original"), 0o644)

	// Turn 1: modify file A.
	app.startCheckpoint()
	app.captureForCheckpoint(ctx, p1)
	app.Exec.WriteFile(ctx, p1, "a-turn1")
	app.endCheckpoint()

	// Turn 2: modify file B.
	app.startCheckpoint()
	app.captureForCheckpoint(ctx, p2)
	app.Exec.WriteFile(ctx, p2, "b-turn2")
	app.endCheckpoint()

	// Verify both modified.
	got1, _ := app.Exec.ReadFile(ctx, p1)
	got2, _ := app.Exec.ReadFile(ctx, p2)
	if got1 != "a-turn1" {
		t.Fatalf("p1: got %q, want %q", got1, "a-turn1")
	}
	if got2 != "b-turn2" {
		t.Fatalf("p2: got %q, want %q", got2, "b-turn2")
	}

	// Rewind 2 turns (undo both).
	result := app.rewind(2)
	if len(result.RestoredPaths) != 2 {
		t.Fatalf("expected 2 restored paths, got %d: %+v", len(result.RestoredPaths), result)
	}

	// Verify both restored to original.
	got1, _ = app.Exec.ReadFile(ctx, p1)
	got2, _ = app.Exec.ReadFile(ctx, p2)
	if got1 != "a-original" {
		t.Fatalf("p1 after rewind 2: got %q, want %q", got1, "a-original")
	}
	if got2 != "b-original" {
		t.Fatalf("p2 after rewind 2: got %q, want %q", got2, "b-original")
	}
}

func TestCheckpoint_SameFileAcrossTurns(t *testing.T) {
	app, dir := checkpointTestApp(t)
	ctx := context.Background()

	p := filepath.Join(dir, "shared.txt")
	os.WriteFile(p, []byte("original"), 0o644)

	// Turn 1: modify shared.
	app.startCheckpoint()
	app.captureForCheckpoint(ctx, p)
	app.Exec.WriteFile(ctx, p, "turn1")
	app.endCheckpoint()

	// Turn 2: modify shared again.
	app.startCheckpoint()
	app.captureForCheckpoint(ctx, p)
	app.Exec.WriteFile(ctx, p, "turn2")
	app.endCheckpoint()

	// Rewind 2 — should restore to original (oldest snapshot wins).
	result := app.rewind(2)
	if len(result.RestoredPaths) != 1 {
		t.Fatalf("expected 1 restored path, got %d", len(result.RestoredPaths))
	}

	got, _ := app.Exec.ReadFile(ctx, p)
	if got != "original" {
		t.Fatalf("after rewind 2: got %q, want %q", got, "original")
	}
}

func TestCheckpoint_Rewind1Of2(t *testing.T) {
	app, dir := checkpointTestApp(t)
	ctx := context.Background()

	p1 := filepath.Join(dir, "a.txt")
	p2 := filepath.Join(dir, "b.txt")
	os.WriteFile(p1, []byte("a-original"), 0o644)
	os.WriteFile(p2, []byte("b-original"), 0o644)

	// Turn 1: modify file A.
	app.startCheckpoint()
	app.captureForCheckpoint(ctx, p1)
	app.Exec.WriteFile(ctx, p1, "a-turn1")
	app.endCheckpoint()

	// Turn 2: modify file B.
	app.startCheckpoint()
	app.captureForCheckpoint(ctx, p2)
	app.Exec.WriteFile(ctx, p2, "b-turn2")
	app.endCheckpoint()

	// Rewind 1 — should undo only turn 2 (file B).
	result := app.rewind(1)
	if len(result.RestoredPaths) != 1 {
		t.Fatalf("expected 1 restored path, got %d: %+v", len(result.RestoredPaths), result)
	}

	// File A should still be "a-turn1" (turn 1 not undone).
	got1, _ := app.Exec.ReadFile(ctx, p1)
	if got1 != "a-turn1" {
		t.Fatalf("p1: got %q, want %q", got1, "a-turn1")
	}

	// File B should be restored to "b-original".
	got2, _ := app.Exec.ReadFile(ctx, p2)
	if got2 != "b-original" {
		t.Fatalf("p2: got %q, want %q", got2, "b-original")
	}
}

func TestCheckpoint_ClearOnNewConversation(t *testing.T) {
	app, dir := checkpointTestApp(t)
	ctx := context.Background()

	p := filepath.Join(dir, "test.txt")
	os.WriteFile(p, []byte("original"), 0o644)

	app.startCheckpoint()
	app.captureForCheckpoint(ctx, p)

	// Simulate what NewConversation does — clear checkpoints.
	app.clearCheckpoints()

	// Checkpoints should be cleared.
	app.cpMu.Lock()
	count := len(app.checkpoints)
	app.cpMu.Unlock()
	if count != 0 {
		t.Fatalf("expected 0 checkpoints after clearCheckpoints, got %d", count)
	}
}

func TestCheckpoint_StatusEmpty(t *testing.T) {
	app, _ := checkpointTestApp(t)
	status := app.checkpointStatus()
	if !strings.Contains(status, "no checkpoints") {
		t.Fatalf("expected 'no checkpoints', got: %s", status)
	}
}

func TestCheckpoint_StatusNonEmpty(t *testing.T) {
	app, dir := checkpointTestApp(t)
	ctx := context.Background()
	p := filepath.Join(dir, "test.txt")
	os.WriteFile(p, []byte("original"), 0o644)

	app.startCheckpoint()
	app.captureForCheckpoint(ctx, p)
	app.endCheckpoint()

	status := app.checkpointStatus()
	if !strings.Contains(status, "1 back") {
		t.Fatalf("expected '1 back' in status, got: %s", status)
	}
	if !strings.Contains(status, "1 file(s)") {
		t.Fatalf("expected '1 file(s)' in status, got: %s", status)
	}
}

func TestCheckpoint_ShellWarning(t *testing.T) {
	app, _ := checkpointTestApp(t)

	app.startCheckpoint()
	app.markCheckpointShell()
	app.endCheckpoint()

	status := app.checkpointStatus()
	if !strings.Contains(status, "⚠ shell") {
		t.Fatalf("expected shell warning in status, got: %s", status)
	}
}

func TestCheckpoint_InvalidRewindN(t *testing.T) {
	app, _ := checkpointTestApp(t)

	// No checkpoints — rewind should error.
	result := app.rewind(1)
	if len(result.Errors) == 0 {
		t.Fatal("expected error for rewind with no checkpoints")
	}
}

func TestCheckpoint_BinaryRoundTrip(t *testing.T) {
	app, dir := checkpointTestApp(t)
	ctx := context.Background()

	// Create a file with arbitrary bytes (including NULs).
	original := []byte{0x00, 0x01, 0x02, 0xFF, 0xFE, 'h', 'e', 'l', 'l', 'o', 0x00}
	p := filepath.Join(dir, "binary.dat")
	os.WriteFile(p, original, 0o644)

	app.startCheckpoint()
	app.captureForCheckpoint(ctx, p)
	app.Exec.WriteFileBytes(ctx, p, []byte("replaced"))
	app.endCheckpoint()

	result := app.rewind(1)
	if len(result.RestoredPaths) != 1 {
		t.Fatalf("expected 1 restored path, got %d: %+v", len(result.RestoredPaths), result)
	}

	got, _ := os.ReadFile(p)
	if string(got) != string(original) {
		t.Fatalf("binary round-trip failed: got %v, want %v", got, original)
	}
}

func TestCheckpoint_CaptureFileOriginalIntegration(t *testing.T) {
	app, dir := checkpointTestApp(t)
	ctx := context.Background()

	p := filepath.Join(dir, "test.txt")
	os.WriteFile(p, []byte("original"), 0o644)

	// Start checkpoint, then call captureFileOriginal (the real hook).
	app.startCheckpoint()
	app.captureFileOriginal(ctx, p)
	app.endCheckpoint()

	// Mutate (after checkpoint ended — simulate turn completed then rewind).
	app.Exec.WriteFile(ctx, p, "changed")

	// Rewind.
	result := app.rewind(1)
	if len(result.RestoredPaths) != 1 {
		t.Fatalf("expected 1 restored, got %d", len(result.RestoredPaths))
	}

	got, _ := app.Exec.ReadFile(ctx, p)
	if got != "original" {
		t.Fatalf("after rewind: got %q, want %q", got, "original")
	}
}

func TestCheckpoint_TooLargeFile(t *testing.T) {
	app, dir := checkpointTestApp(t)
	ctx := context.Background()

	// Create a file larger than checkpointMaxFileSize (1 MB).
	big := make([]byte, checkpointMaxFileSize+1024)
	for i := range big {
		big[i] = byte(i % 256)
	}
	p := filepath.Join(dir, "big.dat")
	os.WriteFile(p, big, 0o644)

	app.startCheckpoint()
	app.captureForCheckpoint(ctx, p)
	app.Exec.WriteFileBytes(ctx, p, []byte("replaced"))
	app.endCheckpoint()

	result := app.rewind(1)

	// Should have 1 TooLarge path.
	if len(result.TooLargePaths) != 1 {
		t.Fatalf("expected 1 too-large path, got %d: %+v", len(result.TooLargePaths), result)
	}

	// File should still be "replaced" (not restored).
	got, _ := app.Exec.ReadFile(ctx, p)
	if got != "replaced" {
		t.Fatalf("too-large file should not be restored, got %q", got)
	}

	// Summary should mention the too-large file.
	summary := result.Summary()
	if !strings.Contains(summary, "could not be restored") {
		t.Fatalf("summary should mention 'could not be restored', got: %s", summary)
	}
}

func TestCheckpoint_CopyOnFirstWrite(t *testing.T) {
	app, dir := checkpointTestApp(t)
	ctx := context.Background()

	p := filepath.Join(dir, "test.txt")
	os.WriteFile(p, []byte("original"), 0o644)

	app.startCheckpoint()

	// First capture — should store "original".
	app.captureForCheckpoint(ctx, p)

	// Mutate to "v1".
	app.Exec.WriteFile(ctx, p, "v1")

	// Second capture — should NOT overwrite (copy-on-first-write).
	app.captureForCheckpoint(ctx, p)

	// Mutate to "v2".
	app.Exec.WriteFile(ctx, p, "v2")

	app.endCheckpoint()

	// Rewind — should restore to "original" (first capture).
	result := app.rewind(1)
	if len(result.RestoredPaths) != 1 {
		t.Fatalf("expected 1 restored, got %d", len(result.RestoredPaths))
	}

	got, _ := app.Exec.ReadFile(ctx, p)
	if got != "original" {
		t.Fatalf("copy-on-first-write failed: got %q, want %q", got, "original")
	}
}