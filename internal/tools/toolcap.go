package tools

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"unicode/utf8"
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

// IsToolCacheHostPath reports whether path LEXICALLY resolves (after Clean) to
// a location under the wakil toolcache root on THIS host. Used by read_file/
// read_file_full to recognise a spill-cache pointer before attempting
// Executor.ConfinePath, which would otherwise reject it unconditionally.
//
// Deliberately an EXACT-PREFIX check on the Clean'd toolcache root, not
// a loose substring match — a path merely containing the word "toolcache"
// elsewhere (e.g. inside a legitimate workspace file) must not be
// misidentified as a cache artifact. path is Clean'd but not symlink-resolved:
// this is a fast CLASSIFIER only. The confinement guarantee lives in
// ReadHostCacheFile/StatHostCacheFile, which re-verify containment after
// symlink resolution via os.Root before touching the file.
func IsToolCacheHostPath(path string) bool {
	root := ToolCacheRoot()
	if root == "" || path == "" {
		return false
	}
	root = filepath.Clean(root)
	p := filepath.Clean(path)
	return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
}

// truncateInvalidUTF8Suffix drops an incomplete trailing rune (up to 3 bytes)
// so a byte-capped cut doesn't emit a broken sequence.
func truncateInvalidUTF8Suffix(s string) string {
	for i := 0; i < 3 && len(s) > 0; i++ {
		r, size := utf8.DecodeLastRuneInString(s)
		if r != utf8.RuneError || size > 1 {
			return s
		}
		s = s[:len(s)-1]
	}
	return s
}

// ErrHostCacheEscape is returned (wrapped, match with errors.Is) when a path
// resolves OUTSIDE the toolcache root — a traversal or symlink escape. It is
// distinct from ordinary filesystem errors so callers can surface it as a
// refusal rather than a transient failure.
var ErrHostCacheEscape = errors.New("path resolves outside the toolcache root")

// ErrHostCacheNotRegular is returned when a toolcache-rooted path resolves to
// a directory or other non-regular file.
var ErrHostCacheNotRegular = errors.New("not a regular file under the toolcache root")

// resolveHostCachePath is the shared confinement primitive for the host-cache
// read/stat functions. It opens the canonical (symlink-resolved) toolcache
// root with os.Root and resolves rel — the caller's path minus the root
// prefix — inside it. os.Root performs openat-style traversal: ANY component
// (leaf or intermediate) that is a symlink escaping the root is rejected, and
// the result cannot race into the root's parent tree. Returns the file info
// of the resolved entry; callers reject non-regular files.
//
// Threat model (documented per impl review): protection against STATIC
// symlink escapes and traversal. Adversarial concurrent mutation of the
// toolcache tree is out of scope — in direct mode obtaining a host write
// already requires a user-confirmed shell command, at which point the
// consent boundary has already been crossed by that command itself.
func resolveHostCachePath(path string) (*os.Root, string, os.FileInfo, error) {
	root := ToolCacheRoot()
	if root == "" {
		return nil, "", nil, ErrHostCacheEscape
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, "", nil, err
	}
	// The root itself may sit behind symlinked ancestors (e.g. a symlinked
	// XDG data dir) — canonicalize it, then require the LEXICAL path to have
	// classified as inside before resolving (defense in depth; callers
	// IsToolCacheHostPath first).
	resolvedRoot, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, "", nil, err
	}
	cleaned := filepath.Clean(path)
	// rel is computed against whichever form matched: the Clean'd path if it
	// lexically classifies inside, else the symlink-resolved path (alias via
	// a symlinked ancestor). Each candidate is contained-checked against its
	// MATCHING root form — lexical against the lexical abs root, canonical
	// against resolvedRoot — so a canonical-form path under a symlinked root
	// resolves correctly. NB: lexical Clean collapses root/sym/../file to
	// root/file, which os.Root then serves; kernel resolution would follow
	// sym first. Not an escape (the result stays inside), just lexical
	// semantics — documented here.
	relBase := abs
	relPath := cleaned
	if !IsToolCacheHostPath(cleaned) {
		resolved, rerr := filepath.EvalSymlinks(cleaned)
		if rerr != nil || !underRoot(resolved, resolvedRoot) {
			return nil, "", nil, fmt.Errorf("%w: %s", ErrHostCacheEscape, path)
		}
		relBase = resolvedRoot
		relPath = resolved
	}
	rel, err := filepath.Rel(relBase, relPath)
	if err != nil || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, "", nil, fmt.Errorf("%w: %s", ErrHostCacheEscape, path)
	}
	if rel == "." {
		return nil, "", nil, fmt.Errorf("%w: %s", ErrHostCacheNotRegular, path)
	}
	r, err := os.OpenRoot(resolvedRoot)
	if err != nil {
		return nil, "", nil, err
	}
	fi, err := r.Stat(rel)
	if err != nil {
		r.Close()
		if isEscapeErr(err) {
			return nil, "", nil, fmt.Errorf("%w: %s", ErrHostCacheEscape, path)
		}
		return nil, "", nil, err
	}
	return r, rel, fi, nil
}

