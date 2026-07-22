package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ExtractSpillPath returns the disk path embedded by CapToolResult,
// StubToolResult, or SpillFullResult in their trailing "… at: PATH]" note,
// or "". It matches only when a known marker prefix sits inside the final
// bracketed segment of the string — so arbitrary " at: " or even
// "full content at: /path]" text in file content does not produce false
// positives.
//
// Handled formats (all at end of string):
//
//	CapToolResult:    "… [+N chars omitted — full content at: PATH]"
//	StubToolResult:   "[budget — N chars at: PATH]"
//	SpillFullResult:  "[full content at: PATH]"
//	MakeEvictionStub: "[evicted — N chars — full content at: PATH]"
func ExtractSpillPath(content string) string {
	// Find the last ']' — it must be the last non-space character of the string
	// for the marker to be a genuine trailing segment.
	trimmed := strings.TrimRight(content, " \t\r\n")
	if !strings.HasSuffix(trimmed, "]") {
		return ""
	}
	// Find the matching '[' that opens this final bracketed segment.
	closeIdx := len(trimmed) - 1
	openIdx := strings.LastIndex(trimmed[:closeIdx], "[")
	if openIdx < 0 {
		return ""
	}
	segment := trimmed[openIdx+1 : closeIdx] // content between [ and ]

	// The segment must start with one of the known prefixes. This is the
	// anchoring that prevents false positives from file body text — only
	// a real Wakil marker at the end of the string matches.
	knownPrefixes := []string{
		"full content at: ",
		"budget — ",
		"+",
		"evicted — ",
		"pre-send trim — ",
		"subagent summary at: ",
	}
	matched := false
	for _, p := range knownPrefixes {
		if strings.HasPrefix(segment, p) {
			matched = true
			break
		}
	}
	if !matched {
		return ""
	}

	// Extract the path: find " at: " inside the segment and take the rest.
	atIdx := strings.LastIndex(segment, " at: ")
	if atIdx < 0 {
		return ""
	}
	path := segment[atIdx+len(" at: "):]
	if path == "" {
		return ""
	}
	return path
}

// MakeEvictionStub replaces a large tool result with a single-line stub that
// records the original size and (when available) the spill-cache path so the
// model can read_file the path if it ever needs the full content again.
func MakeEvictionStub(toolName, content string) string {
	n := len(content)
	if path := ExtractSpillPath(content); path != "" {
		return fmt.Sprintf("[evicted — %d chars — full content at: %s]", n, path)
	}
	return fmt.Sprintf("[evicted — %d chars]", n)
}

// toolCacheBase resolves the wakil data directory (sibling to the sessions
// dir) using the same precedence as toolCacheDir, but without the chatID
// subdirectory. Shared by toolCacheDir and toolCacheRoot so the two never
// drift out of sync. Returns "" if no data dir can be resolved.
func toolCacheBase() string {
	if x := os.Getenv("WAKIL_SESSIONS_DIR"); x != "" {
		return filepath.Dir(x)
	}
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "wakil")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "share", "wakil")
	}
	return ""
}

// toolCacheDir returns the directory used to spill oversized tool results for
// the given chat session. Sibling to the sessions dir; empty string if the
// data dir cannot be resolved (results are still truncated, just not cached).
func toolCacheDir(chatID string) string {
	base := toolCacheBase()
	if base == "" || chatID == "" {
		return ""
	}
	return filepath.Join(base, "toolcache", chatID)
}

// ToolCacheRoot returns the toolcache root directory (all chat sessions),
// e.g. ~/.local/share/wakil/toolcache. Exported so callers outside this
// package (the read_file/read_file_full tool handlers) can recognise a spill
// path WITHOUT needing a chatID — the whole point is to intercept these paths
// before they ever reach a sandboxed Executor.
//
// Root cause this exists to fix: SpillToCache/CapToolResult/StubToolResult/
// SpillFullResult all run on the HOST wakil process and write under this
// root. But the model is later told (via the embedded "... at: PATH" marker)
// to read_file that path — and read_file always routes through
// Executor.ConfinePath first, which rejects anything outside the sandboxed
// workspace root (Docker: not bind-mounted; Direct: outside the workspace
// root either way). The result: a tool result that was capped/spilled is a
// GUARANTEED, deterministic dead end for the model to retry, every time,
// until it exhausts its tool-call budget. IsToolCacheHostPath + ReadHostCacheFile
// let the read_file/read_file_full handlers recognise and serve these paths
// directly from the host filesystem, bypassing the executor round-trip
// entirely — the content never needed to cross that boundary in the first
// place, since Wakil itself (not the sandboxed workspace) owns it.
func ToolCacheRoot() string {
	base := toolCacheBase()
	if base == "" {
		return ""
	}
	return filepath.Join(base, "toolcache")
}

