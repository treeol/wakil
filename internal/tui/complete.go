package tui

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	agent "github.com/treeol/wakil/internal/agent"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Autocomplete picker for two contexts:
//
//   - "@token" file/dir mention: activates when the cursor is inside an @token;
//     Tab or Enter accept, arrows navigate, Esc dismisses.
//
//   - "/command" and "/command <arg>": activates when the line starts with "/";
//     Tab accepts (Enter falls through to handleKey so the command executes).

const compMaxVisible = 8     // suggestion rows shown at once
const compMaxCandidates = 60 // hard cap on candidates considered

// compKind distinguishes the two picker families. The distinction matters for
// Enter: @-file accepts (to avoid sending a message with a partial @mention);
// /command falls through so Enter executes the command directly.
type compKind int

const (
	compKindFile    compKind = iota // @token file/dir mention
	compKindCommand                 // /command or /command <arg>
)

type candidate struct {
	name    string
	isDir   bool // @file: appends "/" on accept and drills in
	hasArgs bool // /cmd: appends " " on accept and re-opens picker for the argument
}

// completionState is the live picker; zero value = inactive.
type completionState struct {
	active  bool
	kind    compKind
	leafLen int // rune count of the leaf being typed (chars to delete on accept)
	cands   []candidate
	sel     int
}

// compSources bundles the data sources for all completion types. The sessions
// field may be nil — fetchSessionShortIDs() reads them from disk on demand.
type compSources struct {
	mentionBase string
	backends    []string // from app.BackendList (names only)
	models      []string // from app.ModelList (fetched from /v1/ilm/models)
	sessions    []string // short session IDs; nil = read from disk when needed
	endpoints   []string // from app.Cfg.Endpoints (keys) + "inherit", for /subagent
}

// allTUICommands is the authoritative command list for "/" picker completion.
// hasArgs marks commands that take a following argument — Tab appends " " and
// re-opens the picker to complete that argument.
var allTUICommands = []candidate{
	{name: "/auto", hasArgs: true},
	{name: "/backend", hasArgs: true},
	{name: "/compact"},
	{name: "/counsel", hasArgs: true},
	{name: "/cwd"},
	{name: "/handoff", hasArgs: true},
	{name: "/exit"},
	{name: "/help"},
	{name: "/history"},
	{name: "/info"},
	{name: "/learn"},
	{name: "/mashura", hasArgs: true},
	{name: "/maxpar", hasArgs: true},
	{name: "/maxctx", hasArgs: true},
	{name: "/mcp", hasArgs: true},
	{name: "/mode"},
	{name: "/model", hasArgs: true},
	{name: "/new"},
	{name: "/plan", hasArgs: true},
	{name: "/quit"},
	{name: "/rawtools"},
	{name: "/repostate", hasArgs: true},
	{name: "/remember", hasArgs: true},
	{name: "/reset"},
	{name: "/resume", hasArgs: true},
	{name: "/session", hasArgs: true},
	{name: "/sessions"},
	{name: "/subagent", hasArgs: true},
	{name: "/submodel", hasArgs: true},
}

// compSrcFromApp builds a compSources from the running App. Sessions are left
// nil and fetched lazily from disk only when the /resume picker is open.
func compSrcFromApp(app *agent.App) compSources {
	if app == nil {
		return compSources{}
	}
	backends := make([]string, len(app.BackendList))
	for i, b := range app.BackendList {
		backends[i] = b.Name
	}
	endpoints := make([]string, 0, len(app.Cfg.Endpoints)+1)
	endpoints = append(endpoints, "inherit")
	for name := range app.Cfg.Endpoints {
		endpoints = append(endpoints, name)
	}
	sort.Strings(endpoints[1:]) // keep "inherit" first, sort the rest
	return compSources{
		mentionBase: app.Cfg.MentionBase,
		backends:    backends,
		models:      app.ModelList,
		endpoints:   endpoints,
	}
}

