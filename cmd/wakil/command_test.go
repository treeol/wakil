package main

import (
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/workflow"
)

// cmdApp builds a minimal App suitable for exercising handleTUICommand without
// any network or TUI program.
func cmdApp() *agent.App {
	return &agent.App{
		Cfg:     config.DefaultConfig(),
		Client:  newTestClient(""),
		Exec:    newFakeExecutor(),
		Session: &agent.Session{},
	}
}

// runCmd invokes the returned agent.Cmd (if any) and returns the produced message.
func runCmd(cmd agent.Cmd) agent.Msg {
	if cmd == nil {
		return nil
	}
	return cmd()
}

// firstSysNote executes cmd and returns the first SysNoteMsg it finds,
// expanding agent.BatchMsg one level deep. Used for /backend tests that now
// return Batch(note, resolveBackendCtxCmd).
func firstSysNote(cmd agent.Cmd) agent.SysNoteMsg {
	if cmd == nil {
		return agent.SysNoteMsg{}
	}
	msg := cmd()
	switch m := msg.(type) {
	case agent.SysNoteMsg:
		return m
	case agent.BatchMsg:
		for _, sub := range m.Cmds {
			if sub == nil {
				continue
			}
			if n, ok := sub().(agent.SysNoteMsg); ok && n.Text != "" {
				return n
			}
		}
	}
	return agent.SysNoteMsg{}
}

func TestHandleTUICommandNonSlashIgnored(t *testing.T) {
	handled, quit, cmd := agent.HandleTUICommand("just a task", cmdApp())
	if handled || quit || cmd != nil {
		t.Errorf("non-slash input must pass through to the agent; got handled=%v quit=%v cmd!=nil=%v", handled, quit, cmd != nil)
	}
}

func TestHandleTUICommandQuit(t *testing.T) {
	for _, q := range []string{"/quit", "/exit"} {
		handled, quit, _ := agent.HandleTUICommand(q, cmdApp())
		if !handled || !quit {
			t.Errorf("%s should be handled and quit; got handled=%v quit=%v", q, handled, quit)
		}
	}
}

func TestHandleTUICommandUnknown(t *testing.T) {
	handled, quit, cmd := agent.HandleTUICommand("/bogus", cmdApp())
	msg, ok := runCmd(cmd).(agent.SysNoteMsg)
	if !handled || quit || !ok || !strings.Contains(msg.Text, "unknown command") {
		t.Errorf("unknown command handling wrong: handled=%v quit=%v msg=%+v", handled, quit, msg)
	}
}

func TestHandleTUICommandAutoToggle(t *testing.T) {
	app := cmdApp()
	// First /auto turns it ON.
	_, _, cmd := agent.HandleTUICommand("/auto", app)
	if !app.AutoApprove {
		t.Fatal("/auto should enable AutoApprove")
	}
	if msg, ok := runCmd(cmd).(agent.SysNoteMsg); !ok || !strings.Contains(msg.Text, "ON") {
		t.Errorf("first /auto note should say ON; got %+v", msg)
	}
	// Second /auto turns it OFF.
	_, _, cmd = agent.HandleTUICommand("/auto", app)
	if app.AutoApprove {
		t.Fatal("second /auto should disable AutoApprove")
	}
	if msg, ok := runCmd(cmd).(agent.SysNoteMsg); !ok || !strings.Contains(msg.Text, "OFF") {
		t.Errorf("second /auto note should say OFF; got %+v", msg)
	}
}

