// Package staging implements a Go client for the kvr wire protocol — a
// length-prefixed binary protocol over a Unix Domain Socket.
//
// The client supports all kvr opcodes: SET, GET, DEL, PING, EXISTS, SETX,
// SCAN (with pagination), MGET, and SAVE. It uses lazy connect with
// reconnect-once on broken pipe, per-call timeouts (2s default), and no
// pooling (callers are tool executions, volume is low).
//
// All kvr content is UNTRUSTED — the Go client returns raw bytes/strings
// without validation. The tool layer is responsible for any safety checks.
package staging

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

// Wire protocol constants (must match kvrust/src/bin/server.rs).
const (
	opSet    = 0
	opGet    = 1
	opDel    = 2
	opPing   = 3
	opExists = 4
	opSetx   = 5
	opScan   = 6
	opMget   = 7
	opSave   = 8

	respOk        = 0x10
	respDeleted   = 0x11
	respNotFound  = 0x12
	respStoreFull = 0x13
	respError     = 0xFF

	maxFrameSize = 1 << 20 // 1 MiB
	maxKeyLen    = 0xFFFF  // u16 limit
	maxMgetKeys  = 256
)

// Typed errors for kvr status codes.
var (
	ErrStoreFull   = errors.New("staging: store is full")
	ErrServerError = errors.New("staging: server error")
)

// defaultTimeout is the per-call timeout for all operations.
const defaultTimeout = 2 * time.Second

// Client is a thread-safe kvr wire protocol client. It connects lazily
// to the UDS socket and reconnects once on broken pipe. No pooling —
// each call acquires the connection mutex.
type Client struct {
	socketPath string
	timeout    time.Duration

	mu   sync.Mutex
	conn net.Conn
}

// NewClient creates a staging client that connects to the given UDS path.
// The connection is established lazily on the first call.
func NewClient(socketPath string) *Client {
	return &Client{
		socketPath: socketPath,
		timeout:    defaultTimeout,
	}
}

// SetTimeout overrides the per-call timeout (default 2s). Must be called
// before concurrent use — not synchronized.
func (c *Client) SetTimeout(d time.Duration) {
	c.timeout = d
}

// Close closes the underlying connection if open.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		err := c.conn.Close()
		c.conn = nil
		return err
	}
	return nil
}

// connect establishes a connection to the UDS socket if not already connected.
// Caller must hold c.mu.
func (c *Client) connect() error {
	if c.conn != nil {
		return nil
	}
	conn, err := net.DialTimeout("unix", c.socketPath, c.timeout)
	if err != nil {
		return fmt.Errorf("dial %s: %w", c.socketPath, err)
	}
	c.conn = conn
	return nil
}

// call sends a request frame and reads the response frame. It handles
// reconnect-once on broken pipe (EPIPE/ECONNRESET). Timeouts and context
// cancellation do NOT trigger a retry. Caller must hold c.mu.
func (c *Client) call(ctx context.Context, payload []byte) ([]byte, error) {
	if err := c.connect(); err != nil {
		return nil, err
	}

	// Per-call deadline: the earlier of the context deadline and the
	// client's default timeout. This ensures a hard 2s cap even when the
	// context has no deadline or a longer one.
	deadline := time.Now().Add(c.timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}

	resp, err := c.sendAndReceive(deadline, payload)
	if err != nil && isBrokenPipe(err) {
		// Reconnect once and retry — but only for broken-pipe errors,
		// not timeouts or context cancellation.
		c.conn.Close()
		c.conn = nil
		if err := c.connect(); err != nil {
			return nil, fmt.Errorf("reconnect: %w", err)
		}
		// Reset deadline for the retry.
		deadline = time.Now().Add(c.timeout)
		if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
			deadline = dl
		}
		resp, err = c.sendAndReceive(deadline, payload)
	}
	// On ANY error (first attempt or retry), discard the connection to
	// prevent stream desync on the next call.
	if err != nil {
		c.conn.Close()
		c.conn = nil
	}
	return resp, err
}

func (c *Client) sendAndReceive(deadline time.Time, payload []byte) ([]byte, error) {
	if err := c.conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("set deadline: %w", err)
	}
	if err := writeFrame(c.conn, payload); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}
	resp, err := readFrame(c.conn)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	return resp, nil
}

