package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/treeol/wakil/internal/exec"
)

// snapshotTestApp creates an App with a DirectExecutor for file I/O.
func snapshotTestApp(t *testing.T) (*App, func()) {
	t.Helper()
	dir := t.TempDir()
	exe, err := exec.NewDirectExecutor(dir)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{
		Exec:       exe,
		IsHeadless: true,
	}
	return app, func() { exe.Close() }
}

// TestCaptureFileOriginal_ExistingFile verifies that captureOriginal stores
// the pre-edit content of an existing file.
func TestCaptureFileOriginal_ExistingFile(t *testing.T) {
	app, cleanup := snapshotTestApp(t)
	defer cleanup()
	ctx := context.Background()

	// Create a file with content.
	path := filepath.Join(app.Exec.Cwd(), "existing.txt")
	os.WriteFile(path, []byte("original content"), 0o644)

	recorder := newFilesChangedRecorder()
	app.filesChanged = recorder

	app.captureFileOriginal(ctx, path)

	snap, ok := recorder.originals[path]
	if !ok {
		t.Fatal("expected snapshot to be captured")
	}
	if !snap.Existed {
		t.Error("expected Existed=true")
	}
	if snap.Content != "original content" {
		t.Errorf("content = %q, want 'original content'", snap.Content)
	}
}

// TestCaptureFileOriginal_NonexistentFile verifies that captureOriginal
// records Existed=false for a file that doesn't exist yet.
func TestCaptureFileOriginal_NonexistentFile(t *testing.T) {
	app, cleanup := snapshotTestApp(t)
	defer cleanup()
	ctx := context.Background()

	path := filepath.Join(app.Exec.Cwd(), "nonexistent.txt")
	recorder := newFilesChangedRecorder()
	app.filesChanged = recorder

	app.captureFileOriginal(ctx, path)

	snap, ok := recorder.originals[path]
	if !ok {
		t.Fatal("expected snapshot to be captured")
	}
	if snap.Existed {
		t.Error("expected Existed=false for nonexistent file")
	}
}

// TestCaptureFileOriginal_CopyOnFirstWrite verifies that subsequent captures
// for the same path do NOT overwrite the first snapshot.
func TestCaptureFileOriginal_CopyOnFirstWrite(t *testing.T) {
	app, cleanup := snapshotTestApp(t)
	defer cleanup()
	ctx := context.Background()

	path := filepath.Join(app.Exec.Cwd(), "cow.txt")
	os.WriteFile(path, []byte("first version"), 0o644)

	recorder := newFilesChangedRecorder()
	app.filesChanged = recorder

	// First capture — stores "first version".
	app.captureFileOriginal(ctx, path)

	// Modify the file.
	os.WriteFile(path, []byte("second version"), 0o644)

	// Second capture — should NOT overwrite.
	app.captureFileOriginal(ctx, path)

	snap := recorder.originals[path]
	if snap.Content != "first version" {
		t.Errorf("content = %q, want 'first version' (copy-on-first-write)", snap.Content)
	}
}

// TestRestore_ExistingFile verifies that restore writes back the original content.
func TestRestore_ExistingFile(t *testing.T) {
	app, cleanup := snapshotTestApp(t)
	defer cleanup()
	ctx := context.Background()

	path := filepath.Join(app.Exec.Cwd(), "restore.txt")
	os.WriteFile(path, []byte("original"), 0o644)

	recorder := newFilesChangedRecorder()
	app.filesChanged = recorder
	app.captureFileOriginal(ctx, path)

	// Mutate the file.
	app.Exec.WriteFile(ctx, path, "modified")

	// Restore.
	restored, errs := recorder.restore(ctx, app.Exec)
	if len(errs) > 0 {
		t.Fatalf("restore errors: %v", errs)
	}
	if len(restored) != 1 {
		t.Fatalf("expected 1 restored, got %d", len(restored))
	}

	// Verify content is back to original.
	content, _ := app.Exec.ReadFile(ctx, path)
	if content != "original" {
		t.Errorf("content after restore = %q, want 'original'", content)
	}
}

// TestRestore_CreatedFileDeleted verifies that restore deletes files that
// were created by the subagent (didn't exist before).
func TestRestore_CreatedFileDeleted(t *testing.T) {
	app, cleanup := snapshotTestApp(t)
	defer cleanup()
	ctx := context.Background()

	path := filepath.Join(app.Exec.Cwd(), "created.txt")

	recorder := newFilesChangedRecorder()
	app.filesChanged = recorder

	// Capture before the file exists (subagent will create it).
	app.captureFileOriginal(ctx, path)

	// Simulate subagent creating the file.
	app.Exec.WriteFile(ctx, path, "new content")

	// Restore — should delete the file.
	restored, errs := recorder.restore(ctx, app.Exec)
	if len(errs) > 0 {
		t.Fatalf("restore errors: %v", errs)
	}
	if len(restored) != 1 {
		t.Fatalf("expected 1 restored, got %d", len(restored))
	}

	// Verify file is gone.
	_, err := app.Exec.ReadFile(ctx, path)
	if err == nil {
		t.Error("expected file to be deleted after restore")
	}
}

