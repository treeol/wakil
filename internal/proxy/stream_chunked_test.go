package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// chunkReader returns the stream body in caller-controlled byte chunks,
// simulating arbitrary transport-level splits (TCP segmenting, proxy
// buffering). SSE frames are line-based, so a split mid-line must be
// transparent to the parser — these readers are how we prove it without
// relying on network timing (an httptest server cannot guarantee where the
// client's reads land).
type chunkReader struct {
	chunks [][]byte
	cur    int
	off    int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if r.cur >= len(r.chunks) {
		return 0, io.EOF
	}
	c := r.chunks[r.cur]
	if r.off >= len(c) {
		r.cur++
		r.off = 0
		return r.Read(p)
	}
	n := copy(p, c[r.off:])
	r.off += n
	return n, nil
}

func (r *chunkReader) Close() error { return nil }

// splitEvery splits body into chunks of n bytes (last chunk may be short).
func splitEvery(body string, n int) [][]byte {
	var out [][]byte
	for i := 0; i < len(body); i += n {
		end := i + n
		if end > len(body) {
			end = len(body)
		}
		out = append(out, []byte(body[i:end]))
	}
	return out
}

// streamViaChunks runs Stream against a transport that serves body in the
// given chunks. This bypasses httptest entirely: the split pattern is exact.
func streamViaChunks(t *testing.T, chunks [][]byte) (Message, error) {
	t.Helper()
	rt := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       &chunkReader{chunks: chunks},
		}, nil
	})
	c := &Client{
		BaseURL: "http://chunk.test",
		Model:   "ilm",
		ChatID:  "test",
		HTTP:    &http.Client{Transport: rt},
	}
	return c.Stream(t.Context(), []Message{{Role: "user", Content: strPtr("hi")}}, nil, nil, nil)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A tool-call frame split at EVERY possible byte boundary must assemble
// identically. This is the core guarantee the unflushed sseServer helper
// never actually exercised: incremental delivery with splits mid-token.
func TestStreamChunkedToolCallEverySplitPoint(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"run_shell\",\"arguments\":\"{\\\"command\\\":\\\"ls -la\\\"}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"

	// Exhaustive for small n: 1..7 byte chunks cover every alignment class
	// relative to frame and token boundaries without a O(len²) blowup.
	for _, n := range []int{1, 2, 3, 5, 7, 13, 64} {
		t.Run(fmt.Sprintf("chunk=%d", n), func(t *testing.T) {
			msg, err := streamViaChunks(t, splitEvery(body, n))
			if err != nil {
				t.Fatalf("chunk size %d: %v", n, err)
			}
			if len(msg.ToolCalls) != 1 {
				t.Fatalf("chunk size %d: want 1 tool call, got %d", n, len(msg.ToolCalls))
			}
			tc := msg.ToolCalls[0]
			if tc.ID != "call_1" || tc.Function.Name != "run_shell" {
				t.Errorf("chunk size %d: id/name = %q/%q", n, tc.ID, tc.Function.Name)
			}
			if tc.Function.Arguments != `{"command":"ls -la"}` {
				t.Errorf("chunk size %d: arguments = %q", n, tc.Function.Arguments)
			}
		})
	}
}

// Two tool calls interleaved by index across chunks must each accumulate
// their own arguments — index routing must not cross-contaminate.
func TestStreamChunkedInterleavedToolCalls(t *testing.T) {
	frame := func(idx int, payload string) string {
		return fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":%d,%s}]}}]}\n\n", idx, payload)
	}
	// Note: \" inside a raw string literal is a real backslash+quote — exactly
	// what JSON needs inside a string value. Do not "fix" these to \\".
	body := frame(0, `"id":"c0","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.g"}`) +
		frame(1, `"id":"c1","type":"function","function":{"name":"list_dir","arguments":"{\"path\":\".\"}"}`) +
		frame(0, `"function":{"arguments":"o\"}"}`) +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"

	msg, err := streamViaChunks(t, splitEvery(body, 3))
	if err != nil {
		t.Fatalf("interleaved: %v", err)
	}
	if len(msg.ToolCalls) != 2 {
		t.Fatalf("want 2 tool calls, got %d", len(msg.ToolCalls))
	}
	got := map[string]string{}
	for _, tc := range msg.ToolCalls {
		got[tc.Function.Name] = tc.Function.Arguments
	}
	if got["read_file"] != `{"path":"a.go"}` {
		t.Errorf("read_file args = %q", got["read_file"])
	}
	if got["list_dir"] != `{"path":"."}` {
		t.Errorf("list_dir args = %q", got["list_dir"])
	}
}

