package browser

// Block 3 (0.8.11): the browser launch waits for CDP readiness and retries
// the connect+handshake (accept-then-EOF) within the launch bound, with a
// clear bounded error instead of a hang.

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestCDPReadinessRetry: a CDP endpoint that refuses the first N
// connections then answers /json/version → ready. One that never answers →
// clear bounded error naming the bound.
func TestCDPReadinessRetry(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var hits atomic.Int32
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n <= 3 {
			// Refuse the first three (browser still starting).
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"Browser":"Chrome/1.0"}`))
	})}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	url := "http://" + ln.Addr().String()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := waitCDPReady(ctx, url); err != nil {
		t.Fatalf("waitCDPReady should succeed after retries: %v", err)
	}
	if got := hits.Load(); got < 4 {
		t.Errorf("server hits = %d, want at least 4 (3 refused then 1 OK)", got)
	}

	// Never ready → bounded error naming the launch timeout.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel2()
	dead := "http://127.0.0.1:1" // nothing listens there
	err = waitCDPReady(ctx2, dead)
	if err == nil {
		t.Fatal("waitCDPReady should fail against a dead endpoint")
	}
	if !strings.Contains(err.Error(), "Chromium CDP not ready") {
		t.Errorf("error should name the step: %v", err)
	}
}

// TestHandshakeEOFAcceptThenRetry: the accept-then-EOF shape — the first
// HTTP request is accepted and the connection is closed without a response
// (the observed Chromium EOF), the next one answers. waitCDPReady must
// treat that as a retry, not a failure.
func TestHandshakeEOFAcceptThenRetry(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var n atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			k := n.Add(1)
			if k == 1 {
				// Accept then EOF: close without any bytes — the exact
				// handshake-EOF shape from the integration failure.
				_ = conn.Close()
				continue
			}
			// Answer /json/version.
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				_, _ = c.Read(buf)
				body := `{"Browser":"Chrome/1.0"}`
				_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: " + itoa(len(body)) + "\r\n\r\n" + body))
			}(conn)
		}
	}()

	url := "http://" + ln.Addr().String()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := waitCDPReady(ctx, url); err != nil {
		t.Fatalf("accept-then-EOF must be retried: %v", err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
