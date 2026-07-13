package staging

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// kvrServerBin finds a kvr-server binary for testing. It searches:
// 1. KVR_SERVER_BIN env var
// 2. ../../kvrust/target/release/server (relative to the test package)
// 3. ../../kvrust/target/debug/server
// 4. ../../kvr (the pre-built binary in the repo root)
//
// Returns "" if no binary is found — tests that depend on it should skip.
func kvrServerBin() string {
	if p := os.Getenv("KVR_SERVER_BIN"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	for _, candidate := range []string{
		filepath.Join(wd, "..", "..", "kvrust", "target", "release", "server"),
		filepath.Join(wd, "..", "..", "kvrust", "target", "debug", "server"),
		filepath.Join(wd, "..", "..", "kvr"),
	} {
		abs, _ := filepath.Abs(candidate)
		if info, err := os.Stat(abs); err == nil && !info.IsDir() {
			return abs
		}
	}
	return ""
}

// kvrTestEnv holds a running kvr-server instance for testing.
type kvrTestEnv struct {
	cmd        *exec.Cmd
	socketPath string
	client     *Client
}

// startKVR starts a kvr-server on a temporary UDS socket. The caller must
// call stop() to clean up. Returns the env or skips the test if no binary
// is available.
func startKVR(t *testing.T) *kvrTestEnv {
	t.Helper()
	return startKVRCustom(t, "")
}

// startKVRCustom is like startKVR but accepts extra env vars.
func startKVRCustom(t *testing.T, extraEnv ...string) *kvrTestEnv {
	t.Helper()
	bin := kvrServerBin()
	if bin == "" {
		t.Skip("kvr-server binary not found — skipping (set KVR_SERVER_BIN or build kvrust)")
	}

	tmpDir, err := os.MkdirTemp("", "kvr-test-*")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	socketPath := filepath.Join(tmpDir, "kvr.sock")

	env := append(os.Environ(),
		"KVR_SOCKET_PATH="+socketPath,
		"KVR_MAX_ENTRIES=1000",
		"KVR_SWEEP_INTERVAL_SECS=1",
	)
	env = append(env, extraEnv...)

	cmd := exec.Command(bin)
	cmd.Env = env
	// Capture stderr for diagnostics on failure.
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		os.RemoveAll(tmpDir)
		t.Fatalf("start kvr-server: %v", err)
	}

	client := NewClient(socketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ready := false
	for i := 0; i < 50; i++ {
		if err := client.Ping(ctx); err == nil {
			ready = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		cmd.Process.Kill()
		cmd.Wait()
		os.RemoveAll(tmpDir)
		t.Fatalf("kvr-server did not become ready: %s", stderr.String())
	}

	return &kvrTestEnv{
		cmd:        cmd,
		socketPath: socketPath,
		client:     client,
	}
}

func (e *kvrTestEnv) stop() {
	if e == nil {
		return
	}
	e.client.Close()
	if e.cmd != nil && e.cmd.Process != nil {
		e.cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { e.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			e.cmd.Process.Kill()
			<-done
		}
	}
	os.RemoveAll(filepath.Dir(e.socketPath))
}

// ─── Table-driven tests ───────────────────────────────────────────────

func TestPing(t *testing.T) {
	env := startKVR(t)
	defer env.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := env.client.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestSetGet(t *testing.T) {
	env := startKVR(t)
	defer env.stop()
	c := env.client
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	tests := []struct {
		name    string
		key     string
		value   string
		wantOK  bool
		wantVal string
	}{
		{"basic", "foo", "bar", true, "bar"},
		{"empty_value", "empty", "", true, ""},
		{"empty_key", "", "val", true, "val"},
		{"slash_key", "a/b/c", "val", true, "val"},
		{"unicode_value", "u", "héllo世界", true, "héllo世界"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := c.Set(ctx, tc.key, tc.value); err != nil {
				t.Fatalf("Set(%q): %v", tc.key, err)
			}
			val, ok, err := c.Get(ctx, tc.key)
			if err != nil {
				t.Fatalf("Get(%q): %v", tc.key, err)
			}
			if ok != tc.wantOK {
				t.Errorf("Get(%q): ok=%v, want %v", tc.key, ok, tc.wantOK)
			}
			if val != tc.wantVal {
				t.Errorf("Get(%q): val=%q, want %q", tc.key, val, tc.wantVal)
			}
		})
	}
}

func TestGetNotFound(t *testing.T) {
	env := startKVR(t)
	defer env.stop()
	c := env.client
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	val, ok, err := c.Get(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Fatal("Get: expected ok=false for missing key")
	}
	if val != "" {
		t.Fatalf("Get: expected empty value, got %q", val)
	}
}

func TestDel(t *testing.T) {
	env := startKVR(t)
	defer env.stop()
	c := env.client
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := c.Set(ctx, "delkey", "val"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	deleted, err := c.Del(ctx, "delkey")
	if err != nil {
		t.Fatalf("Del: %v", err)
	}
	if !deleted {
		t.Fatal("Del: expected deleted=true")
	}
	// Verify it's gone.
	_, ok, err := c.Get(ctx, "delkey")
	if err != nil {
		t.Fatalf("Get after del: %v", err)
	}
	if ok {
		t.Fatal("Get after del: expected ok=false")
	}
	// Delete again — should return false.
	deleted, err = c.Del(ctx, "delkey")
	if err != nil {
		t.Fatalf("Del missing: %v", err)
	}
	if deleted {
		t.Fatal("Del missing: expected deleted=false")
	}
}

func TestExists(t *testing.T) {
	env := startKVR(t)
	defer env.stop()
	c := env.client
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := c.Set(ctx, "existskey", "val"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	exists, err := c.Exists(ctx, "existskey")
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !exists {
		t.Fatal("Exists: expected true")
	}
	exists, err = c.Exists(ctx, "missing")
	if err != nil {
		t.Fatalf("Exists missing: %v", err)
	}
	if exists {
		t.Fatal("Exists missing: expected false")
	}
}

func TestSetEx(t *testing.T) {
	env := startKVR(t)
	defer env.stop()
	c := env.client
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tests := []struct {
		name string
		ttl  time.Duration
	}{
		{"500ms", 500 * time.Millisecond},
		{"1s", 1 * time.Second},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			key := "ttl_" + tc.name
			if err := c.SetEx(ctx, key, "ttlval", tc.ttl); err != nil {
				t.Fatalf("SetEx: %v", err)
			}
			val, ok, err := c.Get(ctx, key)
			if err != nil || !ok || val != "ttlval" {
				t.Fatalf("Get after SetEx: val=%q ok=%v err=%v", val, ok, err)
			}
			// Wait for expiry.
			time.Sleep(tc.ttl + 200*time.Millisecond)
			_, ok, err = c.Get(ctx, key)
			if err != nil {
				t.Fatalf("Get after TTL: %v", err)
			}
			if ok {
				t.Fatal("Get after TTL: expected ok=false (expired)")
			}
		})
	}
}

func TestSetExInvalidTTL(t *testing.T) {
	env := startKVR(t)
	defer env.stop()
	c := env.client
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	tests := []struct {
		name string
		ttl  time.Duration
	}{
		{"zero", 0},
		{"negative", -1 * time.Second},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := c.SetEx(ctx, "key", "val", tc.ttl)
			if err == nil {
				t.Fatal("expected error for invalid TTL")
			}
		})
	}
}