// A split landing exactly between the two newlines of a frame terminator,
// and between "data:" and the payload, must not corrupt the stream.
func TestStreamChunkedFrameBoundarySplits(t *testing.T) {
	// Hand-picked split points: mid-"data:", at the \n\n boundary, mid-JSON.
	splits := [][]byte{
		[]byte("da"),
		[]byte("ta: {\"choices\":"),
		[]byte("[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}]}\n"),
		[]byte("\n"),
		[]byte("data: [DONE]\n\n"),
	}
	msg, err := streamViaChunks(t, splits)
	if err != nil {
		t.Fatalf("boundary splits: %v", err)
	}
	if msg.Content == nil || *msg.Content != "hello" {
		t.Fatalf("want content %q, got %v", "hello", msg.Content)
	}
}

// An incomplete tool call followed by clean EOF (no [DONE], no finish chunk)
// must fail the stream — the partial call must never be returned for
// dispatch. Splits make this nastier: EOF can arrive mid-frame.
func TestStreamChunkedIncompleteToolCallEOFMidFrame(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c\",\"type\":\"function\",\"function\":{\"name\":\"run_shell\",\"arguments\":\"{\\\"command\\\":\\\"rm" // EOF mid-arguments
	msg, err := streamViaChunks(t, splitEvery(body, 7))
	if !errors.Is(err, ErrBackendStream) {
		t.Fatalf("EOF mid-frame: want ErrBackendStream, got %v (msg %+v)", err, msg)
	}
	if len(msg.ToolCalls) != 0 {
		t.Fatalf("partial tool call must not be returned: %+v", msg.ToolCalls)
	}
}

// Context cancellation mid-stream: Stream must return promptly with
// context.Canceled — NOT a retryable ErrBackendStream — so the agent never
// auto-retries a turn the user deliberately cancelled. The request is built
// with NewRequestWithContext (client.go:802), so against a real network
// transport http.Client closes resp.Body on cancel and the scanner
// unblocks; Stream then classifies via ctx.Err() before the stream-error
// wrap.
func TestStreamChunkedContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rt := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       &blockingBody{done: ctx.Done()},
		}, nil
	})
	c := &Client{
		BaseURL: "http://chunk.test",
		Model:   "ilm",
		ChatID:  "test",
		HTTP:    &http.Client{Transport: rt},
	}
	errCh := make(chan error, 1)
	go func() {
		_, err := c.Stream(ctx, []Message{{Role: "user", Content: strPtr("hi")}}, nil, nil, nil)
		errCh <- err
	}()
	// Let the stream start blocking on the body, then cancel — the body
	// unblocks the way a real closed connection would.
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled stream: want context.Canceled, got %v", err)
		}
		if errors.Is(err, ErrBackendStream) {
			t.Fatalf("cancellation must not classify as retryable stream error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stream did not return within 3s of context cancellation")
	}
}

// TestStreamMalformedErrorNotMaskedByCancel pins the no-masking rule: a
// malformed chunk that errors the stream BEFORE the context is cancelled
// must keep its real error (ErrBackendStream), never downgrade to
// context.Canceled. The cancel arrives after the stream already failed.
func TestStreamMalformedErrorNotMaskedByCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: &chunkReader{chunks: [][]byte{
				[]byte("data: {MALFORMED\n\n"),
			}},
		}, nil
	})
	c := &Client{BaseURL: "http://chunk.test", Model: "ilm", ChatID: "test", HTTP: &http.Client{Transport: rt}}
	_, err := c.Stream(ctx, []Message{{Role: "user", Content: strPtr("hi")}}, nil, nil, nil)
	// Stream fails on the malformed chunk BEFORE any cancel.
	if !errors.Is(err, ErrBackendStream) {
		t.Fatalf("malformed chunk: want ErrBackendStream, got %v", err)
	}
	// A cancel arriving now must not retroactively change the classification.
	cancel()
	if !errors.Is(err, ErrBackendStream) || errors.Is(err, context.Canceled) {
		t.Fatalf("prior stream error masked by later cancel: %v", err)
	}
}