// Hidden border: keeps the picker's footprint (matches completionHeight's +2)
// without drawing visible lines, consistent with the other panes.
var styleCompletionBorder = lipgloss.NewStyle().
	Border(lipgloss.HiddenBorder())

// cursorColInLine returns the cursor's rune index within its logical line.
func cursorColInLine(ta textarea.Model) int {
	li := ta.LineInfo()
	return li.StartColumn + li.ColumnOffset
}

// computeCompletion returns the picker state for the current textarea cursor.
// It tries @-file completion first; if that doesn't match, it tries /command.
func computeCompletion(ta textarea.Model, src compSources) completionState {
	if st := computeAtCompletion(ta, src.mentionBase); st.active {
		return st
	}
	return computeSlashCompletion(ta, src)
}

// computeAtCompletion handles "@token" file-mention completion. When the query
// has no "/" (bare leaf like "@store"), it uses recursive repo-wide matching to
// surface files in subdirectories. When the query contains a "/" (like
// "@src/app"), it drills into the specified subdirectory with the original
// single-level behavior. The cursor must be inside a whitespace-bounded
// @token for this to activate.
func computeAtCompletion(ta textarea.Model, base string) completionState {
	lines := strings.Split(ta.Value(), "\n")
	row := ta.Line()
	if row < 0 || row >= len(lines) {
		return completionState{}
	}
	runes := []rune(lines[row])
	col := cursorColInLine(ta)
	if col > len(runes) {
		col = len(runes)
	}

	// Walk back from the cursor to an '@', stopping at whitespace.
	start := -1
	for i := col - 1; i >= 0; i-- {
		if runes[i] == '@' {
			start = i
			break
		}
		if runes[i] == ' ' || runes[i] == '\t' {
			break
		}
	}
	if start < 0 {
		return completionState{}
	}
	// '@' must begin a token (start of line or after whitespace) — avoids emails.
	if start > 0 {
		if p := runes[start-1]; p != ' ' && p != '\t' {
			return completionState{}
		}
	}

	query := string(runes[start+1 : col])
	dirPrefix, leaf := SplitMentionQuery(query)
	var cands []candidate
	if dirPrefix == "" {
		// No explicit directory prefix → recursive repo-wide fuzzy match so
		// typing "@store" surfaces internal/memory/store.go without drilling.
		cands = listCandidatesRecursive(base, leaf)
	} else {
		// User typed a "/" → drill into the specified subdirectory (existing behavior).
		dir := dirPrefix
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(base, dirPrefix)
		}
		cands = listCandidates(dir, leaf)
	}
	return completionState{
		active:  true,
		kind:    compKindFile,
		leafLen: len([]rune(leaf)),
		cands:   cands,
	}
}

