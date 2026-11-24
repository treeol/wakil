// End-to-end TUI test harness (package main, cmd/wakil).
//
// The harness never builds an App, a TUI model, a ConversationManager, or a
// session host by hand. It drives the exact production path main() runs:
//   - config.LoadConfig (internal/config/config.go:1858) with an explicit
//     --config file in a temp dir, then config.ApplyConfigApproval
//     (internal/config/config.go:1832),
//   - wiring.ResolveSandbox (internal/wiring/resolve_sandbox.go:27),
//   - runTUI (cmd/wakil/main.go:264), whose bootstrap runs
//     wiring.NewExecutor (internal/wiring/bootstrap.go:496) and
//     wiring.BootstrapTUI (internal/wiring/bootstrap_tui.go:78), which runs
//     wiring.NewConversationManager
//     (internal/wiring/conversation_manager.go:73),
//     sqlstore.NewSQLiteStore
//     (internal/core/sessionhost/sqlstore/sqlstore.go:43),
//     wiring.BuildApp (internal/wiring/bootstrap.go:97),
//     wiring.NewHostTurnHandle (internal/wiring/hostturn.go:319),
//     sessionhost.New (internal/wiring/conversation_manager.go:273),
//     tui.NewLoadingModel (internal/tui/loading.go:59), and
//     tui.NewTUIModelWithFacade (internal/tui/tui.go:709), with
//     tui.SetProgramSend (internal/tui/tui_wiring_loop.go:331).
//
// Isolation: testutil.Main (cmd/wakil/testmain_test.go:17) already points
// HOME/XDG at a temp tree. Each harness instance additionally points
// XDG_DATA_HOME at its own root, which is also the root repoStateDir
// (internal/agent/repostate.go:117) and stagingDataRoot
// (internal/agent/staging.go:63) resolve from, so sessions, repo-state, and
// the session-host event log are per-instance.
//
// Fake backend: an httptest server speaking OpenAI-style SSE on
// POST .../v1/chat/completions. Scripted responses are consumed one per
// chat request in order; after the script is exhausted a plain content
// chunk "done" is answered. Every chat request body is recorded in order.
// Startup probes are answered too: GET /props serves n_ctx 32768 (the
// context-limit probe for a kind "openai" endpoint on a non-OpenRouter
// host, internal/agent/ctxlimit.go:226) and GET /v1/models serves a
// one-model list (internal/agent/backends.go:195); other paths 404. Note
// the backend-list probe makes no request at all for kind "openai"
// (internal/agent/backends.go:88), so there is nothing to serve there.
// Subagent or auxiliary requests cannot be told apart from the main turn
// on this endpoint, so any of them would consume the script; tests must
// keep the script shaped for the main turn only.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionhost/sqlstore"
	"github.com/treeol/wakil/internal/wiring"
)

// e2eOpts configures one harness instance.
type e2eOpts struct {
	// script holds one SSE frame list per scripted chat request.
	script [][]string
	// Turns, when non-nil, selects the fake server response by the last
	// user message text instead of the ordered script.
	Turns map[string][][]string
	// extra merges top-level keys into the generated config JSON.
	extra map[string]any
	// width and height size the fake terminal.
	width  int
	height int
	// Reuse, when set, restarts the harness on the earlier instance root,
	// workdir, data-root env values, config file, and fake server.
	Reuse *e2e
	// ResumeID is passed to runTUI as the resume id argument.
	ResumeID string
	// tee, when set, receives every renderer flush as its own Write.
	tee io.Writer
	// bootDelay makes the async bootstrap start this much later, so the
	// start-up view is on screen for a while. earlySize sends WindowSizeMsg
	// immediately instead of waiting 100ms.
	bootDelay   time.Duration
	earlySize   bool
	onProgramFn func(*tea.Program)
}

// e2eBuffer is a mutex-protected byte buffer used as a writer and reader.
type e2eBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	tee io.Writer // optional: every Write is also passed here (one renderer flush each)
}

func (b *e2eBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tee != nil {
		_, _ = b.tee.Write(p)
	}
	return b.buf.Write(p)
}

