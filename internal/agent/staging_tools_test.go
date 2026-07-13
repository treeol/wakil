package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/staging"
)

// stagingTestServer starts a kvr-server for staging tool tests.
// Returns the App (with staging client wired) and a cleanup func.
func stagingTestServer(t *testing.T) (*App, func()) {
	t.Helper()
	bin := stagingTestBin(t)
	if bin == "" {
		t.Skip("kvr-server binary not found — skipping staging tool tests")
	}
	tmpDir, err := os.MkdirTemp("", "kvr-tool-test-*")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	socketPath := filepath.Join(tmpDir, "kvr.sock")

	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"KVR_SOCKET_PATH="+socketPath,
		"KVR_MAX_ENTRIES=1000",
		"KVR_SWEEP_INTERVAL_SECS=1",
	)
	if err := cmd.Start(); err != nil {
		os.RemoveAll(tmpDir)
		t.Fatalf("start kvr-server: %v", err)
	}

	client := staging.NewClient(socketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := 0; i < 50; i++ {
		if err := client.Ping(ctx); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	app := &App{
		AgentPrefix:   "main",
		StagingClient: client,
	}

	cleanup := func() {
		client.Close()
		cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			cmd.Process.Kill()
			<-done
		}
		os.RemoveAll(tmpDir)
	}
	return app, cleanup
}

func stagingTestBin(t *testing.T) string {
	t.Helper()
	wd, _ := os.Getwd()
	kvrustDir, _ := filepath.Abs(filepath.Join(wd, "..", "..", "kvrust"))
	_, statErr := os.Stat(filepath.Join(kvrustDir, "Cargo.toml"))
	kvrustPresent := statErr == nil

	for _, candidate := range []string{
		filepath.Join(kvrustDir, "target", "release", "server"),
		filepath.Join(kvrustDir, "target", "debug", "server"),
		filepath.Join(wd, "..", "..", "kvr"),
	} {
		abs, _ := filepath.Abs(candidate)
		if info, err := os.Stat(abs); err == nil && !info.IsDir() {
			return abs
		}
	}
	if kvrustPresent {
		t.Fatalf("kvrust/ is present but kvr-server binary not found — run 'cargo build --release' in kvrust/")
	}
	t.Skip("kvrust/ submodule not present — run 'git submodule update --init'")
	return ""
}

