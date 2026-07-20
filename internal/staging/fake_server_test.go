package staging

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Wire protocol constants for the fake server. INDEPENDENTLY defined from
// the client's constants (client.go) to avoid tautological tests — if both
// sides share the same constant and it's wrong, the test passes anyway.
// These must match kvrust/src/bin/server.rs (the protocol authority).
const (
	fakeOpSet    = 0
	fakeOpGet    = 1
	fakeOpDel    = 2
	fakeOpPing   = 3
	fakeOpExists = 4
	fakeOpSetx   = 5
	fakeOpScan   = 6
	fakeOpMget   = 7
	fakeOpSave   = 8

	fakeRespOk        = 0x10
	fakeRespDeleted   = 0x11
	fakeRespNotFound  = 0x12
	fakeRespStoreFull = 0x13
	fakeRespError     = 0xFF
)

const fakeMaxFrame = 1 << 20 // 1 MiB, must match client's maxFrameSize

// fakeEntry is a stored key-value pair with optional expiry.
type fakeEntry struct {
	value  []byte
	expiry time.Time // zero = no expiry
}

// fakeHandler processes a single request payload and returns a response payload.
// If it returns (nil, false), the server closes the connection without responding.
type fakeHandler func(payload []byte) (response []byte, respond bool)

// fakeServer is a lightweight UDS server that speaks the kvr wire protocol.
// It does NOT depend on the real kvr-server binary — tests always run.
//
// The server has two modes:
//   - Normal: an in-memory store implementing the full protocol (SET/GET/DEL/
//     PING/EXISTS/SETX/SCAN/MGET/SAVE).
//   - Fault injection: a custom fakeHandler per test that can return raw bytes,
//     malformed frames, or close the connection.
type fakeServer struct {
	listener net.Listener
	dir      string

	mu    sync.Mutex
	store map[string]fakeEntry

	// handler overrides the normal dispatch. If nil, normal protocol is used.
	// Set per-test for fault injection.
	handler fakeHandler

	// connCount tracks how many connections were accepted (for reconnect tests).
	// Atomic — incremented from the accept loop goroutine, read from tests.
	connCount int32

	// maxEntries simulates the server's entry limit (0 = unlimited).
	maxEntries int
}

// startFakeServer starts a fake UDS server in a short temp directory.
// The socket path is kept short to avoid UDS path-length limits (~108 bytes
// on Linux). Callers must call srv.stop() (typically via t.Cleanup).
func startFakeServer(t *testing.T) *fakeServer {
	t.Helper()

	// Use a short temp dir path to stay under the UDS path-length limit.
	dir, err := os.MkdirTemp("", "fs-*")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	socketPath := filepath.Join(dir, "s.sock")

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		os.RemoveAll(dir)
		t.Fatalf("listen: %v", err)
	}

	srv := &fakeServer{
		listener: ln,
		dir:      dir,
		store:    make(map[string]fakeEntry),
	}

	go srv.acceptLoop()

	t.Cleanup(srv.stop)
	return srv
}

func (s *fakeServer) socketPath() string {
	return s.listener.Addr().String()
}

func (s *fakeServer) stop() {
	if s.listener != nil {
		s.listener.Close()
	}
	if s.dir != "" {
		os.RemoveAll(s.dir)
	}
}

// setHandler installs a custom handler for fault injection. When set, the
// normal protocol dispatch is bypassed — the handler receives raw payloads
// and returns raw responses.
func (s *fakeServer) setHandler(h fakeHandler) {
		s.mu.Lock()
		s.handler = h
		s.mu.Unlock()
}

// setMaxEntries simulates the server's entry limit.
func (s *fakeServer) setMaxEntries(n int) {
	s.mu.Lock()
	s.maxEntries = n
	s.mu.Unlock()
}

func (s *fakeServer) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return // listener closed
		}
		s.mu.Lock()
		atomic.AddInt32(&s.connCount, 1)
		handler := s.handler
		s.mu.Unlock()
		go s.serveConn(conn, handler)
	}
}

// serveConn handles one connection. If a custom handler is set, it's used
// for every request on this connection. Otherwise, normal protocol dispatch.
func (s *fakeServer) serveConn(conn net.Conn, handler fakeHandler) {
	defer conn.Close()
	for {
		// Set a generous read deadline so the server doesn't hang forever
		// if the client disconnects without closing cleanly.
		conn.SetReadDeadline(time.Now().Add(10 * time.Second))

		payload, err := readFrameRaw(conn)
		if err != nil {
			return // client closed or error
		}

		// Fault injection handler.
		if handler != nil {
			resp, respond := handler(payload)
			if !respond {
				return // close connection without responding
			}
			if err := writeFrameRaw(conn, resp); err != nil {
				return
			}
			continue
		}

		// Normal protocol dispatch.
		resp := s.dispatch(payload)
		if resp == nil {
			return // protocol error — close connection
		}
		if err := writeFrameRaw(conn, resp); err != nil {
			return
		}
	}
}

