package agent

// resume_index.go — the /resume listing index.
//
// Listing sessions used to read and fully decode every transcript. The index
// is a small, derived, rebuildable file next to the session files: one entry
// per session with just what the picker shows. Session files are never
// rewritten or deleted by anything here; the index may be deleted at any time
// and is rebuilt from them.
//
// Correctness never depends on the index being current: ReconcileResumeIndex
// stats every session file, reuses entries whose size and mtime still match,
// and decodes only new or changed files. A save keeps the index current
// (WriteSession), which only makes the reconcile cheaper.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/treeol/wakil/internal/diag"
	"github.com/treeol/wakil/internal/proxy"
)

const (
	// resumeIndexFile is the index file name inside the sessions directory.
	// It starts with a dot and has no .json suffix, so every reader of the
	// directory (ListSessions, plan gc, history reconcile) ignores it.
	resumeIndexFile = ".resume-index"
	// resumeIndexVersion must be bumped whenever the title rules or the entry
	// shape change: an index with another version is treated as empty and
	// rebuilt from the session files.
	resumeIndexVersion = 1
	// sessionTitleRunes bounds a derived title.
	sessionTitleRunes = 60
)

// ResumeEntry is one session in the resume index.
type ResumeEntry struct {
	ID    string `json:"id"`
	File  string `json:"file"`
	Title string `json:"title,omitempty"`
	// Fallback is the first line of the first user message of any kind, shown
	// only when Title is empty (a session with no typed text), exactly as the
	// list showed before titles were derived from typed text.
	Fallback  string    `json:"fallback,omitempty"`
	Label     string    `json:"label,omitempty"`
	Model     string    `json:"model,omitempty"`
	Workspace string    `json:"workspace,omitempty"`
	Created   time.Time `json:"created"`
	Updated   time.Time `json:"updated"`
	Messages  int       `json:"messages"`
	Turns     int       `json:"turns"`
	// Size and ModNS are the session file's stat when the entry was derived:
	// the entry is fresh only while the file still has both.
	Size  int64 `json:"size"`
	ModNS int64 `json:"mod_ns"`
}

type resumeIndexDoc struct {
	Version int           `json:"version"`
	Entries []ResumeEntry `json:"entries"`
}

// resumeIndexMu serializes in-process read-modify-write of the index file. A
// second wakil process can still interleave; a lost entry is rebuilt from its
// session file at the next reconcile.
var resumeIndexMu sync.Mutex

func resumeIndexPath(dir string) string { return filepath.Join(dir, resumeIndexFile) }

// isSessionFileName reports whether name is a session transcript file:
// <chat_id>.json, not a dot file and not a handoff sidecar.
func isSessionFileName(name string) bool {
	return strings.HasSuffix(name, ".json") &&
		!strings.HasPrefix(name, ".") &&
		!strings.HasSuffix(name, ".handoff.json")
}

// readResumeIndexFile loads the index document. A missing, unreadable,
// unparseable or wrong-version index is empty, never an error.
func readResumeIndexFile(dir string) map[string]ResumeEntry {
	out := map[string]ResumeEntry{}
	b, err := os.ReadFile(resumeIndexPath(dir))
	if err != nil {
		return out
	}
	var doc resumeIndexDoc
	if err := json.Unmarshal(b, &doc); err != nil || doc.Version != resumeIndexVersion {
		return out
	}
	for _, e := range doc.Entries {
		if e.File != "" && e.ID != "" {
			out[e.File] = e
		}
	}
	return out
}

func writeResumeIndexFile(dir string, entries map[string]ResumeEntry) error {
	doc := resumeIndexDoc{Version: resumeIndexVersion, Entries: make([]ResumeEntry, 0, len(entries))}
	for _, e := range entries {
		doc.Entries = append(doc.Entries, e)
	}
	sortResumeEntries(doc.Entries)
	return atomicWriteJSONMode(resumeIndexPath(dir), doc, 0o600)
}