func TestHandleTUICommandAutoDestructive(t *testing.T) {
	app := cmdApp()

	// /auto destructive with auto OFF: refused, flag stays false.
	_, _, cmd := agent.HandleTUICommand("/auto destructive", app)
	if app.AllowDestructive {
		t.Fatal("/auto destructive must be refused while auto mode is OFF")
	}
	if msg, ok := runCmd(cmd).(agent.SysNoteMsg); !ok || !strings.Contains(msg.Text, "enable /auto first") {
		t.Errorf("refusal note should say 'enable /auto first'; got %+v", msg)
	}

	// Enable auto, then grant destructive.
	agent.HandleTUICommand("/auto", app)
	_, _, cmd = agent.HandleTUICommand("/auto destructive", app)
	if !app.AllowDestructive {
		t.Fatal("/auto destructive should enable AllowDestructive when auto is ON")
	}
	if msg, ok := runCmd(cmd).(agent.SysNoteMsg); !ok || !strings.Contains(msg.Text, "ON") {
		t.Errorf("grant note should say ON; got %+v", msg)
	}

	// Second /auto destructive revokes.
	agent.HandleTUICommand("/auto destructive", app)
	if app.AllowDestructive {
		t.Fatal("second /auto destructive should revoke the grant")
	}

	// Grant again, then switch auto OFF: grant must be cleared with it.
	agent.HandleTUICommand("/auto destructive", app)
	if !app.AllowDestructive {
		t.Fatal("setup: grant should be ON again")
	}
	agent.HandleTUICommand("/auto", app)
	if app.AutoApprove {
		t.Fatal("setup: auto should be OFF")
	}
	if app.AllowDestructive {
		t.Error("switching /auto OFF must clear the destructive grant")
	}

	// Unknown subcommand → usage note, no state change.
	_, _, cmd = agent.HandleTUICommand("/auto bogus", app)
	if msg, ok := runCmd(cmd).(agent.SysNoteMsg); !ok || !strings.Contains(msg.Text, "usage:") {
		t.Errorf("unknown /auto subcommand should show usage; got %+v", msg)
	}
	if app.AutoApprove || app.AllowDestructive {
		t.Error("unknown subcommand must not change state")
	}
}

func TestHandleTUICommandRawtoolsToggle(t *testing.T) {
	app := cmdApp()
	_, _, cmd := agent.HandleTUICommand("/rawtools", app)
	if !app.RawTools {
		t.Fatal("/rawtools should enable RawTools")
	}
	if msg, ok := runCmd(cmd).(agent.SysNoteMsg); !ok || !strings.Contains(msg.Text, "ON") {
		t.Errorf("/rawtools note should say ON; got %+v", msg)
	}
}

func TestHandleTUICommandNew(t *testing.T) {
	app := cmdApp()
	app.Conv = []proxy.Message{{Role: "user", Content: agent.StrPtr("old")}}
	oldID := app.Client.ChatID
	_, _, cmd := agent.HandleTUICommand("/new", app)
	if len(app.Conv) != 0 {
		t.Error("/new should clear the transcript")
	}
	if app.Client.ChatID == oldID {
		t.Error("/new should mint a new chat_id")
	}
	if _, ok := runCmd(cmd).(agent.NewConvMsg); !ok {
		t.Errorf("/new should emit agent.NewConvMsg")
	}
}

func TestHandleTUICommandInfoCommands(t *testing.T) {
	app := cmdApp()
	cases := map[string]string{
		"/cwd":     "/work",
		"/mode":    "fake",
		"/history": "messages",
	}
	for cmdStr, want := range cases {
		_, _, cmd := agent.HandleTUICommand(cmdStr, app)
		msg, ok := runCmd(cmd).(agent.SysNoteMsg)
		if !ok || !strings.Contains(msg.Text, want) {
			t.Errorf("%s note should contain %q; got %+v", cmdStr, want, msg)
		}
	}
}

func TestHandleTUICommandCompactNothing(t *testing.T) {
	app := cmdApp() // empty transcript → nothing to compact, no summarizer call
	_, _, cmd := agent.HandleTUICommand("/compact", app)
	if msg, ok := runCmd(cmd).(agent.SysNoteMsg); !ok || !strings.Contains(msg.Text, "nothing to compact") {
		t.Errorf("/compact on empty transcript should say nothing to compact; got %+v", msg)
	}
}

func TestHandleTUICommandLearn(t *testing.T) {
	_, _, cmd := agent.HandleTUICommand("/learn", cmdApp())
	if _, ok := runCmd(cmd).(agent.LearnTurnMsg); !ok {
		t.Errorf("/learn should emit agent.LearnTurnMsg")
	}
}