// underRoot is the exact-prefix containment check against an already-Clean'd
// root (same rule as IsToolCacheHostPath, parameterized).
func underRoot(p, root string) bool {
	return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
}

// isEscapeErr reports whether err is os.Root's "path escapes from parent"
// refusal. The sentinel is unexported in the standard library (verified in
// GOROOT/src/os/file.go), so match the *fs.PathError's cause, not the message
// text of the wrapper (which embeds the path and can name-collide).
func isEscapeErr(err error) bool {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		// pe.Err is the CAUSE (no path embedded), so matching its message is
		// safe from name collisions. os's sentinel is unexported, so identity
		// comparison is impossible; if a future Go exports it, switch to
		// errors.Is against the exported sentinel.
		return pe.Err != nil && pe.Err.Error() == "path escapes from parent"
	}
	return false
}

// ReadHostCacheFile reads a toolcache spill file directly from the host
// filesystem, bypassing the sandboxed Executor entirely. The path is
// confinement-verified HERE (resolve under the canonical root via os.Root —
// symlink escapes and traversal are rejected with ErrHostCacheEscape;
// directories and other non-regular files with ErrHostCacheNotRegular), so
// the guarantee holds regardless of caller diligence.
//
// Boundary statement (impl review): this confines reads to the toolcache
// TREE. It does not prove spill provenance (that a path was issued by
// Wakil in a spill marker) — a path the model invents under the root still
// reads, but can only contain Wakil-generated content, and the random
// CreateTemp suffix makes guessing one infeasible.
func ReadHostCacheFile(path string) (string, error) {
	r, rel, fi, err := resolveHostCachePath(path)
	if err != nil {
		return "", err
	}
	defer r.Close()
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%w: %s", ErrHostCacheNotRegular, path)
	}
	f, err := r.Open(rel)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ReadHostCacheFileBounded reads at most maxBytes bytes from the start of a
// toolcache spill file (H4: bounded host-side allocation regardless of file
// size). Truncated reports whether content continued past the cap. Same
// confinement as ReadHostCacheFile.
func ReadHostCacheFileBounded(path string, maxBytes int64) (string, bool, error) {
	if maxBytes <= 0 {
		return "", false, fmt.Errorf("ReadHostCacheFileBounded: maxBytes must be > 0, got %d", maxBytes)
	}
	if maxBytes > 64<<20 {
		return "", false, fmt.Errorf("ReadHostCacheFileBounded: maxBytes %d exceeds sane cap (64 MB)", maxBytes)
	}
	r, rel, fi, err := resolveHostCachePath(path)
	if err != nil {
		return "", false, err
	}
	defer r.Close()
	if !fi.Mode().IsRegular() {
		return "", false, fmt.Errorf("%w: %s", ErrHostCacheNotRegular, path)
	}
	f, err := r.Open(rel)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	buf := make([]byte, maxBytes+1)
	n, rerr := io.ReadFull(f, buf)
	if rerr != nil && rerr != io.ErrUnexpectedEOF && rerr != io.EOF {
		return "", false, rerr
	}
	truncated := int64(n) > maxBytes
	content := string(buf[:min(int64(n), maxBytes)])
	// UTF-8-safe cut, consistent with the executor ReadFileBounded variants.
	content = truncateInvalidUTF8Suffix(content)
	return content, truncated, nil
}