// computeSlashCompletion handles "/" command and argument completion. It
// activates when the current logical line starts with "/" and the cursor is
// on that line. Two sub-contexts:
//
//   - No space before cursor ("/ba"): command picker. The leaf is the full
//     textBeforeCursor (including "/") so that accepting deletes it and
//     inserts the chosen "/command" name in one step.
//
//   - Space before cursor ("/backend op"): argument picker for the command
//     before the space. Only /auto, /backend, /model, /resume, /subagent,
//     /submodel, /repostate, and /handoff have argument completion; other
//     commands are not completed past the space.
func computeSlashCompletion(ta textarea.Model, src compSources) completionState {
	lines := strings.Split(ta.Value(), "\n")
	row := ta.Line()
	if row < 0 || row >= len(lines) {
		return completionState{}
	}
	line := lines[row]
	if len(line) == 0 || line[0] != '/' {
		return completionState{}
	}

	col := cursorColInLine(ta)
	runes := []rune(line)
	if col > len(runes) {
		col = len(runes)
	}
	textBeforeCursor := string(runes[:col])

	spaceIdx := strings.Index(textBeforeCursor, " ")
	if spaceIdx < 0 {
		// Command picker: leaf = full textBeforeCursor (e.g. "/ba").
		return completionState{
			active:  true,
			kind:    compKindCommand,
			leafLen: len([]rune(textBeforeCursor)),
			cands:   listCommandCandidates(textBeforeCursor),
		}
	}

	// Argument picker.
	cmdWord := textBeforeCursor[:spaceIdx] // e.g. "/backend"
	argLeaf := textBeforeCursor[spaceIdx+1:]
	leafLen := len([]rune(argLeaf))

	var cands []candidate
	switch cmdWord {
	case "/auto":
		cands = listNameCandidates(argLeaf, []string{"destructive"})
	case "/repostate":
		cands = listNameCandidates(argLeaf, []string{"clear"})
	case "/backend":
		cands = listNameCandidates(argLeaf, src.backends)
	case "/model":
		cands = listNameCandidates(argLeaf, src.models)
	case "/resume":
		sessions := src.sessions
		if sessions == nil {
			sessions = fetchSessionShortIDs()
		}
		cands = listNameCandidates(argLeaf, sessions)
	case "/subagent":
		cands = listNameCandidates(argLeaf, src.endpoints)
	case "/submodel":
		cands = listNameCandidates(argLeaf, src.models)
	case "/handoff":
		cands = listNameCandidates(argLeaf, []string{"proceed", "stop"})
	case "/counsel":
		cands = listNameCandidates(argLeaf, []string{"auto", "suggest", "off"})
	case "/mashura":
		cands = listNameCandidates(argLeaf, []string{"panel", "map", "model", "maxtokens", "timeout"})
	default:
		return completionState{} // no argument completion for other commands
	}
	return completionState{
		active:  true,
		kind:    compKindCommand,
		leafLen: leafLen,
		cands:   cands,
	}
}

// SplitMentionQuery splits "src/ma" into ("src/", "ma"); a bare "ma" → ("", "ma").
func SplitMentionQuery(q string) (dirPrefix, leaf string) {
	if i := strings.LastIndex(q, "/"); i >= 0 {
		return q[:i+1], q[i+1:]
	}
	return "", q
}

// skipDirs are directories never descended into during recursive file indexing.
var skipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	".tmp-gocache": true,
	"vendor":       true,
	".cache":       true,
	"__pycache__":  true,
	".idea":        true,
	".vscode":      true,
	"dist":         true,
	"build":        true,
	"target":       true,
	".next":        true,
	".turbo":       true,
}

// repoFileIndex caches the list of repo-relative file paths to avoid re-walking
// the filesystem on every keystroke. The index is per-mentionBase and expires
// after fileIndexTTL. A zero-value fileIndex is ready to use.
type repoFileIndex struct {
	entries []indexEntry
	base    string
	expires time.Time
}

type indexEntry struct {
	relPath  string // workspace-relative path (e.g. "internal/memory/store.go")
	baseName string // last path component (e.g. "store.go")
	isDir    bool
}

const fileIndexTTL = 30 * time.Second

// globalFileIndex is the shared cache for the recursive file index. It is
// per-mentionBase; switching workspaces (different mentionBase) triggers a
// rebuild. Safe for concurrent reads (computeCompletion runs on the TUI
// goroutine; the index is not accessed from subagents).
var globalFileIndex repoFileIndex

// buildFileIndex walks the base directory and returns a sorted list of
// workspace-relative paths. Skips skipDirs entries, dotfiles, and respects a
// max depth of 12 to bound traversal. Uses git ls-files when available (faster,
// respects .gitignore, includes untracked files), falls back to filepath.WalkDir.
func buildFileIndex(base string) []indexEntry {
	// Try git ls-files first — it respects .gitignore and is much faster than
	// a full tree walk on large repos. We run it with a 2s timeout so it
	// doesn't block the TUI on slow/network filesystems.
	if entries := buildFileIndexGit(base); entries != nil {
		return entries
	}
	return buildFileIndexWalk(base)
}