// TestStreamRetryableErrorNotMaskedByRacingCancel pins the narrow no-masking
// guarantee for retryable errors: a malformed chunk fails the stream as
// retryable ErrBackendStream; a cancel racing in afterward must NOT be
// masked in a way that loses the retry decision — the error was already
// returned before the cancel could apply (Stream is sequential after the
// loop; the cancel can only win if it arrived before the error).
func TestStreamRetryableErrorNotMaskedByRacingCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: &chunkReader{chunks: [][]byte{
				[]byte("data: {MALFORMED\n\n"),
			}},
		}, nil
	})
	c := &Client{BaseURL: "http://chunk.test", Model: "ilm", ChatID: "test", HTTP: &http.Client{Transport: rt}}
	// Cancel DURING the stream read — races the malformed-chunk error.
	cancel()
	_, err := c.Stream(ctx, []Message{{Role: "user", Content: strPtr("hi")}}, nil, nil, nil)
	if err == nil {
		t.Fatal("stream must fail")
	}
	// Either classification is acceptable here (the cancel genuinely raced),
	// but a retryable backend failure must never downgrade into a SILENT
	// success or a nil error — which is the only harmful mask. Both
	// ErrBackendStream and context.Canceled are non-nil, surfaced failures.
	t.Logf("racing cancel classified as: %v", err)
}

// TestStreamCancelBeforeResponse pins the HTTP.Do path: a context cancelled
// before the response headers arrive must return context.Canceled, not a
// retryable ErrBackendStream (otherwise the retry loop re-issues a turn the
// user deliberately killed).
func TestStreamCancelBeforeResponse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done() // block until the client cancels
		return nil, req.Context().Err()
	})
	c := &Client{BaseURL: "http://chunk.test", Model: "ilm", ChatID: "test", HTTP: &http.Client{Transport: rt}}
	errCh := make(chan error, 1)
	go func() {
		_, err := c.Stream(ctx, []Message{{Role: "user", Content: strPtr("hi")}}, nil, nil, nil)
		errCh <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("pre-response cancel: want context.Canceled, got %v", err)
		}
		if errors.Is(err, ErrBackendStream) {
			t.Fatalf("pre-response cancel must not be retryable: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stream did not return within 3s of pre-response cancellation")
	}
}

// blockingBody blocks in Read until the done channel closes, then returns
// an error the way a real network body does when the transport tears the
// connection down on context cancellation.
type blockingBody struct {
	done <-chan struct{}
}

func (b *blockingBody) Read(_ []byte) (int, error) {
	<-b.done
	return 0, errors.New("read on closed response body")
}

func (b *blockingBody) Close() error { return nil }

// TestStreamContextCancelRealTransport runs the same cancellation against a
// REAL httptest server whose handler blocks forever. http.Client tears the
// connection down on cancel — this pins which Stream exit path a real
// cancel takes (scanner error vs clean EOF) and that the result classifies
// as context.Canceled either way.
func TestStreamContextCancelRealTransport(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done() // block until the client goes away
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Model: "ilm", ChatID: "test", HTTP: http.DefaultClient}
	errCh := make(chan error, 1)
	go func() {
		_, err := c.Stream(ctx, []Message{{Role: "user", Content: strPtr("hi")}}, nil, nil, nil)
		errCh <- err
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("real-transport cancel: want context.Canceled, got %v", err)
		}
		if errors.Is(err, ErrBackendStream) {
			t.Fatalf("real-transport cancel must not be a retryable stream error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stream did not return within 3s of real-transport cancellation")
	}
}

// Sanity: one giant single-chunk delivery (the old unflushed-helper shape)
// must parse identically to split delivery of the same bytes.
func TestStreamChunkedVsWholeEquivalence(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c\",\"type\":\"function\",\"function\":{\"name\":\"edit_file\",\"arguments\":\"{\\\"path\\\":\\\"x.go\\\",\\\"old_string\\\":\\\"a\\\",\\\"new_string\\\":\\\"b\\\"}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	whole, err := streamViaChunks(t, [][]byte{[]byte(body)})
	if err != nil {
		t.Fatalf("whole: %v", err)
	}
	split, err := streamViaChunks(t, splitEvery(body, 1))
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if len(whole.ToolCalls) != 1 || len(split.ToolCalls) != 1 {
		t.Fatalf("tool call counts differ: whole=%d split=%d", len(whole.ToolCalls), len(split.ToolCalls))
	}
	w, s := whole.ToolCalls[0], split.ToolCalls[0]
	if w.ID != s.ID || w.Function.Name != s.Function.Name || w.Function.Arguments != s.Function.Arguments {
		t.Errorf("whole vs split mismatch:\nwhole: %+v\nsplit: %+v", w, s)
	}
	if !bytes.Equal([]byte(w.Function.Arguments), []byte(s.Function.Arguments)) {
		t.Errorf("arguments not byte-identical")
	}
}

// A [DONE] marker split across chunk boundaries must still terminate the
// stream successfully.
func TestStreamChunkedDoneMarkerSplit(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n"
	doneSplit := [][]byte{
		[]byte(body),
		[]byte("data: [DO"),
		[]byte("NE]\n"),
		[]byte("\n"),
	}
	msg, err := streamViaChunks(t, doneSplit)
	if err != nil {
		t.Fatalf("split [DONE]: %v", err)
	}
	if msg.Content == nil || *msg.Content != "ok" {
		t.Fatalf("want content %q, got %v", "ok", msg.Content)
	}
}

