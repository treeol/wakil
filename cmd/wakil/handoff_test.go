package main

import (
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/proxy"
)

// --- /maxctx tests ---

func TestHandleTUICommandMaxctxSet(t *testing.T) {
	app := cmdApp()
	_, _, cmd := agent.HandleTUICommand("/maxctx 200000", app)
	if app.EffectiveCtxMaxCharsOverride != 200000 {
		t.Errorf("EffectiveCtxMaxCharsOverride should be 200000; got %d", app.EffectiveCtxMaxCharsOverride)
	}
	msg, ok := runCmd(cmd).(agent.SysNoteMsg)
	if !ok || !strings.Contains(msg.Text, "200000") {
		t.Errorf("/maxctx 200000 note should contain 200000; got %+v", msg)
	}
}

func TestHandleTUICommandMaxctxShow(t *testing.T) {
	app := cmdApp()
	app.EffectiveCtxMaxCharsOverride = 200000
	_, _, cmd := agent.HandleTUICommand("/maxctx", app)
	msg, ok := runCmd(cmd).(agent.SysNoteMsg)
	if !ok || !strings.Contains(msg.Text, "200000") {
		t.Errorf("/maxctx show should contain 200000; got %+v", msg)
	}
}

func TestHandleTUICommandMaxctxDisabled(t *testing.T) {
	app := cmdApp()
	app.EffectiveCtxMaxCharsOverride = 0 // explicitly disabled
	_, _, cmd := agent.HandleTUICommand("/maxctx", app)
	msg, ok := runCmd(cmd).(agent.SysNoteMsg)
	if !ok || !strings.Contains(msg.Text, "disabled") {
		t.Errorf("/maxctx with cap=0 should say disabled; got %+v", msg)
	}
}

func TestHandleTUICommandMaxctxInvalid(t *testing.T) {
	app := cmdApp()
	_, _, cmd := agent.HandleTUICommand("/maxctx -5", app)
	msg, ok := runCmd(cmd).(agent.SysNoteMsg)
	if !ok || !strings.Contains(msg.Text, "non-negative") {
		t.Errorf("/maxctx -5 should say non-negative; got %+v", msg)
	}
}

func TestHandleTUICommandMaxctxNotSetUsesConfig(t *testing.T) {
	app := cmdApp()
	// EffectiveCtxMaxCharsOverride defaults to 0 (Go zero value) in cmdApp,
	// which means "disabled" — effectiveCtxCap() returns 0.
	_, _, cmd := agent.HandleTUICommand("/maxctx", app)
	msg, ok := runCmd(cmd).(agent.SysNoteMsg)
	if !ok || !strings.Contains(msg.Text, "disabled") {
		t.Errorf("/maxctx with no override and no config should say disabled; got %+v", msg)
	}
}

// --- /handoff tests ---

func TestHandleTUICommandHandoffEmptyConv(t *testing.T) {
	app := cmdApp() // empty Conv
	_, _, cmd := agent.HandleTUICommand("/handoff", app)
	msg, ok := cmd().(agent.HandoffMsg)
	if !ok {
		t.Fatalf("/handoff should emit HandoffMsg; got %T", cmd())
	}
	if msg.Err == nil {
		t.Error("/handoff on empty conversation should return an error")
	}
	if !strings.Contains(msg.Err.Error(), "nothing to hand off") {
		t.Errorf("error should say 'nothing to hand off'; got %v", msg.Err)
	}
}

func TestHandleTUICommandHandoffNoUserMessages(t *testing.T) {
	app := cmdApp()
	// System messages only — no user turn.
	app.Conv = []proxy.Message{{Role: "system", Content: agent.StrPtr("preamble")}}
	_, _, cmd := agent.HandleTUICommand("/handoff", app)
	msg, ok := cmd().(agent.HandoffMsg)
	if !ok {
		t.Fatalf("/handoff should emit HandoffMsg; got %T", cmd())
	}
	if msg.Err == nil || !strings.Contains(msg.Err.Error(), "no user messages") {
		t.Errorf("/handoff with system-only conv should say 'no user messages'; got %v", msg.Err)
	}
}

func TestBuildContinuationPrompt(t *testing.T) {
	prompt := agent.BuildContinuationPrompt("test summary", "abc12345-aaaa-bbbb-cccc-dddddddddddd", "/workspace")
	if !strings.Contains(prompt, "abc12345") {
		t.Error("prompt should contain short old chat ID")
	}
	if !strings.Contains(prompt, "/workspace") {
		t.Error("prompt should contain workspace")
	}
	if !strings.Contains(prompt, "test summary") {
		t.Error("prompt should contain the summary")
	}
	if !strings.Contains(prompt, "untrusted") {
		t.Error("prompt should delimit summary as untrusted")
	}
}

func TestEffectiveCtxCapOverride(t *testing.T) {
	app := cmdApp()
	app.EffectiveCtxMaxCharsOverride = 200000
	if got := app.EffectiveCtxCap(); got != 200000 {
		t.Errorf("effectiveCtxCap with override should be 200000; got %d", got)
	}
}

func TestEffectiveCtxCapNotSetUsesConfig(t *testing.T) {
	app := cmdApp()
	app.EffectiveCtxMaxCharsOverride = -1 // not set → use config
	app.Cfg.EffectiveCtxMaxChars = 150000
	if got := app.EffectiveCtxCap(); got != 150000 {
		t.Errorf("effectiveCtxCap should use config value 150000; got %d", got)
	}
}

func TestEffectiveCtxCapDisabled(t *testing.T) {
	app := cmdApp()
	app.EffectiveCtxMaxCharsOverride = 0 // explicitly disabled
	if got := app.EffectiveCtxCap(); got != 0 {
		t.Errorf("effectiveCtxCap with override=0 should be 0 (disabled); got %d", got)
	}
}