// IsToolCacheHostPath reports whether path resolves (after Clean) to a
// location under the wakil toolcache root on THIS host. Used by read_file/
// read_file_full to recognise a spill-cache pointer before attempting
// Executor.ConfinePath, which would otherwise reject it unconditionally.
//
// Deliberately an EXACT-PREFIX check on the canonicalized toolcache root, not
// a loose substring match — a path merely containing the word "toolcache"
// elsewhere (e.g. inside a legitimate workspace file) must not be
// misidentified as a cache artifact. path is Clean'd but not symlink-resolved:
// spill files are created fresh by os.CreateTemp and never symlinked, so this
// is not a confinement-escape surface — see the doc comment on
// ReadHostCacheFile for the matching read-side guarantee.
func IsToolCacheHostPath(path string) bool {
	root := ToolCacheRoot()
	if root == "" || path == "" {
		return false
	}
	root = filepath.Clean(root)
	p := filepath.Clean(path)
	return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
}

// ReadHostCacheFile reads a toolcache spill file directly from the host
// filesystem, bypassing the sandboxed Executor entirely. Callers MUST verify
// IsToolCacheHostPath(path) first — this function performs no confinement
// check of its own; it trusts the caller's classification because the whole
// point is to serve these paths WITHOUT an Executor/ConfinePath round-trip
// (that round-trip is exactly what makes them unreachable in the first
// place: Docker mode never mounts this directory into the container, and
// Direct mode's workspace root is a different tree entirely).
//
// This is safe as a design (not just as an implementation) because the only
// paths that satisfy IsToolCacheHostPath are ones Wakil itself generated via
// os.CreateTemp under ToolCacheRoot() moments earlier — the model can only
// ever supply back a path it was FIRST handed by Wakil in a "... at: PATH"
// marker; it cannot conjure an arbitrary toolcache-rooted path referring to
// content it wasn't already given, because that path includes a random
// CreateTemp-suffixed filename it cannot predict.
func ReadHostCacheFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// StatHostCacheFile returns the byte size of a toolcache spill file directly
// from the host filesystem (no Executor round-trip) — the toolcache-path
// counterpart to Executor.StatFile, used by read_file/read_file_full's
// pre-read size guards.
func StatHostCacheFile(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// spillToDisk writes content to a uniquely-named temp file under cacheDir and
// returns the path. Returns "" if cacheDir is empty or the write fails.
// Uses os.CreateTemp so two concurrent spills of the same tool never collide.
func spillToDisk(cacheDir, toolName, content string) string {
	if cacheDir == "" {
		return ""
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return ""
	}
	safe := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, toolName)
	f, err := os.CreateTemp(cacheDir, safe+"-*.txt")
	if err != nil {
		return ""
	}
	_, werr := f.WriteString(content)
	f.Close()
	if werr != nil {
		_ = os.Remove(f.Name())
		return ""
	}
	return f.Name()
}

// SpillToCache writes content to the tool-cache directory for the given chatID
// and returns the full path. Returns "" if chatID is empty or the write fails.
// This is the exported entry point for callers outside the tools package (e.g.
// dispatchSubagent writing a durable subagent summary) that need the same
// spill-to-disk mechanism without reimplementing it.
func SpillToCache(chatID, toolName, content string) string {
	return spillToDisk(toolCacheDir(chatID), toolName, content)
}

