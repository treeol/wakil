package staging

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// client_fake_test.go — integration tests using the fake UDS server
// (fake_server_test.go). These tests ALWAYS RUN (no kvr-server binary needed)
// and cover: happy-path operations, error responses, reconnect-once,
// fault injection (malformed/truncated responses), and client-side validation.

// ─── Happy path: full operation round-trip ────────────────────────────

func TestFakePing(t *testing.T) {
	srv := startFakeServer(t)
	c := NewClient(srv.socketPath())
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestFakeSetGet(t *testing.T) {
	srv := startFakeServer(t)
	c := NewClient(srv.socketPath())
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	tests := []struct {
		name  string
		key   string
		value string
	}{
		{"basic", "foo", "bar"},
		{"empty_value", "empty", ""},
		{"slash_key", "a/b/c", "val"},
		{"unicode_value", "u", "héllo世界"},
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
			if !ok {
				t.Fatalf("Get(%q): not found", tc.key)
			}
			if val != tc.value {
				t.Errorf("Get(%q): val=%q, want %q", tc.key, val, tc.value)
			}
		})
	}
}

func TestFakeGetNotFound(t *testing.T) {
	srv := startFakeServer(t)
	c := NewClient(srv.socketPath())
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	val, ok, err := c.Get(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false")
	}
	if val != "" {
		t.Fatalf("expected empty value, got %q", val)
	}
}

