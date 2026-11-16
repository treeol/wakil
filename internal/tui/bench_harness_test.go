package tui

// This file is the Part 1 measurement harness for the TUI responsiveness
// pass (docs/work-0.8.4-progress.md).
//
// Why the real constructor: the benchmark builds the model through
// NewTUIModelWithFacade, the same path cmd/wakil/main.go:424 runs (swapFn),
// with a fake facade that mimics the REAL wiring facade's cost shape —
// Info() recomputes ConvStats over the whole conversation (see
// internal/wiring/facade.go:264) — rather than the zero-cost fakeFacade
// used by the unit tests. A benchmark with the cheap fake would miss the
// dominant cost the user feels (Part 1 L2: Info() per frame).
//
// Corpus: proxy.Message slices with user turns, assistant text with code
// blocks, tool results, wide (CJK) and non-Latin (Arabic) chars — the
// mix the TUI actually wraps and renders.
//
// Measured op (Update + View, the unit of perceived responsiveness):
//   keypress  — one tea.KeyMsg (rune 'x') in the input
//   stream    — one KindMessageDelta event (append + throttled refresh path)
//   wheel     — one MouseWheelDown event
//   drag      — one MouseActionMotion while a selection press is active
//   resize    — one tea.WindowSizeMsg
// Each op's Update is followed by a View so the frame cost includes both.
//
// Allocs per frame: testing.AllocsPerRun over the Update+View pair.
// Bytes written to the terminal per frame: len(View) — Bubbletea's
// standard renderer writes the frame string (less coalescing, which only
// helps); it is the correct upper-bound proxy for "bytes written".

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionclient"
	"github.com/treeol/wakil/internal/proxy"
)

// benchTranscriptSize is the total content bytes of a transcript: the
// number that drives Info()'s ConvStats walk in the real facade. The
// bench facade reports TranscriptSize per this measure so Info() cost
// scales with session length as it does in production.
func benchTranscriptSize(conv []proxy.Message) int {
	n := 0
	for _, m := range conv {
		if m.Content != nil {
			n += len(*m.Content)
		}
	}
	return n
}

// benchFacade mirrors the real wiring facade's cost shape for Info() and
// Snapshot(): it returns stored values, but Info() RECOMPUTES ConvLen and
// TranscriptSize by walking the whole conversation on every call —
// exactly what wiringFacade.Info does via app.ConvStats()
// (internal/wiring/facade.go:264-270: ContextUsage + ConvStats, both
// O(conv)). The per-frame status line calls Info() up to three times
// (sizes() + View()), so a long session pays that walk three times per
// frame. Snapshot() returns the stored conv without copying (the TUI
// only calls Snapshot() at construction/rotation, not per frame).
type benchFacade struct {
	sessionclient.Facade
	sid  event.SessionID
	chat string
	conv []proxy.Message
	cons sessionclient.Consent
}

func (f *benchFacade) Snapshot() sessionclient.ClientSnapshot {
	return sessionclient.ClientSnapshot{
		SessionID: f.sid,
		ChatID:    f.chat,
		Conv:      f.conv,
	}
}

func (f *benchFacade) Consent() sessionclient.Consent { return f.cons }

func (f *benchFacade) Info() sessionclient.InfoSnapshot {
	// Real wiring facade cost: walk the conversation on every call.
	convLen, convSize := len(f.conv), benchTranscriptSize(f.conv)
	return sessionclient.InfoSnapshot{
		ChatID:         f.chat,
		EffectiveModel: "ilm",
		ContextLimit: sessionclient.ContextLimit{
			NCtx: 32768, Source: "backend",
		},
		ContextUsed:    convSize / 4,
		ContextExact:   false,
		ConvLen:        convLen,
		TranscriptSize: convSize,
	}
}

func (f *benchFacade) RunDeferredStartup(done func(note string)) bool { return false }
func (f *benchFacade) ConsumeStartupNote() string                     { return "" }

