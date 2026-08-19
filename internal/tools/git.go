package tools

import (
	"fmt"
	"strings"

	"github.com/treeol/wakil/internal/proxy"
)

// git.go — structured read-only git tools (card #137, local half).
//
// These give the model structured, injection-hardened access to git status/
// diff/log/show/blame instead of a raw `run_shell git ...` round-trip. Every
// tool is READ-ONLY by construction: the handler builds the command from
// structured JSON arguments (no model-supplied flags, refs validated, paths
// confined), so the model cannot reach mutating/destructive subcommands or
// inject options. Forge/PR integration and commit are intentionally out of
// scope here (commit stays behind run_shell's gate; forge depends on the
// credential vault — card #142).
//
// Definitions + parsers live here; the *App handlers that build and run the
// commands live in internal/agent/git_tools.go.

// GitTools returns the read-only git tool definitions. Included in
// DefaultTools (parent) and DiscoveryTools (discovery + tools-tier subagents).
func GitTools() []proxy.Tool {
	return []proxy.Tool{
		{Type: "function", Function: proxy.ToolFunction{
			Name: "git_status",
			Description: "Structured git working-tree status (porcelain-v1, NUL-framed). " +
				"Returns branch, upstream tracking, and a list of changes with index (staged) and worktree (unstaged) status codes. " +
				"Read-only.",
			Parameters: SchemaObj(map[string]interface{}{}),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "git_diff",
			Description: "Show unstaged (default) or staged (`staged=true`) diff for the working tree, " +
				"optionally scoped to a path and/or a revision (`rev`, e.g. \"HEAD~1\" or \"main...feature\"). " +
				"Output is capped; use `path` to narrow. Read-only.",
			Parameters: SchemaObj(map[string]interface{}{
				"staged": BoolProp("Show staged (--cached) changes instead of unstaged (default false)."),
				"rev":    StrProp("Optional revision/range to diff against (e.g. \"HEAD~1\", \"main...feature\", \"A..B\")."),
				"path":   StrProp("Optional path to restrict the diff to (relative to the workspace root)."),
			}),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "git_log",
			Description: "Structured commit log (JSON array) with hash, author, date, and subject. " +
				"Optionally scoped to a revision/range and/or path. Read-only.",
			Parameters: SchemaObj(map[string]interface{}{
				"count": IntProp("Max commits (1-100, default 20)."),
				"rev":   StrProp("Optional revision/range (e.g. \"HEAD~5..HEAD\", \"main\", \"A..B\")."),
				"path":  StrProp("Optional path to restrict history to."),
			}),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "git_show",
			Description: "Show a commit (or object): metadata plus the patch/contents. Defaults to HEAD. " +
				"Output is capped; prefer git_log + git_diff for large histories. Read-only.",
			Parameters: SchemaObj(map[string]interface{}{
				"rev": StrProp("Revision to show (default HEAD)."),
			}),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "git_blame",
			Description: "Line-by-line blame for a file (git blame --porcelain parsed to JSON), " +
				"optionally at a revision. Read-only.",
			Parameters: SchemaObj(map[string]interface{}{
				"path": StrProp("File path (required, relative to the workspace root)."),
				"rev":  StrProp("Optional revision to blame at."),
			}, "path"),
		}},
	}
}

// GitStatusEntry is one parsed `git status --porcelain=v1 -z` record.
// XY is the two-char status code (index then worktree); Path is the primary
// path; OrigPath is set for rename/copy records (the source path).
type GitStatusEntry struct {
	XY       string `json:"xy"`
	Path     string `json:"path"`
	OrigPath string `json:"orig_path,omitempty"`
}

// GitStatusBranch carries the branch/upstream/ahead-behind metadata emitted by
// `--branch` in porcelain v2/`--branch` mode (the "## " header line).
type GitStatusBranch struct {
	Branch   string `json:"branch,omitempty"`
	Upstream string `json:"upstream,omitempty"`
	Ahead    int    `json:"ahead,omitempty"`
	Behind   int    `json:"behind,omitempty"`
}