// TestStagingPrefixEnforcement verifies that staging_put prepends the
// agent's prefix, and staging_delete also uses the prefix. Cross-prefix
// reads are allowed.
func TestStagingPrefixEnforcement(t *testing.T) {
	app, cleanup := stagingTestServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Main agent puts a key without prefix.
	putResult := app.handleStagingPut(ctx, proxy.ToolCall{
		Function: proxy.FunctionCall{Name: "staging_put", Arguments: `{"key":"result","value":"hello"}`},
	})
	if !strings.Contains(putResult, "staged: main/result") {
		t.Fatalf("staging_put: got %q, want prefix main/", putResult)
	}

	// Verify the key is stored as main/result (cross-prefix read).
	getResult := app.handleStagingGet(ctx, proxy.ToolCall{
		Function: proxy.FunctionCall{Name: "staging_get", Arguments: `{"key":"main/result"}`},
	})
	if getResult != "hello" {
		t.Fatalf("staging_get main/result: got %q, want %q", getResult, "hello")
	}

	// Simulate a subagent with a different prefix.
	subApp := &App{
		AgentPrefix:   "sub-abc12345",
		StagingClient: app.StagingClient,
	}

	// Subagent reads main's key (cross-prefix read — allowed).
	subGet := subApp.handleStagingGet(ctx, proxy.ToolCall{
		Function: proxy.FunctionCall{Name: "staging_get", Arguments: `{"key":"main/result"}`},
	})
	if subGet != "hello" {
		t.Fatalf("subagent cross-prefix read: got %q, want %q", subGet, "hello")
	}

	// Subagent writes its own key.
	subPut := subApp.handleStagingPut(ctx, proxy.ToolCall{
		Function: proxy.FunctionCall{Name: "staging_put", Arguments: `{"key":"data","value":"subval"}`},
	})
	if !strings.Contains(subPut, "staged: sub-abc12345/data") {
		t.Fatalf("subagent staging_put: got %q, want prefix sub-abc12345/", subPut)
	}

	// Main agent reads subagent's key (cross-prefix read — allowed).
	mainRead := app.handleStagingGet(ctx, proxy.ToolCall{
		Function: proxy.FunctionCall{Name: "staging_get", Arguments: `{"key":"sub-abc12345/data"}`},
	})
	if mainRead != "subval" {
		t.Fatalf("main cross-prefix read: got %q, want %q", mainRead, "subval")
	}

	// Main agent tries to delete under subagent's prefix — the tool layer
	// prepends main/, so the delete targets main/sub-abc12345/data, which
	// doesn't exist.
	delResult := app.handleStagingDelete(ctx, proxy.ToolCall{
		Function: proxy.FunctionCall{Name: "staging_delete", Arguments: `{"key":"sub-abc12345/data"}`},
	})
	if !strings.Contains(delResult, "not found") {
		t.Fatalf("cross-prefix delete should fail: got %q", delResult)
	}

	// Subagent deletes its own key.
	subDel := subApp.handleStagingDelete(ctx, proxy.ToolCall{
		Function: proxy.FunctionCall{Name: "staging_delete", Arguments: `{"key":"data"}`},
	})
	if !strings.Contains(subDel, "deleted: sub-abc12345/data") {
		t.Fatalf("subagent delete: got %q", subDel)
	}
}

// TestStagingTTLLPlumbing verifies that ttl_seconds is plumbed through
// to SETX and the entry expires after the TTL.
func TestStagingTTLLPlumbing(t *testing.T) {
	app, cleanup := stagingTestServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Put with 1s TTL.
	result := app.handleStagingPut(ctx, proxy.ToolCall{
		Function: proxy.FunctionCall{Name: "staging_put", Arguments: `{"key":"ttltest","value":"temp","ttl_seconds":1}`},
	})
	if !strings.Contains(result, "ttl=1s") {
		t.Fatalf("staging_put with TTL: got %q, want ttl=1s", result)
	}

	// Verify it's present.
	getResult := app.handleStagingGet(ctx, proxy.ToolCall{
		Function: proxy.FunctionCall{Name: "staging_get", Arguments: `{"key":"main/ttltest"}`},
	})
	if getResult != "temp" {
		t.Fatalf("staging_get before TTL: got %q, want %q", getResult, "temp")
	}

	// Wait for expiry.
	time.Sleep(1200 * time.Millisecond)

	// Verify it's gone.
	getResult = app.handleStagingGet(ctx, proxy.ToolCall{
		Function: proxy.FunctionCall{Name: "staging_get", Arguments: `{"key":"main/ttltest"}`},
	})
	if !strings.Contains(getResult, "not found") {
		t.Fatalf("staging_get after TTL: got %q, want not found", getResult)
	}
}