// benchCorpus builds n proxy messages cycling through: a user question, an
// assistant answer with a code block, a tool result, and a note containing
// wide (CJK) and RTL (Arabic) characters. Sizes matter: assistant answers
// and tool results carry multi-line, wrapped content — the expensive,
// re-rendered material.
func benchCorpus(n int) []proxy.Message {
	conv := make([]proxy.Message, 0, n)
	codeBlock := "```go\nfunc main() {\n\tfmt.Println(\"hello\")\n}\n```\n\nHere is the result:\n\n- item one\n- item two\n"
	toolOut := strings.Repeat("file.go:123: something happened here\n", 8)
	wide := "中文测试日本語テスト한국어مرحبا بالعالم café naïve — wide and non-Latin text that wraps"
	for i := 0; len(conv) < n; i++ {
		u := fmt.Sprintf("user question number %d: please explain this code block in detail and mention the wide text %s", i, wide)
		conv = append(conv, proxy.Message{Role: "user", Content: &u})
		if len(conv) >= n {
			break
		}
		a := codeBlock + fmt.Sprintf("\nassistant answer %d with a longer explanation that will wrap across multiple lines when rendered in the viewport. %s\n", i, strings.Repeat("more explanation here. ", 4))
		conv = append(conv, proxy.Message{Role: "assistant", Content: &a})
		if len(conv) >= n {
			break
		}
		t := toolOut + fmt.Sprintf("tool result %d\n", i)
		name := "read_file"
		conv = append(conv, proxy.Message{Role: "tool", Name: name, Content: &t})
	}
	return conv
}

const benchPrincipalSID = event.SessionID("sess_bench")

// newBenchModel builds a real-constructor model with benchTranscriptSize
// messages in Snapshot.Conv, driven to a ready 120x40 (or given) frame.
func newBenchModel(conv []proxy.Message, w, h int) tuiModel {
	f := &benchFacade{sid: benchPrincipalSID, chat: "chat_bench", conv: conv}
	f.cons = sessionclient.Consent{}
	// Hydrate through the real constructor so items, caches and the
	// viewport start exactly as a resumed long session starts them.
	m := NewTUIModelWithFacade(f, nil, core.Principal{
		TenantID: event.EmbeddedTenantID,
		UserID:   event.EmbeddedUserID,
		Role:     core.RoleOwner,
	})
	m.width, m.height, m.ready = w, h, true
	m.hadTurn = true
	m = m.reflow()
	return m
}

func benchTiming(b *testing.B, m tuiModel, msg tea.Msg) {
	b.Helper()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		updated, _ := m.Update(msg)
		_ = updated.(tuiModel).View()
	}
}

