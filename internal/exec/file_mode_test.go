package exec

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestDirectExecutorFileModeChmod(t *testing.T) {
	dir := t.TempDir()
	ex, err := NewDirectExecutor(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p := filepath.Join(dir, "s.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var fm FileModer = ex
	if err := fm.Chmod(ctx, p, 0o755); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	got, err := fm.FileMode(ctx, p)
	if err != nil || got != 0o755 {
		t.Fatalf("FileMode = %o, %v; want 755", got, err)
	}
	if _, err := fm.FileMode(ctx, filepath.Join(dir, "missing")); !errors.Is(err, ErrFileNotFound) {
		t.Errorf("FileMode(missing) err = %v, want ErrFileNotFound", err)
	}
}

func TestParseRawMode(t *testing.T) {
	cases := map[string]os.FileMode{
		"81ed": 0o755,                    // regular 0755
		"81a4": 0o644,                    // regular 0644
		"41ed": os.ModeDir | 0o755,       // directory
		"11a4": os.ModeIrregular | 0o644, // FIFO
	}
	for in, want := range cases {
		if got, err := parseRawMode(in); err != nil || got != want {
			t.Errorf("parseRawMode(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := parseRawMode("rwx"); err == nil {
		t.Error("parseRawMode(rwx) should fail")
	}
}

func TestDirectExecutorFileModeTypes(t *testing.T) {
	dir := t.TempDir()
	ex, _ := NewDirectExecutor(dir)
	ctx := context.Background()
	if m, err := ex.FileMode(ctx, dir); err != nil || m&os.ModeDir == 0 {
		t.Errorf("dir mode = %v, %v", m, err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := mkfifo(fifo); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	if m, err := ex.FileMode(ctx, fifo); err != nil || m&os.ModeIrregular == 0 {
		t.Errorf("fifo mode = %v, %v", m, err)
	}
}

// TestDirectExecutorConfinePathSymlinkedParent: a non-existent file beneath a
// symlinked directory pointing outside the workspace must be rejected (the
// old implementation skipped symlink resolution for non-existent paths).
func TestDirectExecutorConfinePathSymlinkedParent(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	ex, err := NewDirectExecutor(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ws, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "nope"), filepath.Join(ws, "dangling")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(ws, "inner"), filepath.Join(ws, "inlink")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cases := []struct {
		path    string
		wantErr bool
	}{
		{"link/new/file.txt", true},
		{"link/file.txt", true},
		{"dangling", true},
		{"dangling/x", true},
		{"inlink/new/file.txt", false},
		{"fresh/dir/file.txt", false},
	}
	for _, c := range cases {
		got, err := ex.ConfinePath(ctx, c.path)
		if (err != nil) != c.wantErr {
			t.Errorf("ConfinePath(%q) = %q, %v; wantErr %v", c.path, got, err, c.wantErr)
			continue
		}
		if err == nil && strings.HasPrefix(got, outside) {
			t.Errorf("ConfinePath(%q) = %q escapes to %q", c.path, got, outside)
		}
	}
}

func mkfifo(path string) error { return syscall.Mkfifo(path, 0o644) }

func TestDirectExecutorEntryExists(t *testing.T) {
	dir := t.TempDir()
	ex, _ := NewDirectExecutor(dir)
	ctx := context.Background()
	if err := os.Symlink(filepath.Join(dir, "nowhere"), filepath.Join(dir, "dangling ")); err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{"dangling ": true, "dangling": false, "missing/deeper": false, ".": true}
	for p, want := range cases {
		got, err := ex.EntryExists(ctx, filepath.Join(dir, p))
		if err != nil || got != want {
			t.Errorf("EntryExists(%q) = %v, %v; want %v", p, got, err, want)
		}
	}
}