func TestFakeDel(t *testing.T) {
	srv := startFakeServer(t)
	c := NewClient(srv.socketPath())
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := c.Set(ctx, "delkey", "val"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	deleted, err := c.Del(ctx, "delkey")
	if err != nil || !deleted {
		t.Fatalf("Del: deleted=%v err=%v", deleted, err)
	}
	// Delete again — not found.
	deleted, err = c.Del(ctx, "delkey")
	if err != nil || deleted {
		t.Fatalf("Del again: deleted=%v err=%v", deleted, err)
	}
}

func TestFakeExists(t *testing.T) {
	srv := startFakeServer(t)
	c := NewClient(srv.socketPath())
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := c.Set(ctx, "exkey", "val"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	exists, err := c.Exists(ctx, "exkey")
	if err != nil || !exists {
		t.Fatalf("Exists: exists=%v err=%v", exists, err)
	}
	exists, err = c.Exists(ctx, "missing")
	if err != nil || exists {
		t.Fatalf("Exists missing: exists=%v err=%v", exists, err)
	}
}

func TestFakeScan(t *testing.T) {
	srv := startFakeServer(t)
	c := NewClient(srv.socketPath())
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	for _, k := range []string{"scan/a", "scan/b", "scan/c"} {
		if err := c.Set(ctx, k, "v"); err != nil {
			t.Fatalf("Set %s: %v", k, err)
		}
	}
	if err := c.Set(ctx, "other/x", "v"); err != nil {
		t.Fatalf("Set other/x: %v", err)
	}

	result, err := c.Scan(ctx, "scan/", 10, "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(result.Keys) != 3 {
		t.Fatalf("Scan: got %d keys, want 3 (%v)", len(result.Keys), result.Keys)
	}
	if result.More {
		t.Fatal("expected More=false")
	}
}

func TestFakeScanPagination(t *testing.T) {
	srv := startFakeServer(t)
	c := NewClient(srv.socketPath())
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, k := range []string{"p/1", "p/2", "p/3", "p/4", "p/5"} {
		if err := c.Set(ctx, k, "v"); err != nil {
			t.Fatalf("Set %s: %v", k, err)
		}
	}

	var collected []string
	cursor := ""
	for page := 0; page < 10; page++ {
		result, err := c.Scan(ctx, "p/", 2, cursor)
		if err != nil {
			t.Fatalf("Scan page %d: %v", page, err)
		}
		collected = append(collected, result.Keys...)
		if !result.More || len(result.Keys) == 0 {
			break
		}
		cursor = result.Cursor
	}
	if len(collected) != 5 {
		t.Fatalf("collected %d keys, want 5", len(collected))
	}
	// No duplicates
	seen := make(map[string]bool)
	for _, k := range collected {
		if seen[k] {
			t.Fatalf("duplicate key: %s", k)
		}
		seen[k] = true
	}
}

func TestFakeMGet(t *testing.T) {
	srv := startFakeServer(t)
	c := NewClient(srv.socketPath())
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	c.Set(ctx, "m/a", "v1")
	c.Set(ctx, "m/b", "v2")

	values, err := c.MGet(ctx, []string{"m/a", "m/b", "missing"})
	if err != nil {
		t.Fatalf("MGet: %v", err)
	}
	if len(values) != 3 {
		t.Fatalf("got %d values, want 3", len(values))
	}
	if string(values[0]) != "v1" {
		t.Fatalf("values[0] = %q, want %q", string(values[0]), "v1")
	}
	if string(values[1]) != "v2" {
		t.Fatalf("values[1] = %q, want %q", string(values[1]), "v2")
	}
	if values[2] != nil {
		t.Fatalf("values[2] = %v, want nil", values[2])
	}
}

func TestFakeMGetEmpty(t *testing.T) {
	srv := startFakeServer(t)
	c := NewClient(srv.socketPath())
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	values, err := c.MGet(ctx, []string{})
	if err != nil {
		t.Fatalf("MGet empty: %v", err)
	}
	if len(values) != 0 {
		t.Fatalf("got %d values, want 0", len(values))
	}
}

func TestFakeSave(t *testing.T) {
	srv := startFakeServer(t)
	c := NewClient(srv.socketPath())
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := c.Save(ctx); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

// ─── Error responses ──────────────────────────────────────────────────

func TestFakeStoreFull(t *testing.T) {
	srv := startFakeServer(t)
	srv.setMaxEntries(2)
	c := NewClient(srv.socketPath())
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	c.Set(ctx, "k1", "v")
	c.Set(ctx, "k2", "v")
	err := c.Set(ctx, "k3", "v")
	if !errors.Is(err, ErrStoreFull) {
		t.Fatalf("expected ErrStoreFull, got %v", err)
	}
}

func TestFakeServerErrorResponse(t *testing.T) {
	srv := startFakeServer(t)
	// Inject: always respond with RESP_ERROR.
	srv.setHandler(func(payload []byte) ([]byte, bool) {
		return []byte{fakeRespError}, true
	})
	c := NewClient(srv.socketPath())
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := c.Ping(ctx)
	if !errors.Is(err, ErrServerError) {
		t.Fatalf("expected ErrServerError, got %v", err)
	}
}

// ─── Reconnect-once ───────────────────────────────────────────────────

func TestFakeReconnectBrokenPipe(t *testing.T) {
	srv := startFakeServer(t)
	c := NewClient(srv.socketPath())
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// First call works.
	if err := c.Set(ctx, "rc", "1"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	connCountBefore := int(atomic.LoadInt32(&srv.connCount))

	// Close the underlying conn to force EPIPE on next write.
	// conn is still non-nil → next write triggers broken pipe → reconnect → retry.
	c.mu.Lock()
	c.conn.Close()
	c.mu.Unlock()

	// Next call should reconnect and succeed.
	val, ok, err := c.Get(ctx, "rc")
	if err != nil {
		t.Fatalf("Get after reconnect: %v", err)
	}
	if !ok || val != "1" {
		t.Fatalf("Get after reconnect: val=%q ok=%v", val, ok)
	}
	// Verify a new connection was made (connCount increased by at least 1).
	if int(atomic.LoadInt32(&srv.connCount)) <= connCountBefore {
		t.Fatalf("expected connCount to increase after reconnect: before=%d after=%d",
			connCountBefore, srv.connCount)
	}
}

func TestFakeReconnectConnDiscardedAfterError(t *testing.T) {
	srv := startFakeServer(t)
	// First request: return RESP_ERROR (causes ErrServerError, discards conn).
	// Second request: return RESP_OK (new conn, succeeds).
	var reqCount int32
	srv.setHandler(func(payload []byte) ([]byte, bool) {
		n := atomic.AddInt32(&reqCount, 1)
		if n == 1 {
			return []byte{fakeRespError}, true
		}
		return []byte{fakeRespOk}, true
	})
	c := NewClient(srv.socketPath())
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// First Ping fails with server error.
	err := c.Ping(ctx)
	if !errors.Is(err, ErrServerError) {
		t.Fatalf("first Ping: expected ErrServerError, got %v", err)
	}
	// Second Ping succeeds (fresh connection).
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("second Ping: %v", err)
	}
	// Verify two requests were made (proves the first conn was discarded and a
	// new one was established for the second request).
	if got := atomic.LoadInt32(&reqCount); got != 2 {
		t.Fatalf("expected 2 requests (discard+reconnect), got %d", got)
	}
}

// ─── Timeout does NOT retry ──────────────────────────────────────────

func TestFakeTimeoutNoRetry(t *testing.T) {
	srv := startFakeServer(t)
	// Server accepts but never responds → client times out.
	srv.setHandler(func(payload []byte) ([]byte, bool) {
		time.Sleep(5 * time.Second)
		return []byte{fakeRespOk}, true
	})
	c := NewClient(srv.socketPath())
	c.SetTimeout(200 * time.Millisecond)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := c.Ping(ctx)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	// Should NOT be ErrServerError — it's a network timeout.
	if errors.Is(err, ErrServerError) {
		t.Fatal("timeout should not be ErrServerError")
	}
}

// ─── Fault injection: malformed/truncated responses ───────────────────

func TestFakeMalformedResponseZeroLength(t *testing.T) {
	srv := startFakeServer(t)
	// Respond with a zero-length payload (no status byte).
	srv.setHandler(func(payload []byte) ([]byte, bool) {
		return []byte{}, true
	})
	c := NewClient(srv.socketPath())
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Ping with zero-length response → ErrServerError (len < 1).
	err := c.Ping(ctx)
	if !errors.Is(err, ErrServerError) {
		t.Fatalf("expected ErrServerError, got %v", err)
	}
}

func TestFakeTruncatedGetResponse(t *testing.T) {
	srv := startFakeServer(t)
	// GET response: OK status but no val-len/value (truncated).
	srv.setHandler(func(payload []byte) ([]byte, bool) {
		return []byte{fakeRespOk}, true // OK but no val-len → truncated
	})
	c := NewClient(srv.socketPath())
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, _, err := c.Get(ctx, "key")
	if !errors.Is(err, ErrServerError) {
		t.Fatalf("expected ErrServerError for truncated GET, got %v", err)
	}
}

func TestFakeGetTrailingBytes(t *testing.T) {
	srv := startFakeServer(t)
	// GET response: OK + val-len(1) + "x" + trailing byte.
	srv.setHandler(func(payload []byte) ([]byte, bool) {
		return []byte{fakeRespOk, 0x00, 0x00, 0x00, 0x01, 'x', 0xFF}, true
	})
	c := NewClient(srv.socketPath())
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, _, err := c.Get(ctx, "key")
	if !errors.Is(err, ErrServerError) {
		t.Fatalf("expected ErrServerError for trailing bytes, got %v", err)
	}
}

func TestFakeOversizedFrame(t *testing.T) {
	srv := startFakeServer(t)
	// Respond with a frame whose length prefix exceeds maxFrameSize.
	srv.setHandler(func(payload []byte) ([]byte, bool) {
		// We need to write raw bytes to the connection, not through writeFrame.
		// But the fake server uses writeFrameRaw which respects the protocol.
		// Instead, return a payload that when framed will be huge — but
		// writeFrameRaw caps at uint32. Let's write a 1MB+1 payload.
		return make([]byte, fakeMaxFrame+1), true
	})
	c := NewClient(srv.socketPath())
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, _, err := c.Get(ctx, "key")
	if err == nil {
		t.Fatal("expected error for oversized frame")
	}
}

func TestFakeServerClosesConnection(t *testing.T) {
	srv := startFakeServer(t)
	// Server closes the connection after reading the request (respond=false).
	srv.setHandler(func(payload []byte) ([]byte, bool) {
		return nil, false
	})
	c := NewClient(srv.socketPath())
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := c.Ping(ctx)
	if err == nil {
		t.Fatal("expected error when server closes connection")
	}
}

// ─── Close lifecycle ──────────────────────────────────────────────────

func TestFakeDoubleClose(t *testing.T) {
	srv := startFakeServer(t)
	c := NewClient(srv.socketPath())
	if err := c.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	// Second close on idle client should be nil (no conn to close).
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestFakeCallAfterCloseReconnects(t *testing.T) {
	srv := startFakeServer(t)
	c := NewClient(srv.socketPath())
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Close the client (drops the conn), then call Ping — lazy reconnect.
	c.Close()
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping after Close (reconnect): %v", err)
	}
}

// ─── Concurrency (mutex serialization under -race) ────────────────────

func TestFakeConcurrentAccess(t *testing.T) {
	srv := startFakeServer(t)
	c := NewClient(srv.socketPath())
	defer c.Close()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			key := fmt.Sprintf("conc/%d", n)
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

// ─── Client-side validation (no server needed) ────────────────────────

func TestFakeKeyTooLong(t *testing.T) {
	srv := startFakeServer(t)
	c := NewClient(srv.socketPath())
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	longKey := string(make([]byte, maxKeyLen+1))
	err := c.Set(ctx, longKey, "v")
	if err == nil {
		t.Fatal("expected error for oversized key")
	}
}

func TestFakeValueTooLarge(t *testing.T) {
	srv := startFakeServer(t)
	c := NewClient(srv.socketPath())
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	largeValue := string(make([]byte, maxFrameSize))
	err := c.Set(ctx, "big", largeValue)
	if err == nil {
		t.Fatal("expected error for oversized value")
	}
}

func TestFakeSetExHappyPath(t *testing.T) {
	srv := startFakeServer(t)
	c := NewClient(srv.socketPath())
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// SetEx with a 500ms TTL.
	if err := c.SetEx(ctx, "ttlkey", "ttlval", 500*time.Millisecond); err != nil {
		t.Fatalf("SetEx: %v", err)
	}
	// Value should be present immediately.
	val, ok, err := c.Get(ctx, "ttlkey")
	if err != nil || !ok || val != "ttlval" {
		t.Fatalf("Get after SetEx: val=%q ok=%v err=%v", val, ok, err)
	}
	// Wait for expiry.
	time.Sleep(700 * time.Millisecond)
	_, ok, err = c.Get(ctx, "ttlkey")
	if err != nil {
		t.Fatalf("Get after TTL: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false after TTL expiry")
	}
}

func TestFakeSetExInvalidTTL(t *testing.T) {
	srv := startFakeServer(t)
	c := NewClient(srv.socketPath())
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	for _, ttl := range []time.Duration{0, -1 * time.Second} {
		err := c.SetEx(ctx, "key", "val", ttl)
		if err == nil {
			t.Fatalf("expected error for TTL=%v", ttl)
		}
	}
}

func TestFakeSetExSubMillisecond(t *testing.T) {
	srv := startFakeServer(t)
	c := NewClient(srv.socketPath())
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// 500µs → rounds to 0ms → error.
	err := c.SetEx(ctx, "key", "val", 500*time.Microsecond)
	if err == nil {
		t.Fatal("expected error for sub-ms TTL")
	}
}

func TestFakeMGetTooMany(t *testing.T) {
	srv := startFakeServer(t)
	c := NewClient(srv.socketPath())
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	keys := make([]string, maxMgetKeys+1)
	for i := range keys {
		keys[i] = "k"
	}
	_, err := c.MGet(ctx, keys)
	if err == nil {
		t.Fatal("expected error for >256 MGET keys")
	}
}
