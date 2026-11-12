package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/treeol/wakil/internal/diag"
	"github.com/treeol/wakil/internal/proxy"
)

// debugRequestEnvVar switches on the request debug dump: when set to a
// directory path, every model request body is written there, one JSON file
// per request. Unset (the default) means no dump and no cost.
const debugRequestEnvVar = "WAKIL_DEBUG_REQUESTS"

// debugRequestCounter is the running number in debug-dump file names. It
// counts only dumped requests: with the dump off the hook returns before
// touching it, so the counter cannot move when the feature is off.
var debugRequestCounter atomic.Int64

func resetDebugRequestCounter() { debugRequestCounter.Store(0) }

// debugRequestWarned tracks which directories already logged a dump warning,
// so a failing dump logs one warning per directory, not one per request.
var debugRequestWarned sync.Map

// debugWireMessage is the on-the-wire shape of one message (no Turn, no
// Pinned — those never reach the wire).
type debugWireMessage struct {
	Role       string           `json:"role"`
	Content    json.RawMessage  `json:"content"`
	ToolCalls  []proxy.ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	Name       string           `json:"name,omitempty"`
}

// dumpDebugRequest writes the exact body of one model request — messages and
// tool definitions, as sent — to WAKIL_DEBUG_REQUESTS, if that variable is
// set. Files are mode 0600 in a 0700 directory, named with the session id, a
// running number and a timestamp. It never contains an API key or any
// request header: marshaling messages and tools only. A write failure logs
// one warning (once per process per directory) and continues; the hook never
// fails a request. The dump covers the main turn loop only; the rating
// pass, summarizer and Mashūra make their own narrower calls.
func (a *App) dumpDebugRequest(msgs []proxy.Message, tools []proxy.Tool) {
	dir := strings.TrimSpace(os.Getenv(debugRequestEnvVar))
	if dir == "" {
		return
	}
	n := debugRequestCounter.Add(1)
	// Plain wire encoding: string content, matching the client's wire shape
	// when no cache breakpoints are set. The session id is sanitized to a
	// single path element so a stored id can never escape the directory.
	wire := make([]debugWireMessage, len(msgs))
	for i, m := range msgs {
		content, err := json.Marshal(m.Content)
		if err != nil {
			a.debugRequestWarn("marshal content: %v", err)
			return
		}
		wire[i] = debugWireMessage{
			Role:       m.Role,
			Content:    content,
			ToolCalls:  m.ToolCalls,
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		}
	}
	type dumpBody struct {
		Messages []debugWireMessage `json:"messages"`
		Tools    []proxy.Tool       `json:"tools,omitempty"`
	}
	raw, err := json.MarshalIndent(dumpBody{Messages: wire, Tools: tools}, "", "  ")
	if err != nil {
		a.debugRequestWarn("marshal: %v", err)
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		a.debugRequestWarn("mkdir %s: %v", dir, err)
		return
	}
	stamp := time.Now().Format("20060102-150405.000000")
	name := fmt.Sprintf("%s-%06d-%s.json", strings.ReplaceAll(filepath.Base(a.chatID()), "..", "_"), n, stamp)
	if err := os.WriteFile(filepath.Join(dir, name), append(raw, '\n'), 0o600); err != nil {
		a.debugRequestWarn("write: %v", err)
		return
	}
}

// debugRequestWarn logs one warning per directory per process (through the
// diag seam, so it is safe under the TUI alt-screen and lands in the session
// log).
func (a *App) debugRequestWarn(format string, args ...interface{}) {
	dir := strings.TrimSpace(os.Getenv(debugRequestEnvVar))
	if _, dup := debugRequestWarned.LoadOrStore(dir, true); dup {
		return
	}
	diag.Printf("wakil: debug request dump (%s): "+format+"\n", append([]interface{}{dir}, args...)...)
}
