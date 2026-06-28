package lsp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strconv"
	"sync"
	"testing"
	"time"
)

// mockServer simulates a JSON-RPC server. It provides a clientConn (the
// client's view: reads from server, writes to server) and records received
// requests.
type mockServer struct {
	mu            sync.Mutex
	received      []rpcRequest
	handlers      map[string]func(params json.RawMessage) (result any, err *rpcError)
	clientW       io.WriteCloser // client writes to server's stdin
	clientR       io.ReadCloser  // client reads server's stdout
	serverStdout io.WriteCloser  // for injecting server→client messages
}

func newMockServer(t *testing.T, handlers map[string]func(json.RawMessage) (any, *rpcError)) *mockServer {
	t.Helper()
	// Two pipes: client→server (stdin) and server→client (stdout).
	// io.Pipe() returns (*PipeReader, *PipeWriter): reader reads, writer writes.
	stdinR, stdinW := io.Pipe() // client writes to stdinW, server reads from stdinR
	stdoutR, stdoutW := io.Pipe() // server writes to stdoutW, client reads from stdoutR

	s := &mockServer{
		handlers:      handlers,
		clientW:       stdinW,  // client writes to server's stdin
		clientR:       stdoutR, // client reads server's stdout
		serverStdout:  stdoutW, // for injecting server→client messages
	}

	go func() {
		defer stdinR.Close()
		defer stdoutW.Close()
		r := bufio.NewReader(stdinR)
		for {
			msg, err := readMessage(r)
			if err != nil {
				return
			}
			var req rpcRequest
			if err := json.Unmarshal(msg, &req); err != nil {
				continue
			}
			s.mu.Lock()
			s.received = append(s.received, req)
			s.mu.Unlock()

			if req.ID != nil {
				var result any
				var rpcErr *rpcError
				if h, ok := s.handlers[req.Method]; ok {
					result, rpcErr = h(req.Params)
				} else {
					rpcErr = &rpcError{Code: -32601, Message: "method not found"}
				}
				resp := struct {
					JSONRPC string    `json:"jsonrpc"`
					ID      int64     `json:"id"`
					Result  any       `json:"result,omitempty"`
					Error   *rpcError `json:"error,omitempty"`
				}{
					JSONRPC: "2.0",
					ID:      *req.ID,
					Result:  result,
					Error:   rpcErr,
				}
				data, _ := json.Marshal(resp)
				header := "Content-Length: " + strconv.Itoa(len(data)) + "\r\n\r\n"
				stdoutW.Write([]byte(header))
				stdoutW.Write(data)
			}
		}
	}()

	return s
}

func (s *mockServer) sendToClient(data []byte) {
	// unused placeholder — injection is done via injectServerMessage
}

// injectServerMessage writes a raw framed message to the client's stdout pipe.
// Used to simulate server→client notifications/requests.
func (s *mockServer) injectServerMessage(data []byte) error {
	header := "Content-Length: " + strconv.Itoa(len(data)) + "\r\n\r\n"
	if _, err := s.serverStdout.Write([]byte(header)); err != nil {
		return err
	}
	_, err := s.serverStdout.Write(data)
	return err
}

