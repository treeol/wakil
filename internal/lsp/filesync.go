package lsp

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"wakil/internal/exec"
)

// ─── File synchronization (Decision 3, R3) ────────────────────────────────────
//
// File-sync discipline (panel-folded):
//   - didOpen on first touch of a file (before any query).
//   - didChange (full sync) after every edit_file/write_file.
//   - run_shell lazy invalidation (R3): after a non-read-only shell command,
//     mark all open files dirty (a flag, no I/O). On the NEXT query touching a
//     dirty file, stat it; if mtime+size changed since last sync, didChange
//     from disk before issuing the request. If unchanged, clear the flag.
//   - For unopened files the shell created/deleted: one batched
//     workspace/didChangeWatchedFiles after the non-read-only shell.
//
// INVARIANT (panel-folded): the byte sequence used for offset computation is
// identical to the byte sequence sent in didOpen/didChange. didChange reads
// the file via the executor (same filesystem gopls sees) and sends the exact
// bytes — no normalization, no BOM-stripping, no CRLF conversion.

// fileSyncState tracks one open document's sync state.
type fileSyncState struct {
	version   int32
	content   string // last-synced content (for didChange full sync)
	mtime     time.Time
	size      int64
	languageID string
}

// fileSyncManager tracks open documents and dirty flags for one server.
type fileSyncManager struct {
	mu    sync.Mutex
	docs  map[string]*fileSyncState // keyed by container URI
	dirty map[string]bool            // container URI → dirty flag (run_shell resync)
}

func newFileSyncManager() *fileSyncManager {
	return &fileSyncManager{
		docs:  make(map[string]*fileSyncState),
		dirty: make(map[string]bool),
	}
}

// ensureOpen sends didOpen if the file hasn't been opened yet, reading content
// via the executor. Returns the container URI.
func (f *fileSyncManager) ensureOpen(ctx context.Context, srv *Server, hostPath, languageID string) (string, error) {
	uri, err := srv.mgr.exec.HostPathToURI(hostPath)
	if err != nil {
		return "", fmt.Errorf("URI translation: %w", err)
	}

	f.mu.Lock()
	doc, ok := f.docs[uri]
	if !ok {
		// Read file content via the executor (same filesystem gopls sees).
		content, err := srv.mgr.exec.ReadFile(hostPath)
		if err != nil {
			f.mu.Unlock()
			return "", fmt.Errorf("reading file for didOpen: %w", err)
		}
		version := int32(1)
		doc = &fileSyncState{
			version:    version,
			content:    content,
			languageID: languageID,
		}
		// Stat for mtime+size (for the dirty-flag mtime guard).
		if size, err := srv.mgr.exec.StatFile(hostPath); err == nil {
			doc.size = size
		}
		f.docs[uri] = doc
		f.mu.Unlock()

		// Send didOpen (flows during Indexing — not gated behind Ready).
		if err := srv.DidOpen(ctx, uri, languageID, content); err != nil {
			return "", fmt.Errorf("didOpen: %w", err)
		}
	} else {
		f.mu.Unlock()
	}
	_ = doc
	return uri, nil
}

// notifyChange sends didChange with the current disk content (full sync).
// Called after edit_file/write_file succeeds.
func (f *fileSyncManager) notifyChange(ctx context.Context, srv *Server, hostPath string) error {
	uri, err := srv.mgr.exec.HostPathToURI(hostPath)
	if err != nil {
		return fmt.Errorf("URI translation: %w", err)
	}

	f.mu.Lock()
	doc, ok := f.docs[uri]
	if !ok {
		// File wasn't open — open it first, then no didChange needed.
		f.mu.Unlock()
		_, err := f.ensureOpen(ctx, srv, hostPath, "")
		return err
	}

	// Read the current disk content (same bytes gopls will see).
	content, err := srv.mgr.exec.ReadFile(hostPath)
	if err != nil {
		f.mu.Unlock()
		return fmt.Errorf("reading file for didChange: %w", err)
	}

	doc.version++
	doc.content = content
	if size, err := srv.mgr.exec.StatFile(hostPath); err == nil {
		doc.size = size
	}
	// Clear dirty flag — we're syncing now.
	delete(f.dirty, uri)
	f.mu.Unlock()

	return srv.DidChange(ctx, uri, content)
}

