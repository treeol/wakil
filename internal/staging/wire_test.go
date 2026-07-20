package staging

import (
	"encoding/binary"
	"errors"
	"io"
	"testing"
	"time"
)

// wire_test.go — golden byte-vector tests for the client's frame builders
// and response parsers. These tests verify the wire format INDEPENDENTLY of
// the fake server — they assert exact byte sequences, preventing tautological
// tests where both sides share the same constant and a bug goes undetected.
//
// The expected bytes below are hand-derived from the protocol spec, NOT from
// reading the client's builder code. If the client changes its wire format,
// these tests must fail.

// ─── Frame builder golden tests ───────────────────────────────────────

func TestBuildSetFrame(t *testing.T) {
	// SET "key" = "val"
	// Expected: op(0x00) + keyLen(0x0003) + "key" + valLen(0x00000003) + "val"
	// Literal bytes (not client constants) to prevent tautology.
	frame := buildSetFrame("key", "val")
	want := []byte{
		0x00,                                // opSet
		0x00, 0x03,                           // keyLen = 3
		'k', 'e', 'y',                        // "key"
		0x00, 0x00, 0x00, 0x03,               // valLen = 3
		'v', 'a', 'l',                        // "val"
	}
	if !bytesEqual(frame, want) {
		t.Fatalf("buildSetFrame: got % x, want % x", frame, want)
	}
}

func TestBuildSetFrameEmptyValue(t *testing.T) {
	// SET "k" = ""
	frame := buildSetFrame("k", "")
	want := []byte{
		0x00,                                // opSet
		0x00, 0x01, 'k',
		0x00, 0x00, 0x00, 0x00, // valLen = 0
	}
	if !bytesEqual(frame, want) {
		t.Fatalf("buildSetFrame empty: got % x, want % x", frame, want)
	}
}

func TestBuildSetxFrame(t *testing.T) {
	// SETX "key" = "val" ttl=1000ms
	// Expected: op(0x05) + keyLen(2) + key + valLen(4) + val + ttlMs(8)
	frame := buildSetxFrame("key", "val", 1000)
	want := []byte{
		0x05,                                // opSetx
		0x00, 0x03, 'k', 'e', 'y',             // key
		0x00, 0x00, 0x00, 0x03, 'v', 'a', 'l', // value
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03, 0xE8, // ttlMs = 1000
	}
	if !bytesEqual(frame, want) {
		t.Fatalf("buildSetxFrame: got % x, want % x", frame, want)
	}
}

func TestBuildKeyFrameGet(t *testing.T) {
	// GET "abc"
	// Expected: op(0x01) + keyLen(0x0003) + "abc"
	frame := buildKeyFrame(opGet, "abc")
	want := []byte{
		0x01,               // opGet
		0x00, 0x03,          // keyLen = 3
		'a', 'b', 'c',       // "abc"
	}
	if !bytesEqual(frame, want) {
		t.Fatalf("buildKeyFrame GET: got % x, want % x", frame, want)
	}
}

func TestBuildKeyFrameDel(t *testing.T) {
	// DEL "x"
	frame := buildKeyFrame(opDel, "x")
	want := []byte{0x02, 0x00, 0x01, 'x'} // opDel=0x02
	if !bytesEqual(frame, want) {
		t.Fatalf("buildKeyFrame DEL: got % x, want % x", frame, want)
	}
}

func TestBuildKeyFrameExists(t *testing.T) {
	// EXISTS "y"
	frame := buildKeyFrame(opExists, "y")
	want := []byte{0x04, 0x00, 0x01, 'y'} // opExists=0x04
	if !bytesEqual(frame, want) {
		t.Fatalf("buildKeyFrame EXISTS: got % x, want % x", frame, want)
	}
}

func TestBuildScanFrame(t *testing.T) {
	// SCAN prefix="p/" limit=5 cursor=""
	// Layout: op(0x06) + prefixLen(2) + prefix + limit(2) + cursorLen(2) + cursor
	frame := buildScanFrame("p/", 5, "")
	want := []byte{
		0x06,                          // opScan
		0x00, 0x02, 'p', '/',            // prefix
		0x00, 0x05,                      // limit = 5
		0x00, 0x00,                      // cursorLen = 0
	}
	if !bytesEqual(frame, want) {
		t.Fatalf("buildScanFrame: got % x, want % x", frame, want)
	}
}