// TestStagingUnavailable verifies that all staging tools return
// "staging unavailable" when the client is nil.
func TestStagingUnavailable(t *testing.T) {
	app := &App{
		AgentPrefix:   "main",
		StagingClient: nil, // kvr unavailable
	}
	ctx := context.Background()

	tests := []struct {
		name     string
		handler  func(context.Context, proxy.ToolCall) string
		args     string
	}{
		{"put", app.handleStagingPut, `{"key":"k","value":"v"}`},
		{"get", app.handleStagingGet, `{"key":"k"}`},
		{"delete", app.handleStagingDelete, `{"key":"k"}`},
		{"list", app.handleStagingList, `{}`},
		{"get_many", app.handleStagingGetMany, `{"keys":["k"]}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := tc.handler(ctx, proxy.ToolCall{
				Function: proxy.FunctionCall{Name: "staging_" + tc.name, Arguments: tc.args},
			})
			if !strings.Contains(result, "staging unavailable") {
				t.Errorf("%s: got %q, want 'staging unavailable'", tc.name, result)
			}
		})
	}
}

// TestStagingList verifies listing with prefix filter and cap.
func TestStagingList(t *testing.T) {
	app, cleanup := stagingTestServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Put keys with different prefixes.
	for _, k := range []string{"a/1", "a/2", "b/3"} {
		app.handleStagingPut(ctx, proxy.ToolCall{
			Function: proxy.FunctionCall{Name: "staging_put", Arguments: `{"key":"` + k + `","value":"v"}`},
		})
	}

	// List all (prefix empty).
	result := app.handleStagingList(ctx, proxy.ToolCall{
		Function: proxy.FunctionCall{Name: "staging_list", Arguments: `{}`},
	})
	if !strings.Contains(result, "main/a/1") || !strings.Contains(result, "main/a/2") || !strings.Contains(result, "main/b/3") {
		t.Fatalf("staging_list: got %q, want all keys", result)
	}

	// List with prefix "a/".
	result = app.handleStagingList(ctx, proxy.ToolCall{
		Function: proxy.FunctionCall{Name: "staging_list", Arguments: `{"prefix":"main/a/"}`},
	})
	if !strings.Contains(result, "main/a/1") || !strings.Contains(result, "main/a/2") {
		t.Fatalf("staging_list prefix a/: got %q", result)
	}
	if strings.Contains(result, "main/b/3") {
		t.Fatalf("staging_list prefix a/ should exclude b/: got %q", result)
	}
}

// TestStagingGetMany verifies MGET plumbing.
func TestStagingGetMany(t *testing.T) {
	app, cleanup := stagingTestServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	app.handleStagingPut(ctx, proxy.ToolCall{
		Function: proxy.FunctionCall{Name: "staging_put", Arguments: `{"key":"x","value":"val1"}`},
	})
	app.handleStagingPut(ctx, proxy.ToolCall{
		Function: proxy.FunctionCall{Name: "staging_put", Arguments: `{"key":"y","value":"val2"}`},
	})

	result := app.handleStagingGetMany(ctx, proxy.ToolCall{
		Function: proxy.FunctionCall{Name: "staging_get_many", Arguments: `{"keys":["main/x","main/y","main/missing"]}`},
	})
	if !strings.Contains(result, "main/x: val1") {
		t.Fatalf("staging_get_many: missing main/x: val1 in %q", result)
	}
	if !strings.Contains(result, "main/y: val2") {
		t.Fatalf("staging_get_many: missing main/y: val2 in %q", result)
	}
	if !strings.Contains(result, "main/missing: (not found)") {
		t.Fatalf("staging_get_many: missing not found for main/missing in %q", result)
	}
}

// TestStagingSubagentHandoff simulates the subagent handoff scenario:
// subagent writes under its prefix, main agent reads it back cross-prefix.
func TestStagingSubagentHandoff(t *testing.T) {
	app, cleanup := stagingTestServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Simulate a subagent writing a handoff result.
	subApp := &App{
		AgentPrefix:   "sub-deadbeef",
		StagingClient: app.StagingClient,
	}
	subApp.handleStagingPut(ctx, proxy.ToolCall{
		Function: proxy.FunctionCall{Name: "staging_put", Arguments: `{"key":"findings","value":"found the bug in parser.go:42"}`},
	})

	// Main agent reads the subagent's handoff.
	result := app.handleStagingGet(ctx, proxy.ToolCall{
		Function: proxy.FunctionCall{Name: "staging_get", Arguments: `{"key":"sub-deadbeef/findings"}`},
	})
	if result != "found the bug in parser.go:42" {
		t.Fatalf("handoff read: got %q", result)
	}
}