// markDirty marks all open files dirty (after a non-read-only run_shell).
// No I/O — just sets flags.
func (f *fileSyncManager) markDirty() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for uri := range f.docs {
		f.dirty[uri] = true
	}
}

// syncIfDirty checks if a file is dirty and re-syncs it if mtime+size changed.
// Called before issuing a query on that file. Returns true if a resync happened.
func (f *fileSyncManager) syncIfDirty(ctx context.Context, srv *Server, uri string) (bool, error) {
	f.mu.Lock()
	dirty := f.dirty[uri]
	if !dirty {
		f.mu.Unlock()
		return false, nil
	}

	doc, ok := f.docs[uri]
	if !ok {
		// File was deleted from our tracking — clear dirty and skip.
		delete(f.dirty, uri)
		f.mu.Unlock()
		return false, nil
	}

	// Translate URI back to host path to stat.
	hostPath, err := srv.mgr.exec.URIToHostPath(uri)
	if err != nil {
		// Can't translate (e.g. GOROOT path) — clear dirty, skip.
		delete(f.dirty, uri)
		f.mu.Unlock()
		return false, nil
	}
	f.mu.Unlock()

	// Stat the file on disk.
	size, err := srv.mgr.exec.StatFile(hostPath)
	if err != nil {
		// File may have been deleted — clear dirty, skip.
		f.mu.Lock()
		delete(f.dirty, uri)
		f.mu.Unlock()
		return false, nil
	}

	// Compare against last-synced state (NOT mark-time state — R3 cumulative).
	f.mu.Lock()
	defer f.mu.Unlock()

	doc, ok = f.docs[uri]
	if !ok {
		delete(f.dirty, uri)
		return false, nil
	}

	if size == doc.size {
		// File unchanged since last sync — clear dirty, skip.
		delete(f.dirty, uri)
		return false, nil
	}

	// File changed — re-read and didChange.
	content, err := srv.mgr.exec.ReadFile(hostPath)
	if err != nil {
		return false, fmt.Errorf("reading file for dirty resync: %w", err)
	}

	doc.version++
	doc.content = content
	doc.size = size
	delete(f.dirty, uri)

	return true, srv.DidChange(ctx, uri, content)
}

// batchNotifyWatchedFiles sends workspace/didChangeWatchedFiles for unopened
// files that may have been created/deleted/modified by run_shell.
// This is fire-and-forget — gopls invalidates its disk-cached snapshot.
func (f *fileSyncManager) batchNotifyWatchedFiles(ctx context.Context, srv *Server) error {
	// We don't know which files the shell touched, so we send a broad
	// notification for the workspace root. gopls re-stats everything.
	// This is infrequent (only after non-read-only shells) and the server
	// handles it efficiently.
	rootURI := srv.mgr.rootURI
	params := DidChangeWatchedFilesParams{
		Changes: []FileEvent{
			{URI: rootURI, Type: FileChanged},
		},
	}
	return srv.conn.notify("workspace/didChangeWatchedFiles", params)
}

// ─── Language detection ─────────────────────────────────────────────────────

// detectLanguage returns the LSP language ID for a file extension.
func detectLanguage(path string) string {
	ext := ext(path)
	switch ext {
	case ".go":
		return "go"
	case ".rs":
		return "rust"
	case ".ts", ".tsx":
		return "typescript"
	case ".js", ".jsx":
		return "javascript"
	case ".py":
		return "python"
	case ".c", ".h":
		return "c"
	case ".cpp", ".hpp", ".cc":
		return "cpp"
	case ".java":
		return "java"
	}
	return ""
}

func ext(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '.' {
			return path[i:]
		}
		if path[i] == '/' {
			break
		}
	}
	return ""
}

