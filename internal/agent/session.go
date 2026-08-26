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
}

// sessionsDir is where transcripts live: $WAKIL_SESSIONS_DIR, else
// $XDG_DATA_HOME/wakil/sessions, else ~/.local/share/wakil/sessions.
func sessionsDir() string {
	return resolveDataDir("WAKIL_SESSIONS_DIR", "sessions")
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
	return atomicWriteJSON(sessionPath(s.ChatID), s)
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
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var s Session
		if json.Unmarshal(b, &s) != nil {
			continue // skip malformed/partial files
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out, nil
}

// LoadSession returns the session matching idOrPrefix (a full chat_id or unique
// prefix). An empty idOrPrefix returns the most recent session.
func LoadSession(idOrPrefix string) (*Session, error) {
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
	if scope.All || scope.Workspace == "" {
		return all, 0, nil
	}
	// Collect workspace matches and legacy sessions separately, then
	// concatenate (workspace first, legacy after) — both groups are already
	// newest-first because ListSessions sorts by Updated descending and
	// filtering preserves that order. This ensures LoadSessionScoped("",
	// scope) prefers an in-scope session over a legacy one.
	var legacy []Session
	for _, s := range all {
		if sameWorkspace(s.Workspace, scope.Workspace) {
			matched = append(matched, s)
			continue
		}
		if s.Workspace == "" && scope.IncludeLegacy {
			legacy = append(legacy, s)
			continue
		}
		hidden++
	}
	matched = append(matched, legacy...)
	return matched, hidden, nil
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
	matched, _, err := ListSessionsScoped(scope)
	if err != nil {
		return nil, err
	}
	if len(matched) == 0 {
		if scope.All || scope.Workspace == "" {
			return nil, errors.New("no saved sessions found")
		}
		return nil, fmt.Errorf("no saved sessions for %s — use --all / /resume all to search every folder", scope.Workspace)
	}
	s := matched[0]
	return &s, nil
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
	sessions, hidden, err := ListSessionsScoped(scope)
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
		turns, first := SessionTurns(s)
		first = strings.ReplaceAll(first, "\n", " ")
		if len(first) > 50 {
			first = first[:50] + "…"
		}
		id := ShortID(s.ChatID)
		if s.Label != "" {
			id += " [" + s.Label + "]"
		}
		fmt.Fprintf(w, "  %-28s  %s  %2d turns  %s\n",
			id, s.Updated.Format("2006-01-02 15:04"), turns, first)
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
	sessions, hidden, err := ListSessionsScoped(scope)
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
		turns, first := SessionTurns(s)
		first = strings.ReplaceAll(first, "\n", " ")
		if len(first) > 40 {
			first = first[:40] + "…"
		}
		marker := "  "
		if s.ChatID == currentChatID {
			marker = "★ "
		}
		id := ShortID(s.ChatID)
		if s.Label != "" {
			id += " [" + s.Label + "]"
		}
		fmt.Fprintf(&b, "%s%-22s  %s  %d turns  %s\n",
			marker, id, s.Updated.Format("01-02 15:04"), turns, first)
	}
	if hidden > 0 {
		fmt.Fprintf(&b, "\n(%d session(s) in other folders — /sessions all)\n", hidden)
	}
	return strings.TrimRight(b.String(), "\n")
}