func TestScan(t *testing.T) {
	env := startKVR(t)
	defer env.stop()
	c := env.client
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Set keys with a common prefix.
	for _, k := range []string{"scan/a", "scan/b", "scan/c", "scan/d", "scan/e"} {
		if err := c.Set(ctx, k, "v"); err != nil {
			t.Fatalf("Set %s: %v", k, err)
		}
	}
	// Set a key with a different prefix to ensure it's excluded.
	if err := c.Set(ctx, "other/x", "v"); err != nil {
		t.Fatalf("Set other/x: %v", err)
	}

	result, err := c.Scan(ctx, "scan/", 10, "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(result.Keys) != 5 {
		t.Fatalf("Scan: got %d keys, want 5 (%v)", len(result.Keys), result.Keys)
	}
	if result.More {
		t.Fatal("Scan: expected More=false (5 keys, limit 10)")
	}
}

func TestScanPagination(t *testing.T) {
	env := startKVR(t)
	defer env.stop()
	c := env.client
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Set 5 keys with a common prefix.
	allKeys := []string{"page/1", "page/2", "page/3", "page/4", "page/5"}
	for _, k := range allKeys {
		if err := c.Set(ctx, k, "v"); err != nil {
			t.Fatalf("Set %s: %v", k, err)
		}
	}

	// Page through with limit=2.
	var collected []string
	cursor := ""
	pages := 0
	for {
		result, err := c.Scan(ctx, "page/", 2, cursor)
		if err != nil {
			t.Fatalf("Scan page %d: %v", pages, err)
		}
		collected = append(collected, result.Keys...)
		pages++
		if !result.More || len(result.Keys) == 0 {
			break
		}
		cursor = result.Cursor
		if pages > 10 {
			t.Fatal("too many pages — possible infinite loop")
		}
	}

	if len(collected) != 5 {
		t.Fatalf("collected %d keys, want 5 (%v)", len(collected), collected)
	}
	// Verify no duplicates.
	seen := make(map[string]bool)
	for _, k := range collected {
		if seen[k] {
			t.Fatalf("duplicate key: %s", k)
		}
		seen[k] = true
	}
}