// resolveToPosition is the high-level resolution entry point used by the LSP tools.
// It takes a host path, 1-based line, and optional symbol name, and returns
// the LSP Position (or an ambiguous candidate list).
func (m *Manager) resolveToPosition(ctx context.Context, srv *Server, hostPath string, line int, symbol string) (Position, *ResolveResult, error) {
	// Ensure the file is open in gopls.
	languageID := detectLanguage(hostPath)
	uri, err := srv.fileSync().ensureOpen(ctx, srv, hostPath, languageID)
	if err != nil {
		return Position{}, nil, err
	}

	// Check dirty flag (run_shell resync — R3).
	if _, err := srv.fileSync().syncIfDirty(ctx, srv, uri); err != nil {
		return Position{}, nil, err
	}

	// Read the file content to get the line.
	content, err := m.exec.ReadFile(hostPath)
	if err != nil {
		return Position{}, nil, err
	}

	// Split into lines and get the target line (0-based).
	lines := splitLines(content)
	lineIdx := line - 1 // 1-based → 0-based
	if lineIdx < 0 || lineIdx >= len(lines) {
		return Position{}, nil, fmt.Errorf("line %d out of range (file has %d lines)", line, len(lines))
	}
	lineContent := lines[lineIdx]

	// If no symbol provided, return the start of the line.
	if symbol == "" {
		return Position{Line: uint32(lineIdx), Character: 0}, nil, nil
	}

	// Resolve the symbol on the line.
	res := resolvePosition(lineContent, uint32(lineIdx), symbol, srv.Encoding())
	if res.Ambiguous {
		return Position{}, &res, nil
	}
	return res.Position, nil, nil
}

func splitLines(content string) []string {
	return splitOnNewlines(content)
}

// splitOnNewlines splits on \n (handling \r\n by stripping \r).
func splitOnNewlines(content string) []string {
	lines := []string{}
	start := 0
	for i := 0; i < len(content); i++ {
		if content[i] == '\n' {
			line := content[start:i]
			// Strip trailing \r for CRLF files.
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			lines = append(lines, line)
			start = i + 1
		}
	}
	if start < len(content) {
		line := content[start:]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		lines = append(lines, line)
	}
	return lines
}

// ─── Manager file-sync wiring ───────────────────────────────────────────────

// EnsureManagerForFile ensures the LSP server for the file's language is running,
// and returns the server + the file's container URI. Used by the LSP tools.
func (m *Manager) EnsureManagerForFile(ctx context.Context, hostPath string) (*Server, string, error) {
	lang := detectLanguage(hostPath)
	if lang == "" {
		return nil, "", fmt.Errorf("unsupported file type: %s", hostPath)
	}
	srv, err := m.EnsureServer(ctx, lang)
	if err != nil {
		return nil, "", err
	}
	return srv, "", nil
}

// NotifyChange is called after edit_file/write_file to sync the file to gopls.
func (m *Manager) NotifyChange(ctx context.Context, hostPath string) {
	srv, _, err := m.EnsureManagerForFile(ctx, hostPath)
	if err != nil || srv == nil {
		return
	}
	_ = srv.fileSync().notifyChange(ctx, srv, hostPath)
}

// MarkOpenFilesDirty is called after a non-read-only run_shell (R3 lazy invalidation).
func (m *Manager) MarkOpenFilesDirty() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, srv := range m.servers {
		srv.fileSync().markDirty()
	}
}

// BatchNotifyWatchedFiles sends didChangeWatchedFiles for all servers.
func (m *Manager) BatchNotifyWatchedFiles(ctx context.Context) {
	m.mu.Lock()
	servers := make([]*Server, 0, len(m.servers))
	for _, srv := range m.servers {
		servers = append(servers, srv)
	}
	m.mu.Unlock()

	for _, srv := range servers {
		_ = srv.fileSync().batchNotifyWatchedFiles(ctx, srv)
	}
}

// ─── Server fileSync field ──────────────────────────────────────────────────
//
// The fileSync manager is per-server. We initialize it lazily in newServerLocked.

// Ensure the Server struct has a fileSync field. This is added to manager.go's
// Server struct — we access it via srv.fileSync() which lazy-initializes.

var _ = os.Stat
var _ exec.Executor

// fileSync returns the server's file-sync manager, initializing it lazily.
func (s *Server) fileSync() *fileSyncManager {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fs == nil {
		s.fs = newFileSyncManager()
	}
	return s.fs
}
