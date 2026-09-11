package agent

// repomap.go — Repo map: lightweight file-tree outline (card #190).
//
// Builds a compact directory-tree outline of the workspace so the agent
// knows the repo layout on the first turn without burning discovery
// subagents. The outline is:
//
//   - Built on each preamble refresh (once per session start or day rollover).
//     v1 does NOT cache on disk or do incremental mtime-based refresh — it
//     rebuilds each time the preamble is rebuilt, which is at most once per
//     day. This is acceptable because the walk is fast (bounded depth, bounded
//     visit count, 5s timeout).
//   - Injected into the preamble as a day-stable summary line (like the
//     memory digest) — a one-line "Repo map: N files, M dirs" pointer,
//     NOT the full outline (which would bloat the prefix).
//   - The full outline is spilled to the tool cache so the agent can
//     read_file it on demand for detail.
//   - Bounded: depth-limited (4 levels: depth 0–3), noise directories skipped
//     (.git, node_modules, vendor, etc.), byte-capped at 32KB, visit-capped
//     at 500 directory listings. Preamble injection uses a 5s timeout;
//     the /repomap command uses a longer 30s timeout for manual rebuilds.
//
// Design decision: directory-tree outline, NOT symbol-level. Symbol-level
// indexing (via LSP lsp_symbols) is too slow for a 50k-LOC repo on first
// turn (LSP server needs to index first). The file-tree outline gives the
// agent enough to know "where things are" without LSP. The agent can use
// lsp_symbols for symbol-level detail once it knows which directory to
// look in.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	wtools "github.com/treeol/wakil/internal/tools"
)

// repoMapMaxDepth is the maximum directory depth to walk (root = depth 0).
const repoMapMaxDepth = 3

// repoMapMaxBytes caps the full outline size. 32KB is enough for a 50k-LOC
// repo's directory tree at depth 3 without flooding context.
const repoMapMaxBytes = 32 * 1024

// repoMapMaxVisits caps the total number of ListDir calls. This bounds the
// traversal work even on a monorepo with thousands of directories.
const repoMapMaxVisits = 500

// repoMapNoiseDirs are directories that are never included in the outline.
// Dot-prefixed directories are excluded separately by the HasPrefix check
// in walkLevel, so only non-dot noise directories are listed here.
var repoMapNoiseDirs = map[string]bool{
	"node_modules": true, "vendor": true, "__pycache__": true,
	"dist": true, "build": true, "target": true,
	"tmp": true, "coverage": true,
}

// repoMapNoiseSuffixes are file suffixes skipped in the outline.
var repoMapNoiseSuffixes = []string{
	".pyc", ".pyo", ".class", ".o", ".a", ".so", ".dylib", ".dll",
	".exe", ".bin", ".dat", ".db", ".sqlite", ".lock",
}

// RepoMapResult holds the built repo map.
type RepoMapResult struct {
	Outline   string // full outline (spilled to cache for read_file access)
	DirCount  int
	FileCount int
	Partial   bool // true if the walk was cut short by timeout or visit cap
}

// BuildRepoMap walks the workspace directory tree and builds a compact
// outline. Uses the executor for sandbox-safe directory listing.
//
// The outline is a tree of directory names with file names, e.g.:
//
//	cmd/
//	  wakil/
//	    main.go, app_builder.go, …
//	internal/
//	  agent/
//	    app.go, commands.go, …
//	  config/
//	README.md, go.mod, Makefile
func BuildRepoMap(ctx context.Context, exe fileLister) (*RepoMapResult, error) {
	if exe == nil {
		return nil, fmt.Errorf("no executor available")
	}

	var outline strings.Builder
	dirCount := 0
	fileCount := 0
	visits := 0
	partial := false

	err := walkLevel(ctx, exe, ".", 0, &outline, &dirCount, &fileCount, &visits, &partial)
	if err != nil {
		return nil, fmt.Errorf("walk repo: %w", err)
	}

	// Check if context was cancelled during the walk.
	if ctxErr := ctx.Err(); ctxErr != nil {
		partial = true
	}

	// Cap the outline at repoMapMaxBytes.
	outlineStr := outline.String()
	if len(outlineStr) > repoMapMaxBytes {
		outlineStr = truncateUTF8(outlineStr[:repoMapMaxBytes], repoMapMaxBytes)
		outlineStr += fmt.Sprintf("\n… [repo map truncated at %d bytes]", repoMapMaxBytes)
	}

	return &RepoMapResult{
		Outline:   outlineStr,
		DirCount:  dirCount,
		FileCount: fileCount,
		Partial:   partial,
	}, nil
}