// buildFileIndexGit uses `git ls-files --cached --others --exclude-standard -z`
// to list tracked + untracked (non-ignored) files. Falls back to walk on any
// error. Also synthesizes parent directories from the file paths so the
// recursive picker can match directory names. Returns nil if git is unavailable
// or fails.
//
// -z uses NUL-separated output to handle filenames with spaces, newlines, and
// non-ASCII characters correctly (avoids core.quotepath C-quoting).
// --cached includes tracked files; --others adds untracked files;
// --exclude-standard applies .gitignore rules.
func buildFileIndexGit(base string) []indexEntry {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", base, "ls-files",
		"--cached", "--others", "--exclude-standard", "-z")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var entries []indexEntry
	seenDirs := make(map[string]bool)
	// -z output is NUL-separated; split on \x00, not newline.
	for _, line := range strings.Split(string(out), "\x00") {
		if line == "" {
			continue
		}
		// Apply the same filtering as the walk fallback: skip dotfiles and
		// skipDirs so the git path and walk path are consistent.
		name := filepath.Base(line)
		if shouldSkipIndexEntry(name) {
			continue
		}
		entry := indexEntry{
			relPath:  line,
			baseName: name,
		}
		entries = append(entries, entry)
		// Track parent dirs so the recursive picker can also match directory names.
		dir := filepath.Dir(line)
		for dir != "." && dir != "/" && dir != "" {
			dirName := filepath.Base(dir)
			if !seenDirs[dir] && !shouldSkipIndexEntry(dirName) {
				seenDirs[dir] = true
				entries = append(entries, indexEntry{
					relPath:  dir + "/",
					baseName: dirName,
					isDir:    true,
				})
			}
			dir = filepath.Dir(dir)
		}
	}
	return entries
}

// shouldSkipIndexEntry returns true if a file/directory name should be excluded
// from the recursive index. Applies the same rules as the walk fallback: skip
// dotfiles (unless the user explicitly types a dot prefix — handled by the
// caller's leaf filtering) and skipDirs entries.
func shouldSkipIndexEntry(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	return skipDirs[name]
}

// buildFileIndexWalk is the fallback filesystem walker when git is not available.
func buildFileIndexWalk(base string) []indexEntry {
	var entries []indexEntry
	const maxDepth = 12
	_ = filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return filepath.SkipDir
		}
		rel, _ := filepath.Rel(base, path)
		if rel == "." {
			return nil
		}
		name := d.Name()
		// Skip hidden dirs/files and skipDirs.
		if strings.HasPrefix(name, ".") || skipDirs[name] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// Depth check.
		depth := strings.Count(rel, string(filepath.Separator))
		if depth > maxDepth {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		entry := indexEntry{
			relPath:  rel,
			baseName: name,
			isDir:    d.IsDir(),
		}
		if entry.isDir {
			entry.relPath = rel + "/"
		}
		entries = append(entries, entry)
		return nil
	})
	return entries
}

// getFileIndex returns a cached file index for base, rebuilding if the cache
// is stale or for a different base.
func getFileIndex(base string) []indexEntry {
	now := time.Now()
	if globalFileIndex.base == base && now.Before(globalFileIndex.expires) {
		return globalFileIndex.entries
	}
	entries := buildFileIndex(base)
	globalFileIndex = repoFileIndex{
		entries: entries,
		base:    base,
		expires: now.Add(fileIndexTTL),
	}
	return entries
}