// isBrokenPipe reports whether the error indicates a broken connection
// (EPIPE, ECONNRESET, or EOF). Timeouts and context cancellation are
// explicitly excluded — they should not trigger a retry.
func isBrokenPipe(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.ErrClosedPipe) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	// Check for connection reset / broken pipe specifically.
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		// Timeouts are net.Error with Timeout()=true — don't treat those
		// as broken pipe. Only actual connection errors (reset, pipe, etc.)
		// qualify.
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return false
		}
		return true
	}
	return false
}

// writeFrame writes a length-prefixed frame: 4B BE length + payload.
func writeFrame(w io.Writer, payload []byte) error {
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

// readFrame reads a length-prefixed frame: 4B BE length + payload.
// Returns an error if the frame exceeds maxFrameSize.
func readFrame(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	frameLen := binary.BigEndian.Uint32(hdr[:])
	if frameLen > maxFrameSize {
		return nil, fmt.Errorf("oversized frame (%d bytes)", frameLen)
	}
	if frameLen == 0 {
		return []byte{}, nil
	}
	frame := make([]byte, frameLen)
	if _, err := io.ReadFull(r, frame); err != nil {
		return nil, err
	}
	return frame, nil
}

// validateKey checks that the key is not too long (u16 limit). The server
// rejects non-UTF-8 keys, but the client does not validate UTF-8 — the
// server's RESP_ERROR is sufficient and avoids duplicating the check.
func validateKey(key string) error {
	if len(key) > maxKeyLen {
		return fmt.Errorf("key too long (%d bytes, max %d)", len(key), maxKeyLen)
	}
	return nil
}

// validateSetValue checks that the value plus frame overhead does not
// exceed maxFrameSize. The SET frame is: 1 (op) + 2 (key-len) + key + 4
// (val-len) + value. SETX adds 8 (ttl-ms).
func validateSetValue(key, value string) error {
	// Frame overhead: 1 (op) + 2 (key-len) + key + 4 (val-len) + value + 8 (ttl for SETX)
	overhead := 1 + 2 + len(key) + 4 + 8
	total := overhead + len(value)
	if total > maxFrameSize {
		return fmt.Errorf("frame too large (%d bytes, max %d): key=%d value=%d", total, maxFrameSize, len(key), len(value))
	}
	return nil
}

// validateScanLimit caps the SCAN limit to a valid u16 range.
func validateScanLimit(limit int) (int, error) {
	if limit < 0 {
		return 0, fmt.Errorf("limit must be >= 0")
	}
	if limit > maxKeyLen {
		// Cap at 65535 (u16 max); server caps at 1024 internally.
		limit = maxKeyLen
	}
	return limit, nil
}

// Ping sends a PING and expects RESP_OK. Returns nil if the server is alive.
func (c *Client) Ping(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	resp, err := c.call(ctx, []byte{opPing})
	if err != nil {
		return err
	}
	if len(resp) != 1 || resp[0] != respOk {
		return fmt.Errorf("%w: ping returned status 0x%02x (len %d)", ErrServerError, statusByte(resp), len(resp))
	}
	return nil
}

// Set stores a key-value pair. Returns ErrStoreFull if the store's entry
// limit has been reached.
func (c *Client) Set(ctx context.Context, key, value string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if err := validateSetValue(key, value); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	frame := buildSetFrame(key, value)
	resp, err := c.call(ctx, frame)
	if err != nil {
		return err
	}
	return statusToError(resp)
}

// SetEx stores a key-value pair with a TTL. ttl must be > 0.
func (c *Client) SetEx(ctx context.Context, key, value string, ttl time.Duration) error {
	if ttl <= 0 {
		return fmt.Errorf("ttl must be > 0")
	}
	if err := validateKey(key); err != nil {
		return err
	}
	if err := validateSetValue(key, value); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	ttlMs := uint64(ttl.Milliseconds())
	if ttlMs == 0 {
		return fmt.Errorf("ttl too small (rounds to 0ms)")
	}
	frame := buildSetxFrame(key, value, ttlMs)
	resp, err := c.call(ctx, frame)
	if err != nil {
		return err
	}
	return statusToError(resp)
}

// Get retrieves a value by key. Returns the value, true, nil if found;
// "", false, nil if not found; "", false, err on error.
func (c *Client) Get(ctx context.Context, key string) (string, bool, error) {
	if err := validateKey(key); err != nil {
		return "", false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	frame := buildKeyFrame(opGet, key)
	resp, err := c.call(ctx, frame)
	if err != nil {
		return "", false, err
	}
	if len(resp) < 1 {
		return "", false, ErrServerError
	}
	switch resp[0] {
	case respOk:
		// OK + 4B val-len + val. Must be exactly 5+valLen bytes.
		if len(resp) < 5 {
			return "", false, fmt.Errorf("%w: truncated GET response", ErrServerError)
		}
		valLen := binary.BigEndian.Uint32(resp[1:5])
		if len(resp) != int(5+valLen) {
			return "", false, fmt.Errorf("%w: GET response has trailing bytes", ErrServerError)
		}
		return string(resp[5 : 5+valLen]), true, nil
	case respNotFound:
		if len(resp) != 1 {
			return "", false, fmt.Errorf("%w: NOT_FOUND has trailing bytes", ErrServerError)
		}
		return "", false, nil
	case respStoreFull:
		if len(resp) != 1 {
			return "", false, fmt.Errorf("%w: STORE_FULL has trailing bytes", ErrServerError)
		}
		return "", false, ErrStoreFull
	default:
		return "", false, fmt.Errorf("%w: GET returned status 0x%02x", ErrServerError, resp[0])
	}
}

// Del deletes a key. Returns true if the key existed and was deleted,
// false if the key was not found.
func (c *Client) Del(ctx context.Context, key string) (bool, error) {
	if err := validateKey(key); err != nil {
		return false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	frame := buildKeyFrame(opDel, key)
	resp, err := c.call(ctx, frame)
	if err != nil {
		return false, err
	}
	if len(resp) < 1 {
		return false, ErrServerError
	}
	switch resp[0] {
	case respDeleted:
		if len(resp) != 1 {
			return false, fmt.Errorf("%w: DELETED has trailing bytes", ErrServerError)
		}
		return true, nil
	case respNotFound:
		if len(resp) != 1 {
			return false, fmt.Errorf("%w: NOT_FOUND has trailing bytes", ErrServerError)
		}
		return false, nil
	default:
		return false, fmt.Errorf("%w: DEL returned status 0x%02x", ErrServerError, resp[0])
	}
}

// Exists checks whether a key exists. Returns true if the key exists.
func (c *Client) Exists(ctx context.Context, key string) (bool, error) {
	if err := validateKey(key); err != nil {
		return false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	frame := buildKeyFrame(opExists, key)
	resp, err := c.call(ctx, frame)
	if err != nil {
		return false, err
	}
	if len(resp) < 1 {
		return false, ErrServerError
	}
	switch resp[0] {
	case respOk:
		if len(resp) != 1 {
			return false, fmt.Errorf("%w: EXISTS OK has trailing bytes", ErrServerError)
		}
		return true, nil
	case respNotFound:
		if len(resp) != 1 {
			return false, fmt.Errorf("%w: NOT_FOUND has trailing bytes", ErrServerError)
		}
		return false, nil
	default:
		return false, fmt.Errorf("%w: EXISTS returned status 0x%02x", ErrServerError, resp[0])
	}
}

// ScanResult holds the result of a SCAN operation.
type ScanResult struct {
	Keys   []string
	More   bool   // true if more keys are available (pagination)
	Cursor string // opaque cursor for the next page (last key returned)
}

// Scan returns up to limit keys matching the given prefix, starting after
// the cursor. Pass an empty cursor to start from the beginning. limit is
// capped at 1024 by the server.
func (c *Client) Scan(ctx context.Context, prefix string, limit int, cursor string) (result *ScanResult, err error) {
	if err := validateKey(prefix); err != nil {
		return nil, err
	}
	if err := validateKey(cursor); err != nil {
		return nil, err
	}
	limit, err = validateScanLimit(limit)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	frame := buildScanFrame(prefix, limit, cursor)
	resp, err := c.call(ctx, frame)
	if err != nil {
		return nil, err
	}
	if len(resp) < 1 {
		return nil, ErrServerError
	}
	if resp[0] != respOk {
		return nil, fmt.Errorf("%w: SCAN returned status 0x%02x", ErrServerError, resp[0])
	}
	return parseScanResponse(resp[1:])
}

// MGet retrieves multiple values by key. Returns a slice where each element
// is the value (or nil if the key was not found). len(keys) must be <= 256.
func (c *Client) MGet(ctx context.Context, keys []string) ([][]byte, error) {
	if len(keys) > maxMgetKeys {
		return nil, fmt.Errorf("MGET: too many keys (%d, max %d)", len(keys), maxMgetKeys)
	}
	for _, k := range keys {
		if err := validateKey(k); err != nil {
			return nil, err
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	frame := buildMGetFrame(keys)
	resp, err := c.call(ctx, frame)
	if err != nil {
		return nil, err
	}
	if len(resp) < 1 {
		return nil, ErrServerError
	}
	if resp[0] != respOk {
		return nil, fmt.Errorf("%w: MGET returned status 0x%02x", ErrServerError, resp[0])
	}
	return parseMGetResponse(resp[1:], len(keys))
}

// Save triggers a synchronous snapshot save. Returns nil on success.
func (c *Client) Save(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	resp, err := c.call(ctx, []byte{opSave})
	if err != nil {
		return err
	}
	if len(resp) != 1 || resp[0] != respOk {
		return fmt.Errorf("%w: SAVE returned status 0x%02x (len %d)", ErrServerError, statusByte(resp), len(resp))
	}
	return nil
}

// ─── Frame builders ───────────────────────────────────────────────────

func buildSetFrame(key, value string) []byte {
	kl := len(key)
	vl := len(value)
	frame := make([]byte, 0, 1+2+kl+4+vl)
	frame = append(frame, opSet)
	var u16 [2]byte
	var u32 [4]byte
	binary.BigEndian.PutUint16(u16[:], uint16(kl))
	frame = append(frame, u16[:]...)
	frame = append(frame, key...)
	binary.BigEndian.PutUint32(u32[:], uint32(vl))
	frame = append(frame, u32[:]...)
	frame = append(frame, value...)
	return frame
}

func buildSetxFrame(key, value string, ttlMs uint64) []byte {
	kl := len(key)
	vl := len(value)
	frame := make([]byte, 0, 1+2+kl+4+vl+8)
	frame = append(frame, opSetx)
	var u16 [2]byte
	var u32 [4]byte
	var u64 [8]byte
	binary.BigEndian.PutUint16(u16[:], uint16(kl))
	frame = append(frame, u16[:]...)
	frame = append(frame, key...)
	binary.BigEndian.PutUint32(u32[:], uint32(vl))
	frame = append(frame, u32[:]...)
	frame = append(frame, value...)
	binary.BigEndian.PutUint64(u64[:], ttlMs)
	frame = append(frame, u64[:]...)
	return frame
}

func buildKeyFrame(opcode uint8, key string) []byte {
	kl := len(key)
	frame := make([]byte, 0, 1+2+kl)
	frame = append(frame, opcode)
	var u16 [2]byte
	binary.BigEndian.PutUint16(u16[:], uint16(kl))
	frame = append(frame, u16[:]...)
	frame = append(frame, key...)
	return frame
}

func buildScanFrame(prefix string, limit int, cursor string) []byte {
	pl := len(prefix)
	cl := len(cursor)
	frame := make([]byte, 0, 1+2+pl+2+2+cl)
	frame = append(frame, opScan)
	var u16 [2]byte
	binary.BigEndian.PutUint16(u16[:], uint16(pl))
	frame = append(frame, u16[:]...)
	frame = append(frame, prefix...)
	binary.BigEndian.PutUint16(u16[:], uint16(limit))
	frame = append(frame, u16[:]...)
	binary.BigEndian.PutUint16(u16[:], uint16(cl))
	frame = append(frame, u16[:]...)
	frame = append(frame, cursor...)
	return frame
}

func buildMGetFrame(keys []string) []byte {
	count := len(keys)
	frame := make([]byte, 0, 1+2)
	frame = append(frame, opMget)
	var u16 [2]byte
	binary.BigEndian.PutUint16(u16[:], uint16(count))
	frame = append(frame, u16[:]...)
	for _, key := range keys {
		kl := len(key)
		binary.BigEndian.PutUint16(u16[:], uint16(kl))
		frame = append(frame, u16[:]...)
		frame = append(frame, key...)
	}
	return frame
}

// ─── Response parsers ─────────────────────────────────────────────────

func parseScanResponse(body []byte) (*ScanResult, error) {
	if len(body) < 3 { // 2 (count) + 1 (more flag) minimum
		return nil, fmt.Errorf("%w: SCAN response too short", ErrServerError)
	}
	count := binary.BigEndian.Uint16(body[0:2])
	pos := 2
	keys := make([]string, 0, count)
	for i := 0; i < int(count); i++ {
		if pos+2 > len(body) {
			return nil, fmt.Errorf("%w: SCAN key length truncated at %d", ErrServerError, i)
		}
		kl := binary.BigEndian.Uint16(body[pos : pos+2])
		pos += 2
		if pos+int(kl) > len(body) {
			return nil, fmt.Errorf("%w: SCAN key %d truncated", ErrServerError, i)
		}
		keys = append(keys, string(body[pos:pos+int(kl)]))
		pos += int(kl)
	}
	if pos >= len(body) {
		return nil, fmt.Errorf("%w: SCAN missing more-flag", ErrServerError)
	}
	moreByte := body[pos]
	if moreByte != 0x00 && moreByte != 0x01 {
		return nil, fmt.Errorf("%w: SCAN invalid more-flag 0x%02x", ErrServerError, moreByte)
	}
	more := moreByte == 0x01
	pos++
	// Strict: no trailing bytes allowed.
	if pos != len(body) {
		return nil, fmt.Errorf("%w: SCAN has trailing bytes", ErrServerError)
	}
	// The cursor for the next page is the last key returned.
	var cursor string
	if len(keys) > 0 {
		cursor = keys[len(keys)-1]
	}
	return &ScanResult{Keys: keys, More: more, Cursor: cursor}, nil
}

func parseMGetResponse(body []byte, count int) ([][]byte, error) {
	pos := 0
	values := make([][]byte, count)
	for i := 0; i < count; i++ {
		if pos+1 > len(body) {
			return nil, fmt.Errorf("%w: MGET flag %d truncated", ErrServerError, i)
		}
		flag := body[pos]
		pos++
		if flag == 0x00 {
			values[i] = nil
			continue
		}
		if flag != 0x01 {
			return nil, fmt.Errorf("%w: MGET unknown flag 0x%02x at %d", ErrServerError, flag, i)
		}
		if pos+4 > len(body) {
			return nil, fmt.Errorf("%w: MGET val-len %d truncated", ErrServerError, i)
		}
		vl := binary.BigEndian.Uint32(body[pos : pos+4])
		pos += 4
		if pos+int(vl) > len(body) {
			return nil, fmt.Errorf("%w: MGET value %d truncated", ErrServerError, i)
		}
		val := make([]byte, vl)
		copy(val, body[pos:pos+int(vl)])
		values[i] = val
		pos += int(vl)
	}
	// Strict: no trailing bytes allowed.
	if pos != len(body) {
		return nil, fmt.Errorf("%w: MGET has trailing bytes", ErrServerError)
	}
	return values, nil
}

// statusToError maps a single-byte response to a typed error.
func statusToError(resp []byte) error {
	if len(resp) < 1 {
		return ErrServerError
	}
	if len(resp) != 1 {
		return fmt.Errorf("%w: response has trailing bytes", ErrServerError)
	}
	switch resp[0] {
	case respOk:
		return nil
	case respStoreFull:
		return ErrStoreFull
	default:
		return fmt.Errorf("%w: status 0x%02x", ErrServerError, resp[0])
	}
}

// statusByte extracts the first byte or returns respError.
func statusByte(resp []byte) byte {
	if len(resp) < 1 {
		return respError
	}
	return resp[0]
}

// MaxKeyLen returns the maximum key length in bytes.
func MaxKeyLen() int { return maxKeyLen }

// MaxMGetKeys returns the maximum number of keys per MGET.
func MaxMGetKeys() int { return maxMgetKeys }

// MaxFrameSize returns the maximum frame size in bytes.
func MaxFrameSize() int { return maxFrameSize }

// IsServerUnavailable reports whether err indicates the kvr server is not
// reachable (connection refused, no such file, etc.). Used by the tool
// layer to report "staging unavailable".
func IsServerUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		// Connection refused, no such file or directory, etc.
		s := opErr.Err.Error()
		if strings.Contains(s, "connection refused") ||
			strings.Contains(s, "no such file") ||
			strings.Contains(s, "connect: connection") {
			return true
		}
	}
	return false
}
