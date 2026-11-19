package agent

// Recovery of collapse archives and spill files: every tool the system prompt
// names for it (read_file, read_file_full, search_files) must work on the
// session's OWN toolcache/<chat>/ files and on nothing else under toolcache/.
// A refusal on the session's own archive path must not count toward the
// path-confinement breaker.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
	wtools "github.com/treeol/wakil/internal/tools"
)

// archiveApp is an App whose chat is chatID, on an executor that rejects every
// path the way a sandboxed ConfinePath rejects host-side files, and whose
// shell cannot see the toolcache (RunShell would fail the test: the host-side
// search must not go through it).
func archiveApp(t *testing.T, chatID string) (*App, *fakeExecutor) {
	t.Helper()
	exec := newFakeExecutor()
	exec.confineErrFn = func(path string) error { return &confinementRejection{path: path} }
	app := &App{
		Cfg:     config.DefaultConfig(),
		Exec:    exec,
		Out:     io.Discard,
		Client:  &proxy.Client{ChatID: chatID},
		Confirm: func(_, _, _ string, _ bool) bool { return true },
	}
	return app, exec
}

func archiveSearchCall(args map[string]string) proxy.ToolCall {
	return proxy.ToolCall{Function: proxy.FunctionCall{Name: "search_files", Arguments: mustJSON(args)}}
}

// A case-insensitive search (a bool argument, so built apart from the string map).
func TestSearchFilesOwnArchiveCaseInsensitive(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	app, _ := archiveApp(t, "chat-ci")
	archive := wtools.SpillToCache("chat-ci", "turn-1", "Mixed CASE line\n")
	args := fmt.Sprintf(`{"pattern":"mixed case","path":%q,"case_insensitive":true}`, archive)
	res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{Name: "search_files", Arguments: args}})
	if !res.ok || !strings.Contains(res.text, "Mixed CASE line") {
		t.Fatalf("case-insensitive search: ok=%v %q", res.ok, res.text)
	}
	args = fmt.Sprintf(`{"pattern":"mixed case","path":%q}`, archive)
	res = app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{Name: "search_files", Arguments: args}})
	if res.text != "(no matches)" {
		t.Errorf("case-sensitive search must not match: %q", res.text)
	}
}

func TestSearchFilesWorksOnOwnArchive(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	app, exec := archiveApp(t, "chat-own")
	archive := wtools.SpillToCache("chat-own", "turn-3", "alpha\nthe NEEDLE is here\nomega\n")
	other := wtools.SpillToCache("chat-own", "turn-4", "nothing to see\n")
	if archive == "" || other == "" {
		t.Fatal("setup: spill failed")
	}

	// One archive file.
	res := app.handleToolCall(context.Background(), archiveSearchCall(map[string]string{"pattern": "NEEDLE", "path": archive}))
	if !res.ok || !strings.Contains(res.text, archive+":2:the NEEDLE is here") {
		t.Fatalf("search on the own archive file: ok=%v %q", res.ok, res.text)
	}
	// The session's archive directory, recursively; a miss is "(no matches)".
	dir := filepath.Dir(archive)
	res = app.handleToolCall(context.Background(), archiveSearchCall(map[string]string{"pattern": "NEEDLE", "path": dir}))
	if !res.ok || strings.Count(res.text, "NEEDLE") != 1 || strings.Contains(res.text, "nothing to see") {
		t.Fatalf("search on the own archive directory: ok=%v %q", res.ok, res.text)
	}
	res = app.handleToolCall(context.Background(), archiveSearchCall(map[string]string{"pattern": "absent-xyz", "path": dir}))
	if !res.ok || res.text != "(no matches)" {
		t.Errorf("no match: ok=%v %q", res.ok, res.text)
	}
	// file_pattern filters by file name.
	res = app.handleToolCall(context.Background(), archiveSearchCall(map[string]string{"pattern": "NEEDLE", "path": dir, "file_pattern": "nomatch-*.txt"}))
	if res.text != "(no matches)" {
		t.Errorf("file_pattern must filter: %q", res.text)
	}
	if n := len(exec.shellCalls); n != 0 {
		t.Errorf("the host-side search must not use the sandbox shell, saw %d call(s): %v", n, exec.shellCalls)
	}
}

func TestSearchFilesRefusesOtherSessionsArchive(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	app, exec := archiveApp(t, "chat-own")
	foreign := wtools.SpillToCache("chat-foreign", "turn-1", "SECRET-OF-ANOTHER-SESSION\n")
	if foreign == "" {
		t.Fatal("setup: spill failed")
	}
	for _, p := range []string{
		foreign,                // a file of another session
		filepath.Dir(foreign),  // its directory
		wtools.ToolCacheRoot(), // the whole toolcache
		filepath.Join(wtools.ToolCacheRoot(), "chat-own", "..", "chat-foreign", "turn-1"), // traversal out of the own dir
		filepath.Join(wtools.ToolCacheRoot(), "chat-own-evil"),                            // a sibling sharing the prefix
	} {
		res := app.handleToolCall(context.Background(), archiveSearchCall(map[string]string{"pattern": "SECRET", "path": p}))
		if res.ok || strings.Contains(res.text, "SECRET-OF-ANOTHER-SESSION") {
			t.Errorf("path %q must be refused and must not leak: ok=%v %q", p, res.ok, res.text)
		}
		if !strings.Contains(res.text, "outside workspace") {
			t.Errorf("path %q: want the ordinary confinement refusal, got %q", p, res.text)
		}
	}
	_ = exec
}