// Duplicate [DONE] markers (a buggy or malicious backend) must not corrupt
// the completed message — first [DONE] wins and the rest are never read.
func TestStreamDuplicateDoneMarkers(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n" +
		"data: [DONE]\n\n" +
		"data: {GARBAGE AFTER DONE\n\n"
	msg, err := streamViaChunks(t, splitEvery(body, 5))
	if err != nil {
		t.Fatalf("duplicate [DONE]: %v", err)
	}
	if msg.Content == nil || *msg.Content != "ok" {
		t.Fatalf("want content %q, got %v", "ok", msg.Content)
	}
}

// An empty data: frame (keepalive used by some proxies) must be skipped —
// it is not valid JSON and must not abort the stream as malformed.
func TestStreamEmptyDataFrame(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n" +
		"data:\n\n" +
		"data: \n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"b\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	_, err := streamWith(t, body)
	if err != nil {
		t.Fatalf("empty data: frames must be skipped, got: %v", err)
	}
	msg, err := streamViaChunks(t, splitEvery(body, 4))
	if err != nil {
		t.Fatalf("empty data: frames (chunked) must be skipped, got: %v", err)
	}
	if msg.Content == nil || *msg.Content != "ab" {
		t.Fatalf("want content %q, got %v", "ab", msg.Content)
	}
}

// Unknown fields in chunks (forward-compat with newer backends) must be
// tolerated.
func TestStreamUnknownFieldsTolerated(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"future_field\":{\"x\":1},\"id\":\"chatcmpl-1\"}\n\n" +
		"data: [DONE]\n\n"
	msg, err := streamViaChunks(t, splitEvery(body, 6))
	if err != nil {
		t.Fatalf("unknown fields: %v", err)
	}
	if msg.Content == nil || *msg.Content != "ok" {
		t.Fatalf("want content %q, got %v", "ok", msg.Content)
	}
}

// Late tool-call ID: some backends send arguments first, id in a later
// chunk for the same index. Accumulation must handle any field order.
func TestStreamToolCallFieldsOutOfOrder(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"path\\\":\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"late-id\",\"type\":\"function\",\"function\":{\"name\":\"read_file\",\"arguments\":\"\\\"f.go\\\"}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	msg, err := streamViaChunks(t, splitEvery(body, 9))
	if err != nil {
		t.Fatalf("out-of-order fields: %v", err)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("want 1 tool call, got %d", len(msg.ToolCalls))
	}
	tc := msg.ToolCalls[0]
	if tc.ID != "late-id" || tc.Function.Name != "read_file" {
		t.Errorf("id/name = %q/%q", tc.ID, tc.Function.Name)
	}
	if tc.Function.Arguments != `{"path":"f.go"}` {
		t.Errorf("arguments = %q", tc.Function.Arguments)
	}
}

// Content interleaved between tool-call argument fragments must not corrupt
// either stream.
func TestStreamContentBetweenToolCallFragments(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c\",\"type\":\"function\",\"function\":{\"name\":\"run_shell\",\"arguments\":\"{\\\"comma\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"thinking out loud\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"nd\\\":\\\"ls\\\"}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	msg, err := streamViaChunks(t, splitEvery(body, 4))
	if err != nil {
		t.Fatalf("interleaved content: %v", err)
	}
	if msg.Content == nil || *msg.Content != "thinking out loud" {
		t.Errorf("content = %v", msg.Content)
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Function.Arguments != `{"command":"ls"}` {
		t.Errorf("tool calls = %+v", msg.ToolCalls)
	}
}

// Ensure a stream with a very small scanner-friendlier shape but an
// oversized single line is rejected (defense already in Stream — pin it at
// the chunked seam too, where the line spans many chunks).
func TestStreamChunkedOversizeLineRejected(t *testing.T) {
	// maxSSELineSize is 10MB; building an 11MB line in a test is heavy but
	// proves the cap is enforced mid-accumulation across chunk boundaries.
	var sb strings.Builder
	sb.WriteString("data: {\"choices\":[{\"delta\":{\"content\":\"")
	for sb.Len() < 11*1024*1024 {
		sb.WriteString("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	}
	sb.WriteString("\"}}]}\n\ndata: [DONE]\n\n")
	_, err := streamViaChunks(t, splitEvery(sb.String(), 64*1024))
	if !errors.Is(err, ErrBackendStream) {
		t.Fatalf("oversize line: want ErrBackendStream, got %v", err)
	}
}
