package proxy

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// stream_fuzz_test.go — Go native fuzz test for SSE stream parsing.
// CI runs the seed corpus (go test); longer fuzzing is manual.
//
// Invariant: boundary invariance — parsing the same SSE body split at
// random byte boundaries must produce the same Message as parsing it
// as a single chunk. This is the core guarantee of an incremental SSE
// parser: TCP segmenting must not affect parsed events.

// sseBody is a complete SSE stream with content + tool call + [DONE].
const sseBody = "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n" +
	"data: {\"choices\":[{\"delta\":{\"content\":\" World\"}}]}\n\n" +
	"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"run_shell\",\"arguments\":\"{\\\"command\\\":\\\"ls\\\"}\"}}]}}]}\n\n" +
	"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
	"data: [DONE]\n\n"

// FuzzSSEChunkSplit feeds the SSE body split at a random byte offset and
// asserts the parsed result matches the single-chunk parse.
func FuzzSSEChunkSplit(f *testing.F) {
	// Seed with known split points that cover alignment classes.
	body := []byte(sseBody)
	for _, split := range []int{0, 1, 2, 5, 13, 50, len(body) / 2, len(body) - 1, len(body)} {
		f.Add(split)
	}

	// Pre-compute the expected result (single-chunk parse) using a helper
	// that doesn't require *testing.T.
	rt := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       &chunkReader{chunks: [][]byte{[]byte(sseBody)}},
		}, nil
	})
	c := &Client{
		BaseURL: "http://chunk.test",
		Model:   "ilm",
		ChatID:  "test",
		HTTP:    &http.Client{Transport: rt},
	}
	ctx0, cancel0 := context.WithTimeout(context.Background(), 5e9)
	defer cancel0()
	expected, err := c.Stream(ctx0, []Message{{Role: "user", Content: strPtr("hi")}}, nil, nil, nil)
	if err != nil {
		f.Fatalf("single-chunk parse failed: %v", err)
	}

	f.Fuzz(func(t *testing.T, split int) {
		body := []byte(sseBody)
		if split < 0 || split >= len(body) {
			split = split % (len(body) + 1)
			if split < 0 {
				split = -split
			}
		}

		// Split into two chunks at the given offset.
		var chunks [][]byte
		if split == 0 {
			chunks = [][]byte{body}
		} else if split >= len(body) {
			chunks = [][]byte{body}
		} else {
			chunks = [][]byte{body[:split], body[split:]}
		}

		msg, err := streamViaChunks(t, chunks)
		if err != nil {
			t.Fatalf("split=%d: parse error: %v", split, err)
		}

		// Content must match (Content is *string — compare values, not pointers).
		var gotContent, wantContent string
		if msg.Content != nil {
			gotContent = *msg.Content
		}
		if expected.Content != nil {
			wantContent = *expected.Content
		}
		if gotContent != wantContent {
			t.Errorf("split=%d: content mismatch: got %q, want %q", split, gotContent, wantContent)
		}

		// Tool calls must match.
		if len(msg.ToolCalls) != len(expected.ToolCalls) {
			t.Fatalf("split=%d: tool call count: got %d, want %d", split, len(msg.ToolCalls), len(expected.ToolCalls))
		}
		for i, tc := range msg.ToolCalls {
			exp := expected.ToolCalls[i]
			if tc.ID != exp.ID || tc.Function.Name != exp.Function.Name {
				t.Errorf("split=%d: tool call %d mismatch: got %q/%q, want %q/%q",
					split, i, tc.ID, tc.Function.Name, exp.ID, exp.Function.Name)
			}
			if tc.Function.Arguments != exp.Function.Arguments {
				t.Errorf("split=%d: tool call %d arguments: got %q, want %q",
					split, i, tc.Function.Arguments, exp.Function.Arguments)
			}
		}
	})
}

// FuzzSSEMalformed feeds arbitrary byte sequences as SSE bodies and asserts
// panic-freedom (the parser must never crash on garbage input).
func FuzzSSEMalformed(f *testing.F) {
	seeds := []string{
		"",                             // empty
		"\n\n",                         // empty events
		"data: \n\n",                   // empty data
		"data: [DONE]\n\n",             // done only
		"data: {broken\n\n",            // malformed JSON
		"data: \x00\xff\n\n",           // binary in data
		"event: ping\ndata: hi\n\n",    // event field
		"retry: 1000\n\n",              // retry field
		": comment\n\n",                // comment
		"data: line1\ndata: line2\n\n", // multi-line data
		string(make([]byte, 100)),      // all null bytes
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, body string) {
		// Panic-freedom only — the parser must never crash on arbitrary input.
		chunks := [][]byte{[]byte(body)}
		_, _ = streamViaChunksSafe(t, chunks)
	})
}

// streamViaChunksSafe is like streamViaChunks but uses a context with a
// short timeout to prevent hangs on malformed input that might cause the
// scanner to block.
func streamViaChunksSafe(t *testing.T, chunks [][]byte) (Message, error) {
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
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	return c.Stream(ctx, []Message{{Role: "user", Content: strPtr("hi")}}, nil, nil, nil)
}

// Ensure the format import is used (fmt used in seed generation).
var _ = fmt.Sprintf