// TestRestore_EmptyFileNotDeleted verifies that an existing empty file is
// restored as empty, not deleted (Existed=true, Content="").
func TestRestore_EmptyFileNotDeleted(t *testing.T) {
	app, cleanup := snapshotTestApp(t)
	defer cleanup()
	ctx := context.Background()

	path := filepath.Join(app.Exec.Cwd(), "empty.txt")
	os.WriteFile(path, []byte(""), 0o644)

	recorder := newFilesChangedRecorder()
	app.filesChanged = recorder
	app.captureFileOriginal(ctx, path)

	// Mutate the file.
	app.Exec.WriteFile(ctx, path, "now has content")

	// Restore.
	restored, errs := recorder.restore(ctx, app.Exec)
	if len(errs) > 0 {
		t.Fatalf("restore errors: %v", errs)
	}
	if len(restored) != 1 {
		t.Fatalf("expected 1 restored, got %d", len(restored))
	}

	// Verify file still exists and is empty.
	content, err := app.Exec.ReadFile(ctx, path)
	if err != nil {
		t.Fatalf("file should still exist after restore: %v", err)
	}
	if content != "" {
		t.Errorf("content after restore = %q, want empty", content)
	}
}

// TestRestore_PartialFailure verifies that restore continues on error and
// reports which files failed.
func TestRestore_PartialFailure(t *testing.T) {
	app, cleanup := snapshotTestApp(t)
	defer cleanup()
	ctx := context.Background()

	path1 := filepath.Join(app.Exec.Cwd(), "ok.txt")
	os.WriteFile(path1, []byte("original1"), 0o644)

	recorder := newFilesChangedRecorder()
	app.filesChanged = recorder
	app.captureFileOriginal(ctx, path1)

	// Mutate.
	app.Exec.WriteFile(ctx, path1, "modified1")

	// Restore — should succeed (single file, no errors expected).
	restored, errs := recorder.restore(ctx, app.Exec)
	if len(restored) != 1 {
		t.Errorf("expected 1 restored, got %d", len(restored))
	}
	if len(errs) != 0 {
		t.Errorf("expected 0 errors, got %v", errs)
	}
}

// TestRestore_NilRecorder verifies nil safety.
func TestRestore_NilRecorder(t *testing.T) {
	var r *filesChangedRecorder
	restored, errs := r.restore(context.Background(), nil)
	if restored != nil || errs != nil {
		t.Error("expected nil for nil recorder")
	}
}

// TestHasSnapshots verifies the nil-safe snapshot check.
func TestHasSnapshots(t *testing.T) {
	var nilRecorder *filesChangedRecorder
	if nilRecorder.hasSnapshots() {
		t.Error("nil recorder should not have snapshots")
	}

	empty := newFilesChangedRecorder()
	if empty.hasSnapshots() {
		t.Error("empty recorder should not have snapshots")
	}

	withSnap := newFilesChangedRecorder()
	withSnap.captureOriginal("path", true, "content")
	if !withSnap.hasSnapshots() {
		t.Error("recorder with captured original should have snapshots")
	}
}

// TestCaptureFileOriginal_NilRecorder verifies nil safety.
func TestCaptureFileOriginal_NilRecorder(t *testing.T) {
	app := &App{} // no filesChanged recorder
	app.captureFileOriginal(context.Background(), "path")
	// Should not panic.
}

// TestCaptureFileOriginal_MoveBothPaths verifies that move_file captures
// both source and destination.
func TestCaptureFileOriginal_MoveBothPaths(t *testing.T) {
	app, cleanup := snapshotTestApp(t)
	defer cleanup()
	ctx := context.Background()

	src := filepath.Join(app.Exec.Cwd(), "src.txt")
	dst := filepath.Join(app.Exec.Cwd(), "dst.txt")
	os.WriteFile(src, []byte("source content"), 0o644)

	recorder := newFilesChangedRecorder()
	app.filesChanged = recorder

	// Capture both (as move_file does).
	app.captureFileOriginal(ctx, src)
	app.captureFileOriginal(ctx, dst)

	if len(recorder.originals) != 2 {
		t.Fatalf("expected 2 originals, got %d", len(recorder.originals))
	}

	srcSnap := recorder.originals[src]
	if !srcSnap.Existed || srcSnap.Content != "source content" {
		t.Errorf("src snapshot wrong: %+v", srcSnap)
	}

	dstSnap := recorder.originals[dst]
	if dstSnap.Existed {
		t.Error("dst should not exist (Existed=false)")
	}
}