// listCandidatesRecursive lists files/dirs across the entire repo whose basename
// or path contains leaf (case-insensitive). Ranked: basename-prefix first,
// then path-substring, then alphabetical. When a candidate is a file in a
// subdirectory, the candidate name is the workspace-relative path (e.g.
// "internal/memory/store.go") so the user can disambiguate. On accept, the
// full relative path is inserted.
//
// The candidate.isDir flag is preserved so accepting a directory still drills
// in with "/" appended.
func listCandidatesRecursive(base, leaf string) []candidate {
	entries := getFileIndex(base)
	leafLower := strings.ToLower(leaf)
	var cands []candidate
	for _, e := range entries {
		if leaf != "" {
			// Match against both basename and full relative path.
			if !strings.Contains(strings.ToLower(e.baseName), leafLower) &&
				!strings.Contains(strings.ToLower(e.relPath), leafLower) {
				continue
			}
		}
		cands = append(cands, candidate{name: strings.TrimSuffix(e.relPath, "/"), isDir: e.isDir})
	}
	// Rank: basename-prefix first, then dirs-first, then shorter path
	// (shallower = more relevant), then alphabetical.
	sort.SliceStable(cands, func(i, j int) bool {
		ci, cj := cands[i], cands[j]
		bi := strings.HasPrefix(strings.ToLower(filepath.Base(ci.name)), leafLower)
		bj := strings.HasPrefix(strings.ToLower(filepath.Base(cj.name)), leafLower)
		if bi != bj {
			return bi
		}
		// Within same basename-prefix tier, dirs first.
		if ci.isDir != cj.isDir {
			return ci.isDir
		}
		// Shorter paths first (shallower = more relevant).
		if len(ci.name) != len(cj.name) {
			return len(ci.name) < len(cj.name)
		}
		return ci.name < cj.name
	})
	if len(cands) > compMaxCandidates {
		cands = cands[:compMaxCandidates]
	}
	return cands
}

// listCandidates lists entries in dir matching leaf (case-insensitive
// substring), ranked prefix-first, directories first, then alphabetical.
func listCandidates(dir, leaf string) []candidate {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	leafLower := strings.ToLower(leaf)
	var cands []candidate
	for _, e := range entries {
		name := e.Name()
		// Hide dotfiles unless the user is explicitly typing a dot prefix.
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(leaf, ".") {
			continue
		}
		if leaf != "" && !strings.Contains(strings.ToLower(name), leafLower) {
			continue
		}
		cands = append(cands, candidate{name: name, isDir: e.IsDir()})
	}
	sort.SliceStable(cands, func(i, j int) bool {
		pi := strings.HasPrefix(strings.ToLower(cands[i].name), leafLower)
		pj := strings.HasPrefix(strings.ToLower(cands[j].name), leafLower)
		if pi != pj {
			return pi
		}
		if cands[i].isDir != cands[j].isDir {
			return cands[i].isDir
		}
		return cands[i].name < cands[j].name
	})
	if len(cands) > compMaxCandidates {
		cands = cands[:compMaxCandidates]
	}
	return cands
}

// listCommandCandidates filters allTUICommands by prefix match against leaf
// (the full textBeforeCursor including "/"). An empty or sole "/" matches all.
func listCommandCandidates(leaf string) []candidate {
	leafLower := strings.ToLower(leaf)
	var cands []candidate
	for _, cmd := range allTUICommands {
		if !strings.HasPrefix(strings.ToLower(cmd.name), leafLower) {
			continue
		}
		cands = append(cands, cmd)
	}
	return cands
}

// listNameCandidates filters names by case-insensitive substring match against
// leaf, ranked prefix-first then alphabetical. Used for /backend, /model, and
// /resume argument completion.
func listNameCandidates(leaf string, names []string) []candidate {
	leafLower := strings.ToLower(leaf)
	var cands []candidate
	for _, name := range names {
		if leaf != "" && !strings.Contains(strings.ToLower(name), leafLower) {
			continue
		}
		cands = append(cands, candidate{name: name})
	}
	sort.SliceStable(cands, func(i, j int) bool {
		pi := strings.HasPrefix(strings.ToLower(cands[i].name), leafLower)
		pj := strings.HasPrefix(strings.ToLower(cands[j].name), leafLower)
		if pi != pj {
			return pi
		}
		return cands[i].name < cands[j].name
	})
	return cands
}

// fetchSessionShortIDs reads the local session store and returns short IDs for
// all saved sessions, sorted most-recent first. Returns nil on failure.
func fetchSessionShortIDs() []string {
	sessions, err := agent.ListSessions()
	if err != nil || len(sessions) == 0 {
		return nil
	}
	ids := make([]string, len(sessions))
	for i, s := range sessions {
		ids[i] = agent.ShortID(s.ChatID)
	}
	return ids
}