// StubToolResult spills the entire result to disk and returns a ~50-char
// pointer stub. Used when the per-turn tool budget is fully exhausted — the
// model gets a pointer it can read_file if it needs the content, but zero
// bytes of the raw output enter ctx.
func StubToolResult(result, toolName, chatID string) string {
	n := len(result)
	if path := spillToDisk(toolCacheDir(chatID), toolName, result); path != "" {
		return fmt.Sprintf("[budget — %d chars at: %s]", n, path)
	}
	// Spill failed — the full content is lost (not recoverable via read_file).
	// The explicit marker tells the model NOT to attempt a read — there is no
	// spill file. The content is gone.
	return fmt.Sprintf("[budget — %d chars — SPILL FAILED (content unrecoverable)]", n)
}

// rangedTools are tools whose output can be re-read with offset/limit
// parameters, so their truncation marker carries that hint. read_file is the
// only capped tool with ranged access — read_file_full bypasses CapToolResult
// entirely (it routes through SpillFullResult).
var rangedTools = map[string]bool{
	"read_file": true,
}

// capSuffix renders the truncation-feedback marker appended by CapToolResult.
// The whole marker lives inside ONE trailing bracketed segment that starts
// with "+" — this keeps ExtractSpillPath (and its proxy-side duplicate, which
// matches the same known prefixes) able to recover the embedded spill path,
// while giving the model an explicit signal that content is missing and how
// to get the remainder. Silent truncation is the failure mode this fixes:
// without the marker the model re-reads the same file, gets truncated again,
// and loops.
func capSuffix(toolName string, shown, total int, spillPath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n… [+%d chars omitted — TRUNCATED: showing %d of %d chars", total-shown, shown, total)
	if rangedTools[toolName] {
		b.WriteString("; use offset/limit parameters to read the remainder")
	} else {
		b.WriteString("; result truncated")
	}
	if spillPath != "" {
		fmt.Fprintf(&b, " — full content at: %s", spillPath)
	}
	b.WriteString("]")
	return b.String()
}

// CapToolResult enforces the per-result context cap. When the result exceeds
// cap characters the full content is written to a cache file and the in-context
// version is replaced with the leading chars plus an explicit truncation
// marker (see capSuffix) pointing at the file. The model can read the full
// content later with read_file if needed.
//
// The marker counts toward the cap: content + marker together never exceed
// cap, so capping cannot push a result over the very limit it enforces.
//
// cap ≤ 0 means unlimited — the result passes through unchanged.
// chatID is used to scope the cache directory; if empty the spill path note
// is omitted but the truncation (and marker) still apply.
func CapToolResult(result, toolName, chatID string, cap int) string {
	if cap <= 0 || len(result) <= cap {
		return result
	}
	spillPath := spillToDisk(toolCacheDir(chatID), toolName, result)
	total := len(result)

	// Size the kept head so head+marker fits within cap. First render uses
	// upper-bound digits (shown=cap, omitted=total) so the real render can
	// only be shorter or equal; the loop is a belt-and-suspenders guard
	// against digit-count drift between renders.
	head := cap - len(capSuffix(toolName, cap, total, spillPath))
	if head < 0 {
		head = 0
	}
	suffix := capSuffix(toolName, head, total, spillPath)
	for head > 0 && head+len(suffix) > cap {
		head = cap - len(suffix)
		if head < 0 {
			head = 0
		}
		suffix = capSuffix(toolName, head, total, spillPath)
	}
	return result[:head] + suffix
}

// SpillFullResult writes the full result to the spill cache and returns the
// complete content with a trailing path marker so that evictStaleToolResults
// and the pre-send MaxRequestBytes trim can extract the path via
// ExtractSpillPath and produce a recoverable stub.
//
// Used by read_file_full, which keeps full content in context (bypassing
// ToolResultCap) but still needs a recovery path when eviction or pre-send
// trimming fires. The trailing marker is harmless to the model — it appears
// after the file content, clearly labelled.
func SpillFullResult(result, toolName, chatID string) string {
	if len(result) <= 200 {
		// Small results won't be evicted or trimmed; skip the spill to avoid
		// orphaned cache files for trivially short reads.
		return result
	}
	spillPath := spillToDisk(toolCacheDir(chatID), toolName, result)
	if spillPath == "" {
		return result
	}
	return result + fmt.Sprintf("\n[full content at: %s]", spillPath)
}
