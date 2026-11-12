package counsel

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ErrBackendStream is a sentinel for retryable backend stream errors from
// counsel streaming calls. Mirrors proxy.ErrBackendStream but is local to
// avoid a package dependency.
var ErrBackendStream = errors.New("counsel backend stream error")

// counselStreamIdleTimeout is the default idle timeout for streaming Mashūra
// calls: if no bytes arrive for this duration, the stream is considered
// stalled and a retryable error is returned. This replaces the 300s total
// timeout — reasoning models can legitimately take minutes to produce the
// first token, but a silent connection for 120s is a stall.
const counselStreamIdleTimeout = 120 * time.Second

// counselStreamTotalCap is the absolute upper bound on a single streaming
// Mashūra call. A call that runs this long without completing is cancelled.
const counselStreamTotalCap = 30 * time.Minute

// streamIdleReader wraps an io.ReadCloser with an idle deadline. If no bytes
// arrive within the timeout, the reader closes the underlying body and returns
// a streamIdleErr on the next Read. This mirrors proxy/client.go's idleReader
// but is local to counsel to avoid a package dependency.
type streamIdleReader struct {
	src     io.ReadCloser
	timeout time.Duration
	timer   *time.Timer
	mu      sync.Mutex
	closed  bool
}

type streamIdleErr struct{ d time.Duration }

func (e *streamIdleErr) Error() string {
	return fmt.Sprintf("counsel stream idle for %s — upstream stall", e.d)
}

func (e *streamIdleErr) Unwrap() error { return ErrBackendStream }

func newStreamIdleReader(src io.ReadCloser, timeout time.Duration) *streamIdleReader {
	r := &streamIdleReader{src: src, timeout: timeout}
	r.resetTimer()
	return r
}

func (r *streamIdleReader) resetTimer() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	if r.timer != nil {
		r.timer.Stop()
	}
	r.timer = time.AfterFunc(r.timeout, func() {
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			return
		}
		r.closed = true
		r.mu.Unlock()
		_ = r.src.Close()
	})
}

func (r *streamIdleReader) Read(p []byte) (int, error) {
	n, err := r.src.Read(p)
	if n > 0 {
		r.resetTimer()
	}
	if err != nil {
		r.mu.Lock()
		closed := r.closed
		r.mu.Unlock()
		if closed {
			return n, &streamIdleErr{d: r.timeout}
		}
	}
	return n, err
}

func (r *streamIdleReader) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	if r.timer != nil {
		r.timer.Stop()
	}
	r.mu.Unlock()
	return r.src.Close()
}

