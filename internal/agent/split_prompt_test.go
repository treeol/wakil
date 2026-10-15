package agent

import (
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
)

// --- Classifier tests ---

func TestClassifyRequest_Greetings(t *testing.T) {
	tests := []string{
		"hi", "hello", "hey", "hi there", "hello there",
		"thanks", "thank you", "ok", "okay", "got it",
		"cool", "nice", "great", "yes", "no",
	}
	for _, msg := range tests {
		if classifyRequest(msg) {
			t.Errorf("classifyRequest(%q) = true, want false (greeting)", msg)
		}
	}
}

func TestClassifyRequest_Tasks(t *testing.T) {
	tests := []string{
		"check this repo and tell me about the architecture",
		"fix the bug in auth.go",
		"implement a function that parses JSON",
		"run the test suite",
		"search for all uses of the Foo function",
		"can you help me debug this error?",
		"please review my code",
	}
	for _, msg := range tests {
		if !classifyRequest(msg) {
			t.Errorf("classifyRequest(%q) = false, want true (task)", msg)
		}
	}
}

func TestClassifyRequest_CodeReferences(t *testing.T) {
	tests := []string{
		"look at foo/bar.go and explain it",
		"what does `make test` do here?",
		"check https://example.com for the API docs",
		// Short references under 30 chars — must still activate.
		"foo/bar.go",
		"main.py",
		"`ls`",
		"$ ls -la",
		"https://example.com",
	}
	for _, msg := range tests {
		if !classifyRequest(msg) {
			t.Errorf("classifyRequest(%q) = false, want true (code reference)", msg)
		}
	}
}

func TestClassifyRequest_ConservativeDefault(t *testing.T) {
	// Longer messages that aren't greetings should default to true (conservative).
	msg := "I was wondering about the meaning of life and whether you could help me figure it out"
	if !classifyRequest(msg) {
		t.Errorf("classifyRequest(%q) = false, want true (conservative default for 30+ chars)", msg)
	}
}

func TestClassifyRequest_Ambiguous(t *testing.T) {
	// "can you help me?" has "can you" keyword → true (conservative).
	if !classifyRequest("can you help me?") {
		t.Error(`classifyRequest("can you help me?") = false, want true (has "can you")`)
	}
}

// --- Prompt mode tests ---

func TestPromptModeDefault(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	if app.PromptMode() != config.PromptModeFull {
		t.Errorf("PromptMode() = %q, want %q", app.PromptMode(), config.PromptModeFull)
	}
}

func TestSetPromptMode(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	if err := app.SetPromptMode("core"); err != nil {
		t.Fatalf("SetPromptMode(core) error: %v", err)
	}
	if !app.IsPromptCore() {
		t.Error("IsPromptCore() = false, want true")
	}
	if err := app.SetPromptMode("full"); err != nil {
		t.Fatalf("SetPromptMode(full) error: %v", err)
	}
	if app.IsPromptCore() {
		t.Error("IsPromptCore() = true, want false")
	}
}

func TestSetPromptModeInvalid(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	if err := app.SetPromptMode("bogus"); err == nil {
		t.Error("SetPromptMode(bogus) returned nil error, want error")
	}
}

// --- Activation tests ---

func TestMaybeActivateExtension_FullMode(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.AgentPrompt = "full prompt"
	app.AgentCorePrompt = "core prompt"
	app.AgentExtensionPrompt = "extension prompt"
	// Full mode (default) — should NOT activate.
	app.maybeActivateExtension("fix the bug in auth.go")
	if app.extensionActivated {
		t.Error("extensionActivated = true in full mode, want false")
	}
}

func TestMaybeActivateExtension_CoreMode_Greeting(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetPromptMode("core")
	app.AgentCorePrompt = "core prompt"
	app.AgentExtensionPrompt = "extension prompt"
	app.InjectDate = false // avoid preamble insertion
	app.Conv = []proxy.Message{{Role: "system", Content: StrPtr("core preamble"), Pinned: true, Turn: 0}}
	app.maybeActivateExtension("hi there")
	if app.extensionActivated {
		t.Error("extensionActivated = true for greeting, want false")
	}
	if len(app.Conv) != 1 {
		t.Errorf("Conv length = %d, want 1 (no extension appended)", len(app.Conv))
	}
}

