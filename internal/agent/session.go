package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/treeol/wakil/internal/diag"
	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/workflow"
)

// Session is the persisted record of one conversation. The full transcript is
// stored so a session can be reloaded verbatim, and the chat_id is kept so the
// proxy's server-side memory for that conversation can be re-attached on resume.
type Session struct {
	ChatID        string                  `json:"chat_id"`
	Model         string                  `json:"model"`
	EndpointName  string                  `json:"endpoint_name,omitempty"`
	Label         string                  `json:"label,omitempty"`
	Workspace     string                  `json:"workspace,omitempty"`
	Created       time.Time               `json:"created"`
	Updated       time.Time               `json:"updated"`
	Conv          []proxy.Message         `json:"conv"`
	SavedWorkflow *workflow.WorkflowState `json:"saved_workflow,omitempty"`

	// Long-session collapse state (persisted so /resume restores the
	// projection). Collapsed maps turn → archive record; Keep is the set of
	// turns exempted from collapse by /keep.
	Collapsed []CollapseRecord `json:"collapsed,omitempty"`
	Keep      []int            `json:"keep,omitempty"`

	// Round-pruning state (prune.go), persisted next to Collapsed/Keep.
	// Present only when context_prune was on; old sessions have none and
	// resume with an empty prune state.
	PruneRecords       []RoundRecord `json:"prune_records,omitempty"`
	PruneLocked        []RereadLock  `json:"prune_locked,omitempty"`
	PruneBytesSaved    int           `json:"prune_bytes_saved,omitempty"`
	PruneArchiveFailed bool          `json:"prune_archive_failed,omitempty"`
	PruneMissingPrev   int           `json:"prune_missing_prev,omitempty"`
	PruneReread        int           `json:"prune_reread,omitempty"`
	PruneMarkCount     int           `json:"prune_mark_count,omitempty"`
	PruneOverride      string        `json:"prune_override,omitempty"` // "on"|"off" per-chat /prune toggle; "" = inherit config

	// CostRows persists the session cost totals so they survive save/restore
	// (/resume). Empty on old sessions (resume with no prior cost history).
	CostRows []proxy.CostRow `json:"cost_rows,omitempty"`

	// LoadedModules persists the context-module names loaded in this session
	// (context_modules on) so they survive /resume. Empty on old sessions
	// (saved by v0.8.10, which did not persist them) and when the switch is
	// off; a resumed session with the switch on then behaves as before.
	LoadedModules []string `json:"loaded_modules,omitempty"`
}

// sessionsDir is where transcripts live: $WAKIL_SESSIONS_DIR, else
// $XDG_DATA_HOME/wakil/sessions, else ~/.local/share/wakil/sessions.
func sessionsDir() string {
	return resolveDataDir("WAKIL_SESSIONS_DIR", "sessions")
}

// SessionsDir is the exported form of sessionsDir, for use by the CLI.
func SessionsDir() string {
	return sessionsDir()
}

// SessionFilePath is the exported form of sessionPath: the one place that
// knows how a chat ID maps to its session file. "" when no sessions dir is
// configured. The `wakil plan gc` liveness check goes through this instead of
// re-deriving the file name.
func SessionFilePath(chatID string) string {
	return sessionPath(chatID)
}

func sessionPath(chatID string) string {
	dir := sessionsDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, chatID+".json")
}

// writeSession persists s using a crash-durable atomic write (temp file +
// fsync + rename) so a crash mid-write can't corrupt an existing transcript.
func WriteSession(s *Session) error {
	dir := sessionsDir()
	if dir == "" {
		return errors.New("cannot determine sessions directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := atomicWriteJSON(sessionPath(s.ChatID), s); err != nil {
		return err
	}
	// Keep the resume index current. Best effort: the index is derived and
	// rebuildable, so a failure here never fails the save.
	updateResumeIndexEntry(dir, s)
	return nil
}

// ListSessions returns all saved sessions, most-recently-updated first.
func ListSessions() ([]Session, error) {
	dir := sessionsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Session
	for _, e := range entries {
		if e.IsDir() || !isSessionFileName(e.Name()) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			if !os.IsNotExist(err) {
				diag.Printf("ListSessions: read %s: %v", e.Name(), err)
			}
			continue
		}
		var s Session
		if err := json.Unmarshal(b, &s); err != nil {
			diag.Printf("ListSessions: unmarshal %s: %v", e.Name(), err)
			continue // skip malformed/partial files
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Updated.Equal(out[j].Updated) {
			return out[i].Updated.After(out[j].Updated)
		}
		return out[i].ChatID < out[j].ChatID
	})
	return out, nil
}

