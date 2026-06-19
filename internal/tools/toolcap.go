package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ExtractSpillPath returns the disk path embedded by CapToolResult or
// StubToolResult in their trailing "… at: PATH]" note, or "".
// Handles both:
//
//	CapToolResult:  "… [+N chars omitted — full content at: PATH]"
//	StubToolResult: "[budget — N chars at: PATH]"
func ExtractSpillPath(content string) string {
	const marker = " at: "
	i := strings.LastIndex(content, marker)
	if i < 0 {
		return ""
	}
	rest := content[i+len(marker):]
	if j := strings.IndexByte(rest, ']'); j >= 0 {
		return rest[:j]
	}
	return strings.TrimSpace(rest)
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

// toolCacheDir returns the directory used to spill oversized tool results for
// the given chat session. Sibling to the sessions dir; empty string if the
// data dir cannot be resolved (results are still truncated, just not cached).
func toolCacheDir(chatID string) string {
	base := ""
	if x := os.Getenv("WAKIL_SESSIONS_DIR"); x != "" {
		base = filepath.Dir(x)
	} else if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		base = filepath.Join(x, "wakil")
	} else if home, err := os.UserHomeDir(); err == nil {
		base = filepath.Join(home, ".local", "share", "wakil")
	}
	if base == "" || chatID == "" {
		return ""
	}
	return filepath.Join(base, "toolcache", chatID)
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
		os.Remove(f.Name())
		return ""
	}
	return f.Name()
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
	return fmt.Sprintf("[budget — %d chars]", n)
}

// CapToolResult enforces the per-result context cap. When the result exceeds
// cap characters the full content is written to a cache file and the in-context
// version is replaced with the leading cap chars plus a note pointing at the
// file. The model can read the full content later with read_file if needed.
//
// cap ≤ 0 means unlimited — the result passes through unchanged.
// chatID is used to scope the cache directory; if empty the spill path note
// says "(cache unavailable)" but the truncation still applies.
func CapToolResult(result, toolName, chatID string, cap int) string {
	if cap <= 0 || len(result) <= cap {
		return result
	}
	spillPath := spillToDisk(toolCacheDir(chatID), toolName, result)
	omitted := len(result) - cap
	var suffix string
	if spillPath != "" {
		suffix = fmt.Sprintf("\n… [+%d chars omitted — full content at: %s]", omitted, spillPath)
	} else {
		suffix = fmt.Sprintf("\n… [+%d chars omitted]", omitted)
	}
	return result[:cap] + suffix
}