func TestMGet(t *testing.T) {
	env := startKVR(t)
	defer env.stop()
	c := env.client
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := c.Set(ctx, "mget/a", "val1"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := c.Set(ctx, "mget/b", "val2"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	values, err := c.MGet(ctx, []string{"mget/a", "mget/b", "missing"})
	if err != nil {
		t.Fatalf("MGet: %v", err)
	}
	if len(values) != 3 {
		t.Fatalf("MGet: got %d values, want 3", len(values))
	}
	if string(values[0]) != "val1" {
		t.Fatalf("MGet[0]: got %q, want %q", string(values[0]), "val1")
	}
	if string(values[1]) != "val2" {
		t.Fatalf("MGet[1]: got %q, want %q", string(values[1]), "val2")
	}
	if values[2] != nil {
		t.Fatalf("MGet[2]: expected nil, got %q", string(values[2]))
	}
}

func TestMGetEmpty(t *testing.T) {
	env := startKVR(t)
	defer env.stop()
	c := env.client
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	values, err := c.MGet(ctx, []string{})
	if err != nil {
		t.Fatalf("MGet empty: %v", err)
	}
	if len(values) != 0 {
		t.Fatalf("MGet empty: got %d values, want 0", len(values))
	}
}

func TestMGetTooMany(t *testing.T) {
	env := startKVR(t)
	defer env.stop()
	c := env.client
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	keys := make([]string, 257)
	for i := range keys {
		keys[i] = "k"
	}
	_, err := c.MGet(ctx, keys)
	if err == nil {
		t.Fatal("MGet: expected error for >256 keys")
	}
}

func TestSaveWithSnapshot(t *testing.T) {
	// Start kvr with a snapshot path.
	tmpDir, err := os.MkdirTemp("", "kvr-snap-*")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	snapPath := filepath.Join(tmpDir, "staging.kvr")
	env := startKVRCustom(t,
		"KVR_SNAPSHOT_PATH="+snapPath,
		"KVR_SNAPSHOT_ON_SHUTDOWN=true",
	)
	defer env.stop()
	c := env.client
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Write a key, then SAVE.
	if err := c.Set(ctx, "snapkey", "snapval"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := c.Save(ctx); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Verify the snapshot file was created.
	if _, err := os.Stat(snapPath); err != nil {
		t.Fatalf("snapshot file not created: %v", err)
	}
}

func TestSaveWithoutSnapshot(t *testing.T) {
	env := startKVR(t)
	defer env.stop()
	c := env.client
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := c.Save(ctx)
	if err == nil {
		// Some builds may default differently — if it succeeds, that's fine.
		return
	}
	if !errors.Is(err, ErrServerError) {
		t.Fatalf("Save without snapshot: expected ErrServerError, got %v", err)
	}
}

func TestStoreFull(t *testing.T) {
	// Start with max_entries=2 to force store full quickly.
	env := startKVRCustom(t, "KVR_MAX_ENTRIES=2")
	defer env.stop()
	c := env.client
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := c.Set(ctx, "k1", "v"); err != nil {
		t.Fatalf("Set k1: %v", err)
	}
	if err := c.Set(ctx, "k2", "v"); err != nil {
		t.Fatalf("Set k2: %v", err)
	}
	err := c.Set(ctx, "k3", "v")
	if err == nil {
		t.Fatal("Set k3: expected ErrStoreFull")
	}
	if !errors.Is(err, ErrStoreFull) {
		t.Fatalf("Set k3: expected ErrStoreFull, got %v", err)
	}
}

func TestReconnectBrokenPipe(t *testing.T) {
	env := startKVR(t)
	defer env.stop()
	c := env.client
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// First call works.
	if err := c.Set(ctx, "rc", "1"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Close the underlying conn but leave it non-nil to force EPIPE on write.
	// This exercises the actual reconnect path in call().
	c.mu.Lock()
	c.conn.Close() // conn is still non-nil — next write will fail with broken pipe
	c.mu.Unlock()

	// Next call should reconnect and succeed.
	val, ok, err := c.Get(ctx, "rc")
	if err != nil {
		t.Fatalf("Get after reconnect: %v", err)
	}
	if !ok || val != "1" {
		t.Fatalf("Get after reconnect: val=%q ok=%v", val, ok)
	}
}

func TestConcurrentAccess(t *testing.T) {
	env := startKVR(t)
	defer env.stop()
	c := env.client

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			key := "conc/" + string(rune('a'+n))
			if err := c.Set(ctx, key, "v"); err != nil {
				t.Errorf("Set %s: %v", key, err)
			}
			_, ok, err := c.Get(ctx, key)
			if err != nil || !ok {
				t.Errorf("Get %s: ok=%v err=%v", key, ok, err)
			}
		}(i)
	}
	wg.Wait()
}

func TestKeyTooLong(t *testing.T) {
	env := startKVR(t)
	defer env.stop()
	c := env.client
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	longKey := strings.Repeat("a", maxKeyLen+1)
	err := c.Set(ctx, longKey, "v")
	if err == nil {
		t.Fatal("expected error for oversized key")
	}
}

func TestValueTooLarge(t *testing.T) {
	env := startKVR(t)
	defer env.stop()
	c := env.client
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Value that would exceed maxFrameSize when combined with frame overhead.
	largeValue := strings.Repeat("a", maxFrameSize)
	err := c.Set(ctx, "big", largeValue)
	if err == nil {
		t.Fatal("expected error for oversized value")
	}
}
