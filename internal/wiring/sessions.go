package wiring

// sessions.go: package-level session helpers for cmd/wakil (card #148 Gate
// #1, cmd half). These thin wrappers keep internal/agent out of package
// main — after the m4 cut main.go was the last production file still
// importing it. Behaviour is delegated verbatim; no logic lives here.

import (
	"io"

	"github.com/treeol/wakil/internal/agent"
)

// PrintSessions writes a human-readable session list to w, scoped to the
// given workspace by default. Pass all=true to list every session
// regardless of workspace.
func PrintSessions(w io.Writer, workspace string, all bool) {
	agent.PrintSessions(w, workspace, all)
}

// ResolveRecentSession resolves the most recent session in scope (or
// everywhere when all is true) to its chat ID, for a bare --resume.
// It returns an error when no session matches the scope; an empty string
// with a nil error is not produced today but the caller treats "" as
// "no resume target".
func ResolveRecentSession(workspace string, all bool) (string, error) {
	s, err := agent.LoadSessionScoped("", agent.SessionScope{Workspace: workspace, All: all})
	if err != nil {
		return "", err
	}
	if s == nil {
		return "", nil
	}
	return s.ChatID, nil
}

// ShortID abbreviates a chat ID to its first 8 characters for display.
func ShortID(s string) string {
	return agent.ShortID(s)
}
