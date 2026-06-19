package tools

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// "@" file/folder mentions. A token like "@src/main.go" in a user message is
// resolved against the configured base (host filesystem, independent of the
// exec sandbox) and its content/listing is injected into the outgoing message.
// The user sees a chip; the raw content goes to the proxy, not the visible input.

const (
	maxMentionBytes   = 50 * 1024 // per-file content cap
	maxMentionLines   = 800       // per-file line cap
	maxMentionDepth   = 2         // folder tree depth
	maxMentionEntries = 200       // folder entry cap
)

// mentionRe matches "@path" where @ starts the input or follows whitespace.
var mentionRe = regexp.MustCompile(`(^|\s)@([^\s]+)`)

// mentionBlockSep marks where injected "@"-content begins in an outgoing
// message (see ResolveMentions: input + "\n\n" + "--- @…").
const mentionBlockSep = "\n\n--- @"

// UserQueryText recovers the user's typed text from an outgoing message,
// dropping any injected "@" file/folder blocks.
func UserQueryText(outgoing string) string {
	if i := strings.Index(outgoing, mentionBlockSep); i >= 0 {
		return outgoing[:i]
	}
	return outgoing
}

// dirSkip are noise directories never descended into when listing a folder.
var dirSkip = map[string]bool{".git": true, "node_modules": true, ".venv": true, "vendor": true}

// MentionRef is the user-visible record of one "@" reference (rendered as a chip).
type MentionRef struct {
	Token string // path as typed, e.g. "src/main.go"
	Ok    bool   // resolved & injected
	Note  string // size / entry count, or why it failed ("not found", "binary, skipped")
}

// ResolveMentions scans input for "@path" tokens and returns the outgoing message
// (typed text plus injected file/folder blocks) and the chip refs. If there are
// no mentions, outgoing == input and refs is nil.
func ResolveMentions(input, base string) (outgoing string, refs []MentionRef) {
	matches := mentionRe.FindAllStringSubmatch(input, -1)
	if len(matches) == 0 {
		return input, nil
	}
	var blocks []string
	seen := map[string]bool{}
	for _, mt := range matches {
		tok := strings.TrimRight(mt[2], ".,;:!?)") // drop trailing sentence punctuation
		if tok == "" || seen[tok] {
			continue
		}
		seen[tok] = true
		ref, block := resolveMention(tok, base)
		refs = append(refs, ref)
		if block != "" {
			blocks = append(blocks, block)
		}
	}
	if len(blocks) == 0 {
		return input, refs
	}
	return input + "\n\n" + strings.Join(blocks, "\n\n"), refs
}

func resolveMention(tok, base string) (MentionRef, string) {
	path := tok
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, tok)
	}
	info, err := os.Stat(path)
	if err != nil {
		return MentionRef{Token: tok, Note: "not found"}, ""
	}
	if info.IsDir() {
		listing, n := listDirCapped(path, tok)
		return MentionRef{Token: tok, Ok: true, Note: fmt.Sprintf("%d entries", n)},
			fmt.Sprintf("--- @%s (directory) ---\n%s", tok, listing)
	}
	content, truncated, binary, err := readFileCapped(path)
	if err != nil {
		return MentionRef{Token: tok, Note: "unreadable"}, ""
	}
	if binary {
		return MentionRef{Token: tok, Ok: true, Note: "binary, skipped"},
			fmt.Sprintf("--- @%s ---\n[binary file, not injected]", tok)
	}
	notice := ""
	if truncated {
		notice = fmt.Sprintf("\n[… truncated to %d KB / %d lines …]", maxMentionBytes/1024, maxMentionLines)
	}
	return MentionRef{Token: tok, Ok: true, Note: HumanSize(info.Size())},
		fmt.Sprintf("--- @%s (%s) ---\n```%s\n%s\n```%s", tok, HumanSize(info.Size()), langForExt(path), content, notice)
}

// readFileCapped reads at most maxMentionBytes / maxMentionLines, reporting
// truncation and whether the file looks binary (contains a NUL byte).
func readFileCapped(path string) (content string, truncated, binary bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, false, err
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, maxMentionBytes+1))
	if err != nil {
		return "", false, false, err
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return "", false, true, nil
	}
	if len(data) > maxMentionBytes {
		data = data[:maxMentionBytes]
		truncated = true
	}
	s := string(data)
	if lines := strings.Split(s, "\n"); len(lines) > maxMentionLines {
		s = strings.Join(lines[:maxMentionLines], "\n")
		truncated = true
	}
	return s, truncated, false, nil
}

// listDirCapped renders an indented tree of names (no file contents), capped by
// depth and entry count. Returns the listing and the number of entries shown.
func listDirCapped(root, tok string) (string, int) {
	var b strings.Builder
	label := tok
	if !strings.HasSuffix(label, "/") {
		label += "/"
	}
	b.WriteString(label + "\n")

	count := 0
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if depth > maxMentionDepth || count >= maxMentionEntries {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		sort.SliceStable(entries, func(i, j int) bool {
			if entries[i].IsDir() != entries[j].IsDir() {
				return entries[i].IsDir()
			}
			return entries[i].Name() < entries[j].Name()
		})
		for _, e := range entries {
			if count >= maxMentionEntries {
				b.WriteString(strings.Repeat("  ", depth) + "[… more entries omitted …]\n")
				return
			}
			name := e.Name()
			if e.IsDir() {
				name += "/"
			}
			b.WriteString(strings.Repeat("  ", depth) + name + "\n")
			count++
			if e.IsDir() && !dirSkip[e.Name()] {
				walk(filepath.Join(dir, e.Name()), depth+1)
			}
		}
	}
	walk(root, 1)
	return strings.TrimRight(b.String(), "\n"), count
}

// ChipsLine renders the mention chips shown under the user's message.
func ChipsLine(refs []MentionRef) string {
	parts := make([]string, 0, len(refs))
	for _, r := range refs {
		icon := "📎"
		label := r.Token
		if !r.Ok {
			icon = "⚠"
		}
		if r.Note != "" {
			label += " (" + r.Note + ")"
		}
		parts = append(parts, icon+" "+label)
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render(strings.Join(parts, "   "))
}

// HumanSize formats a byte count as a human-readable string.
func HumanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func langForExt(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "go"
	case ".py":
		return "python"
	case ".js", ".mjs", ".cjs":
		return "javascript"
	case ".ts":
		return "typescript"
	case ".rs":
		return "rust"
	case ".c", ".h":
		return "c"
	case ".cpp", ".cc", ".hpp":
		return "cpp"
	case ".java":
		return "java"
	case ".rb":
		return "ruby"
	case ".sh", ".bash":
		return "bash"
	case ".json":
		return "json"
	case ".yaml", ".yml":
		return "yaml"
	case ".toml":
		return "toml"
	case ".md":
		return "markdown"
	case ".html":
		return "html"
	case ".css":
		return "css"
	case ".sql":
		return "sql"
	default:
		return ""
	}
}