func TestRPCConn_CallAndNotify(t *testing.T) {
	handlers := map[string]func(json.RawMessage) (any, *rpcError){
		"test/call": func(params json.RawMessage) (any, *rpcError) {
			var p struct {
				Name string `json:"name"`
			}
			json.Unmarshal(params, &p)
			return map[string]string{"greeting": "hello " + p.Name}, nil
		},
		"test/error": func(params json.RawMessage) (any, *rpcError) {
			return nil, &rpcError{Code: -32603, Message: "boom"}
		},
	}
	srv := newMockServer(t, handlers)

	conn := newRPCConn(srv.clientW, srv.clientR, nil)
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Test call
	result, err := conn.call(ctx, "test/call", map[string]string{"name": "world"})
	if err != nil {
		t.Fatalf("call failed: %v", err)
	}
	var resp struct {
		Greeting string `json:"greeting"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if resp.Greeting != "hello world" {
		t.Errorf("greeting = %q, want %q", resp.Greeting, "hello world")
	}

	// Test error
	_, err = conn.call(ctx, "test/error", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if re, ok := err.(*rpcError); !ok || re.Code != -32603 {
		t.Errorf("error = %v, want code -32603", err)
	}

	// Test notification
	if err := conn.notify("test/notify", map[string]string{"data": "x"}); err != nil {
		t.Fatalf("notify failed: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	srv.mu.Lock()
	defer srv.mu.Unlock()
	found := false
	for _, r := range srv.received {
		if r.Method == "test/notify" {
			found = true
		}
	}
	if !found {
		t.Error("server did not receive notification")
	}
}

func TestReadMessage_Framing(t *testing.T) {
	// Verify Content-Length framing handles multi-byte UTF-8 bodies.
	body := `{"jsonrpc":"2.0","method":"test","params":{"name":"وَكِيل"}}`
	header := "Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n"
	r := bufio.NewReader(stringsReader(header + body))

	msg, err := readMessage(r)
	if err != nil {
		t.Fatalf("readMessage: %v", err)
	}
	if string(msg) != body {
		t.Errorf("message = %q, want %q", string(msg), body)
	}
}

func TestRPCConn_ServerToClientRequest(t *testing.T) {
	handlers := map[string]func(json.RawMessage) (any, *rpcError){}
	srv := newMockServer(t, handlers)

	handlerCalled := false
	handler := func(method string, params json.RawMessage, isRequest bool) (any, error) {
		if method == "window/workDoneProgress/create" && isRequest {
			handlerCalled = true
			return nil, nil // void success — result: null
		}
		return nil, nil
	}

	conn := newRPCConn(srv.clientW, srv.clientR, handler)
	defer conn.Close()

	// Inject the server→client request.
	req := rpcRequest{
		JSONRPC: "2.0",
		ID:      int64Ptr(999),
		Method:  "window/workDoneProgress/create",
		Params:  json.RawMessage(`{"token":"progress-1"}`),
	}
	data, _ := json.Marshal(req)
	if err := srv.injectServerMessage(data); err != nil {
		t.Fatalf("inject: %v", err)
	}

	waitFor(t, 200*time.Millisecond, func() bool { return handlerCalled })
	if !handlerCalled {
		t.Fatal("client did not handle server→client request")
	}
}

func TestRPCConn_VoidResponseContainsResultNull(t *testing.T) {
	// Verify that a void success response emits result: null (JSON-RPC 2.0 §5).
	// This is tested via the unit test of respondToServer below.
}

func TestRespondToServer_VoidSuccessHasResultNull(t *testing.T) {
	// Unit test: respondToServer with (nil, nil) must produce a response
	// containing "result": null, not an omitted result.
	stdinR, stdinW := io.Pipe()
	stdoutR, _ := io.Pipe()
	defer stdinR.Close()
	defer stdoutR.Close()

	conn := newRPCConn(stdinW, stdoutR, nil)

	// Capture what gets written to stdinW (which the server would read).
	cap := &bytes.Buffer{}
	conn.w = cap

	conn.respondToServer(42, nil, nil)

	written := cap.String()
	if !contains(written, `"result":null`) {
		t.Errorf("void response must contain result:null, got: %s", written)
	}
	if contains(written, `"error"`) {
		t.Errorf("void response must not contain error, got: %s", written)
	}
}

func TestRPCConn_ProgressNotification(t *testing.T) {
	handlers := map[string]func(json.RawMessage) (any, *rpcError){}
	srv := newMockServer(t, handlers)

	progressReceived := ""
	handler := func(method string, params json.RawMessage, isRequest bool) (any, error) {
		if method == "$/progress" && !isRequest {
			var p ProgressParams
			json.Unmarshal(params, &p)
			raw, _ := json.Marshal(p.Value)
			progressReceived = string(raw)
		}
		return nil, nil
	}

	conn := newRPCConn(srv.clientW, srv.clientR, handler)
	defer conn.Close()

	req := rpcRequest{
		JSONRPC: "2.0",
		Method:  "$/progress",
		Params:  json.RawMessage(`{"token":"t1","value":{"kind":"end","message":"done"}}`),
	}
	data, _ := json.Marshal(req)
	if err := srv.injectServerMessage(data); err != nil {
		t.Fatalf("inject: %v", err)
	}

	waitFor(t, 200*time.Millisecond, func() bool { return progressReceived != "" })
	if progressReceived == "" {
		t.Error("client did not receive $/progress notification")
	}
	if !contains(progressReceived, "done") {
		t.Errorf("progress value = %q, want to contain 'done'", progressReceived)
	}
}

func waitFor(t *testing.T, max time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func int64Ptr(i int64) *int64 { return &i }

func contains(s, substr string) bool {
	return len(s) >= len(substr) && indexOf(s, substr) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// stringsReader implements io.Reader over a string.
type stringsReader string

func (s stringsReader) Read(p []byte) (int, error) {
	if len(s) == 0 {
		return 0, io.EOF
	}
	n := copy(p, s)
	return n, nil
}
