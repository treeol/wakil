package wiring

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/exec"
	"github.com/treeol/wakil/internal/sandbox"
	"github.com/treeol/wakil/internal/sandbox/fake"
	"github.com/treeol/wakil/internal/staging"
)

// lazyKVRBackend is a fake Backend whose in-sandbox staging service exists only
// after Start — like the hisn backend: nothing runs before the first call.
type lazyKVRBackend struct {
	*fake.Backend
	sock       string
	noService  bool // the session starts but the service never answers
	mu         sync.Mutex
	up         bool
	ln         net.Listener
	store      map[string]string
	startCalls int
}

func (b *lazyKVRBackend) Start(ctx context.Context, s sandbox.Session) error {
	if err := b.Backend.Start(ctx, s); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.startCalls++
	if b.noService {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(b.sock), 0o700); err != nil {
		return err
	}
	ln, err := net.Listen("unix", b.sock)
	if err != nil {
		return err
	}
	b.ln, b.up, b.store = ln, true, map[string]string{}
	go b.serve(ln)
	return nil
}

func (b *lazyKVRBackend) Stop(ctx context.Context) error {
	b.mu.Lock()
	if b.ln != nil {
		_ = b.ln.Close()
	}
	b.up = false
	b.mu.Unlock()
	return b.Backend.Stop(ctx)
}

func (b *lazyKVRBackend) PlannedKVRSocket() string { return b.sock }
func (b *lazyKVRBackend) KVRSocket() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.up {
		return b.sock
	}
	return ""
}

// serve speaks just enough of the kvr wire protocol for SET/GET/PING.
func (b *lazyKVRBackend) serve(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			for {
				var h [4]byte
				if _, err := io.ReadFull(c, h[:]); err != nil {
					return
				}
				body := make([]byte, binary.BigEndian.Uint32(h[:]))
				if _, err := io.ReadFull(c, body); err != nil {
					return
				}
				reply := func(p []byte) {
					var l [4]byte
					binary.BigEndian.PutUint32(l[:], uint32(len(p)))
					_, _ = c.Write(append(l[:], p...))
				}
				switch body[0] {
				case 3: // PING
					reply([]byte{0x10})
				case 0: // SET: op, klen(2), key, vlen(4), value
					kl := int(binary.BigEndian.Uint16(body[1:3]))
					key := string(body[3 : 3+kl])
					vl := int(binary.BigEndian.Uint32(body[3+kl : 7+kl]))
					b.mu.Lock()
					b.store[key] = string(body[7+kl : 7+kl+vl])
					b.mu.Unlock()
					reply([]byte{0x10})
				case 1: // GET: op, klen(2), key  -> OK, vlen(4), value
					kl := int(binary.BigEndian.Uint16(body[1:3]))
					b.mu.Lock()
					v, ok := b.store[string(body[3:3+kl])]
					b.mu.Unlock()
					if !ok {
						reply([]byte{0x12})
					} else {
						out := []byte{0x10, 0, 0, 0, 0}
						binary.BigEndian.PutUint32(out[1:], uint32(len(v)))
						reply(append(out, v...))
					}
				default:
					reply([]byte{0xFF})
				}
			}
		}()
	}
}

func newLazyExecutor(t *testing.T, noService bool) (*exec.BackendExecutor, *lazyKVRBackend) {
	t.Helper()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	// A short socket path (unix socket path limit).
	sockDir, err := os.MkdirTemp("", "kv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	b := &lazyKVRBackend{Backend: fake.New(root, root), sock: filepath.Join(sockDir, "kvr.sock"), noService: noService}
	exe := exec.NewBackendExecutor(b, exec.BackendOptions{Label: "stub"})
	t.Cleanup(func() { _ = exe.Close() })
	if err := exe.BindChat(context.Background(), "chat-1"); err != nil {
		t.Fatal(err)
	}
	return exe, b
}

// Mashūra R2 B1: the staging client must work although the session starts
// lazily (nothing runs before the first call), and must refuse when the
// service never came up.
func TestInSandboxStagingClientStartsSessionLazily(t *testing.T) {
	exe, b := newLazyExecutor(t, false)
	if b.startCalls != 0 {
		t.Fatal("precondition: no session before the first call")
	}
	planned := plannedKVR(exe)
	if planned != b.sock {
		t.Fatalf("planned socket = %q, want %q (known before any session)", planned, b.sock)
	}
	// The real guard also vets the socket's location (<workspace>/.wakil): the
	// stub keeps its socket in a short temp dir, so the location check is the
	// one thing replaced here; session start and service-up checks are real.
	c := staging.NewClient(planned)
	c.SetPathGuard(func() error {
		if err := inSandboxKVRGuard(exe, planned)(); err != nil && !strings.Contains(err.Error(), "not directly under") {
			return err
		}
		return nil
	})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := c.Set(ctx, "agent/k", "v1"); err != nil {
		t.Fatalf("the first staging call must start the session and reach the service: %v", err)
	}
	if b.startCalls != 1 {
		t.Fatalf("the guard must start the session exactly once, started %d times", b.startCalls)
	}
	if got, found, err := c.Get(ctx, "agent/k"); err != nil || !found || got != "v1" {
		t.Fatalf("staging put/get through the lazily started service: %q found=%v err=%v", got, found, err)
	}
	if b.startCalls != 1 {
		t.Errorf("a running session is not restarted: %d starts", b.startCalls)
	}
	// And the real, unmodified guard refuses a socket outside <workspace>/.wakil.
	if err := inSandboxKVRGuard(exe, planned)(); err == nil || !strings.Contains(err.Error(), "not directly under") {
		t.Errorf("the real guard must vet the location, got %v", err)
	}
}

func TestInSandboxStagingRefusedWhenServiceDown(t *testing.T) {
	exe, _ := newLazyExecutor(t, true)
	err := inSandboxKVRGuard(exe, plannedKVR(exe))()
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("a missing service must refuse the dial with the reason, got %v", err)
	}
}

func TestPlannedKVROnlyForInSandboxStores(t *testing.T) {
	d, err := exec.NewDirectExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if plannedKVR(d) != "" {
		t.Error("direct mode has no planned in-sandbox store")
	}
	root, _ := filepath.EvalSymlinks(t.TempDir())
	plain := exec.NewBackendExecutor(fake.New(root, root), exec.BackendOptions{Label: "x"})
	if plannedKVR(plain) != "" {
		t.Error("a backend without a store plans no socket")
	}
}
