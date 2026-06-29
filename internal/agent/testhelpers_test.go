package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"wakil/internal/config"
	"wakil/internal/exec"
	"wakil/internal/proxy"
	wtools "wakil/internal/tools"
)

func newTestClient(url string) *proxy.Client {
	return &proxy.Client{BaseURL: url, Model: "ilm", ChatID: "test", HTTP: http.DefaultClient}
}

type fakeExecutor struct {
	shellCalls  []string
	shellResult string // when non-empty, returned by RunShell instead of "ran: " + cmd
	writeCalls  map[string]string
	files       map[string]string
	dirs        map[string]bool
}

func newFakeExecutor() *fakeExecutor {
	return &fakeExecutor{writeCalls: map[string]string{}, files: map[string]string{}, dirs: map[string]bool{}}
}
func (f *fakeExecutor) RunShell(_ context.Context, c string) (string, error) {
	f.shellCalls = append(f.shellCalls, c)
	if f.shellResult != "" {
		return f.shellResult, nil
	}
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
	// Only succeed for "." (list all) or explicitly registered dirs.
	if p != "." && !f.dirs[p] {
		return "", fmt.Errorf("no such directory: %s", p)
	}
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
func (f *fakeExecutor) Cwd() string          { return "/work" }
func (f *fakeExecutor) WorkspaceRoot() string { return "/work" }
func (f *fakeExecutor) Describe() string      { return "fake" }
func (f *fakeExecutor) Close() error          { return nil }
func (f *fakeExecutor) SandboxTools() string  { return "" }
func (f *fakeExecutor) Generation() int       { return 1 }
func (f *fakeExecutor) ConfinePath(_ context.Context, path string) (string, error) {
	return path, nil
}
func (f *fakeExecutor) DeletePath(_ context.Context, path string) error  { return nil }
func (f *fakeExecutor) MovePath(_ context.Context, src, dst string) error { return nil }
func (f *fakeExecutor) StartBackground(_ context.Context, command, logPath string) (int, int, error) {
	return 1234, 1234, nil
}
func (f *fakeExecutor) KillPgid(_ context.Context, pgid, sig int) error           { return nil }
func (f *fakeExecutor) IsProcessAlive(_ context.Context, pid int) bool             { return false }
func (f *fakeExecutor) ReadFileTail(_ context.Context, path string, maxBytes int64) (string, error) {
	return "", nil
}
func (f *fakeExecutor) StartInteractive(_ context.Context, command string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, int, error) {
	return nil, nil, nil, 0, fmt.Errorf("not implemented in fake executor")
}
func (f *fakeExecutor) HostPathToURI(hostPath string) (string, error) {
	return "file://" + hostPath, nil
}
func (f *fakeExecutor) URIToHostPath(uri string) (string, error) {
	return strings.TrimPrefix(uri, "file://"), nil
}

func newTestApp(url string, executor exec.Executor, confirm Confirmer) *App {
	return &App{
		Cfg:     config.DefaultConfig(),
		Client:  newTestClient(url),
		Exec:    executor,
		Tools:   wtools.DefaultTools("/work"),
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

func toolCallFrames(id, name string, argParts ...string) []string {
	frames := []string{
		fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":%q,"arguments":""}}]},"finish_reason":null}]}`, id, name),
	}
	for _, p := range argParts {
		frames = append(frames, fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":%q}}]},"finish_reason":null}]}`, p))
	}
	return append(frames, `{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
}