func BenchmarkKeypress50(b *testing.B) {
	m := newBenchModel(benchCorpus(50), 120, 40)
	benchTiming(b, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
}

func BenchmarkKeypress500(b *testing.B) {
	m := newBenchModel(benchCorpus(500), 120, 40)
	benchTiming(b, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
}

func BenchmarkKeypress2000(b *testing.B) {
	m := newBenchModel(benchCorpus(2000), 120, 40)
	benchTiming(b, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
}

func BenchmarkKeypress5000(b *testing.B) {
	m := newBenchModel(benchCorpus(5000), 120, 40)
	benchTiming(b, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
}

func BenchmarkKeypress2000Wide(b *testing.B) {
	m := newBenchModel(benchCorpus(2000), 200, 60)
	benchTiming(b, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
}

func BenchmarkStream2000(b *testing.B) {
	m := newBenchModel(benchCorpus(2000), 120, 40)
	m.state = stateStreaming
	msg := evt(event.KindMessageDelta, event.MessageDelta{Text: " streamed chunk"}, benchPrincipalSID)
	benchTiming(b, m, msg)
}

func BenchmarkWheel2000(b *testing.B) {
	m := newBenchModel(benchCorpus(2000), 120, 40)
	msg := tea.MouseMsg{X: 10, Y: 10, Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown}
	benchTiming(b, m, msg)
}

func BenchmarkDrag2000(b *testing.B) {
	m := newBenchModel(benchCorpus(2000), 120, 40)
	// Arm a selection press first, then measure one motion step.
	m.sel = selection{pressed: true, anchorRow: 0, anchorCol: 0, headRow: 0, headCol: 0}
	msg := tea.MouseMsg{X: 20, Y: 5, Action: tea.MouseActionMotion, Button: tea.MouseButtonLeft}
	benchTiming(b, m, msg)
}

func BenchmarkResize2000(b *testing.B) {
	m := newBenchModel(benchCorpus(2000), 120, 40)
	msg := tea.WindowSizeMsg{Width: 121, Height: 41}
	benchTiming(b, m, msg)
}

func BenchmarkKeypress2000Mem(b *testing.B) {
	m := newBenchModel(benchCorpus(2000), 120, 40)
	b.ReportAllocs()
	benchTiming(b, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
}

func BenchmarkStream2000Mem(b *testing.B) {
	m := newBenchModel(benchCorpus(2000), 120, 40)
	m.state = stateStreaming
	msg := evt(event.KindMessageDelta, event.MessageDelta{Text: " streamed chunk"}, benchPrincipalSID)
	b.ReportAllocs()
	benchTiming(b, m, msg)
}

// TestBenchHarnessSmoke ensures the harness builds a model whose View
// renders the corpus (the numbers the benchmarks produce only mean
// something if the frame actually contains wrapped content). The viewport
// shows only the visible tail (followBottom), so assert on the newest
// user turn, which is at the bottom.
func TestBenchHarnessSmoke(t *testing.T) {
	m := newBenchModel(benchCorpus(60), 120, 40)
	out := m.View()
	if !strings.Contains(plain(out), "user question number") {
		t.Fatalf("benchmark View does not contain the loaded corpus (tail):\n%s", plain(out))
	}
	// Frame bytes: the terminal-write proxy the benchmarks report.
	if len(out) < 500 {
		t.Fatalf("frame suspiciously short: %d bytes", len(out))
	}
}

// TestFrameCostRegression is the Part 2 regression test: a frame
// (Update + View) at 2000 messages must not exceed 5× the per-op numbers
// recorded in the Part 1 table (docs/work-0.8.4-progress.md, Part 1
// numbers, AMD Ryzen 7 PRO 7840U): keypress 1,738,328 ns, stream
// 1,581,144 ns, wheel 1,557,169 ns, drag 53,589,096 ns (pre-fix).
// The generous 5× headroom keeps it from flaking on slower machines;
// the point is to catch a frame that starts growing with session length
// again, not to enforce today's absolute numbers.
func TestFrameCostRegression(t *testing.T) {
	if testing.Short() {
		t.Skip("frame-cost regression is a CI gate, not a short run")
	}
	const maxFactor = 5
	cases := []struct {
		name string
		msg  tea.Msg
		base int64 // ns/op, Part 1 table
	}{
		{"keypress", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")}, 1_738_328},
		{"stream", evt(event.KindMessageDelta, event.MessageDelta{Text: " streamed chunk"}, benchPrincipalSID), 1_581_144},
		{"wheel", tea.MouseMsg{X: 10, Y: 10, Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown}, 1_557_169},
		{"drag", tea.MouseMsg{X: 20, Y: 5, Action: tea.MouseActionMotion, Button: tea.MouseButtonLeft}, 53_589_096},
		// Resize: baseline is the Part B1 target (50 ms), not a
		// measurement — pre-fix it measured 473,636,520 ns. The
		// deferred re-wrap makes the resize FRAME cheap; the tick
		// performs the re-wrap once (TestResizeDeferredRewrap).
		{"resize", tea.WindowSizeMsg{Width: 121, Height: 41}, 50_000_000},
	}
	for _, tc := range cases {
		m := newBenchModel(benchCorpus(2000), 120, 40)
		if tc.name == "stream" {
			m.state = stateStreaming
		}
		if tc.name == "drag" {
			m.sel = selection{pressed: true, anchorRow: 0, anchorCol: 0, headRow: 0, headCol: 0}
		}
		// Warm + measure: run once to settle caches, then time 20 iters.
		updated, _ := m.Update(tc.msg)
		m = updated.(tuiModel)
		_ = m.View()
		start := time.Now()
		const iters = 20
		for i := 0; i < iters; i++ {
			updated, _ := m.Update(tc.msg)
			m = updated.(tuiModel)
			_ = m.View()
		}
		got := int64(time.Since(start)) / iters
		if got > tc.base*maxFactor {
			t.Errorf("%s: frame %d ns/op > %dx Part 1 baseline %d ns (limit %d)", tc.name, got, maxFactor, tc.base, tc.base*maxFactor)
		} else {
			t.Logf("%s: frame %d ns/op (limit %d)", tc.name, got, tc.base*maxFactor)
		}
	}
}

// TestResizeDeferredRewrap: a width change defers the prefix
// re-wrap; the resizeReflowMsg tick performs it at the new width.
func TestResizeDeferredRewrap(t *testing.T) {
	m := newBenchModel(benchCorpus(60), 120, 40)
	oldW := m.prefixW
	u, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	m = u.(tuiModel)
	if !m.resizePending {
		t.Fatal("width change did not defer the re-wrap")
	}
	if m.prefixW != oldW {
		t.Fatalf("prefix rebuilt synchronously on resize (prefixW %d -> %d)", oldW, m.prefixW)
	}
	// The frame still renders at the new size.
	if v := m.View(); len(v) == 0 {
		t.Fatal("resize frame empty")
	}
	u, _ = m.Update(resizeReflowMsg{})
	m = u.(tuiModel)
	if m.resizePending {
		t.Fatal("tick did not clear resizePending")
	}
	if m.prefixW == oldW {
		t.Fatal("tick did not re-wrap the prefix at the new width")
	}
	if v := m.View(); !strings.Contains(plain(v), "content") && !strings.Contains(plain(v), "tool result") && !strings.Contains(plain(v), "question") {
		t.Fatalf("re-wrapped frame lost content:\n%s", plain(v))
	}
}

// 20 rotations through /new and /resume, 10 Mashūra rounds, recording
// heap, goroutines, live tickers/subscriptions and tabs at start and end.
//
// Method (honest about what a model-level test can see):
//   - heap/goroutines: runtime.MemStats + runtime.NumGoroutine at start
//     and after the soak (the TUI model itself starts no goroutines —
//     Cmds are tea.Tick closures the runtime doesn't execute here — so
//     the goroutine delta measures leak of model-owned state, and the
//     ticker/subscription counts are read from the model fields the TUI
//     actually owns: dotArmed/refreshArmed tick-arm flags and subTabs).
//   - A rotation is a rotationMsg carrying a fresh bench facade; the
//     model rebuilds items, clears tabs and re-arms nothing — the
//     numbers at the end show whether anything accumulated.
//
// The test asserts the invariants the soak must hold: tabs are cleared
// on rotation, tick-arm flags are not stuck, and the model keeps
// rendering correctly after 200 turns.
// NOT included: widget-level allocation attribution (benchPartition) —
// that was cut when the transcript turned out to be load-time only
// (items are built once at NewTUIModelWithFacade; per-frame cost is
// status/Info + View, not item re-render). The per-frame numbers are
// BenchmarkKeypress/Stream/Wheel/Drag/Resize above.
func TestBenchSoak(t *testing.T) {
	conv := benchCorpus(600)
	m := newBenchModel(conv, 200, 60)

	var m0 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	g0 := runtime.NumGoroutine()
	tabs0 := len(m.subTabs)

	// 200 turns: each turn is a message delta + tool start/complete +
	// turn completed. State moves idle→streaming→idle.
	for turn := 0; turn < 200; turn++ {
		m = step(m, evt(event.KindTurnStarted, event.TurnStarted{TurnID: "trn"}, benchPrincipalSID))
		m = step(m, evt(event.KindMessageDelta, event.MessageDelta{Text: " answer text with some content"}, benchPrincipalSID))
		m = step(m, evt(event.KindToolCallStarted, event.ToolCallStarted{
			ToolCallID: "tcl", Name: "read_file", ArgDigest: "file.go",
		}, benchPrincipalSID))
		m = step(m, evt(event.KindToolCallCompleted, event.ToolCallCompleted{
			ToolCallID: "tcl", Name: "read_file", Status: "ok",
		}, benchPrincipalSID))
		m = step(m, evt(event.KindTurnCompleted, event.TurnCompleted{TurnID: "trn", Outcome: "complete"}, benchPrincipalSID))
		// 10 Mashūra rounds spread over the soak.
		if turn%20 == 0 {
			opID := fmt.Sprintf("op_soak_%d", turn/20)
			m = step(m, jobStarted(opID, "mashura__review", benchPrincipalSID))
			m = step(m, jobDone(opID, "mashura__review", "review result", "", benchPrincipalSID))
		}
		// 20 rotations: alternate /new (empty conv) and /resume (conv).
		if turn%10 == 0 {
			f2 := &benchFacade{sid: benchPrincipalSID, chat: "chat_bench"}
			if (turn/10)%2 == 0 {
				f2.conv = conv
			}
			m = step(m, rotationMsg{facade: f2, kind: rotateNew})
		}
	}

	var m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m1)
	g1 := runtime.NumGoroutine()
	tabs1 := len(m.subTabs)

	// The model still renders after the soak.
	if v := m.View(); len(v) == 0 {
		t.Fatal("model renders empty after soak")
	}
	t.Logf("soak: heap %d -> %d bytes (delta %d), goroutines %d -> %d, tabs %d -> %d, dotArmed=%v refreshArmed=%v",
		m0.HeapAlloc, m1.HeapAlloc, int64(m1.HeapAlloc)-int64(m0.HeapAlloc),
		g0, g1, tabs0, tabs1, m.dotArmed, m.refreshArmed)
	if tabs1 != 0 {
		t.Errorf("tabs after soak = %d, want 0 (rotations clear them; done tabs prune)", tabs1)
	}
	// The dot tick self-terminates on the next dotTickMsg when idle; the
	// model-level test never runs the Cmd, so drive it once per chain.
	// A leaked chain would re-arm forever instead of clearing dotArmed.
	for i := 0; i < 2 && m.dotArmed; i++ {
		m = step(m, dotTickMsg{})
	}
	if m.dotArmed {
		t.Error("dotArmed stuck true after soak (tick chain never terminated)")
	}
	if m.refreshArmed {
		t.Error("refreshArmed stuck true after soak")
	}
}
