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
	if x := os.Getenv("WAKIL_SESSIONS_DIR"); x != "" {
		return x
	}
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "wakil", "sessions")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "wakil", "sessions")
}

func sessionPath(chatID string) string {
	return filepath.Join(sessionsDir(), chatID+".json")
}

// writeSession persists s atomically (temp file + rename) so a crash mid-write
// can't corrupt an existing transcript.
func WriteSession(s *Session) error {
	dir := sessionsDir()
	if dir == "" {
		return errors.New("cannot determine sessions directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	path := sessionPath(s.ChatID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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
}

// ListSessionsScoped returns saved sessions filtered by scope, most-recently-
// updated first. When scope.All is true, or scope.Workspace is empty, every
// session is returned (equivalent to ListSessions). Otherwise only sessions
// whose recorded Workspace canonically matches scope.Workspace are returned —
// sessions with no recorded Workspace (legacy, or saved with no resolvable
// workspace) are excluded from a scoped result. hidden reports how many
// sessions were filtered out, so callers can surface an "N hidden — use all"
// hint.
func ListSessionsScoped(scope SessionScope) (matched []Session, hidden int, err error) {
	all, err := ListSessions()
	if err != nil {
		return nil, 0, err
	}
	if scope.All || scope.Workspace == "" {
		return all, 0, nil
	}
	for _, s := range all {
		if sameWorkspace(s.Workspace, scope.Workspace) {
			matched = append(matched, s)
		} else {
			hidden++
		}
	}
	return matched, hidden, nil
}

// LoadSessionScoped resolves idOrPrefix the same way LoadSession does, with
// one difference: an EMPTY idOrPrefix ("give me the latest") is resolved
// against scope, defaulting to the most recent session in scope.Workspace.
// An explicit idOrPrefix always matches against the full global list, exactly
// like LoadSession — an id/prefix the user typed should resolve regardless of
// which folder it was saved from, so hints like "resume with <id>" always
// work.
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

// PrintSessions writes a human-readable session list to w, scoped to the
// current workspace (ws) by default. Printed OLDEST-first — deliberately the
// reverse of the internal storage order — so in a scrolling terminal the most
// recent session lands at the bottom, next to the shell prompt, without
// requiring the reader to scroll up past everything else. Pass all=true (or
// an empty ws) to list every session regardless of workspace.
func PrintSessions(w io.Writer, ws string, all bool) {
	sessions, hidden, err := ListSessionsScoped(SessionScope{Workspace: ws, All: all})
	if err != nil {
		fmt.Fprintln(w, "error listing sessions:", err)
		return
	}
	scopeLabel := "all repos"
	if !all && ws != "" {
		scopeLabel = ws
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