// GitStatusResult is the structured result of git_status.
type GitStatusResult struct {
	Branch    GitStatusBranch  `json:"branch,omitempty"`
	Entries   []GitStatusEntry `json:"entries"`
	Truncated bool             `json:"truncated,omitempty"` // true when entries were capped
}

// ParseGitStatus parses `git status --porcelain=v1 -z --branch` output.
// The `--branch` header is the NUL-terminated "## ..." line; entries follow as
// NUL-terminated records. Rename/copy entries carry an extra NUL-separated
// orig-path field (porcelain v1 format: "XY new\0old\0").
func ParseGitStatus(out string) GitStatusResult {
	res := GitStatusResult{Entries: []GitStatusEntry{}}
	fields := strings.Split(out, "\x00")
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if f == "" {
			continue
		}
		if strings.HasPrefix(f, "## ") {
			res.Branch = parseBranchHeader(f)
			continue
		}
		if len(f) < 3 {
			continue // malformed short record — skip rather than misparse
		}
		xy := f[:2]
		path := f[3:]
		e := GitStatusEntry{XY: xy, Path: path}
		// Rename/copy (X or Y == 'R'/'C') carries a second NUL-separated path.
		if (xy[0] == 'R' || xy[0] == 'C' || xy[1] == 'R' || xy[1] == 'C') && i+1 < len(fields) {
			e.OrigPath = fields[i+1]
			i++
		}
		res.Entries = append(res.Entries, e)
	}
	return res
}

// parseBranchHeader parses the "## branch...upstream [ahead N, behind M]" line.
func parseBranchHeader(h string) GitStatusBranch {
	// "## " prefix already stripped by caller.
	rest := strings.TrimPrefix(h, "## ")
	var b GitStatusBranch
	// Split off the tracking/ab-state bracket, if present.
	if i := strings.LastIndex(rest, " ["); i >= 0 && strings.HasSuffix(rest, "]") {
		state := rest[i+2 : len(rest)-1]
		rest = rest[:i]
		for _, part := range strings.Split(state, ", ") {
			var n int
			if _, err := fmt.Sscanf(part, "ahead %d", &n); err == nil {
				b.Ahead = n
			} else if _, err := fmt.Sscanf(part, "behind %d", &n); err == nil {
				b.Behind = n
			}
		}
	}
	// The branch/upstream pair: "branch" or "branch...upstream". A detached HEAD
	// renders as "HEAD (no branch)".
	if rest == "HEAD (no branch)" {
		b.Branch = "HEAD"
		return b
	}
	if i := strings.Index(rest, "..."); i >= 0 {
		b.Branch = rest[:i]
		b.Upstream = rest[i+3:]
	} else {
		b.Branch = rest
	}
	// Unborn branch shows as "No commits yet on <branch>".
	b.Branch = strings.TrimPrefix(b.Branch, "No commits yet on ")
	return b
}

// GitLogEntry is one parsed commit from `git log` output.
type GitLogEntry struct {
	Hash    string `json:"hash"`
	Parents string `json:"parents,omitempty"` // space-separated parent hashes
	Author  string `json:"author"`
	Email   string `json:"email"`
	Date    string `json:"date"` // ISO 8601
	Subject string `json:"subject"`
}

// gitLogFormat is the `git log --format` template. It is NUL-FRAMED (fields
// separated by %x00, records separated by %x1e) so parsing is collision-proof
// regardless of what author names, emails, or subjects contain (quotes,
// newlines, backslashes). Emitting JSON inline was rejected: git's %s is NOT
// JSON-escaped, so a subject containing `"` would either break the line or —
// worse, since subject is the last key — inject a duplicate `"hash"` key that
// wins under Go's last-key-wins unmarshalling, allowing commit-spoofing.
const gitLogFormat = `%H%x00%P%x00%an%x00%ae%x00%aI%x00%s%x1e`

// GitLogFormat exposes the log format template to the agent handlers so they
// can shellQuote it into the `--format=` argument.
func GitLogFormat() string { return gitLogFormat }

