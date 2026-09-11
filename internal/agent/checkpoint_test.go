package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/exec"
	"github.com/treeol/wakil/internal/proxy"
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

func TestCheckpoint_UnknownPathSkipped(t *testing.T) {
	app, dir := checkpointTestApp(t)
	ctx := context.Background()

	// Create a directory at the capture path — DirectExecutor's ReadFile uses
	// os.ReadFile, which returns EISDIR ("is a directory"), NOT a not-found
	// error. This triggers the Unknown path (non-not-found read error).
	p := filepath.Join(dir, "unreadable")
	if err := os.Mkdir(p, 0o755); err != nil {
		t.Fatal(err)
	}

	app.startCheckpoint()
	app.captureForCheckpoint(ctx, p)
	app.endCheckpoint()

	// Verify it was captured as Unknown.
	app.cpMu.Lock()
	cp := app.activeCheckpointLocked()
	var snap FileSnapshot
	if cp != nil {
		if s, ok := cp.Files[p]; ok {
			snap = s
		}
	}
	app.cpMu.Unlock()

	if !snap.Unknown {
		t.Fatalf("expected Unknown=true for unreadable path, got %+v", snap)
	}

	// Rewind should skip it (not delete, not restore, no errors).
	result := app.rewind(1)
	if len(result.UnknownPaths) != 1 {
		t.Fatalf("expected 1 unknown path in rewind, got %d: %+v", len(result.UnknownPaths), result)
	}
	if len(result.RestoredPaths) != 0 {
		t.Fatalf("expected 0 restored paths, got %d", len(result.RestoredPaths))
	}
	if len(result.DeletedPaths) != 0 {
		t.Fatalf("expected 0 deleted paths, got %d", len(result.DeletedPaths))
	}
	if len(result.Errors) != 0 {
		t.Fatalf("expected 0 errors, got %d: %+v", len(result.Errors), result.Errors)
	}

	// The directory should still exist (rewind skips unknown paths).
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("unknown path should still exist after rewind: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("path should still be a directory, got mode %v", info.Mode())
	}
}

func TestCheckpoint_RewindWhileActive(t *testing.T) {
	app, dir := checkpointTestApp(t)
	ctx := context.Background()

	p := filepath.Join(dir, "test.txt")
	os.WriteFile(p, []byte("original"), 0o644)

	// Start checkpoint but DON'T end it — simulates a turn in progress.
	app.startCheckpoint()
	app.captureForCheckpoint(ctx, p)
	app.Exec.WriteFile(ctx, p, "modified")

	// Attempt rewind — should fail with "cannot rewind while a turn is in progress".
	result := app.rewind(1)
	if len(result.Errors) == 0 {
		t.Fatal("expected error for rewind while checkpoint is active")
	}
	if !strings.Contains(result.Errors[0], "in progress") {
		t.Fatalf("expected 'in progress' error, got: %s", result.Errors[0])
	}
	if result.TurnsRewound != 0 {
		t.Fatalf("expected 0 turns rewound, got %d", result.TurnsRewound)
	}

	// Verify state is unchanged: checkpoint still exists, file still modified.
	app.cpMu.Lock()
	cpCount := len(app.checkpoints)
	app.cpMu.Unlock()
	if cpCount != 1 {
		t.Fatalf("expected 1 checkpoint (rewind rejected), got %d", cpCount)
	}
	got, _ := app.Exec.ReadFile(ctx, p)
	if got != "modified" {
		t.Fatalf("file should still be 'modified' after rejected rewind, got %q", got)
	}

	// End the turn and verify rewind still works.
	app.endCheckpoint()
	result2 := app.rewind(1)
	if len(result2.RestoredPaths) != 1 {
		t.Fatalf("after ending turn, expected 1 restored path, got %d: %+v", len(result2.RestoredPaths), result2)
	}
	got2, _ := app.Exec.ReadFile(ctx, p)
	if got2 != "original" {
		t.Fatalf("file should be restored to 'original', got %q", got2)
	}
}

