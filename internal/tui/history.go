package tui

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

const historyMaxLines = 5000

// historyPath returns the path to the shared input history file:
// $WAKIL_HISTORY_FILE, else $XDG_DATA_HOME/wakil/input_history, else
// ~/.local/share/wakil/input_history.
func historyPath() string {
	if x := os.Getenv("WAKIL_HISTORY_FILE"); x != "" {
		return x
	}
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "wakil", "input_history")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "wakil", "input_history")
}

// loadHistory reads the history file and returns entries most-recent-first
// (the file is stored oldest-first, one entry per line).
func loadHistory() []string {
	path := historyPath()
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil // file doesn't exist yet — fine
	}
	defer f.Close()

	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			lines = append(lines, line)
		}
	}
	// Reverse so [0] is most recent.
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	return lines
}

// appendHistory appends entry to the history file (oldest-first on disk).
// It is a best-effort write: errors are silently ignored.
func appendHistory(entry string) {
	path := historyPath()
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(entry + "\n")
}