func TestBuildScanFrameWithCursor(t *testing.T) {
	frame := buildScanFrame("p/", 5, "page/3")
	want := []byte{
		0x06, // opScan
		0x00, 0x02, 'p', '/',
		0x00, 0x05,
		0x00, 0x06, 'p', 'a', 'g', 'e', '/', '3',
	}
	if !bytesEqual(frame, want) {
		t.Fatalf("buildScanFrame cursor: got % x, want % x", frame, want)
	}
}

func TestBuildMGetFrame(t *testing.T) {
	// MGET ["a", "bb"]
	// Layout: op(0x07) + count(2) + [keyLen(2) + key]...
	frame := buildMGetFrame([]string{"a", "bb"})
	want := []byte{
		0x07,                  // opMget
		0x00, 0x02,              // count = 2
		0x00, 0x01, 'a',         // key "a"
		0x00, 0x02, 'b', 'b',    // key "bb"
	}
	if !bytesEqual(frame, want) {
		t.Fatalf("buildMGetFrame: got % x, want % x", frame, want)
	}
}

func TestBuildMGetFrameEmpty(t *testing.T) {
	frame := buildMGetFrame([]string{})
	want := []byte{0x07, 0x00, 0x00} // opMget + count=0
	if !bytesEqual(frame, want) {
		t.Fatalf("buildMGetFrame empty: got % x, want % x", frame, want)
	}
}

// ─── Response parser golden tests ─────────────────────────────────────

func TestParseScanResponseGolden(t *testing.T) {
	// OK + count(2) + keys + moreFlag(1)
	// Keys: "a", "b", more=false
	body := []byte{
		0x00, 0x02,                   // count = 2
		0x00, 0x01, 'a',              // key "a"
		0x00, 0x01, 'b',              // key "b"
		0x00,                         // more = false
	}
	result, err := parseScanResponse(body)
	if err != nil {
		t.Fatalf("parseScanResponse: %v", err)
	}
	if len(result.Keys) != 2 || result.Keys[0] != "a" || result.Keys[1] != "b" {
		t.Fatalf("keys = %v", result.Keys)
	}
	if result.More {
		t.Fatal("expected More=false")
	}
	if result.Cursor != "b" {
		t.Fatalf("cursor = %q, want %q", result.Cursor, "b")
	}
}

func TestParseScanResponseTruncated(t *testing.T) {
	// count=2 but only 1 key present
	body := []byte{
		0x00, 0x02,        // count = 2
		0x00, 0x01, 'a',   // only 1 key
		0x00,              // missing: key 2 + more flag
	}
	_, err := parseScanResponse(body)
	if err == nil {
		t.Fatal("expected error for truncated SCAN response")
	}
	if !errors.Is(err, ErrServerError) {
		t.Fatalf("expected ErrServerError, got %v", err)
	}
}

func TestParseScanResponseInvalidMoreFlag(t *testing.T) {
	body := []byte{
		0x00, 0x00, // count = 0
		0x02,       // invalid more flag
	}
	_, err := parseScanResponse(body)
	if err == nil {
		t.Fatal("expected error for invalid more flag")
	}
	if !errors.Is(err, ErrServerError) {
		t.Fatalf("expected ErrServerError, got %v", err)
	}
}

func TestParseScanResponseTrailingBytes(t *testing.T) {
	body := []byte{
		0x00, 0x01, 'a', // count=1, key="a"
		0x00,             // more=false
		0xFF,             // trailing byte
	}
	_, err := parseScanResponse(body)
	if err == nil {
		t.Fatal("expected error for trailing bytes in SCAN")
	}
}

func TestParseMGetResponseGolden(t *testing.T) {
	// 2 values: "hi" found, nil not found
	body := []byte{
		0x01,                   // found
		0x00, 0x00, 0x00, 0x02, // valLen = 2
		'h', 'i',
		0x00, // not found
	}
	values, err := parseMGetResponse(body, 2)
	if err != nil {
		t.Fatalf("parseMGetResponse: %v", err)
	}
	if len(values) != 2 {
		t.Fatalf("got %d values, want 2", len(values))
	}
	if string(values[0]) != "hi" {
		t.Fatalf("values[0] = %q, want %q", string(values[0]), "hi")
	}
	if values[1] != nil {
		t.Fatalf("values[1] = %v, want nil", values[1])
	}
}

