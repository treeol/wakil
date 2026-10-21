package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// SweepToolCache removes per-chat tool-cache directories whose newest regular
// file is older than retention, and returns how many directories it removed.
//
// Scope and safety:
//   - Only DIRECT children of root are considered; each must be a real
//     directory (symlinks are skipped, never followed).
//   - Freshness is the newest mtime among the directory's regular files (the
//     directory's own mtime only changes on add/remove, and spill files are
//     written once). An empty directory falls back to its own mtime.
//   - The tool cache is flat (spill writers create files directly in the
//     per-chat dir). A directory that contains ANY non-regular entry
//     (subdirectory, symlink, …) is an unknown layout and is kept.
//   - Directories named in keep (active chat IDs) are never removed.
//   - retention <= 0 disables the sweep entirely.
//   - Removal failures do not stop the sweep; they are aggregated into the
//     returned error alongside the count of directories actually removed.
//
// Consequence (accepted): a session older than the retention window that is
// resumed in a LATER process (or via /resume after startup) may hold "full
// content at: PATH" pointers whose file is gone; reads of such paths fail with
// a not-found error, as for any missing spill. Callers exempt the chat they
// know will be resumed at startup via keep.
func SweepToolCache(root string, retention time.Duration, now time.Time, keep map[string]bool) (int, error) {
	if root == "" || retention <= 0 {
		return 0, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	cutoff := now.Add(-retention)
	removed := 0
	var errs []error
	for _, e := range entries {
		if keep[e.Name()] {
			continue
		}
		// ReadDir reports the entry type without following symlinks.
		if !e.IsDir() || e.Type()&os.ModeSymlink != 0 {
			continue
		}
		dir := filepath.Join(root, e.Name())
		newest, ok := newestMtime(dir)
		if !ok || newest.After(cutoff) {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", e.Name(), err))
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

// newestMtime returns the newest modification time among the regular files
// directly inside dir (falling back to dir's own mtime when it has none).
// ok=false when dir cannot be inspected or contains anything other than
// regular files — the caller then keeps it.
func newestMtime(dir string) (time.Time, bool) {
	info, err := os.Lstat(dir)
	if err != nil {
		return time.Time{}, false
	}
	newest := info.ModTime()
	files, err := os.ReadDir(dir)
	if err != nil {
		return time.Time{}, false
	}
	var sawFile bool
	for _, f := range files {
		if !f.Type().IsRegular() {
			return time.Time{}, false // unknown layout: keep
		}
		fi, err := f.Info()
		if err != nil {
			return time.Time{}, false
		}
		if !sawFile || fi.ModTime().After(newest) {
			newest = fi.ModTime()
			sawFile = true
		}
	}
	return newest, true
}