func (b *e2eBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// e2eFakeServer is the scripted OpenAI-style SSE backend.
type e2eFakeServer struct {
	t         *testing.T
	srv       *httptest.Server
	mu        sync.Mutex
	script    [][]string
	call      int
	turns     map[string][][]string
	turnCalls map[string]int
	reqs      [][]byte
	props     int // GET /props probes served
}

// e2eLastUserText returns the text of the last message with the user role.
func e2eLastUserText(body []byte) string {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return ""
	}
	text := ""
	for _, msg := range req.Messages {
		if msg.Role != "user" {
			continue
		}
		if err := json.Unmarshal(msg.Content, &text); err == nil {
			continue
		}
		var parts []struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(msg.Content, &parts); err == nil {
			var b strings.Builder
			for _, part := range parts {
				b.WriteString(part.Text)
			}
			text = b.String()
		}
	}
	return text
}

func e2eContentChunk(s string) string {
	return fmt.Sprintf(`{"choices":[{"delta":{"content":%q},"finish_reason":null}]}`, s)
}

func e2eToolCallFrames(id, name, args string) []string {
	return []string{
		fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":%q,"arguments":""}}]},"finish_reason":null}]}`, id, name),
		fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":%q}}]},"finish_reason":null}]}`, args),
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	}
}

func newE2EFakeServer(t *testing.T, script [][]string, turns map[string][][]string) *e2eFakeServer {
	t.Helper()
	f := &e2eFakeServer{t: t, script: script, turns: turns, turnCalls: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *e2eFakeServer) handle(w http.ResponseWriter, r *http.Request) {
	// Startup probes first: they must not consume the chat script.
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/props"):
		f.mu.Lock()
		f.props++
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"n_ctx":32768,"n_ctx_train":32768,"default_generation_settings":{"n_ctx":32768}}`))
		return
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/v1/models"):
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"ilm"}]}`))
		return
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
		// Chat request: record and consume one scripted response.
	default:
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, append([]byte(nil), body...))
	frames := []string{e2eContentChunk("done")}
	if f.turns != nil {
		userText := e2eLastUserText(body)
		best := ""
		for key := range f.turns {
			if strings.Contains(userText, key) && len(key) > len(best) {
				best = key
			}
		}
		if best != "" {
			call := f.turnCalls[best]
			responses := f.turns[best]
			if call < len(responses) {
				frames = responses[call]
			}
			f.turnCalls[best] = call + 1
		}
	} else if f.call < len(f.script) {
		frames = f.script[f.call]
		f.call++
	}
	f.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, ok := w.(http.Flusher)
	if !ok {
		f.t.Error("response writer does not implement http.Flusher")
		return
	}
	flusher.Flush()
	for _, fr := range frames {
		fmt.Fprintf(w, "data: %s\n\n", fr)
		flusher.Flush()
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// propsServed returns how many GET /props startup probes have been served.
func (f *e2eFakeServer) propsServed() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.props
}

func (f *e2eFakeServer) requests() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.reqs...)
}

// e2e is one running TUI instance driven through runTUI.
type e2e struct {
	t        *testing.T
	cfg      config.Config
	workdir  string
	server   *e2eFakeServer
	prog     *tea.Program
	exited   chan struct{}
	code     int
	inWrite  *os.File
	out      *e2eBuffer
	stdoutLa *e2eBuffer
	stderrLa *e2eBuffer
	origOut  *os.File
	origErr  *os.File
	root     string
	xdgData  string
	sessions string
	cfgPath  string
	done     sync.Once
	storeMu  sync.Mutex
	evStore  *sqlstore.SQLiteStore
}