func TestHandleTUICommandMCPNoServers(t *testing.T) {
	_, _, cmd := agent.HandleTUICommand("/mcp", cmdApp())
	if msg, ok := runCmd(cmd).(agent.SysNoteMsg); !ok || msg.Text == "" {
		t.Errorf("/mcp should always return a note; got %+v", msg)
	}
}

func TestHandleTUICommandBackendSet(t *testing.T) {
	app := cmdApp()
	_, _, cmd := agent.HandleTUICommand("/backend openrouter", app)
	if app.SelectedBackend != "openrouter" {
		t.Errorf("SelectedBackend should be openrouter; got %q", app.SelectedBackend)
	}
	// /backend returns Batch(note, resolveBackendCtxCmd); extract the note.
	msg := firstSysNote(cmd)
	if !strings.Contains(msg.Text, "openrouter") {
		t.Errorf("/backend openrouter note should mention the name; got %+v", msg)
	}
}

func TestHandleTUICommandBackendSetPersistsAcrossCommand(t *testing.T) {
	// /backend sets SelectedBackend; a second /backend call overwrites it.
	app := cmdApp()
	agent.HandleTUICommand("/backend openrouter", app)
	agent.HandleTUICommand("/backend together", app)
	if app.SelectedBackend != "together" {
		t.Errorf("SelectedBackend should be together after second set; got %q", app.SelectedBackend)
	}
}

func TestHandleTUICommandBackendStatusNoArg(t *testing.T) {
	app := cmdApp()
	app.SelectedBackend = "openrouter"
	// Inject a last-used backend value.
	app.Client.SetLastUsedBackend("local")

	_, _, cmd := agent.HandleTUICommand("/backend", app)
	msg, ok := runCmd(cmd).(agent.SysNoteMsg)
	if !ok {
		t.Fatal("/backend with no arg should emit SysNoteMsg")
	}
	if !strings.Contains(msg.Text, "openrouter") {
		t.Errorf("note should show selected backend; got %q", msg.Text)
	}
	if !strings.Contains(msg.Text, "local") {
		t.Errorf("note should show last-used backend; got %q", msg.Text)
	}
}

func TestHandleTUICommandBackendStatusNoArgDefault(t *testing.T) {
	app := cmdApp()
	// SelectedBackend is "" and no last-used.
	_, _, cmd := agent.HandleTUICommand("/backend", app)
	msg, ok := runCmd(cmd).(agent.SysNoteMsg)
	if !ok {
		t.Fatal("/backend with no arg should emit SysNoteMsg")
	}
	if !strings.Contains(msg.Text, "proxy default") {
		t.Errorf("note should say proxy default when no backend selected; got %q", msg.Text)
	}
	if !strings.Contains(msg.Text, "none yet") {
		t.Errorf("note should say none yet when no turn completed; got %q", msg.Text)
	}
}

func TestHandlePlanCommandNoWorkflow(t *testing.T) {
	app := cmdApp()
	// /plan with no subcommand → usage.
	_, _, cmd := agent.HandleTUICommand("/plan", app)
	if msg, ok := runCmd(cmd).(agent.SysNoteMsg); !ok || !strings.Contains(msg.Text, "usage") {
		t.Errorf("/plan alone should show usage; got %+v", msg)
	}
	// /plan status with no active workflow.
	_, _, cmd = agent.HandleTUICommand("/plan status", app)
	if msg, ok := runCmd(cmd).(agent.SysNoteMsg); !ok || !strings.Contains(msg.Text, "no active workflow") {
		t.Errorf("/plan status should report no workflow; got %+v", msg)
	}
	// /plan abort clears any workflow and acks.
	app.Workflow = &workflow.WorkflowState{Phase: workflow.WFImplement}
	_, _, cmd = agent.HandleTUICommand("/plan abort", app)
	if msg, ok := runCmd(cmd).(agent.SysNoteMsg); !ok || !strings.Contains(msg.Text, "aborted") {
		t.Errorf("/plan abort should ack; got %+v", msg)
	}
	if app.Workflow != nil {
		t.Error("/plan abort should clear the workflow")
	}
}