func TestMaybeActivateExtension_CoreMode_Task(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetPromptMode("core")
	app.AgentCorePrompt = "core prompt"
	app.AgentExtensionPrompt = "extension prompt"
	app.InjectDate = false
	app.Conv = []proxy.Message{{Role: "system", Content: StrPtr("core preamble"), Pinned: true, Turn: 0}}
	app.maybeActivateExtension("check this repo and tell me about the architecture")
	if !app.extensionActivated {
		t.Error("extensionActivated = false for task, want true")
	}
	if len(app.Conv) != 2 {
		t.Fatalf("Conv length = %d, want 2 (core + extension)", len(app.Conv))
	}
	if app.Conv[1].Role != "system" {
		t.Errorf("Conv[1].Role = %q, want %q", app.Conv[1].Role, "system")
	}
	if app.Conv[1].Turn != 0 {
		t.Errorf("Conv[1].Turn = %d, want 0 (survives collapse projection)", app.Conv[1].Turn)
	}
	if !app.Conv[1].Pinned {
		t.Error("Conv[1].Pinned = false, want true (survives compaction)")
	}
	if DerefStr(app.Conv[1].Content) != "extension prompt" {
		t.Errorf("Conv[1].Content = %q, want %q", DerefStr(app.Conv[1].Content), "extension prompt")
	}
}

func TestMaybeActivateExtension_Sticky(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetPromptMode("core")
	app.AgentCorePrompt = "core prompt"
	app.AgentExtensionPrompt = "extension prompt"
	app.InjectDate = false
	app.Conv = []proxy.Message{{Role: "system", Content: StrPtr("core preamble"), Pinned: true, Turn: 0}}
	// First call with a task → activate.
	app.maybeActivateExtension("fix the bug")
	if !app.extensionActivated {
		t.Fatal("first activation failed")
	}
	convLen := len(app.Conv)
	// Second call with a task → should NOT duplicate.
	app.maybeActivateExtension("run the tests")
	if len(app.Conv) != convLen {
		t.Errorf("Conv length = %d after second activation, want %d (no duplicate)", len(app.Conv), convLen)
	}
}

func TestMaybeActivateExtension_NoExtensionPrompt(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetPromptMode("core")
	app.AgentCorePrompt = "core prompt"
	app.AgentExtensionPrompt = "" // no extension configured
	app.InjectDate = false
	app.Conv = []proxy.Message{{Role: "system", Content: StrPtr("core preamble"), Pinned: true, Turn: 0}}
	app.maybeActivateExtension("fix the bug")
	if app.extensionActivated {
		t.Error("extensionActivated = true with empty extension prompt, want false")
	}
}

// --- Preamble tests ---

func TestBuildPreamble_CoreMode(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetPromptMode("core")
	app.AgentPrompt = "full prompt"
	app.AgentCorePrompt = "core prompt"
	app.AgentExtensionPrompt = "extension prompt"
	app.InjectDate = true
	preamble := app.buildPreamble("Monday, 15 October 2026")
	if !strings.Contains(preamble, "core prompt") {
		t.Error("preamble should contain core prompt in core mode")
	}
	if strings.Contains(preamble, "full prompt") {
		t.Error("preamble should NOT contain full prompt in core mode")
	}
	if strings.Contains(preamble, "extension prompt") {
		t.Error("preamble should NOT contain extension prompt (lazy-loaded separately)")
	}
}

func TestBuildPreamble_FullMode(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.AgentPrompt = "full prompt"
	app.AgentCorePrompt = "core prompt"
	app.AgentExtensionPrompt = "extension prompt"
	app.InjectDate = true
	preamble := app.buildPreamble("Monday, 15 October 2026")
	if !strings.Contains(preamble, "full prompt") {
		t.Error("preamble should contain full prompt in full mode")
	}
	if strings.Contains(preamble, "core prompt") {
		t.Error("preamble should NOT contain core prompt in full mode")
	}
}

// --- Collapse interaction tests ---

func TestContextProjection_ExtensionSurvivesCollapse(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.SetPromptMode("core")
	app.AgentCorePrompt = "core prompt"
	app.AgentExtensionPrompt = "extension prompt"
	app.InjectDate = false

	// Build a.Conv with: [core preamble, extension, collapsed turn 1, turn 2 (current)]
	app.Conv = []proxy.Message{
		{Role: "system", Content: StrPtr("core preamble"), Pinned: true, Turn: 0},
		{Role: "system", Content: StrPtr("extension prompt"), Pinned: true, Turn: 0},
	}
	// Add a collapsed turn (turn 1).
	turn1 := oneCollapsedTurn(1)
	for i := range turn1 {
		turn1[i].Turn = 1
	}
	app.Conv = append(app.Conv, turn1...)
	// Add current turn (turn 2).
	app.Conv = append(app.Conv, proxy.Message{Role: "user", Content: StrPtr("current request"), Turn: 2})

	app.turnSeq = 2
	app.curTurn = 2
	app.collapsed = map[int]CollapseRecord{
		1: {Turn: 1, Note: "[collapsed turn 1]"},
	}

	proj := app.contextProjection()

	// The extension message (Turn==0) must survive projection.
	foundExtension := false
	for _, m := range proj {
		if m.Role == "system" && DerefStr(m.Content) == "extension prompt" {
			foundExtension = true
			break
		}
	}
	if !foundExtension {
		t.Error("extension system message not found in projection — must survive collapse (Turn==0)")
	}
}

