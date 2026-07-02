// Package lsp implements a minimal LSP (Language Server Protocol) client
// for wakil's code-intelligence tools. It speaks JSON-RPC 2.0 over stdio
// to a language server process (e.g. gopls) spawned via the executor.
//
// This is a hand-rolled implementation: the useful parts of the go-sdk's
// JSON-RPC transport are internal/unimportable, go.lsp.dev/protocol lacks
// LSP 3.17 positionEncoding fields, and the subset needed is ~7 methods.
// Struct shapes are sourced from gopls's own internal/protocol package
// (generated from the LSP meta-model).
package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
)

// ─── JSON-RPC 2.0 framing ─────────────────────────────────────────────────────

// rpcConn is a bidirectional JSON-RPC 2.0 connection over stdio.
// One writer (mutex-guarded), one reader goroutine that dispatches
// responses by ID and routes notifications to the handler.
// Designed for per-server write serialization (L1): the caller (Manager)
// owns one rpcConn per server, and all requests are serialized here.
type rpcConn struct {
	w io.Writer     // server stdin
	r *bufio.Reader // server stdout (buffered for Content-Length framing)
	c io.Closer     // closes stdin (the reliable hard-stop: LSP servers exit on stdin EOF)

	nextID atomic.Int64
	mu     sync.Mutex // serializes writes
	closed bool

	pending   map[int64]chan *rpcResponse
	pendingMu sync.Mutex

	// notification handler — called for server→client notifications/requests
	// that are not responses to our calls (e.g. $/progress, window/showMessage,
	// window/workDoneProgress/create, client/registerCapability).
	notifyHandler func(method string, params json.RawMessage, isRequest bool) (result any, err error)
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"` // nil for notifications
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("json-rpc error %d: %s", e.Code, e.Message)
}

// newRPCConn wraps a server's stdin/stdout into a JSON-RPC connection.
// The reader goroutine starts immediately; call Close to stop it.
func newRPCConn(stdin io.WriteCloser, stdout io.Reader, notifyHandler func(string, json.RawMessage, bool) (any, error)) *rpcConn {
	c := &rpcConn{
		w:             stdin,
		r:             bufio.NewReader(stdout),
		c:             stdin,
		pending:       make(map[int64]chan *rpcResponse),
		notifyHandler: notifyHandler,
	}
	go c.readLoop()
	return c
}

// write sends a JSON-RPC message (serialized — L1).
func (c *rpcConn) write(msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshaling json-rpc message: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("connection closed")
	}
	// Content-Length framing
	header := "Content-Length: " + strconv.Itoa(len(data)) + "\r\n\r\n"
	if _, err := c.w.Write([]byte(header)); err != nil {
		return fmt.Errorf("writing header: %w", err)
	}
	if _, err := c.w.Write(data); err != nil {
		return fmt.Errorf("writing body: %w", err)
	}
	return nil
}

// notify sends a notification (no ID, no response expected).
func (c *rpcConn) notify(method string, params any) error {
	p, err := marshalParams(params)
	if err != nil {
		return err
	}
	return c.write(rpcRequest{JSONRPC: "2.0", Method: method, Params: p})
}

// call sends a request and blocks until the response arrives or ctx is done.
func (c *rpcConn) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	ch := make(chan *rpcResponse, 1)

	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()

	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}()

	p, err := marshalParams(params)
	if err != nil {
		return nil, err
	}
	if err := c.write(rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: p}); err != nil {
		return nil, err
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case resp := <-ch:
		if resp == nil {
			return nil, fmt.Errorf("connection closed before response")
		}
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	}
}