// sortResumeEntries orders entries newest first, ties broken by id so the
// order is deterministic (ListSessions uses the same tie-break).
func sortResumeEntries(e []ResumeEntry) {
	sort.Slice(e, func(i, j int) bool {
		if !e[i].Updated.Equal(e[j].Updated) {
			return e[i].Updated.After(e[j].Updated)
		}
		return e[i].ID < e[j].ID
	})
}

// entryFromSession derives the index entry for a decoded session.
func entryFromSession(s Session, file string, size, modNS int64) ResumeEntry {
	turns, _ := SessionTurns(s)
	return ResumeEntry{
		ID:        s.ChatID,
		File:      file,
		Title:     SessionTitle(s.Conv),
		Fallback:  sessionFallbackTitle(s),
		Label:     s.Label,
		Model:     s.Model,
		Workspace: s.Workspace,
		Created:   s.Created,
		Updated:   s.Updated,
		Messages:  len(s.Conv),
		Turns:     turns,
		Size:      size,
		ModNS:     modNS,
	}
}

// updateResumeIndexEntry records a freshly written session in the index. It is
// best effort: a failure is logged and never fails the save.
func updateResumeIndexEntry(dir string, s *Session) {
	file := s.ChatID + ".json"
	info, err := os.Stat(filepath.Join(dir, file))
	if err != nil {
		return
	}
	entry := entryFromSession(*s, file, info.Size(), info.ModTime().UnixNano())
	resumeIndexMu.Lock()
	defer resumeIndexMu.Unlock()
	cur := readResumeIndexFile(dir)
	cur[file] = entry
	if err := writeResumeIndexFile(dir, cur); err != nil {
		diag.Printf("resume index: write: %v\n", err)
	}
}

// ReadResumeIndex returns the entries currently in the index (newest first)
// without touching any session file. It is the picker's first paint: fast, and
// possibly stale or empty until ReconcileResumeIndex runs.
func ReadResumeIndex() []ResumeEntry {
	dir := sessionsDir()
	if dir == "" {
		return nil
	}
	idx := readResumeIndexFile(dir)
	out := make([]ResumeEntry, 0, len(idx))
	for _, e := range idx {
		out = append(out, e)
	}
	sortResumeEntries(out)
	return out
}