func startE2E(t *testing.T, opts e2eOpts) *e2e {
	t.Helper()
	if opts.width == 0 {
		opts.width = 120
	}
	if opts.height == 0 {
		opts.height = 40
	}
	root := t.TempDir()
	if opts.Reuse != nil {
		opts.Reuse.shutdown()
		root = opts.Reuse.root
	}
	workdir := filepath.Join(root, "work")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The program is started from inside its project directory, as a user does. In direct mode a shell command that is run through the background path (shell_timeout_sec > 0) starts in the process working directory, not in --workdir, so a harness started elsewhere would run model commands in the repository.
	t.Chdir(workdir)
	// Per-instance data root: repoStateDir and stagingDataRoot resolve
	// from XDG_DATA_HOME (internal/agent/persist.go:18), so this isolates
	// sessions, repo-state, and the session-host event log.
	xdgData := filepath.Join(root, "xdg")
	sessionsDir := filepath.Join(root, "sessions")
	t.Setenv("XDG_DATA_HOME", xdgData)
	t.Setenv("WAKIL_SESSIONS_DIR", sessionsDir)
	// Neutralise ambient endpoint, workdir, and backend env vars so
	// LoadConfig reads only the generated file and flags. Empty values
	// are ignored by the env readers (config.go envStr and friends).
	for _, k := range []string{
		"ILM_BASE_URL", "WAKIL_BASE_URL", "ILM_HOST", "WAKIL_HOST",
		"ILM_PORT", "WAKIL_PORT", "ILM_API_KEY", "WAKIL_API_KEY",
		"ILM_MODEL", "WAKIL_MODEL", "ILM_EXEC_MODE", "WAKIL_EXEC_MODE",
		"WAKIL_SANDBOX_BACKEND", "WAKIL_OUTPUT_MODE", "WAKIL_CLIPBOARD_MODE",
		"ILM_CONTAINER_IMAGE", "WAKIL_IMAGE", "ILM_WORKDIR", "WAKIL_WORKDIR",
		"ILM_HOST_WORKDIR", "WAKIL_HOST_WORKDIR", "ILM_DOCKER_SOCKET",
		"WAKIL_DOCKER_SOCKET", "ILM_SSH_SIGNING", "WAKIL_SSH_SIGNING",
		"WAKIL_KVR_DISABLED", "WAKIL_KVR_MAX_ENTRIES",
		"WAKIL_KVR_SWEEP_INTERVAL_SECS", "WAKIL_KVR_SNAPSHOT_INTERVAL_SECS",
		"WAKIL_TRACE_SESSIONS", "WAKIL_TRACE_DIR", "WAKIL_CONTEXT_MODE",
		"WAKIL_PROMPT_MODE", "WAKIL_CONFIG", "SEARXNG_URL",
		"WAKIL_MENTION_BASE", "WAKIL_REPO_STATE_DIR",
	} {
		t.Setenv(k, "")
	}
	srv := (*e2eFakeServer)(nil)
	cfgPath := filepath.Join(root, "config.json")
	if opts.Reuse != nil {
		srv = opts.Reuse.server
		if opts.Turns != nil {
			srv.mu.Lock()
			srv.turns = opts.Turns
			srv.mu.Unlock()
		}
		cfgPath = opts.Reuse.cfgPath
	} else {
		srv = newE2EFakeServer(t, opts.script, opts.Turns)
		cfgMap := map[string]any{
			"endpoints": map[string]any{
				"test": map[string]any{
					"kind":     "openai",
					"base_url": srv.srv.URL,
					"model":    "ilm",
				},
			},
			"default_endpoint": "test",
		}
		for k, v := range opts.extra {
			cfgMap[k] = v
		}
		raw, err := json.Marshal(cfgMap)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := config.LoadConfig([]string{
		"--config", cfgPath, "--sandbox", "direct", "--workdir", workdir,
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	config.ApplyConfigApproval(&cfg)
	cfg, err = wiring.ResolveSandbox(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ResolveSandbox: %v", err)
	}
	e := &e2e{
		t:        t,
		cfg:      cfg,
		workdir:  workdir,
		root:     root,
		xdgData:  xdgData,
		sessions: sessionsDir,
		cfgPath:  cfgPath,
		server:   srv,
		exited:   make(chan struct{}),
		out:      &e2eBuffer{tee: opts.tee},
		stdoutLa: &e2eBuffer{},
		stderrLa: &e2eBuffer{},
	}
	// Stdio capture: anything writing os.Stdout/os.Stderr while the TUI
	// runs is an out-of-band write, since program output goes to e.out.
	e.origOut, e.origErr = os.Stdout, os.Stderr
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	e.inWrite = nil
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = io.Copy(e.stdoutLa, outR) }()
	go func() { _, _ = io.Copy(e.stderrLa, errR) }()
	os.Stdout, os.Stderr = outW, errW
	e.inWrite = inW
	hooks := tuiHooks{
		programOptions: []tea.ProgramOption{
			tea.WithInput(inR),
			tea.WithOutput(e.out),
		},
		beforeBootstrap: func() { time.Sleep(opts.bootDelay) },
		onProgram: func(prog *tea.Program) {
			e.prog = prog
			if opts.onProgramFn != nil {
				opts.onProgramFn(prog)
			}
			if opts.earlySize {
				go prog.Send(tea.WindowSizeMsg{Width: opts.width, Height: opts.height})
				return
			}
			// A non-TTY output gets no WindowSizeMsg from Bubble Tea,
			// so send one explicitly. The loading model stores it and
			// forwards it to the real model at swap
			// (internal/tui/loading.go:100), whose handler runs
			// facade.RunDeferredStartup (internal/tui/tui.go:786).
			go func() {
				time.Sleep(100 * time.Millisecond)
				prog.Send(tea.WindowSizeMsg{Width: opts.width, Height: opts.height})
			}()
		},
	}
	go func() {
		e.code = runTUI(cfg, opts.ResumeID, hooks)
		close(e.exited)
	}()
	// Shutdown always waits for runTUI and restores stdio.
	t.Cleanup(func() { e.shutdown() })
	return e
}

func (e *e2e) shutdown() {
	e.done.Do(func() {
		e.quit()
		os.Stdout, os.Stderr = e.origOut, e.origErr
		e.storeMu.Lock()
		if e.evStore != nil {
			_ = e.evStore.Close()
		}
		e.storeMu.Unlock()
	})
}

// waitStderr waits until the captured stderr contains substr and returns
// the stderr text. Stderr is a pipe drained by a separate goroutine, so a
// write the program has already made may not be in the buffer yet when the
// program exits; reading the buffer once after waitExit can miss it.
func (e *e2e) waitStderr(substr string, timeout time.Duration) string {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, stderr := e.outOfBand(); strings.Contains(stderr, substr) {
			return stderr
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, stderr := e.outOfBand()
	e.t.Fatalf("timed out waiting for stderr text %q\n--- stderr ---\n%s\n--- tail ---\n%s", substr, stderr, e.tail(500))
	return ""
}

func (e *e2e) outOfBand() (string, string) {
	return e.stdoutLa.String(), e.stderrLa.String()
}

func (e *e2e) typeText(s string) {
	e.t.Helper()
	if _, err := io.WriteString(e.inWrite, s); err != nil {
		e.t.Fatalf("typeText: %v", err)
	}
}

func (e *e2e) enter() {
	e.t.Helper()
	if _, err := io.WriteString(e.inWrite, "\r"); err != nil {
		e.t.Fatalf("enter: %v", err)
	}
}

func (e *e2e) key(s string) {
	e.t.Helper()
	if _, err := io.WriteString(e.inWrite, s); err != nil {
		e.t.Fatalf("key: %v", err)
	}
}

// submit types a line and presses Enter as its own write, after a gap
// longer than pasteBurstGap (250ms, internal/tui/tui.go:144). A short
// line never reaches pasteBurstMinRunes (40 runes), so the paste-burst
// collapse cannot fire on it. A slash command typed this way opens the
// completion picker, but Enter is not consumed for /command completions
// (internal/tui/complete.go:710), so handleKey dispatches the command.
func (e *e2e) submit(line string) {
	e.t.Helper()
	e.typeText(line)
	time.Sleep(350 * time.Millisecond)
	e.enter()
}

func (e *e2e) quit() {
	e.t.Helper()
	select {
	case <-e.exited:
		return
	default:
	}
	e.submit("/quit")
	if !e.waitExit(5 * time.Second) {
		// Fall back to the idle double-press quit gate.
		e.key("\x03")
		time.Sleep(100 * time.Millisecond)
		e.key("\x03")
		if !e.waitExit(5*time.Second) && e.prog != nil {
			e.prog.Kill()
			e.waitExit(5 * time.Second)
		}
	}
}

func (e *e2e) waitExit(d time.Duration) bool {
	select {
	case <-e.exited:
		return true
	case <-time.After(d):
		return false
	}
}

func (e *e2e) screen() string { return stripE2EANSI(e.out.String()) }

// stripE2EANSI removes ANSI/CSI/OSC escape sequences from program output.
func stripE2EANSI(s string) string {
	var b strings.Builder
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		if runes[i] != '\x1b' {
			b.WriteRune(runes[i])
			continue
		}
		if i+1 >= len(runes) {
			break
		}
		switch runes[i+1] {
		case '[':
			// CSI: consume to the final byte (0x40-0x7e).
			j := i + 2
			for j < len(runes) && (runes[j] < '@' || runes[j] > '~') {
				j++
			}
			i = j
		case ']':
			// OSC: consume to BEL or the ST terminator.
			j := i + 2
			for j < len(runes) && runes[j] != '\x07' {
				if runes[j] == '\x1b' && j+1 < len(runes) && runes[j+1] == '\\' {
					j++
					break
				}
				j++
			}
			i = j
		default:
			i++
		}
	}
	return b.String()
}

func (e *e2e) tail(n int) string {
	s := e.screen()
	if len(s) > n {
		s = s[len(s)-n:]
	}
	out, err := e.outOfBand()
	return fmt.Sprintf("screen tail:\n%s\nstdout:\n%s\nstderr:\n%s", s, out, err)
}

// mark returns the current length of the ANSI-stripped screen text.
func (e *e2e) mark() int { return len(e.screen()) }

// screenSince returns the screen text after the given mark.
func (e *e2e) screenSince(m int) string {
	s := e.screen()
	if m < 0 || m > len(s) {
		return ""
	}
	return s[m:]
}

func (e *e2e) waitScreen(substr string, timeout time.Duration) {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(e.screen(), substr) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.t.Fatalf("timed out waiting for screen text %q\n%s", substr, e.tail(2000))
}

// waitScreenLine polls the screen until a line satisfies pred, then
// returns that line. It splits the ANSI-stripped screen on newlines and
// carriage returns.
func (e *e2e) waitScreenLine(timeout time.Duration, desc string, pred func(line string) bool) string {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, line := range strings.FieldsFunc(e.screen(), func(r rune) bool { return r == '\n' || r == '\r' }) {
			if pred(line) {
				return line
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.t.Fatalf("timed out waiting for screen line: %s\n%s", desc, e.tail(2000))
	return ""
}

// clearScreenSeq is what Bubble Tea writes for tea.ClearScreen. The TUI
// returns that command exactly when a rotation (/new, /resume, /handoff)
// has been applied (internal/tui/tui_wiring_loop.go, end of applyRotation),
// after the textarea was reset and the rotating guard cleared. The program
// also writes it once when it enters the alt screen, so callers compare
// counts, never presence.
const clearScreenSeq = "\x1b[2J"

// clearScreens counts the clear-screen sequences written so far, from the
// raw (not ANSI-stripped) output.
func (e *e2e) clearScreens() int { return strings.Count(e.out.String(), clearScreenSeq) }

// submitRotation submits a command that rotates the session (/new,
// /resume <id>, /handoff) and waits until the TUI has applied the rotation.
// The host writes the new session row about 370 ms before the TUI swaps
// facades, and the swap resets the textarea, so a line typed after seeing
// the row can be erased by the swap and the following Enter submits nothing.
// The count is read before the command is sent, so a rotation that finishes
// before the wait starts is still seen.
func (e *e2e) submitRotation(line string) {
	e.t.Helper()
	before := e.clearScreens()
	e.submit(line)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if e.clearScreens() > before {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("timed out waiting for the TUI to apply the rotation after %q\n%s", line, e.tail(2000))
}

// waitFileContent waits until the file at path holds want.
func (e *e2e) waitFileContent(path, want string, timeout time.Duration) {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && string(data) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("timed out waiting for %s to hold %q\n%s", path, want, e.tail(2000))
}

// waitProbes waits until the deferred startup probe (GET /props) has been
// served at least once.
func (e *e2e) waitProbes(timeout time.Duration) {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if e.server.propsServed() >= 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("timed out waiting for the startup probe\n%s", e.tail(2000))
}

func (e *e2e) requests() [][]byte { return e.server.requests() }

func (e *e2e) waitRequests(n int, timeout time.Duration) {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(e.requests()) >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.t.Fatalf("timed out waiting for %d model requests, got %d\n%s", n, len(e.requests()), e.tail(2000))
}

// store opens the harness's own read connection to the durable event log.
// WAL allows a second reader alongside the program's store.
func (e *e2e) store() (*sqlstore.SQLiteStore, error) {
	e.storeMu.Lock()
	defer e.storeMu.Unlock()
	if e.evStore != nil {
		return e.evStore, nil
	}
	path := agent.SessionHostDBPath(agent.ProjectDir(e.cfg))
	if path == "" {
		return nil, fmt.Errorf("empty session-host db path")
	}
	st, err := sqlstore.NewSQLiteStore(context.Background(), path)
	if err != nil {
		return nil, err
	}
	e.evStore = st
	return st, nil
}

// events returns every durable event across all sessions, ordered by
// session creation then sequence. Read decodes and validates every
// payload, so a read error is a finding and is returned, not swallowed.
func (e *e2e) events() ([]event.Event, error) {
	st, err := e.store()
	if err != nil {
		return nil, err
	}
	rows, err := st.DB().QueryContext(context.Background(),
		`SELECT id FROM sessions ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan session: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	_ = rows.Close()
	var out []event.Event
	for _, id := range ids {
		evs, err := st.Read(context.Background(), event.SessionID(id), event.Seq(0), 0)
		if err != nil {
			return nil, fmt.Errorf("read session %s: %w", id, err)
		}
		out = append(out, evs...)
	}
	return out, nil
}

func (e *e2e) countKind(kind event.Kind) int {
	evs, err := e.events()
	if err != nil {
		return 0
	}
	n := 0
	for _, ev := range evs {
		if ev.Kind == kind {
			n++
		}
	}
	return n
}

func (e *e2e) waitKind(kind event.Kind, n int, timeout time.Duration) {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		evs, err := e.events()
		if err != nil {
			lastErr = err
		} else {
			c := 0
			for _, ev := range evs {
				if ev.Kind == kind {
					c++
				}
			}
			if c >= n {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.t.Fatalf("timed out waiting for %d events of kind %s (last read error: %v)\n%s",
		n, kind, lastErr, e.tail(2000))
}

// waitTurnDone waits for the first turn_completed durable event.
func (e *e2e) waitTurnDone(timeout time.Duration) {
	e.t.Helper()
	e.waitKind(event.KindTurnCompleted, 1, timeout)
}

// waitTurns waits for at least n turn_completed durable events.
func (e *e2e) waitTurns(n int, timeout time.Duration) {
	e.t.Helper()
	e.waitKind(event.KindTurnCompleted, n, timeout)
}

// sessionIDs returns session ids in creation order.
func (e *e2e) sessionIDs() ([]string, error) {
	st, err := e.store()
	if err != nil {
		return nil, err
	}
	rows, err := st.DB().QueryContext(context.Background(),
		`SELECT id FROM sessions ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	return ids, nil
}

// eventsBySession returns events grouped by session in creation order.
func (e *e2e) eventsBySession() (ids []string, by map[string][]event.Event, err error) {
	ids, err = e.sessionIDs()
	if err != nil {
		return nil, nil, err
	}
	st, err := e.store()
	if err != nil {
		return nil, nil, err
	}
	by = make(map[string][]event.Event, len(ids))
	for _, id := range ids {
		evs, err := st.Read(context.Background(), event.SessionID(id), event.Seq(0), 0)
		if err != nil {
			return nil, nil, fmt.Errorf("read session %s: %w", id, err)
		}
		by[id] = evs
	}
	return ids, by, nil
}

// chatIDs returns chat ids from saved session files in modification order.
func (e *e2e) chatIDs() ([]string, error) {
	entries, err := os.ReadDir(e.sessions)
	if err != nil {
		return nil, fmt.Errorf("read sessions dir: %w", err)
	}
	type item struct {
		id  string
		mod time.Time
	}
	var items []item
	for _, entry := range entries {
		// Session transcripts only: <chat_id>.json. The sessions dir also holds
		// the derived resume index (.resume-index), handoff sidecars
		// (<id>.handoff.json) and atomic-write temp files (.tmp-*), none of
		// which is a session.
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".handoff.json") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("stat session file: %w", err)
		}
		items = append(items, item{id: strings.TrimSuffix(name, filepath.Ext(name)), mod: info.ModTime()})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].mod.Equal(items[j].mod) {
			return items[i].id < items[j].id
		}
		return items[i].mod.Before(items[j].mod)
	})
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.id
	}
	return out, nil
}
