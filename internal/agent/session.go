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

	"wakil/internal/proxy"
	"wakil/internal/workflow"
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

// printSessions writes a human-readable session list to w.
func PrintSessions(w io.Writer) {
	sessions, err := ListSessions()
	if err != nil {
		fmt.Fprintln(w, "error listing sessions:", err)
		return
	}
	if len(sessions) == 0 {
		fmt.Fprintln(w, "no saved sessions in", sessionsDir())
		return
	}
	fmt.Fprintf(w, "saved sessions (%s):\n", sessionsDir())
	for _, s := range sessions {
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
	fmt.Fprintln(w, "\nresume with:  wakil --resume            (most recent)")
	fmt.Fprintln(w, "              wakil --resume-id <id>    (by chat_id or prefix)")
}

// sessionListText renders the saved-session list for the /sessions command,
// marking the current session with a star.
func SessionListText(currentChatID string) string {
	sessions, err := ListSessions()
	if err != nil {
		return "error listing sessions: " + err.Error()
	}
	if len(sessions) == 0 {
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
	return strings.TrimRight(b.String(), "\n")
}