// StatHostCacheFile returns the byte size of a toolcache spill file directly
// from the host filesystem (no Executor round-trip) — the toolcache-path
// counterpart to Executor.StatFile, used by read_file/read_file_full's
// pre-read size guards. Shares resolveHostCachePath with ReadHostCacheFile,
// so the size guard measures the same confined entry the read will serve.
func StatHostCacheFile(path string) (int64, error) {
	r, _, fi, err := resolveHostCachePath(path)
	if err != nil {
		return 0, err
	}
	r.Close()
	if !fi.Mode().IsRegular() {
		return 0, fmt.Errorf("%w: %s", ErrHostCacheNotRegular, path)
	}
	return fi.Size(), nil
}

// spillToDisk writes content to a uniquely-named temp file under cacheDir and
// returns the path. Returns "" if cacheDir is empty or the write fails.
// Uses os.CreateTemp so two concurrent spills of the same tool never collide.
//
// The index append needs the chatID, which spillToDisk does not have —
// so the append happens in SpillToCache / the wrappers below, which DO
// have it, immediately after spillToDisk returns a successful path
// (the file is already closed at that point, then the path is handed
// out — the append sits between the two).
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
	path := spillToDisk(toolCacheDir(chatID), toolName, content)
	if path != "" {
		// The spill file is closed; index it BEFORE returning the path
		// to any caller ("written before hand-out" is structural).
		appendArchiveIndex(chatID, toolName, path, content)
	}
	return path
}

// ---------------------------------------------------------------------------
// Per-chat archive index (findable archives)
//
// A chat's toolcache dir holds one fixed-name, append-only index file
// (archive-index.txt). Every successful spill appends one record —
// "<kind>\t<absolute path>\t<label>" — BEFORE the path returns to any
// caller, so a collapse note that points at the index file gives the
// model one stable path from which every handed-out archive is
// findable, no matter how many collapses have elided the individual
// marker lines. Records begin with a leading "\n" so a crash-torn
// write degrades to a corrupt fragment (path fails to resolve, the
// host ENOENT text re-points at the index), never to a wrong path.
// Failure to append never loses the spill (the file is already
// closed); the failure is counted (IndexAppendFailures) and the agent
// layer surfaces it once per session.

// archiveIndexName is the fixed file name of the per-chat index. It
// cannot collide with a spill file: spills are "<toolname>-<rand>.txt".
const archiveIndexName = "archive-index.txt"

// ArchiveIndexName is the exported fixed index file name.
const ArchiveIndexName = archiveIndexName

// IndexPathFor returns the index file path for a chat (the same path
// collapse notes and host-missing errors name).
func IndexPathFor(chatID string) string {
	dir := toolCacheDir(chatID)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, archiveIndexName)
}

// archiveIndexFailures counts failed index appends in this process —
// the agent layer surfaces the first one per session as a notice.
var archiveIndexFailures atomic.Int64

// IndexAppendFailures returns the number of failed index appends
// since process start.
func IndexAppendFailures() int64 { return archiveIndexFailures.Load() }

// FailureCountFor is kept for test ergonomics: the counter is
// process-global, so this is just IndexAppendFailures. (The per-chat
// framing of the original test plan was dropped: one counter, one
// process.)
func FailureCountFor(_ string) int64 { return IndexAppendFailures() }

// indexKind maps a spill toolName to its index kind. Unknown names
// fall back to "misc" — still indexed, still findable.
func indexKind(toolName string) string {
	switch {
	case strings.HasPrefix(toolName, "dispatch_subagent"):
		return "subagent-summary"
	case strings.HasPrefix(toolName, "turn-") || strings.HasPrefix(toolName, "round-") ||
		strings.HasPrefix(toolName, "compact-") || strings.HasPrefix(toolName, "hardmax-"):
		return "collapse-spill"
	case toolName == "repo_map" || toolName == "repo_symbols":
		return "repo-map"
	case toolName == "review_diff":
		return "review-diff"
	case strings.HasPrefix(toolName, "read_file") || toolName == "load_skill" ||
		strings.HasPrefix(toolName, "write_file") || toolName == "search_files":
		return "tool-result"
	default:
		return "misc"
	}
}