// handleCompletionKey processes picker navigation while it is open with results.
// Returns the updated model and whether the key was consumed.
// For /command completions, Enter is NOT consumed so it falls through to
// handleKey and executes the command directly; Tab always accepts.
func (m tuiModel) handleCompletionKey(msg tea.KeyMsg) (tuiModel, bool) {
	if !m.comp.active || len(m.comp.cands) == 0 {
		return m, false
	}
	switch msg.String() {
	case "up", "ctrl+p":
		m.comp.sel = (m.comp.sel - 1 + len(m.comp.cands)) % len(m.comp.cands)
		return m, true
	case "down", "ctrl+n":
		m.comp.sel = (m.comp.sel + 1) % len(m.comp.cands)
		return m, true
	case "tab":
		return m.acceptCompletion(), true
	case "enter":
		// @file: Enter accepts (avoids sending a message with a partial @mention).
		// /command: Enter falls through so handleKey can execute the command.
		if m.comp.kind == compKindCommand {
			return m, false
		}
		return m.acceptCompletion(), true
	case "esc":
		m.comp = completionState{}
		return m, true
	}
	return m, false
}

// acceptCompletion replaces the typed leaf with the selected candidate.
// For @-file dirs and /commands-with-args it inserts a trailing separator
// ("/" or " " respectively) and re-opens the picker; otherwise it closes it.
func (m tuiModel) acceptCompletion() tuiModel {
	if m.comp.sel < 0 || m.comp.sel >= len(m.comp.cands) {
		return m
	}
	c := m.comp.cands[m.comp.sel]
	for i := 0; i < m.comp.leafLen; i++ {
		m.ta, _ = m.ta.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	}
	ins := c.name
	reopen := false
	switch {
	case c.isDir:
		ins += "/"
		reopen = true
	case c.hasArgs:
		ins += " "
		reopen = true
	}
	m.ta.InsertString(ins)
	if reopen {
		m.comp = computeCompletion(m.ta, compSrcFromApp(m.app))
	} else {
		m.comp = completionState{}
	}
	return m
}

// completionHeight is the outer (bordered) height of the picker, 0 when closed.
// Must agree with renderCompletion so the layout reserves the right space.
func (m tuiModel) completionHeight() int {
	if !m.comp.active {
		return 0
	}
	if len(m.comp.cands) == 0 {
		return 1 + 2 // "no matches" + border
	}
	rows := len(m.comp.cands)
	if rows > compMaxVisible {
		rows = compMaxVisible + 1 // extra row for the "+N more" line
	}
	return rows + 2
}

// renderCompletion draws the picker box (width matches the input pane).
func (m tuiModel) renderCompletion() string {
	cs := m.comp.cands
	var b strings.Builder
	if len(cs) == 0 {
		b.WriteString(dim2("  no matches"))
	} else {
		start := 0
		if m.comp.sel >= compMaxVisible {
			start = m.comp.sel - compMaxVisible + 1
		}
		end := start + compMaxVisible
		if end > len(cs) {
			end = len(cs)
		}
		for i := start; i < end; i++ {
			name := cs[i].name
			if cs[i].isDir {
				name += "/"
			}
			var row string
			switch {
			case i == m.comp.sel:
				row = lipgloss.NewStyle().Reverse(true).Render(" " + name + " ")
			case cs[i].isDir:
				row = "  " + lipgloss.NewStyle().Foreground(lipgloss.Color("33")).Render(name)
			default:
				row = "  " + lipgloss.NewStyle().Foreground(lipgloss.Color("252")).Render(name)
			}
			b.WriteString(row)
			if i < end-1 {
				b.WriteByte('\n')
			}
		}
		if len(cs) > compMaxVisible {
			b.WriteString("\n" + dim2(sprint("  +%d more", len(cs)-compMaxVisible)))
		}
	}
	return styleCompletionBorder.Width(m.width - 2).Render(b.String())
}
