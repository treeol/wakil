package tui

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	agent "github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/exec"
	"github.com/treeol/wakil/internal/proxy"
)

func strPtr(s string) *string { return &s }

func newHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 30 * 1e9 // 30s in nanoseconds
	return &http.Client{Transport: tr}
}

// plain strips ANSI escape sequences from s.
func plain(s string) string {
	re := regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)
	return re.ReplaceAllString(s, "")
}

// step applies one message to the TUI model and returns the updated model.
func step(m tuiModel, msg interface{}) tuiModel {
	updated, _ := m.Update(msg.(tea.Msg))
	return updated.(tuiModel)
}

func newTestClient(url string) *proxy.Client {
	return &proxy.Client{BaseURL: url, Model: "ilm", ChatID: "test", HTTP: http.DefaultClient}
}

type fakeExecutor struct {
	shellCalls []string
	writeCalls map[string]string
	files      map[string]string
	dirs       map[string]bool
}

func newFakeExecutor() *fakeExecutor {
	return &fakeExecutor{writeCalls: map[string]string{}, files: map[string]string{}, dirs: map[string]bool{}}
}
func (f *fakeExecutor) RunShell(_ context.Context, c string) (string, error) {
	f.shellCalls = append(f.shellCalls, c)
	return "ran: " + c, nil
}
func (f *fakeExecutor) StatFile(p string) (int64, error) {
	if v, ok := f.files[p]; ok {
		return int64(len(v)), nil
	}
	return 0, fmt.Errorf("no such file: %s", p)
}
func (f *fakeExecutor) ReadFile(p string) (string, error) {
	if f.dirs[p] {
		return "", fmt.Errorf("read %s: is a directory", p)
	}
	if v, ok := f.files[p]; ok {
		return v, nil
	}
	return "", fmt.Errorf("no such file: %s", p)
}
func (f *fakeExecutor) ListDir(p string) (string, error) {
	var names []string
	for k := range f.files {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, "\n"), nil
}
func (f *fakeExecutor) WriteFile(p, c string) (string, error) {
	f.writeCalls[p] = c
	f.files[p] = c
	return fmt.Sprintf("wrote %d bytes to %s", len(c), p), nil
}
func (f *fakeExecutor) Cwd() string           { return "/work" }
func (f *fakeExecutor) WorkspaceRoot() string { return "/work" }
func (f *fakeExecutor) Describe() string      { return "fake" }
func (f *fakeExecutor) Close() error          { return nil }
func (f *fakeExecutor) SandboxTools() string  { return "" }
func (f *fakeExecutor) Generation() int       { return 1 }
func (f *fakeExecutor) KVRSocketPath() string { return "" }
func (f *fakeExecutor) KVRAvailable() bool    { return false }
func (f *fakeExecutor) ConfinePath(_ context.Context, path string) (string, error) {
	return path, nil
}
func (f *fakeExecutor) DeletePath(_ context.Context, path string) error   { return nil }
func (f *fakeExecutor) MovePath(_ context.Context, src, dst string) error { return nil }
func (f *fakeExecutor) StartBackground(_ context.Context, command, logPath string) (int, int, error) {
	return 1234, 1234, nil
}
func (f *fakeExecutor) KillPgid(_ context.Context, pgid, sig int) error { return nil }
func (f *fakeExecutor) IsProcessAlive(_ context.Context, pid int) bool  { return false }
func (f *fakeExecutor) ReadFileTail(_ context.Context, path string, maxBytes int64) (string, error) {
	return "", nil
}
func (f *fakeExecutor) StartInteractive(_ context.Context, command string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, int, error) {
	return nil, nil, nil, 0, fmt.Errorf("not implemented")
}
func (f *fakeExecutor) HostPathToURI(hostPath string) (string, error) {
	return "file://" + hostPath, nil
}
func (f *fakeExecutor) URIToHostPath(uri string) (string, error) {
	return strings.TrimPrefix(uri, "file://"), nil
}

func newTestApp(url string, executor exec.Executor, confirm agent.Confirmer) *agent.App {
	return &agent.App{
		Cfg:     config.DefaultConfig(),
		Client:  newTestClient(url),
		Exec:    executor,
		Out:     io.Discard,
		Confirm: confirm,
	}
}

func sseServer(t *testing.T, framesPerCall ...[]string) *httptest.Server {
	t.Helper()
	call := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			http.NotFound(w, r)
			return
		}
		frames := framesPerCall[0]
		if call < len(framesPerCall) {
			frames = framesPerCall[call]
		}
		call++
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

func contentChunk(s string) string {
	return fmt.Sprintf(`{"choices":[{"delta":{"content":%q},"finish_reason":null}]}`, s)
}