// dispatch implements the normal kvr protocol over the in-memory store.
func (s *fakeServer) dispatch(payload []byte) []byte {
	if len(payload) < 1 {
		return []byte{fakeRespError}
	}
	op := payload[0]
	switch op {
	case fakeOpPing:
		return []byte{fakeRespOk}
	case fakeOpSet:
		return s.handleSet(payload, false)
	case fakeOpSetx:
		return s.handleSet(payload, true)
	case fakeOpGet:
		return s.handleGet(payload)
	case fakeOpDel:
		return s.handleDel(payload)
	case fakeOpExists:
		return s.handleExists(payload)
	case fakeOpScan:
		return s.handleScan(payload)
	case fakeOpMget:
		return s.handleMGet(payload)
	case fakeOpSave:
		return []byte{fakeRespOk}
	default:
		return []byte{fakeRespError}
	}
}

func (s *fakeServer) handleSet(payload []byte, withTTL bool) []byte {
	// Layout: op(1) + keyLen(2) + key + valLen(4) + val [+ ttlMs(8)]
	pos := 1
	if pos+2 > len(payload) {
		return []byte{fakeRespError}
	}
	kl := int(binary.BigEndian.Uint16(payload[pos:]))
	pos += 2
	if pos+kl > len(payload) {
		return []byte{fakeRespError}
	}
	key := string(payload[pos : pos+kl])
	pos += kl
	if pos+4 > len(payload) {
		return []byte{fakeRespError}
	}
	vl := int(binary.BigEndian.Uint32(payload[pos:]))
	pos += 4
	if pos+vl > len(payload) {
		return []byte{fakeRespError}
	}
	val := make([]byte, vl)
	copy(val, payload[pos:pos+vl])
	pos += vl

	var entry fakeEntry
	entry.value = val
	if withTTL {
		if pos+8 > len(payload) {
			return []byte{fakeRespError}
		}
		ttlMs := binary.BigEndian.Uint64(payload[pos:])
		if ttlMs == 0 {
			return []byte{fakeRespError}
		}
		entry.expiry = time.Now().Add(time.Duration(ttlMs) * time.Millisecond)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.maxEntries > 0 {
		if _, exists := s.store[key]; !exists && len(s.store) >= s.maxEntries {
			return []byte{fakeRespStoreFull}
		}
	}
	s.store[key] = entry
	return []byte{fakeRespOk}
}

func (s *fakeServer) handleGet(payload []byte) []byte {
	key, ok := parseKeyPayload(payload)
	if !ok {
		return []byte{fakeRespError}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, exists := s.store[key]
	if !exists || entry.expired() {
		return []byte{fakeRespNotFound}
	}
	// Response: OK(1) + valLen(4) + val
	resp := make([]byte, 5+len(entry.value))
	resp[0] = fakeRespOk
	binary.BigEndian.PutUint32(resp[1:5], uint32(len(entry.value)))
	copy(resp[5:], entry.value)
	return resp
}

func (s *fakeServer) handleDel(payload []byte) []byte {
	key, ok := parseKeyPayload(payload)
	if !ok {
		return []byte{fakeRespError}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.store[key]; !exists {
		return []byte{fakeRespNotFound}
	}
	delete(s.store, key)
	return []byte{fakeRespDeleted}
}

func (s *fakeServer) handleExists(payload []byte) []byte {
	key, ok := parseKeyPayload(payload)
	if !ok {
		return []byte{fakeRespError}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, exists := s.store[key]
	if !exists || entry.expired() {
		return []byte{fakeRespNotFound}
	}
	return []byte{fakeRespOk}
}

func (s *fakeServer) handleScan(payload []byte) []byte {
	// Layout: op(1) + prefixLen(2) + prefix + limit(2) + cursorLen(2) + cursor
	pos := 1
	if pos+2 > len(payload) {
		return []byte{fakeRespError}
	}
	pl := int(binary.BigEndian.Uint16(payload[pos:]))
	pos += 2
	if pos+pl > len(payload) {
		return []byte{fakeRespError}
	}
	prefix := string(payload[pos : pos+pl])
	pos += pl
	if pos+2 > len(payload) {
		return []byte{fakeRespError}
	}
	limit := int(binary.BigEndian.Uint16(payload[pos:]))
	pos += 2
	if pos+2 > len(payload) {
		return []byte{fakeRespError}
	}
	cl := int(binary.BigEndian.Uint16(payload[pos:]))
	pos += 2
	if pos+cl > len(payload) {
		return []byte{fakeRespError}
	}
	cursor := string(payload[pos : pos+cl])

	s.mu.Lock()
	defer s.mu.Unlock()

	// Collect matching keys (sorted, after cursor).
	var keys []string
	for k, entry := range s.store {
		if entry.expired() {
			continue
		}
		if len(k) < len(prefix) || k[:len(prefix)] != prefix {
			continue
		}
		if cursor != "" && k <= cursor {
			continue
		}
		keys = append(keys, k)
	}
	// Sort keys for deterministic pagination.
	sortStrings(keys)

	more := false
	if limit > 0 && len(keys) > limit {
		keys = keys[:limit]
		more = true
	}

	// Build response: OK(1) + count(2) + keys... + moreFlag(1)
	var resp []byte
	resp = append(resp, fakeRespOk)
	var cnt [2]byte
	binary.BigEndian.PutUint16(cnt[:], uint16(len(keys)))
	resp = append(resp, cnt[:]...)
	for _, k := range keys {
		var kl [2]byte
		binary.BigEndian.PutUint16(kl[:], uint16(len(k)))
		resp = append(resp, kl[:]...)
		resp = append(resp, k...)
	}
	if more {
		resp = append(resp, 0x01)
	} else {
		resp = append(resp, 0x00)
	}
	return resp
}

func (s *fakeServer) handleMGet(payload []byte) []byte {
	// Layout: op(1) + count(2) + [keyLen(2) + key]...
	pos := 1
	if pos+2 > len(payload) {
		return []byte{fakeRespError}
	}
	count := int(binary.BigEndian.Uint16(payload[pos:]))
	pos += 2
	keys := make([]string, 0, count)
	for i := 0; i < count; i++ {
		if pos+2 > len(payload) {
			return []byte{fakeRespError}
		}
		kl := int(binary.BigEndian.Uint16(payload[pos:]))
		pos += 2
		if pos+kl > len(payload) {
			return []byte{fakeRespError}
		}
		keys = append(keys, string(payload[pos:pos+kl]))
		pos += kl
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	resp := []byte{fakeRespOk}
	for _, k := range keys {
		entry, exists := s.store[k]
		if !exists || entry.expired() {
			resp = append(resp, 0x00) // not found
			continue
		}
		resp = append(resp, 0x01) // found
		var vl [4]byte
		binary.BigEndian.PutUint32(vl[:], uint32(len(entry.value)))
		resp = append(resp, vl[:]...)
		resp = append(resp, entry.value...)
	}
	return resp
}

// ─── Frame I/O (independent of client.go's writeFrame/readFrame) ──────

// readFrameRaw reads a 4B BE length-prefixed frame. Independent of the
// client's readFrame to avoid tautological tests.
func readFrameRaw(r interface{ Read([]byte) (int, error) }) ([]byte, error) {
	var hdr [4]byte
	if _, err := readFull(r, hdr[:]); err != nil {
		return nil, err
	}
	frameLen := binary.BigEndian.Uint32(hdr[:])
	if frameLen > fakeMaxFrame {
		return nil, fmt.Errorf("oversized frame (%d bytes)", frameLen)
	}
	if frameLen == 0 {
		return []byte{}, nil
	}
	frame := make([]byte, frameLen)
	if _, err := readFull(r, frame); err != nil {
		return nil, err
	}
	return frame, nil
}

// writeFrameRaw writes a 4B BE length-prefixed frame.
func writeFrameRaw(w interface{ Write([]byte) (int, error) }, payload []byte) error {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// readFull reads exactly len(buf) bytes, independent of io.ReadFull.
func readFull(r interface{ Read([]byte) (int, error) }, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// ─── Helpers ─────────────────────────────────────────────────────────

// parseKeyPayload extracts the key from an op+keyLen+key payload.
func parseKeyPayload(payload []byte) (string, bool) {
	if len(payload) < 3 {
		return "", false
	}
	kl := int(binary.BigEndian.Uint16(payload[1:3]))
	if 3+kl > len(payload) {
		return "", false
	}
	return string(payload[3 : 3+kl]), true
}

func (e fakeEntry) expired() bool {
	return !e.expiry.IsZero() && time.Now().After(e.expiry)
}

// sortStrings sorts a slice of strings in ascending order (simple insertion
// sort — the test datasets are tiny and this avoids importing sort).
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