// ReconcileResumeIndex brings the index in line with the session files and
// returns the entries, newest first. Cost: one ReadDir plus one stat per
// file; only new or changed files are read and decoded, so a warm index
// decodes nothing. Session files are only read.
func ReconcileResumeIndex() ([]ResumeEntry, error) {
	dir := sessionsDir()
	if dir == "" {
		return nil, nil
	}
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	before := readResumeIndexFile(dir)
	present := make(map[string]bool, len(dirEntries))
	fresh := map[string]ResumeEntry{}
	out := make([]ResumeEntry, 0, len(dirEntries))
	for _, de := range dirEntries {
		name := de.Name()
		if de.IsDir() || !isSessionFileName(name) {
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		present[name] = true
		if old, ok := before[name]; ok && old.Size == info.Size() && old.ModNS == info.ModTime().UnixNano() {
			out = append(out, old)
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			if !os.IsNotExist(err) {
				diag.Printf("resume index: read %s: %v\n", name, err)
			}
			delete(present, name)
			continue
		}
		var s Session
		if err := json.Unmarshal(b, &s); err != nil {
			diag.Printf("resume index: unmarshal %s: %v\n", name, err)
			delete(present, name)
			continue // skip malformed/partial files, as ListSessions does
		}
		e := entryFromSession(s, name, info.Size(), info.ModTime().UnixNano())
		fresh[name] = e
		out = append(out, e)
	}
	var vanished []string
	for name := range before {
		if !present[name] {
			vanished = append(vanished, name)
		}
	}
	if len(fresh) > 0 || len(vanished) > 0 {
		resumeIndexMu.Lock()
		cur := readResumeIndexFile(dir)
		for _, name := range vanished {
			delete(cur, name)
		}
		for name, e := range fresh {
			cur[name] = e
		}
		if err := writeResumeIndexFile(dir, cur); err != nil {
			diag.Printf("resume index: write: %v\n", err)
		}
		resumeIndexMu.Unlock()
	}
	sortResumeEntries(out)
	return out, nil
}

// ListResumeEntries is the entry-based mirror of ListSessionsScoped plus
// ListLegacySessions: the entries in scope (workspace matches first, legacy
// empty-workspace sessions after them when IncludeLegacy is set), the entries
// recorded under the pre-0.5.0 shared workspace path, and the hidden count.
// With reconcile true the index is brought up to date first (stat-only when
// warm); with false only the index as it stands is read.
func ListResumeEntries(scope SessionScope, reconcile bool) (matched, legacyShared []ResumeEntry, hidden int, err error) {
	var all []ResumeEntry
	if reconcile {
		all, err = ReconcileResumeIndex()
		if err != nil {
			return nil, nil, 0, err
		}
	} else {
		all = ReadResumeIndex()
	}
	ws := func(e ResumeEntry) string { return e.Workspace }
	matched, hidden = scopeByWorkspace(all, ws, scope)
	legacyShared = legacyByWorkspace(all, ws, scope)
	return matched, legacyShared, hidden, nil
}

// SessionTitle derives a session's list title from what the user typed: the
// first line of the first user message that has typed text, trimmed and cut to
// sessionTitleRunes. Injected envelopes (memory and session retrieval, handoff,
// async results) are stripped first; messages Wakil itself writes in the user
// role (continuation and forced-final prompts, budget lines, automatic counsel
// rounds, workflow directives, the handoff continue line) are never used. It
// returns "" when nothing was typed.
func SessionTitle(conv []proxy.Message) string {
	for _, m := range conv {
		if m.Role != "user" {
			continue
		}
		if t := typedTitle(DerefStr(m.Content)); t != "" {
			return t
		}
	}
	return ""
}

// DisplayTitle is the text a list shows for the session: the typed-text title,
// else the first user message line (today's fallback), else "".
func (e ResumeEntry) DisplayTitle() string {
	if e.Title != "" {
		return e.Title
	}
	return e.Fallback
}

// sessionFallbackTitle is the first line of the first user message, whatever
// it is, cut like a title. It reproduces the pre-index listing for sessions
// that have no typed text.
func sessionFallbackTitle(s Session) string {
	_, first := SessionTurns(s)
	return cutTitle(titleFirstLine(first))
}

func titleFirstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	return strings.TrimSpace(line)
}

// cutTitle bounds line to sessionTitleRunes runes (multi-byte safe).
func cutTitle(line string) string {
	if utf8.RuneCountInString(line) > sessionTitleRunes {
		r := []rune(line)
		return strings.TrimSpace(string(r[:sessionTitleRunes])) + "…"
	}
	return line
}

// workflowDirectivePrefix opens every workflow.WorkflowState.Directive text.
const workflowDirectivePrefix = "[WORKFLOW "

// syntheticUserPrefixes are the openings of user-role messages Wakil writes
// itself. Every entry is a constant (or a prefix constant) used where the
// message is built, so a test can build each real message and check it here.
var syntheticUserPrefixes = []string{
	ForcedFinalPrompt,
	ToolLimitPrompt,
	BudgetExhaustedPrompt,
	confinementPromptPrefix,
	continuationPrompt,
	subagentBudgetPrefix,
	subagentBudgetLowPrefix,
	autoCounselPrefix,
	handoffContinueTail,
	workflowDirectivePrefix,
}

func isSyntheticUserText(text string) bool {
	for _, p := range syntheticUserPrefixes {
		if strings.HasPrefix(text, p) {
			return true
		}
	}
	return false
}

func typedTitle(content string) string {
	text := strings.TrimSpace(stripRetrievalBlock(content))
	if text == "" || isSyntheticUserText(text) {
		return ""
	}
	line := titleFirstLine(text)
	if line == "" {
		return ""
	}
	return cutTitle(line)
}