// ParseGitLog parses NUL-framed commit records (gitLogFormat): fields are
// NUL-separated, records are RS (%x1e)-separated. The last field (subject) may
// itself contain NUL? No — git sanitizes newlines in %s to spaces but a subject
// cannot contain NUL. Fields are split on NUL; records on RS. Tolerant: an
// incomplete trailing record is skipped.
func ParseGitLog(out string) []GitLogEntry {
	var entries []GitLogEntry
	for _, rec := range strings.Split(out, "\x1e") {
		rec = strings.TrimSpace(rec)
		if rec == "" {
			continue
		}
		fields := strings.Split(rec, "\x00")
		if len(fields) < 6 {
			continue // malformed record — skip
		}
		entries = append(entries, GitLogEntry{
			Hash:    fields[0],
			Parents: fields[1],
			Author:  fields[2],
			Email:   fields[3],
			Date:    fields[4],
			Subject: fields[5],
		})
	}
	return entries
}

// GitBlameHunk is one parsed group from `git blame --porcelain`. git's porcelain
// blame format emits a `hash orig-line final-line num-lines` header, then
// key-value metadata lines, repeated once per line but with metadata duplicated
// per line. We collapse per-commit hunks to keep the result compact and
// structured: one record per contiguous run of lines from the same commit.
type GitBlameHunk struct {
	Hash       string `json:"hash"`
	Author     string `json:"author"`
	AuthorTime string `json:"author_time,omitempty"`
	Summary    string `json:"summary,omitempty"`
	StartLine  int    `json:"start_line"`
	NumLines   int    `json:"num_lines"`
}

// ParseGitBlame parses `git blame --porcelain` output into compact hunks.
// The porcelain format is: a header line "<40-hex> <orig> <final> [<num-lines>]"
// (num-lines omitted = 1), followed by metadata key-value lines and content
// lines (which start with a tab). We collapse consecutive lines of the same
// commit into one hunk. Unknown/malformed records are skipped; the parser is
// tolerant by design (blame output is version-dependent).
func ParseGitBlame(out string) []GitBlameHunk {
	var hunks []GitBlameHunk
	var cur *GitBlameHunk
	// metadata caches author/summary per commit hash: `--porcelain` emits them
	// only the FIRST time a commit appears, so a later non-adjacent hunk from
	// the same commit would otherwise lose its metadata.
	metadata := map[string]GitBlameHunk{}
	lines := strings.Split(out, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if !isBlameHeader(line) {
			continue
		}
		hash := line[:40]
		var orig, start, n int
		if _, err := fmt.Sscanf(line[41:], "%d %d %d", &orig, &start, &n); err != nil {
			// 2-field form (num-lines omitted): a single line.
			n = 1
			if _, err2 := fmt.Sscanf(line[41:], "%d %d", &orig, &start); err2 != nil {
				continue
			}
		}
		if cur == nil || cur.Hash != hash {
			hunks = append(hunks, GitBlameHunk{})
			cur = &hunks[len(hunks)-1]
			cur.Hash = hash
			cur.StartLine = start
			cur.NumLines = n
			// Restore cached metadata for a commit we saw earlier.
			if m, ok := metadata[hash]; ok {
				cur.Author = m.Author
				cur.AuthorTime = m.AuthorTime
				cur.Summary = m.Summary
			}
		} else {
			if end := start + n - 1; end > cur.StartLine+cur.NumLines-1 {
				cur.NumLines = end - cur.StartLine + 1
			}
		}
		// Consume metadata lines until the next header or a content line.
		for i+1 < len(lines) {
			next := lines[i+1]
			if strings.HasPrefix(next, "\t") || isBlameHeader(next) || next == "" {
				break
			}
			i++
			if k, v, ok := strings.Cut(next, " "); ok {
				switch k {
				case "author":
					cur.Author = v
				case "author-time":
					cur.AuthorTime = v
				case "summary":
					cur.Summary = v
				}
			}
		}
		// Cache the (possibly newly-read) metadata for this hash.
		metadata[hash] = GitBlameHunk{Author: cur.Author, AuthorTime: cur.AuthorTime, Summary: cur.Summary}
	}
	return hunks
}

func isBlameHeader(line string) bool {
	return len(line) > 41 && line[40] == ' ' && isHex(line[:40])
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return len(s) > 0
}