// --- NewConversation reset test ---

func TestNewConversation_ResetsExtensionActivation(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetPromptMode("core")
	app.extensionActivated = true
	app.NewConversation("new-chat-id")
	if app.extensionActivated {
		t.Error("extensionActivated = true after NewConversation, want false")
	}
}

// --- Config normalization tests ---

func TestNormalizePromptMode(t *testing.T) {
	tests := []struct {
		input string
		want  string
		ok    bool
	}{
		{"", config.PromptModeFull, true},
		{"full", config.PromptModeFull, true},
		{"core", config.PromptModeCore, true},
		{"bogus", "", false},
	}
	for _, tt := range tests {
		got, ok := config.NormalizePromptMode(tt.input)
		if got != tt.want || ok != tt.ok {
			t.Errorf("NormalizePromptMode(%q) = (%q, %v), want (%q, %v)", tt.input, got, ok, tt.want, tt.ok)
		}
	}
}

// --- RepoState persistence test ---

func TestSaveRestoreRepoState_PromptMode(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetPromptMode("core")
	app.saveRepoState(func(s *RepoState) { s.PromptMode = "core" })
}

// --- Runtime mode switch test: switching to core after startup uses core prompts ---

func TestBuildPreamble_RuntimeSwitchToCore(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	// Startup in full mode: AgentPrompt set, but core/extension loaded too
	// (bootstrap now loads them unconditionally).
	app.AgentPrompt = "full prompt"
	app.AgentCorePrompt = "core prompt"
	app.AgentExtensionPrompt = "extension prompt"
	app.InjectDate = true
	// Simulate a day-stable preamble already built in full mode.
	app.preambleDay = "Monday, 15 October 2026"
	// Switch at runtime via /prompt core.
	if err := app.SetPromptMode("core"); err != nil {
		t.Fatalf("SetPromptMode(core) error: %v", err)
	}
	// preambleDay must be invalidated so the next turn rebuilds Conv[0].
	if app.preambleDay != "" {
		t.Error("preambleDay not invalidated on mode switch — next turn would keep the full prompt")
	}
	// Rebuild the preamble — must now use the core prompt.
	preamble := app.buildPreamble("Monday, 15 October 2026")
	if !strings.Contains(preamble, "core prompt") {
		t.Error("preamble after runtime switch should contain core prompt")
	}
	if strings.Contains(preamble, "full prompt") {
		t.Error("preamble after runtime switch should NOT contain full prompt")
	}
}

// --- Wire invariant test: extension message not between tool-call and result ---

func TestMaybeActivateExtension_NotBetweenToolCallAndResult(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetPromptMode("core")
	app.AgentCorePrompt = "core prompt"
	app.AgentExtensionPrompt = "extension prompt"
	app.InjectDate = false
	// Simulate a Conv with a pending tool call (assistant with tool_calls,
	// no result yet). The extension should still be inserted after Conv[0],
	// NOT between the tool call and the eventual result.
	app.Conv = []proxy.Message{
		{Role: "system", Content: StrPtr("core preamble"), Pinned: true, Turn: 0},
		tcMsg("tc1", "read_file", `{"path":"foo.go"}`),
	}
	app.maybeActivateExtension("fix the bug")
	if !app.extensionActivated {
		t.Fatal("activation failed")
	}
	// Extension should be at index 1, tool call pushed to index 2.
	if app.Conv[1].Role != "system" {
		t.Errorf("Conv[1].Role = %q, want %q (extension should be after preamble)", app.Conv[1].Role, "system")
	}
	if app.Conv[2].Role != "assistant" || len(app.Conv[2].ToolCalls) == 0 {
		t.Errorf("Conv[2] should be the assistant tool-call message, got role=%q", app.Conv[2].Role)
	}
}

// --- helper ---
