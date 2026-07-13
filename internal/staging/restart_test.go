package staging

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestRestartSnapshotPersistence (A7, non-negotiable) verifies the full
// snapshot lifecycle: write via client → graceful stop (SIGTERM → snapshot
// save) → start new server on same snapshot path → read via client →
// value present, expired entries absent.
//
// This test proves the kvr signal handling → snapshot save → snapshot load
// chain. The Docker entrypoint trap (SIGTERM → kvr SIGTERM) is verified
// separately by the executor's graceful teardown (docker stop -t 10);
// this test exercises the kvr-level signal + snapshot + load path directly.
func TestRestartSnapshotPersistence(t *testing.T) {
	bin := kvrServerBin(t)
	if bin == "" {
		t.Skip("kvr-server binary not found — skipping (set KVR_SERVER_BIN or build kvrust)")
	}

	tmpDir, err := os.MkdirTemp("", "kvr-restart-*")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	socketPath1 := filepath.Join(tmpDir, "kvr1.sock")
	snapPath := filepath.Join(tmpDir, "staging.kvr")

	// --- Phase 1: Start server, write data, graceful stop ---

	client1, stop1 := startKVRManual(t, bin, socketPath1, snapPath)
	t.Cleanup(stop1)

	ctx1, cancel1 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel1()

	// Write a persistent key (no TTL).
	if err := client1.Set(ctx1, "persist/key", "persisted_value"); err != nil {
		t.Fatalf("Phase 1 Set: %v", err)
	}

	// Write a short-TTL key (should expire before restart).
	if err := client1.SetEx(ctx1, "ttl/key", "temp_value", 500*time.Millisecond); err != nil {
		t.Fatalf("Phase 1 SetEx: %v", err)
	}

	// Verify both are present before stop.
	val, ok, err := client1.Get(ctx1, "persist/key")
	if err != nil || !ok || val != "persisted_value" {
		t.Fatalf("Phase 1 Get persist/key: val=%q ok=%v err=%v", val, ok, err)
	}
	_, ok, err = client1.Get(ctx1, "ttl/key")
	if err != nil || !ok {
		t.Fatalf("Phase 1 Get ttl/key: ok=%v err=%v", ok, err)
	}

	// Wait for the TTL to expire before stopping (so the snapshot doesn't
	// contain the expired key, or if it does, the loader should skip it).
	time.Sleep(700 * time.Millisecond)

	// Graceful stop: SIGTERM → kvr saves snapshot → exits.
	// This is the same signal chain as docker stop -t 10 → entrypoint trap →
	// SIGTERM → kvr shutdown → snapshot save.
	stop1()

	// Verify the snapshot file was created.
	if _, err := os.Stat(snapPath); err != nil {
		t.Fatalf("snapshot file not created at %s: %v", snapPath, err)
	}

	// --- Phase 2: Start new server on same snapshot path, verify data ---

	socketPath2 := filepath.Join(tmpDir, "kvr2.sock")
	client2, stop2 := startKVRManual(t, bin, socketPath2, snapPath)
	defer stop2()

	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()

	// The persistent key should be present (restored from snapshot).
	val, ok, err = client2.Get(ctx2, "persist/key")
	if err != nil {
		t.Fatalf("Phase 2 Get persist/key: %v", err)
	}
	if !ok {
		t.Fatal("Phase 2 Get persist/key: expected ok=true (restored from snapshot)")
	}
	if val != "persisted_value" {
		t.Fatalf("Phase 2 Get persist/key: got %q, want %q", val, "persisted_value")
	}

	// The short-TTL key should be absent (either skipped on load because
	// it was already expired, or expired after load).
	_, ok, err = client2.Get(ctx2, "ttl/key")
	if err != nil {
		t.Fatalf("Phase 2 Get ttl/key: %v", err)
	}
	if ok {
		t.Fatal("Phase 2 Get ttl/key: expected ok=false (expired entry should be absent)")
	}
}

// startKVRManual starts a kvr-server with explicit socket and snapshot paths.
// Returns a client and a stop function. The stop function sends SIGTERM
// (graceful shutdown → snapshot save) and waits.
func startKVRManual(t *testing.T, bin, socketPath, snapPath string) (*Client, func()) {
	t.Helper()
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"KVR_SOCKET_PATH="+socketPath,
		"KVR_MAX_ENTRIES=1000",
		"KVR_SWEEP_INTERVAL_SECS=1",
		"KVR_SNAPSHOT_PATH="+snapPath,
		"KVR_SNAPSHOT_ON_SHUTDOWN=true",
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start kvr-server: %v", err)
	}

	client := NewClient(socketPath)
	ready := false
	for i := 0; i < 50; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if err := client.Ping(ctx); err == nil {
			ready = true
		}
		cancel()
		if ready {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatalf("kvr-server did not become ready")
	}

	stop := func() {
		client.Close()
		if cmd.Process != nil {
			// SIGTERM → kvr graceful shutdown → snapshot save → exit.
			// This is the same signal docker stop sends to the entrypoint.
			cmd.Process.Signal(syscall.SIGTERM)
			done := make(chan struct{})
			go func() { cmd.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				cmd.Process.Kill()
				<-done
			}
		}
	}
	return client, stop
}