// readLoop reads JSON-RPC messages and dispatches them.
func (c *rpcConn) readLoop() {
	for {
		msg, err := readMessage(c.r)
		if err != nil {
			// Connection broken — retire all pending calls.
			c.pendingMu.Lock()
			for id, ch := range c.pending {
				ch <- nil
				delete(c.pending, id)
			}
			c.pendingMu.Unlock()
			return
		}

		// Distinguish request/notification (has "method") from response.
		var wire struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      *int64          `json:"id,omitempty"`
			Method  string          `json:"method,omitempty"`
			Params  json.RawMessage `json:"params,omitempty"`
			Result  json.RawMessage `json:"result,omitempty"`
			Error   *rpcError       `json:"error,omitempty"`
		}
		if err := json.Unmarshal(msg, &wire); err != nil {
			continue // skip malformed
		}

		if wire.Method != "" {
			// It's a request (has ID) or notification (no ID).
			isRequest := wire.ID != nil
			if c.notifyHandler != nil {
				result, herr := c.notifyHandler(wire.Method, wire.Params, isRequest)
				if isRequest {
					// Must send a response for server→client requests.
					c.respondToServer(*wire.ID, result, herr)
				}
			} else if isRequest {
				// No handler — respond with method-not-found.
				c.respondToServer(*wire.ID, nil, &rpcError{Code: -32601, Message: "method not found"})
			}
			continue
		}

		// It's a response — route by ID.
		if wire.ID != nil {
			c.pendingMu.Lock()
			ch, ok := c.pending[*wire.ID]
			c.pendingMu.Unlock()
			if ok {
				ch <- &rpcResponse{Result: wire.Result, Error: wire.Error}
			}
		}
	}
}

// respondToServer sends a response to a server→client request.
// JSON-RPC 2.0 §5: a success response MUST include result (may be null).
// For void requests like window/workDoneProgress/create, result: null is correct.
func (c *rpcConn) respondToServer(id int64, result any, err error) {
	var rpcErr *rpcError
	if err != nil {
		if re, ok := err.(*rpcError); ok {
			rpcErr = re
		} else {
			rpcErr = &rpcError{Code: -32603, Message: err.Error()}
		}
	}

	// On success, result MUST be present (even if null). On error, result is omitted.
	var resultRaw json.RawMessage
	if rpcErr == nil {
		if result == nil {
			resultRaw = json.RawMessage("null")
		} else {
			resultRaw, _ = json.Marshal(result)
		}
	}

	resp := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int64           `json:"id"`
		Result  json.RawMessage `json:"result,omitempty"`
		Error   *rpcError       `json:"error,omitempty"`
	}{
		JSONRPC: "2.0",
		ID:      id,
		Result:  resultRaw,
		Error:   rpcErr,
	}
	_ = c.write(resp)
}

// Close closes stdin (the reliable hard-stop: LSP servers exit on stdin EOF).
func (c *rpcConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return c.c.Close()
}

// readMessage reads one Content-Length-framed JSON-RPC message.
func readMessage(r *bufio.Reader) ([]byte, error) {
	// Read headers until blank line.
	contentLength := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = trimCRLF(line)
		if line == "" {
			break // end of headers
		}
		if len(line) > len("Content-Length:") && line[:len("Content-Length:")] == "Content-Length:" {
			val := trimSpace(line[len("Content-Length:"):])
			n, err := strconv.Atoi(val)
			if err != nil {
				return nil, fmt.Errorf("parsing Content-Length %q: %w", val, err)
			}
			contentLength = n
		}
	}
	if contentLength < 0 {
		return nil, fmt.Errorf("no Content-Length header")
	}
	// Guard against unreasonable Content-Length values (allocation DoS).
	if contentLength > 100*1024*1024 { // 100 MB
		return nil, fmt.Errorf("Content-Length %d exceeds 100 MB limit", contentLength)
	}
	body := make([]byte, contentLength)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}

func trimCRLF(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\r' || s[len(s)-1] == '\n') {
		s = s[:len(s)-1]
	}
	return s
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

// marshalParams marshals params to json.RawMessage, handling nil gracefully.
func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	data, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("marshaling params: %w", err)
	}
	return json.RawMessage(data), nil
}