func TestCheckpoint_ConvTruncated(t *testing.T) {
	app, _ := checkpointTestApp(t)

	// Populate Conv with just a user message (as startCheckpoint is called
	// after the user message is appended but before the assistant responds).
	userMsg := "hello"
	app.Conv = []proxy.Message{
		{Role: "user", Content: &userMsg},
	}

	// Start checkpoint — ConvLen should be 1 (just the user message).
	app.startCheckpoint()

	// Simulate an assistant response being appended.
	assistantMsg := "I will help."
	app.Conv = append(app.Conv, proxy.Message{Role: "assistant", Content: &assistantMsg})
	// Also simulate a tool result.
	toolMsg := "tool result"
	app.Conv = append(app.Conv, proxy.Message{Role: "tool", Content: &toolMsg})

	app.endCheckpoint()

	// Rewind should truncate Conv back to ConvLen=1 (just the user message).
	result := app.rewind(1)
	if !result.ConvTruncated {
		t.Fatal("expected ConvTruncated=true")
	}

	app.convMu.RLock()
	convLen := len(app.Conv)
	firstRole := ""
	firstContent := ""
	if convLen > 0 {
		firstRole = app.Conv[0].Role
		firstContent = DerefStr(app.Conv[0].Content)
	}
	app.convMu.RUnlock()
	if convLen != 1 {
		t.Fatalf("expected Conv length 1 after rewind, got %d", convLen)
	}
	if firstRole != "user" {
		t.Fatalf("expected retained message role 'user', got %q", firstRole)
	}
	if firstContent != "hello" {
		t.Fatalf("expected retained message content 'hello', got %q", firstContent)
	}
}

func TestCheckpoint_ConvNotTruncatedStaleBoundary(t *testing.T) {
	app, _ := checkpointTestApp(t)

	// Populate Conv with only an assistant message (no user message at ConvLen-1).
	assistantMsg := "response"
	app.Conv = []proxy.Message{
		{Role: "assistant", Content: &assistantMsg},
	}

	// Start checkpoint — ConvLen=1, but Conv[0] is "assistant", not "user".
	app.startCheckpoint()
	app.endCheckpoint()

	// Append more messages to verify they survive rewind (no truncation).
	extraMsg := "extra"
	app.Conv = append(app.Conv, proxy.Message{Role: "user", Content: &extraMsg})

	// Rewind should NOT truncate (boundary is invalid — Conv[0] is not "user").
	result := app.rewind(1)
	if result.ConvTruncated {
		t.Fatal("expected ConvTruncated=false for stale boundary")
	}

	// Verify Conv is completely unchanged (both original + appended messages).
	app.convMu.RLock()
	convLen := len(app.Conv)
	app.convMu.RUnlock()
	if convLen != 2 {
		t.Fatalf("expected Conv length 2 (unchanged), got %d", convLen)
	}
}

func TestCheckpoint_EvictionByCount(t *testing.T) {
	app, dir := checkpointTestApp(t)
	ctx := context.Background()

	// Create more checkpoints than checkpointMaxKeep (20).
	// Each turn captures a different file so they have content.
	for i := 0; i < checkpointMaxKeep+5; i++ {
		p := filepath.Join(dir, fmt.Sprintf("file_%d.txt", i))
		os.WriteFile(p, []byte("content"), 0o644)
		app.startCheckpoint()
		app.captureForCheckpoint(ctx, p)
		app.endCheckpoint()
	}

	app.cpMu.Lock()
	count := len(app.checkpoints)
	firstTurn := -1
	lastTurn := -1
	if count > 0 {
		firstTurn = app.checkpoints[0].TurnIndex
		lastTurn = app.checkpoints[len(app.checkpoints)-1].TurnIndex
	}
	totalBytes := app.cpTotalBytes
	app.cpMu.Unlock()

	if count != checkpointMaxKeep {
		t.Fatalf("expected exactly %d checkpoints, got %d", checkpointMaxKeep, count)
	}

	// FIFO eviction: oldest 5 should be gone. First surviving turn is turn 6.
	expectedFirst := 6
	if firstTurn != expectedFirst {
		t.Fatalf("expected first surviving TurnIndex=%d (FIFO), got %d", expectedFirst, firstTurn)
	}
	// Last surviving turn is the most recent one created.
	expectedLast := checkpointMaxKeep + 5
	if lastTurn != expectedLast {
		t.Fatalf("expected last surviving TurnIndex=%d, got %d", expectedLast, lastTurn)
	}

	// Each checkpoint captured one file with "content" (7 bytes).
	expectedBytes := checkpointMaxKeep * len("content")
	if totalBytes != expectedBytes {
		t.Fatalf("expected cpTotalBytes=%d, got %d", expectedBytes, totalBytes)
	}
}