// walkLevel lists one directory, writes its files, and recurses into subdirs.
// Depth 0 = root. Stops when depth exceeds repoMapMaxDepth, visit cap is
// reached, or context is cancelled.
func walkLevel(ctx context.Context, exe fileLister, dirPath string, depth int, out *strings.Builder, dirCount, fileCount, visits *int, partial *bool) error {
	if depth > repoMapMaxDepth {
		return nil
	}
	if *visits >= repoMapMaxVisits {
		*partial = true
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		*partial = true
		return nil
	}

	*visits++
	listing, err := exe.ListDir(ctx, dirPath)
	if err != nil {
		if depth == 0 {
			return fmt.Errorf("list root: %w", err)
		}
		return nil // skip directories we can't read (non-root)
	}

	// Parse the listing into dirs and files.
	var dirs, files []string
	for _, line := range strings.Split(listing, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasSuffix(line, "/") {
			name := strings.TrimSuffix(line, "/")
			if !repoMapNoiseDirs[name] && !strings.HasPrefix(name, ".") {
				dirs = append(dirs, name)
			}
		} else {
			if !isNoiseFile(line) {
				files = append(files, line)
			}
		}
	}
	sort.Strings(dirs)
	sort.Strings(files)

	indent := strings.Repeat("  ", depth)

	// Write files at this level.
	if len(files) > 0 {
		maxShow := 8
		if depth > 0 {
			maxShow = 6
		}
		shown := files
		if len(shown) > maxShow {
			shown = shown[:maxShow]
		}
		out.WriteString(indent + strings.Join(shown, ", "))
		if len(files) > maxShow {
			out.WriteString(fmt.Sprintf(", … %d more", len(files)-maxShow))
		}
		out.WriteString("\n")
		*fileCount += len(files)
	}

	// Recurse into subdirectories.
	for _, dir := range dirs {
		*dirCount++
		out.WriteString(indent + dir + "/\n")
		subPath := dir
		if dirPath != "." {
			subPath = dirPath + "/" + dir
		}
		walkLevel(ctx, exe, subPath, depth+1, out, dirCount, fileCount, visits, partial)
	}

	return nil
}

// isNoiseFile returns true for files that should be excluded from the outline.
func isNoiseFile(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	for _, suffix := range repoMapNoiseSuffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// injectRepoMap builds the repo map and injects a summary line into the
// preamble parts. The full outline is spilled to cache.
// Returns the summary line (or "" if no map could be built).
func injectRepoMap(ctx context.Context, exe fileLister, chatID string) string {
	result, err := BuildRepoMap(ctx, exe)
	if err != nil || result == nil {
		return ""
	}

	// Spill the full outline to cache so the agent can read_file it.
	spillPath := wtools.SpillToCache(chatID, "repo_map", result.Outline)
	if spillPath == "" {
		// Fallback: just use the counts without a pointer.
		line := fmt.Sprintf("Repo map: %d files in %d directories (depth ≤%d).",
			result.FileCount, result.DirCount, repoMapMaxDepth)
		if result.Partial {
			line += " (partial — some directories skipped)"
		}
		return line
	}

	line := fmt.Sprintf("Repo map: %d files in %d directories (depth ≤%d). "+
		"Full outline at: %s",
		result.FileCount, result.DirCount, repoMapMaxDepth, spillPath)
	if result.Partial {
		line += " (partial — some directories skipped)"
	}
	return line
}

// handleRepoMapCommand handles the /repomap slash command. It rebuilds the
// repo map and returns a summary with a preview.
func handleRepoMapCommand(ctx context.Context, app *App) (string, error) {
	if app == nil || app.Exec == nil {
		return "", fmt.Errorf("no executor available")
	}

	repoCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	result, err := BuildRepoMap(repoCtx, app.Exec)
	if err != nil {
		return "", fmt.Errorf("build repo map: %w", err)
	}

	spillPath := wtools.SpillToCache(app.chatID(), "repo_map", result.Outline)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Repo map: %d files in %d directories (depth ≤%d).",
		result.FileCount, result.DirCount, repoMapMaxDepth))
	if result.Partial {
		sb.WriteString(" (partial — some directories skipped)")
	}
	sb.WriteString("\n\n")
	if spillPath != "" {
		sb.WriteString(fmt.Sprintf("Full outline at: %s\n\n", spillPath))
	}
	// Show the first 2KB of the outline as a preview.
	preview := result.Outline
	if len(preview) > 2048 {
		preview = truncateUTF8(preview[:2048], 2048) + "…"
	}
	sb.WriteString(preview)
	return sb.String(), nil
}
