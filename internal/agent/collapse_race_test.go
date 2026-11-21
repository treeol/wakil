package agent

import (
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/treeol/wakil/internal/proxy"
)

// TestHandleCollapseContextCurTurnRace runs the model-called collapse_context
// handler while another goroutine changes curTurn under stateMu.Lock, the way
// NewConversationTransition does on an RPC goroutine. Under -race the handler
// must not read curTurn outside stateMu. (A race test is probabilistic by
// nature: it fails before the fix because the handler reads curTurn at
// several places after releasing the lock, and passes after because it reads
// it once under the lock.)
func TestHandleCollapseContextCurTurnRace(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Cfg.ContextCollapseMinBytes = 0
	call := proxy.ToolCall{ID: "ccR", Type: "function", Function: proxy.FunctionCall{
		Name: "collapse_context", Arguments: `{"summary":"s"}`}}
	app.Conv = []proxy.Message{
		{Role: "user", Content: StrPtr("do it"), Turn: 1},
		tcMsg("t1", "read_file", `{"path":"a.go"}`),
		trMsg("t1", "read_file", strings.Repeat("payload line\n", 100)),
		{Role: "assistant", ToolCalls: []proxy.ToolCall{call}, Turn: 1},
	}
	for i := range app.Conv {
		app.Conv[i].Turn = 1
	}
	app.turnSeq, app.curTurn = 1, 1

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer close(stop)
		for i := 0; i < 2000; i++ {
			// Reset the record so every call runs the whole handler.
			app.stateMu.Lock()
			app.collapsed = nil
			app.stateMu.Unlock()
			app.handleCollapseContext(call)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			app.stateMu.Lock()
			app.curTurn = 1 + i%2
			app.stateMu.Unlock()
			runtime.Gosched()
		}
	}()
	wg.Wait()
}