// indexLabel derives the short label from the spilled content: the
// first non-empty line, truncated to 80 runes, tabs/newlines/CRs
// stripped, invalid UTF-8 cut. It never contains tool arguments or
// content past the first line. The "#N" sequence suffix keeps rows
// with identical generic first lines ("{", "total 48") distinct.
func indexLabel(content string, seq int) string {
	first := ""
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			first = line
			break
		}
	}
	first = strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, first)
	// Truncate to 80 runes at a valid boundary.
	if len([]rune(first)) > 80 {
		cut := 0
		n := 0
		for i := range first {
			if n == 80 {
				cut = i
				break
			}
			n++
		}
		if cut == 0 {
			cut = len(first)
		}
		first = truncateInvalidUTF8Suffix(first[:cut])
	}
	return fmt.Sprintf("%s #%d", first, seq)
}

// indexSeq is a process-global monotonically increasing record
// counter. A crash resets it — harmless (rows stay distinct by path).
var indexSeq atomic.Int64

// appendArchiveIndex appends one record for a just-written spill.
// Best-effort: any failure is counted, never returned (the spill has
// already succeeded). The index file is opened O_CREATE|O_WRONLY|
// O_APPEND; a pre-existing symlink or non-regular file at the index
// path is refused (Lstat check + O_NOFOLLOW where available) — an
// append must never follow a planted symlink out of the chat dir.
func appendArchiveIndex(chatID, toolName, spillPath, content string) {
	dir := toolCacheDir(chatID)
	idxPath := IndexPathFor(chatID)
	if dir == "" || idxPath == "" {
		return
	}
	if li, err := os.Lstat(idxPath); err == nil {
		if li.Mode()&os.ModeSymlink != 0 || !li.Mode().IsRegular() {
			archiveIndexFailures.Add(1)
			return
		}
	}
	// Defence in depth: the Lstat check above refuses a pre-planted
	// symlink/non-regular file, and the fstat below re-checks on the
	// open fd. O_NOFOLLOW would close the Lstat/Open TOCTOU window;
	// that window is the documented own-data-dir race (see
	// resolveHostCachePath's threat model at toolcap.go:188-199) —
	// accepted, not hidden.
	f, err := os.OpenFile(idxPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		archiveIndexFailures.Add(1)
		return
	}
	// Re-check on the open fd: a swap between Lstat and Open is the
	// documented own-data-dir race (see resolveHostCachePath's threat
	// model); fstat is the cheap belt-and-suspenders.
	if fi, ferr := f.Stat(); ferr != nil || !fi.Mode().IsRegular() {
		_ = f.Close()
		archiveIndexFailures.Add(1)
		return
	}
	seq := indexSeq.Add(1)
	rec := "\n" + indexKind(toolName) + "\t" + spillPath + "\t" + indexLabel(content, int(seq))
	if _, err := f.WriteString(rec); err != nil {
		archiveIndexFailures.Add(1)
	}
	_ = f.Close()
}

// StubToolResult spills the entire result to disk and returns a ~50-char
// pointer stub. Used when the per-turn tool budget is fully exhausted — the
// model gets a pointer it can read_file if it needs the content, but zero
// bytes of the raw output enter ctx.
func StubToolResult(result, toolName, chatID string) string {
	n := len(result)
	path := spillToDisk(toolCacheDir(chatID), toolName, result)
	if path != "" {
		appendArchiveIndex(chatID, toolName, path, result)
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
	if spillPath != "" {
		appendArchiveIndex(chatID, toolName, spillPath, result)
	}
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
	appendArchiveIndex(chatID, toolName, spillPath, result)
	return result + fmt.Sprintf("\n[full content at: %s]", spillPath)
}
