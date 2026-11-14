package wiring

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionclient"
)

// counselServerFake returns a fake Mashūra transport that answers every
// panel call with a cited reply (never VOID) and counts how many times it
// was hit.
func counselServerFake(t *testing.T) (*int, func()) {
	t.Helper()
	var mu sync.Mutex
	count := new(int)
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		*count = calls
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		// The panel dispatch reads the usage object for cost accounting;
		// the reply text must end with a LAST SECTION READ citation.
		text := "diagnosis: fix the loop exit condition\nLAST SECTION READ: Recent tool calls"
		writeCitedReplyFake(w, r, text, `{"input_tokens":100,"output_tokens":50}`)
	}))
	agent.SetOracleEndpointHook(srv.URL)
	return count, func() {
		agent.SetOracleEndpointHook("")
		srv.Close()
	}
}

// writeCitedReplyFake answers like a diligent reviewer: it ends its text
// with the mandatory citation. A fake that returns fixed text without it
// is (correctly) VOID.
func writeCitedReplyFake(w http.ResponseWriter, r *http.Request, text, usageJSON string) {
	raw, _ := io.ReadAll(r.Body)
	var body struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(raw, &body)
	user := ""
	if len(body.Messages) > 0 {
		user = body.Messages[len(body.Messages)-1].Content
	}
	if i := strings.Index(user, "── Round 1 responses from other panel members ──"); i >= 0 {
		user = user[:i]
	}
	last, inFence := "", false
	for _, l := range strings.Split(user, "\n") {
		if strings.HasPrefix(l, "```") {
			inFence = !inFence
		}
		if inFence {
			continue
		}
		if strings.HasPrefix(l, "#") {
			last = strings.TrimSpace(strings.TrimLeft(l, "# "))
		} else if strings.HasPrefix(l, "── ") && strings.HasSuffix(l, " ──") {
			last = strings.TrimSuffix(strings.TrimPrefix(l, "── "), " ──")
		}
	}
	if last != "" {
		text += "\nLAST SECTION READ: " + last
	}
	var usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	}
	_ = json.Unmarshal([]byte(usageJSON), &usage)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "data: %s\n\n", mustJSONChunkFake(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": text}, "finish_reason": nil}}}))
	fmt.Fprintf(w, "data: %s\n\n", mustJSONChunkFake(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}}}))
	fmt.Fprintf(w, "data: %s\n\n", mustJSONChunkFake(map[string]any{"choices": []any{}, "usage": map[string]any{"prompt_tokens": usage.InputTokens, "completion_tokens": usage.OutputTokens}}))
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func mustJSONChunkFake(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func toolCallFramesFake(id, name string, argParts ...string) []string {
	frames := []string{
		fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":%q,"arguments":""}}]},"finish_reason":null}]}`, id, name),
	}
	for _, p := range argParts {
		frames = append(frames, fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":%q}}]},"finish_reason":null}]}`, p))
	}
	return append(frames, `{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
}

var _ sessionclient.Facade

// struggleFramesFake builds the scripted model frames for the
// four-identical-writes sequence: write_file then failing run_shell,
// repeated four times, ending with a final text.
func struggleFramesFake() [][]string {
	var calls [][]string
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("c%d", i)
		args := fmt.Sprintf(`{"path":"answer.txt","content":%q}`, "same")
		calls = append(calls, toolCallFramesFake(id, "write_file", args))
		calls = append(calls, toolCallFramesFake(id+"s", "run_shell", `{"command":"./check.sh"}`))
	}
	calls = append(calls, []string{contentChunk("done")})
	return calls
}

// TestStrugglePathThroughRealStartup drives the real startup path
// (BootstrapTUI — the same bootstrap cmd/wakil calls) with an empty config
// and temp directories, sets the level with the same code /mashura <level>
// runs (DispatchCommand), then drives the four-identical-writes sequence
// with the fake transport.
//
// The earlier end-to-end test in internal/agent built an App by hand and
// passed while the real program left the cap at 0. This test closes that
// gap: the App, its cap and its counsel state come from bootstrap, not
// from a hand-set MaxCounsel.
func TestStrugglePathThroughRealStartup(t *testing.T) {
	t.Setenv("MASHURA_TEST_OR_KEY", "x")
	t.Setenv("MASHURA_TEST_KEY", "x")

	runLevel := func(t *testing.T, level string) (string, int) {
		t.Helper()
		count, cleanupCounsel := counselServerFake(t)
		t.Cleanup(cleanupCounsel)
		srv := sseServer(t, struggleFramesFake()...)
		t.Cleanup(srv.Close)
		exe := &struggleExec{shellErr: fmt.Errorf("check failed")}

		// Empty config (DefaultConfig zero CounselMaxPerSession, no
		// AutoCounsel), temp workdir, direct executor — the same bootstrap
		// and wiring main.go uses.
		cfg := config.DefaultConfig()
		cfg.WorkDir = t.TempDir()
		cfg.ExecMode = "direct"
		cfg.Mashura = &config.MashuraConfig{
			Level:   level,
			Members: []string{"openrouter:test-model-a", "openrouter:test-model-b"},
		}
		cfg.OpenRouterAPIKeyEnv = "MASHURA_TEST_OR_KEY"
		cfg.OracleAPIKeyEnv = "MASHURA_TEST_KEY"
		cfg.OracleModel = "test-model"
		// Point the session client at the fake model server.
		cfg.Endpoint = config.EndpointConfig{Kind: config.EndpointKindOpenAI, BaseURL: srv.URL, Model: "ilm"}
		cfg.BaseURL = srv.URL

		// Deliver via BootstrapTUI's deliver callback. It must be created
		// before BootstrapTUI, so declare the delivery state here and pass
		// it into the bootstrap call below.
		var mu2 sync.Mutex
		var delivered []event.Event
		var out strings.Builder
		deliver := func(ev event.Event) {
			mu2.Lock()
			defer mu2.Unlock()
			delivered = append(delivered, ev)
			if ev.Kind == event.KindMessageDelta {
				if d, ok := ev.Payload.(event.MessageDelta); ok {
					out.WriteString(d.Text)
				}
			}
		}
		rt, cleanup, err := BootstrapTUI(cfg, exe, "", deliver, BootstrapTUIOpts{
			CounselMode: cfg.AutoCounsel,          // "" = not set, level decides
			CounselMax:  cfg.CounselMaxPerSession, // 0 = not set, default cap applies
		})
		if err != nil {
			t.Fatalf("BootstrapTUI: %v", err)
		}
		defer cleanup()

		f, ok := rt.Facade.(*wiringFacade)
		if !ok {
			t.Fatalf("Facade is %T, want *wiringFacade", rt.Facade)
		}
		app := f.app

		// The App built by bootstrap: counsel state must come from the
		// startup path, not from a hand-set cap.
		if app.MaxCounsel != 0 {
			t.Errorf("bootstrap left MaxCounsel=%d, want 0 (default cap applies at use time)", app.MaxCounsel)
		}
		if got := app.AutoCounselCap(); got != agent.DefaultMaxCounsel {
			t.Errorf("AutoCounselCap = %d, want default %d", got, agent.DefaultMaxCounsel)
		}
		if mode, _ := app.ResolveCounselMode(); mode != startupExpectedMode(level) {
			t.Errorf("level %s: resolved counsel mode = %q, want %q", level, mode, startupExpectedMode(level))
		}

		// Run /mashura <level> with the same code the user runs.
		cr := f.DispatchCommand("/mashura " + level)
		if !cr.Handled {
			t.Fatalf("/mashura %s: not handled", level)
		}
		// Auto-approve so the panel can run without a human confirmer,
		// as /auto does.
		f.SetAutoApprove(true)

		// Drive the turn through the delivered event stream (TestBootstrapTUIFresh
		// pattern): StartEventPump streams through deliver; SubmitInput starts
		// the turn; wait for its completion.
		rt.StartEventPump(context.Background())
		snap := rt.Facade.Snapshot()
		if _, err := rt.Facade.SubmitInput(context.Background(), rt.Principal, core.SubmitInputRequest{
			SessionID: snap.SessionID,
			Text:      "write answer",
		}); err != nil {
			t.Fatalf("SubmitInput: %v", err)
		}
		waitUntil(t, func() bool {
			mu2.Lock()
			defer mu2.Unlock()
			for _, ev := range delivered {
				if ev.Kind == event.KindTurnCompleted {
					return true
				}
			}
			return false
		})
		return out.String(), *count
	}

	for _, level := range []string{"off", "low", "medium", "high"} {
		t.Run(level, func(t *testing.T) {
			out, n := runLevel(t, level)
			switch level {
			case "off":
				if strings.Contains(out, "struggle detected") || strings.Contains(out, "starting mashura debug") {
					t.Errorf("off: want no struggle output, got %q", out)
				}
				if n != 0 {
					t.Errorf("off: panel fired %d times, want 0", n)
				}
			case "low", "medium":
				if !strings.Contains(out, "· struggle detected — mashura__debug can diagnose; symptom:") {
					t.Errorf("%s: missing suggest note; out=%q", level, out)
				}
				if strings.Contains(out, "starting mashura debug") {
					t.Errorf("%s: suggest case must not announce a panel; out=%q", level, out)
				}
				if n != 0 {
					t.Errorf("%s: panel fired %d times, want 0", level, n)
				}
			case "high":
				if n == 0 {
					t.Errorf("high: panel never fired; out=%q", out)
				}
				if !strings.Contains(out, "⚡ struggle detected: starting mashura debug (level high, 2 members, single pass, round 1 of 2)") {
					t.Errorf("high: missing single-pass announce with default cap; out=%q", out)
				}
			}
		})
	}
}

func startupExpectedMode(level string) string {
	switch level {
	case "off":
		return "off"
	case "low", "medium":
		return "suggest"
	default:
		return "auto"
	}
}

// struggleExec is a fake executor whose run_shell fails (so the check after
// each write fails) and whose writes succeed.
type struggleExec struct {
	fakeExec
	shellErr error
}

func (e *struggleExec) RunShell(ctx context.Context, c string) (string, error) {
	return "check failed", e.shellErr
}
