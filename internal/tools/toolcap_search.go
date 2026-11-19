package tools

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Limits of SearchHostCache, matching the spirit of the shell grep it stands
// in for (a capped, bounded result).
const (
	hostSearchMaxMatches = 200
	hostSearchMaxFile    = 16 << 20 // bytes read per file
	hostSearchMaxLine    = 400      // bytes kept per matching line
)

// validChatSegment reports whether chatID is usable as ONE directory name
// under the toolcache root (no separators, not "." or "..").
func validChatSegment(chatID string) bool {
	return chatID != "" && chatID != "." && chatID != ".." &&
		!strings.ContainsAny(chatID, `/\`) && filepath.Base(chatID) == chatID
}

// OwnToolCachePath reports whether path LEXICALLY lies in (or is) the toolcache
// directory of chatID — this session's own spill and archive files — and not
// elsewhere under the toolcache root. Like IsToolCacheHostPath it is a
// classifier only; SearchHostCache re-verifies containment with os.Root.
func OwnToolCachePath(chatID, path string) bool {
	if !validChatSegment(chatID) || path == "" {
		return false
	}
	dir := toolCacheDir(chatID)
	if dir == "" {
		return false
	}
	dir = filepath.Clean(dir)
	p := filepath.Clean(path)
	return p == dir || strings.HasPrefix(p, dir+string(filepath.Separator))
}

// SearchHostCache is search_files for this session's own toolcache directory:
// a grep -rn -I style search done on the host, because the sandbox cannot see
// these files. It reads ONLY under toolcache/<chatID>/ — the directory is opened
// as an os.Root, so a path (or a symlink inside it) leading to another chat's
// directory or anywhere else fails. path may be the directory or one file in
// it. pattern is a Go regular expression, or a literal string when it does not
// compile (grep's basic-regex dialect is not reproduced); filePattern is an
// optional glob on the file name (grep --include). The result is
// "path:line:text" lines, "" for no match.
func SearchHostCache(chatID, path, pattern, filePattern string, caseInsensitive bool) (string, error) {
	if !OwnToolCachePath(chatID, path) {
		return "", fmt.Errorf("%w: %s", ErrHostCacheEscape, path)
	}
	dir := filepath.Clean(toolCacheDir(chatID))
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(dir, filepath.Clean(path))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %s", ErrHostCacheEscape, path)
	}
	re, err := compileSearchPattern(pattern, caseInsensitive)
	if err != nil {
		return "", err
	}
	if filePattern != "" {
		if _, err := filepath.Match(filePattern, ""); err != nil {
			return "", fmt.Errorf("bad file_pattern %q: %w", filePattern, err)
		}
	}

	r, err := os.OpenRoot(resolvedDir)
	if err != nil {
		return "", err
	}
	defer r.Close()
	fsys := r.FS()
	start := filepath.ToSlash(rel)

	var out strings.Builder
	matches := 0
	capped := false
	searchFile := func(name string) error {
		if filePattern != "" {
			if ok, _ := filepath.Match(filePattern, filepath.Base(name)); !ok {
				return nil
			}
		}
		f, err := fsys.Open(name)
		if err != nil {
			return nil // unreadable entry: skipped, like grep without -s noise
		}
		data, err := io.ReadAll(io.LimitReader(f, hostSearchMaxFile))
		f.Close()
		if err != nil {
			return nil
		}
		if bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
			return nil // binary (grep -I)
		}
		shown := filepath.Join(dir, filepath.FromSlash(name))
		for i, line := range strings.Split(string(data), "\n") {
			if !re.MatchString(line) {
				continue
			}
			if matches == hostSearchMaxMatches {
				capped = true
				return fs.SkipAll
			}
			matches++
			if len(line) > hostSearchMaxLine {
				line = truncateInvalidUTF8Suffix(line[:hostSearchMaxLine]) + "…"
			}
			fmt.Fprintf(&out, "%s:%d:%s\n", shown, i+1, line)
		}
		return nil
	}

	fi, err := fs.Stat(fsys, start)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		if !fi.Mode().IsRegular() {
			return "", fmt.Errorf("%w: %s", ErrHostCacheNotRegular, path)
		}
		if err := searchFile(start); err != nil && err != fs.SkipAll {
			return "", err
		}
	} else {
		werr := fs.WalkDir(fsys, start, func(name string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if !d.Type().IsRegular() { // directories recurse; symlinks and specials are not followed
				return nil
			}
			return searchFile(name)
		})
		if werr != nil && werr != fs.SkipAll {
			return "", werr
		}
	}
	res := strings.TrimRight(out.String(), "\n")
	if capped {
		res += fmt.Sprintf("\n… [capped at %d matches — narrow the pattern or path]", hostSearchMaxMatches)
	}
	return res, nil
}

// compileSearchPattern compiles pattern as a regular expression, falling back
// to a literal match when it is not a valid one.
func compileSearchPattern(pattern string, caseInsensitive bool) (*regexp.Regexp, error) {
	prefix := ""
	if caseInsensitive {
		prefix = "(?i)"
	}
	if re, err := regexp.Compile(prefix + pattern); err == nil {
		return re, nil
	}
	return regexp.Compile(prefix + regexp.QuoteMeta(pattern))
}