// LoadSession returns the session matching idOrPrefix (a full chat_id or unique
// prefix). An empty idOrPrefix returns the most recent session.
func LoadSession(idOrPrefix string) (*Session, error) {
	if idOrPrefix != "" {
		if s, ok, err := loadSessionByName(idOrPrefix); ok || err != nil {
			return s, err
		}
	}
	sessions, err := ListSessions()
	if err != nil {
		return nil, err
	}
	if len(sessions) == 0 {
		return nil, errors.New("no saved sessions found")
	}
	if idOrPrefix == "" {
		s := sessions[0]
		return &s, nil
	}
	var matches []Session
	for _, s := range sessions {
		if s.ChatID == idOrPrefix || strings.HasPrefix(s.ChatID, idOrPrefix) {
			matches = append(matches, s)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no session matching %q", idOrPrefix)
	case 1:
		return &matches[0], nil
	default:
		return nil, fmt.Errorf("%q is ambiguous — matches %d sessions", idOrPrefix, len(matches))
	}
}

// safeSessionID reports whether id can be used to build a session file name:
// non-empty, no path separator, no "..", not a dot file.
func safeSessionID(id string) bool {
	return id != "" && !strings.ContainsAny(id, `/\`) && !strings.Contains(id, "..") && !strings.HasPrefix(id, ".")
}

// loadSessionByName resolves idOrPrefix by file name and reads ONLY the
// matching file, instead of decoding every transcript. ok is false when the
// fast path cannot decide (unsafe id, no name match, a name match whose inner
// chat_id disagrees, unreadable file) and the caller must use the full scan.
// Anything but exactly one name match defers to the scan, which also owns the
// "ambiguous" error.
// Candidates are directory names; handoff sidecars and dot files never count.
func loadSessionByName(idOrPrefix string) (*Session, bool, error) {
	if !safeSessionID(idOrPrefix) {
		return nil, false, nil
	}
	dir := sessionsDir()
	if dir == "" {
		return nil, false, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, false, nil
	}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !isSessionFileName(n) {
			continue
		}
		if strings.HasPrefix(strings.TrimSuffix(n, ".json"), idOrPrefix) {
			names = append(names, n)
		}
	}
	// Exactly one file-name candidate is the fast path. Zero or several fall
	// back to the full scan, which counts decodable sessions by inner chat_id:
	// a corrupt or mismatched file must not turn a unique match into
	// "ambiguous".
	if len(names) != 1 {
		return nil, false, nil
	}
	b, err := os.ReadFile(filepath.Join(dir, names[0]))
	if err != nil {
		return nil, false, nil
	}
	var s Session
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, false, nil
	}
	if s.ChatID == "" || !strings.HasPrefix(s.ChatID, idOrPrefix) {
		return nil, false, nil // inner id disagrees with the file name: scan decides
	}
	return &s, true, nil
}

// SessionScope narrows a session listing/load to one workspace (folder), or
// to everything when All is true. Workspace is matched via canonicalWorkspace
// (Abs + EvalSymlinks) — the same identity repo-state uses — so a session
// saved from a symlinked or relative path still matches consistently.
type SessionScope struct {
	Workspace string // canonical match target; ignored when All is true
	All       bool   // true = return every session regardless of Workspace

	// IncludeLegacy also returns sessions whose recorded Workspace is empty
	// (saved before workspace recording existed, or by a process whose config
	// resolved no workspace). Without it they are excluded from every scoped
	// result — invisible in listings and resume pickers even under --all's
	// workspace-scoped default paths, which made them look lost. When
	// IncludeLegacy is set they are appended after workspace matches (so bare
	// --resume still prefers an in-scope session). They are NOT counted toward
	// hidden. Note: resuming a legacy session and saving it backfills its
	// Workspace to the current one (a deliberate one-way migration — the
	// session joins the workspace going forward). Session-history
	// reconciliation must not set this flag (an unknown-workspace transcript
	// belongs to no folder).
	IncludeLegacy bool
	// LegacyWorkspace is the pre-0.5.0 shared workspace path (see
	// LegacyWorkspacePath). Sessions recorded under it, and not under
	// Workspace, are returned separately as legacy (0.5.0, C2) instead of hidden.
	LegacyWorkspace string
}

// ListSessionsScoped returns saved sessions filtered by scope, most-recently-
// updated first. When scope.All is true, or scope.Workspace is empty, every
// session is returned (equivalent to ListSessions). Otherwise only sessions
// whose recorded Workspace canonically matches scope.Workspace are returned;
// when scope.IncludeLegacy is set, sessions with no recorded Workspace
// (legacy) are appended AFTER the workspace matches so bare --resume prefers
// an in-scope session over a legacy one. hidden reports how many sessions
// were filtered out (excluded other-workspace sessions; legacy sessions are
// never counted as hidden when IncludeLegacy is set), so callers can surface
// an "N hidden — use all" hint.
func ListSessionsScoped(scope SessionScope) (matched []Session, hidden int, err error) {
	all, err := ListSessions()
	if err != nil {
		return nil, 0, err
	}
	matched, hidden = scopeByWorkspace(all, func(s Session) string { return s.Workspace }, scope)
	return matched, hidden, nil
}

// scopeByWorkspace applies a SessionScope to a newest-first list: workspace
// matches first, then legacy (empty-workspace) items when IncludeLegacy is set,
// everything else counted as hidden (except items recorded under
// scope.LegacyWorkspace, which are listed separately by legacyByWorkspace).
// It is shared by the full-decode and the index listings so they cannot drift.
// Workspace keys are memoised for the call: each distinct workspace string is
// canonicalised (Abs + EvalSymlinks + sha256) once, not once per item.
func scopeByWorkspace[T any](all []T, workspaceOf func(T) string, scope SessionScope) (matched []T, hidden int) {
	if scope.All || scope.Workspace == "" {
		return all, 0
	}
	same := newSameWorkspace()
	var legacy []T
	for _, it := range all {
		w := workspaceOf(it)
		if same(w, scope.Workspace) {
			matched = append(matched, it)
			continue
		}
		if w == "" && scope.IncludeLegacy {
			legacy = append(legacy, it)
			continue
		}
		// 0.5.0: sessions recorded under the legacy shared path are not hidden;
		// they are listed as legacy (ListLegacySessions) and not counted here.
		if scope.LegacyWorkspace != "" && same(w, scope.LegacyWorkspace) {
			continue
		}
		hidden++
	}
	return append(matched, legacy...), hidden
}

// legacyByWorkspace returns the items recorded under the pre-0.5.0 shared
// workspace path, in the input order.
func legacyByWorkspace[T any](all []T, workspaceOf func(T) string, scope SessionScope) []T {
	if scope.LegacyWorkspace == "" || sameWorkspace(scope.LegacyWorkspace, scope.Workspace) {
		return nil
	}
	same := newSameWorkspace()
	var out []T
	for _, it := range all {
		if same(workspaceOf(it), scope.LegacyWorkspace) {
			out = append(out, it)
		}
	}
	return out
}

// newSameWorkspace returns sameWorkspace with a per-call key cache.
func newSameWorkspace() func(a, b string) bool {
	keys := map[string]string{}
	key := func(ws string) string {
		if k, ok := keys[ws]; ok {
			return k
		}
		k := workspaceKey(ws)
		keys[ws] = k
		return k
	}
	return func(a, b string) bool {
		ka, kb := key(a), key(b)
		return ka != "" && kb != "" && ka == kb
	}
}

// ListLegacySessions returns the sessions recorded under the pre-0.5.0 shared
// workspace path (scope.LegacyWorkspace), most-recently-updated first, when
// that path is not the project itself (0.5.0, C2). They are read-only in the
// resume picker's "legacy (shared store)" section; nothing is moved or rewritten.
func ListLegacySessions(scope SessionScope) ([]Session, error) {
	if scope.LegacyWorkspace == "" || sameWorkspace(scope.LegacyWorkspace, scope.Workspace) {
		return nil, nil
	}
	all, err := ListSessions()
	if err != nil {
		return nil, err
	}
	return legacyByWorkspace(all, func(s Session) string { return s.Workspace }, scope), nil
}

// LoadSessionScoped resolves idOrPrefix the same way LoadSession does, with
// one difference: an EMPTY idOrPrefix ("give me the latest") is resolved
// against scope, defaulting to the most recent session in scope.Workspace.
// An explicit idOrPrefix always matches against the full global list, exactly
// like LoadSession — an id/prefix the user typed should resolve regardless of
// which folder it was saved from, so hints like "resume with <id>" always
// work.
//
// With scope.IncludeLegacy set and no in-scope match for an empty idOrPrefix,
// the fallback list still contains legacy (empty-workspace) sessions appended
// after workspace matches — so `wakil --resume` can find a pre-workspace-
// recording session instead of failing outright.
func LoadSessionScoped(idOrPrefix string, scope SessionScope) (*Session, error) {
	if idOrPrefix != "" {
		return LoadSession(idOrPrefix)
	}
	matched, _, _, err := ListResumeEntries(scope, true)
	if err != nil {
		return nil, err
	}
	if len(matched) == 0 {
		if scope.All || scope.Workspace == "" {
			return nil, errors.New("no saved sessions found")
		}
		return nil, fmt.Errorf("no saved sessions for %s — use --all / /resume all to search every folder", scope.Workspace)
	}
	// Read just the newest session's file; fall back to the full scan if the
	// file moved or changed shape since the reconcile.
	if s, ok := loadSessionFile(matched[0].File, matched[0].ID); ok {
		return s, nil
	}
	return LoadSession(matched[0].ID)
}

// loadSessionFile reads one session file by name and accepts it only when its
// inner chat_id equals wantID.
func loadSessionFile(name, wantID string) (*Session, bool) {
	dir := sessionsDir()
	if dir == "" || !isSessionFileName(name) || strings.ContainsAny(name, `/\`) {
		return nil, false
	}
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return nil, false
	}
	var s Session
	if err := json.Unmarshal(b, &s); err != nil || s.ChatID != wantID {
		return nil, false
	}
	return &s, true
}

// sessionTurns counts user turns and returns the first user message (for listing).
func SessionTurns(s Session) (int, string) {
	turns, first := 0, ""
	for _, m := range s.Conv {
		if m.Role == "user" {
			turns++
			if first == "" {
				first = DerefStr(m.Content)
			}
		}
	}
	return turns, first
}

// PrintSessionsWithScope writes a human-readable session list to w for the
// given scope. Printed OLDEST-first — deliberately the reverse of the internal
// storage order — so in a scrolling terminal the most recent session lands at
// the bottom, next to the shell prompt, without requiring the reader to scroll
// up past everything else. With All=true every session is listed regardless of
// workspace; otherwise the list is scoped to Workspace, plus legacy (empty-
// workspace) sessions when IncludeLegacy is set.
func PrintSessionsWithScope(w io.Writer, scope SessionScope) {
	sessions, _, hidden, err := ListResumeEntries(scope, true)
	if err != nil {
		fmt.Fprintln(w, "error listing sessions:", err)
		return
	}
	scopeLabel := "all repos"
	if !scope.All && scope.Workspace != "" {
		scopeLabel = scope.Workspace
	}
	if len(sessions) == 0 {
		fmt.Fprintln(w, "no saved sessions for", scopeLabel, "in", sessionsDir())
		if hidden > 0 {
			fmt.Fprintf(w, "(%d session(s) in other folders — pass --all to see them)\n", hidden)
		}
		return
	}
	fmt.Fprintf(w, "saved sessions for %s (%s):\n", scopeLabel, sessionsDir())
	// Reverse to oldest-first for display; sessions is newest-first internally.
	for i := len(sessions) - 1; i >= 0; i-- {
		s := sessions[i]
		id := ShortID(s.ID)
		if s.Label != "" {
			id += " [" + s.Label + "]"
		}
		fmt.Fprintf(w, "  %-28s  %s  %2d turns  %s\n",
			id, s.Updated.Format("2006-01-02 15:04"), s.Turns, s.DisplayTitle())
	}
	if hidden > 0 {
		fmt.Fprintf(w, "\n(%d session(s) in other folders hidden — pass --all to see them)\n", hidden)
	}
	fmt.Fprintln(w, "\nresume with:  wakil --resume            (most recent in this folder)")
	fmt.Fprintln(w, "              wakil --resume-id <id>    (by chat_id or prefix, any folder)")
}

// SessionListText renders the saved-session list for the /sessions TUI
// command, marking the current session with a star, scoped per scope.
// Newest-first (unlike PrintSessions) — this renders into a fixed note in the
// TUI, not a scrolling terminal dump, so newest-on-top is the natural read
// order there.
func SessionListText(currentChatID string, scope SessionScope) string {
	sessions, _, hidden, err := ListResumeEntries(scope, true)
	if err != nil {
		return "error listing sessions: " + err.Error()
	}
	if len(sessions) == 0 {
		if hidden > 0 {
			return fmt.Sprintf("no saved sessions for this folder (%d in other folders — /sessions all)", hidden)
		}
		return "no saved sessions yet"
	}
	var b strings.Builder
	for _, s := range sessions {
		marker := "  "
		if s.ID == currentChatID {
			marker = "★ "
		}
		id := ShortID(s.ID)
		if s.Label != "" {
			id += " [" + s.Label + "]"
		}
		fmt.Fprintf(&b, "%s%-22s  %s  %d turns  %s\n",
			marker, id, s.Updated.Format("01-02 15:04"), s.Turns, s.DisplayTitle())
	}
	if hidden > 0 {
		fmt.Fprintf(&b, "\n(%d session(s) in other folders — /sessions all)\n", hidden)
	}
	return strings.TrimRight(b.String(), "\n")
}