func TestParseMGetResponseUnknownFlag(t *testing.T) {
	body := []byte{0x03} // unknown flag 0x03
	_, err := parseMGetResponse(body, 1)
	if err == nil {
		t.Fatal("expected error for unknown MGET flag")
	}
	if !errors.Is(err, ErrServerError) {
		t.Fatalf("expected ErrServerError, got %v", err)
	}
}

func TestParseMGetResponseTrailingBytes(t *testing.T) {
	body := []byte{
		0x00, // not found (1 key)
		0xFF, // trailing
	}
	_, err := parseMGetResponse(body, 1)
	if err == nil {
		t.Fatal("expected error for trailing bytes in MGET")
	}
}

// ─── readFrame/writeFrame golden tests ─────────────────────────────────

func TestReadFrameOversized(t *testing.T) {
	// Frame with length > maxFrameSize (1 MiB)
	hdr := make([]byte, 4)
	binary.BigEndian.PutUint32(hdr, maxFrameSize+1)
	_, err := readFrame(&bytesReader{data: hdr})
	if err == nil {
		t.Fatal("expected error for oversized frame")
	}
}

func TestReadFrameEmpty(t *testing.T) {
	// Zero-length frame → empty byte slice, no error
	hdr := make([]byte, 4) // all zeros = length 0
	resp, err := readFrame(&bytesReader{data: hdr})
	if err != nil {
		t.Fatalf("readFrame empty: %v", err)
	}
	if len(resp) != 0 {
		t.Fatalf("expected empty frame, got %d bytes", len(resp))
	}
}

func TestReadFrameTruncatedHeader(t *testing.T) {
	// Only 2 bytes of the 4-byte header
	_, err := readFrame(&bytesReader{data: []byte{0x00, 0x00}})
	if err == nil {
		t.Fatal("expected error for truncated header")
	}
}

func TestReadFrameTruncatedPayload(t *testing.T) {
	// Header says 10 bytes, but only 3 available
	hdr := make([]byte, 4)
	binary.BigEndian.PutUint32(hdr, 10)
	data := append(hdr, 0x01, 0x02, 0x03)
	_, err := readFrame(&bytesReader{data: data})
	if err == nil {
		t.Fatal("expected error for truncated payload")
	}
}

func TestWriteFrameGolden(t *testing.T) {
	w := &bytesWriter{}
	payload := []byte{0x01, 0x02, 0x03}
	if err := writeFrame(w, payload); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
	// Expected: len(4B BE) + payload
	want := []byte{0x00, 0x00, 0x00, 0x03, 0x01, 0x02, 0x03}
	if !bytesEqual(w.buf, want) {
		t.Fatalf("writeFrame: got % x, want % x", w.buf, want)
	}
}

func TestWriteFrameEmpty(t *testing.T) {
	w := &bytesWriter{}
	if err := writeFrame(w, []byte{}); err != nil {
		t.Fatalf("writeFrame empty: %v", err)
	}
	want := []byte{0x00, 0x00, 0x00, 0x00}
	if !bytesEqual(w.buf, want) {
		t.Fatalf("writeFrame empty: got % x, want % x", w.buf, want)
	}
}

// ─── Validation golden tests ──────────────────────────────────────────

func TestValidateKeyBoundary(t *testing.T) {
	// Exactly maxKeyLen bytes → OK
	key := string(make([]byte, maxKeyLen)) // all zeros, valid bytes
	if err := validateKey(key); err != nil {
		t.Fatalf("validateKey at boundary: %v", err)
	}
	// One byte over → error
	key = string(make([]byte, maxKeyLen+1))
	if err := validateKey(key); err == nil {
		t.Fatal("expected error for oversized key")
	}
}