// streamRequest sends a POST request with streaming and returns the full
// response body (collected from SSE events), status code, and error. The
// request is sent with "stream": true in the body; the SSE response is
// parsed to collect content and usage. The idle timeout (120s) catches
// stalled streams; the total cap (30min) bounds the overall call.
//
// parseSSE is called for each SSE data line; it appends to the content and
// usage accumulators. Returns the accumulated content text, usage, and error.
func streamRequest(ctx context.Context, endpoint string, headers map[string]string, body []byte, idle time.Duration, total time.Duration, parseSSE func(data []byte) (content string, usage *streamUsage, done bool)) (string, *streamUsage, int, error) {
	// Layer a total-cap context on top of the caller's context.
	tctx, cancel := context.WithTimeout(ctx, total)
	defer cancel()

	req, err := http.NewRequestWithContext(tctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", nil, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := counselClient.Do(req)
	if err != nil {
		return "", nil, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		return string(raw), nil, resp.StatusCode, nil
	}

	// Check if the response is SSE or non-streaming JSON. Some servers and
	// proxies return application/json even when stream:true was sent. In that
	// case, try the SSE parser on the raw body — it may extract content and
	// usage from the JSON shape (e.g. OpenRouter's chunk format matches the
	// non-streaming response for some fields). If not, return empty content.
	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/json") {
		raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if rerr != nil {
			return "", nil, http.StatusOK, fmt.Errorf("read response: %w", rerr)
		}
		if cap := panelCaptureFrom(ctx); cap != nil {
			// Non-SSE body: the whole JSON response is one "line" for the capture.
			cap.observe(string(raw))
		}
		c, u, _ := parseSSE(raw)
		return c, u, http.StatusOK, nil
	}

	// Wrap the response body with an idle-deadline reader.
	bodyReader := newStreamIdleReader(resp.Body, idle)
	defer bodyReader.Close()

	scanner := bufio.NewScanner(bodyReader)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024) // 10 MB max line

	var content strings.Builder
	var usage *streamUsage
	gotTerminal := false // set when [DONE] or a parser signals done
	// dataLines accumulates data: lines for the current SSE event. Per the
	// SSE spec, multiple data: lines within one event are joined with \n
	// before dispatching to the parser. A blank line terminates the event.
	var dataLines []string

	// mergeEvent processes the accumulated data: lines (joined with \n)
	// and returns true if the stream should stop (terminal event or done).
	mergeEvent := func() bool {
		if len(dataLines) == 0 {
			return false
		}
		data := strings.Join(dataLines, "\n")
		dataLines = nil
		if cap := panelCaptureFrom(ctx); cap != nil && data != "[DONE]" {
			cap.observe(data)
		}
		if data == "[DONE]" {
			gotTerminal = true
			return true
		}
		if data == "" {
			return false
		}
		c, u, done := parseSSE([]byte(data))
		if c != "" {
			content.WriteString(c)
		}
		if u != nil {
			// Merge usage: the message_start event carries input/output
			// tokens; the message_delta event carries the stop_reason.
			// Don't overwrite — merge fields.
			if usage == nil {
				usage = u
			} else {
				if u.InputTokens != 0 {
					usage.InputTokens = u.InputTokens
				}
				if u.OutputTokens != 0 {
					usage.OutputTokens = u.OutputTokens
				}
				if u.ReasoningTokens != 0 {
					usage.ReasoningTokens = u.ReasoningTokens
				}
				if u.WebSearchRequests != 0 {
					usage.WebSearchRequests = u.WebSearchRequests
				}
				if u.FinishReason != "" {
					usage.FinishReason = u.FinishReason
				}
				if u.ErrorMsg != "" {
					usage.ErrorMsg = u.ErrorMsg
				}
				if u.HasProviderCost {
					usage.ProviderCostUSD = u.ProviderCostUSD
					usage.HasProviderCost = true
				}
				if u.ServedModel != "" {
					usage.ServedModel = u.ServedModel
				}
			}
		}
		if done {
			gotTerminal = true
			return true
		}
		return false
	}

	for scanner.Scan() {
		s := strings.TrimRight(scanner.Text(), "\r")
		if s == "" {
			// Blank line = SSE event delimiter. Process accumulated data.
			if mergeEvent() {
				break
			}
			continue
		}
		if strings.HasPrefix(s, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(s[len("data:"):]))
			continue
		}
		// Other SSE fields (event:, id:, retry:) are ignored per spec.
	}
	// Flush any remaining accumulated data (stream ended without a final
	// blank line — unusual but valid per spec).
	if !gotTerminal {
		mergeEvent()
	}
	if err := scanner.Err(); err != nil {
		if ctxErr := tctx.Err(); ctxErr != nil {
			return content.String(), usage, http.StatusOK, ctxErr
		}
		// Check if it was an idle timeout.
		var idleErr *streamIdleErr
		if errors.As(err, &idleErr) {
			return content.String(), usage, http.StatusOK, idleErr
		}
		return content.String(), usage, http.StatusOK, fmt.Errorf("SSE stream: %w", err)
	}

	// If the stream ended without a terminal event ([DONE] or the parser
	// signalling done), the connection was cut mid-stream. Return an error
	// rather than accepting partial content as a complete answer.
	if !gotTerminal {
		return content.String(), usage, http.StatusOK, fmt.Errorf("SSE stream ended without a terminal event (partial content %d bytes)", content.Len())
	}

	return content.String(), usage, http.StatusOK, nil
}

// streamUsage is the parsed usage from an SSE stream (either Anthropic or
// OpenRouter). The fields are set by the provider-specific parsers.
type streamUsage struct {
	InputTokens       int64
	OutputTokens      int64
	ReasoningTokens   int64
	WebSearchRequests int64
	FinishReason      string
	ErrorMsg          string
	// ProviderCostUSD is the provider-reported cost (usage.cost, OpenRouter)
	// when present. HasProviderCost distinguishes absent from a genuine $0.
	ProviderCostUSD float64
	HasProviderCost bool
	// ServedModel is the model the provider reports it actually served
	// (the response's "model" field). It matters for routers such as
	// openrouter/auto, where the requested id is not the priced model.
	ServedModel string
}