// A symlink inside the own directory that points at another session's file is
// not followed.
func TestSearchFilesDoesNotFollowSymlinkOutOfOwnArchive(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	app, _ := archiveApp(t, "chat-own")
	own := wtools.SpillToCache("chat-own", "turn-1", "mine\n")
	foreign := wtools.SpillToCache("chat-foreign", "turn-1", "SECRET-OF-ANOTHER-SESSION\n")
	link := filepath.Join(filepath.Dir(own), "link.txt")
	if err := os.Symlink(foreign, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	res := app.handleToolCall(context.Background(), archiveSearchCall(map[string]string{"pattern": "SECRET", "path": filepath.Dir(own)}))
	if strings.Contains(res.text, "SECRET-OF-ANOTHER-SESSION") {
		t.Fatalf("a symlink led out of the own archive directory: %q", res.text)
	}
	res = app.handleToolCall(context.Background(), archiveSearchCall(map[string]string{"pattern": "SECRET", "path": link}))
	if strings.Contains(res.text, "SECRET-OF-ANOTHER-SESSION") {
		t.Fatalf("searching the symlink itself leaked: %q", res.text)
	}
}

// A chat ID that is not one directory name never matches (no chat-id based
// traversal), and an empty chat ID has no own archive.
func TestOwnToolCachePathRejectsBadChatIDs(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	root := wtools.ToolCacheRoot()
	for _, id := range []string{"", ".", "..", "a/b", `a\b`, "../x"} {
		if wtools.OwnToolCachePath(id, filepath.Join(root, id, "f.txt")) || wtools.OwnToolCachePath(id, root) {
			t.Errorf("chat ID %q must have no own archive", id)
		}
	}
	if !wtools.OwnToolCachePath("c1", filepath.Join(root, "c1", "f.txt")) || !wtools.OwnToolCachePath("c1", filepath.Join(root, "c1")) {
		t.Error("the own directory and its files must match")
	}
	if wtools.OwnToolCachePath("c1", filepath.Join(root, "c10", "f.txt")) {
		t.Error("a sibling directory sharing the chat-ID prefix must not match")
	}
}

// The breaker: refusals on the own archive never count; the same two refusals
// on any other unreachable path still trip it.
func TestConfinementBreakerIgnoresOwnArchiveRefusals(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	archive := wtools.SpillToCache("chat-brk", "turn-9", "archived\n")
	if archive == "" {
		t.Fatal("setup: spill failed")
	}
	dir := filepath.Dir(archive)
	foreignDir := filepath.Join(wtools.ToolCacheRoot(), "chat-other")

	run := func(t *testing.T, path string) *App {
		t.Helper()
		// Four rounds on one path with tools that all hit ConfinePath: more than
		// the threshold (2), so a counted path would trip and force the wrap-up.
		srv := sseServer(t,
			toolCallFrames("c1", "list_dir", fmt.Sprintf(`{"path":%q}`, path)),
			toolCallFrames("c2", "find_files", fmt.Sprintf(`{"pattern":"*","path":%q}`, path)),
			toolCallFrames("c3", "list_dir", fmt.Sprintf(`{"path":%q}`, path)),
			toolCallFrames("c4", "find_files", fmt.Sprintf(`{"pattern":"*.txt","path":%q}`, path)),
			[]string{contentChunk("done")},
		)
		defer srv.Close()
		exec := newFakeExecutor()
		exec.confineErrFn = func(p string) error { return &confinementRejection{path: p} }
		cfg := config.DefaultConfig()
		cfg.MaxToolIterations = 12
		app := &App{
			Cfg:     cfg,
			Client:  newTestClient(srv.URL),
			Exec:    exec,
			Tools:   wtools.DiscoveryTools("/work"),
			Confirm: func(_, _, _ string, _ bool) bool { return true },
			Out:     io.Discard,
		}
		app.Client.ChatID = "chat-brk"
		if _, err := app.Send(context.Background(), "look"); err != nil {
			t.Fatal(err)
		}
		return app
	}

	t.Run("own archive: breaker does not trip", func(t *testing.T) {
		app := run(t, dir)
		if app.confinementTripped {
			t.Fatalf("refusals on the own archive tripped the breaker (paths %v)", app.confinementPathsHit)
		}
		tools := 0
		for _, m := range app.Conv {
			if m.Role == "tool" {
				tools++
				if m.Content != nil && strings.Contains(*m.Content, "outside workspace") {
					t.Errorf("an own-archive refusal must not read as a confinement error: %q", *m.Content)
				}
				if m.Content != nil && !strings.Contains(*m.Content, "read_file") {
					t.Errorf("the refusal must name the tools that work: %q", *m.Content)
				}
			}
		}
		if tools != 4 {
			t.Errorf("tool rounds = %d, want all 4 to run", tools)
		}
	})

	t.Run("another session's archive: breaker still trips", func(t *testing.T) {
		app := run(t, foreignDir)
		if !app.confinementTripped {
			t.Fatal("another session's archive path is unreachable and must still trip the breaker")
		}
	})
}

// The system prompt names read_file and search_files for archive recovery; both
// are covered above and in toolcache_intercept_test.go. This pins that the
// prompt does not promise a tool the code does not serve.
func TestPromptArchiveRecoveryToolsAreServed(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	app, _ := archiveApp(t, "chat-p")
	archive := wtools.SpillToCache("chat-p", "turn-1", "line one\nline two\n")
	for name, args := range map[string]map[string]string{
		"read_file":      {"path": archive},
		"read_file_full": {"path": archive},
		"search_files":   {"pattern": "two", "path": archive},
	} {
		res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{Name: name, Arguments: mustJSON(args)}})
		if !res.ok || !strings.Contains(res.text, "line two") {
			t.Errorf("%s on the own archive: ok=%v %q", name, res.ok, res.text)
		}
	}
}