func TestValidateSetValueBoundary(t *testing.T) {
	// validateSetValue includes 8 bytes of SETX overhead even for plain SET.
	// maxFrameSize = 1MiB. overhead = 1 + 2 + keyLen + 4 + 8 = 15 + keyLen.
	// For key "k" (1 byte): overhead = 16. Max value = maxFrameSize - 16.
	overhead := 1 + 2 + 1 + 4 + 8 // 16
	maxVal := maxFrameSize - overhead
	if err := validateSetValue("k", string(make([]byte, maxVal))); err != nil {
		t.Fatalf("validateSetValue at boundary: %v", err)
	}
	if err := validateSetValue("k", string(make([]byte, maxVal+1))); err == nil {
		t.Fatal("expected error for value exceeding frame size")
	}
}

func TestValidateScanLimitBoundary(t *testing.T) {
	// Negative → error
	if _, err := validateScanLimit(-1); err == nil {
		t.Fatal("expected error for negative limit")
	}
	// 0 → OK (returns 0)
	limit, err := validateScanLimit(0)
	if err != nil || limit != 0 {
		t.Fatalf("validateScanLimit(0): limit=%d err=%v", limit, err)
	}
	// > maxKeyLen → capped to maxKeyLen
	limit, err = validateScanLimit(maxKeyLen + 1)
	if err != nil || limit != maxKeyLen {
		t.Fatalf("validateScanLimit(>max): limit=%d err=%v", limit, err)
	}
	// Exactly maxKeyLen → unchanged
	limit, err = validateScanLimit(maxKeyLen)
	if err != nil || limit != maxKeyLen {
		t.Fatalf("validateScanLimit(maxKeyLen): limit=%d err=%v", limit, err)
	}
}

func TestStatusToErrorGolden(t *testing.T) {
	// Literal status bytes (not client constants) to prevent tautology.
	if err := statusToError([]byte{0x10}); err != nil { // respOk
		t.Fatalf("respOk: %v", err)
	}
	if !errors.Is(statusToError([]byte{0x13}), ErrStoreFull) { // respStoreFull
		t.Fatal("expected ErrStoreFull")
	}
	if !errors.Is(statusToError([]byte{0xFF}), ErrServerError) { // respError
		t.Fatal("expected ErrServerError for respError")
	}
	// Empty response
	if !errors.Is(statusToError([]byte{}), ErrServerError) {
		t.Fatal("expected ErrServerError for empty response")
	}
	// Trailing bytes
	if !errors.Is(statusToError([]byte{0x10, 0xFF}), ErrServerError) { // respOk + trailing
		t.Fatal("expected ErrServerError for trailing bytes")
	}
	// Unknown status
	if !errors.Is(statusToError([]byte{0x99}), ErrServerError) {
		t.Fatal("expected ErrServerError for unknown status")
	}
}

func TestIsBrokenPipeGolden(t *testing.T) {
	if isBrokenPipe(nil) {
		t.Fatal("nil should not be broken pipe")
	}
	if !isBrokenPipe(io.EOF) {
		t.Fatal("EOF should be broken pipe")
	}
	if !isBrokenPipe(io.ErrUnexpectedEOF) {
		t.Fatal("ErrUnexpectedEOF should be broken pipe")
	}
	if !isBrokenPipe(io.ErrClosedPipe) {
		t.Fatal("ErrClosedPipe should be broken pipe")
	}
}

// ─── IsServerUnavailable tests (no server needed) ─────────────────────

func TestIsServerUnavailableMissingSocket(t *testing.T) {
	// A client pointing at a nonexistent socket should report unavailable.
	c := NewClient("/nonexistent/path/that/does/not/exist.sock")
	c.SetTimeout(500 * time.Millisecond)
	err := c.Ping(t.Context())
	if err == nil {
		t.Fatal("expected error pinging nonexistent socket")
	}
	if !IsServerUnavailable(err) {
		t.Fatalf("expected IsServerUnavailable=true, got err: %v", err)
	}
}

// ─── Test helpers ─────────────────────────────────────────────────────

// bytesReader is a minimal io.Reader for testing readFrame without a socket.
type bytesReader struct {
	data []byte
	pos  int
}

func (r *bytesReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

// bytesWriter is a minimal io.Writer for testing writeFrame.
type bytesWriter struct {
	buf []byte
}

func (w *bytesWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	return len(p), nil
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
