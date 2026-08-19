package tui

// control_routing_test.go: proves the chunk-6 mutation seam is actually used —
// representative TUI interactions must route through Control/StateApply, not
// through m.app (the review's exit criterion 4: end-state parity alone is not
// enough; tests must show routing).
//
// Strategy: bind recording fakes to m.control / m.apply (leaving m.app as a
// real *agent.App), trigger the interaction, and assert (a) the fake recorded
// the call and (b) the real App's corresponding field was NOT mutated. If a
// call site reverted to writing m.app directly, the fake records nothing and
// the real App field changes — both detectable.

import (
	"context"
	"testing"

	agent "github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/workflow"
)

// recordingControl implements agent.Control, recording each method name.
type recordingControl struct {
	calls []string
}

func (r *recordingControl) record(name string) { r.calls = append(r.calls, name) }

func (r *recordingControl) SetAutoApprove(v bool)         { r.record("SetAutoApprove") }
func (r *recordingControl) SetAllowDestructive(v bool)    { r.record("SetAllowDestructive") }
func (r *recordingControl) RevokeAuto()                   { r.record("RevokeAuto") }
func (r *recordingControl) NewConversation(chatID string) { r.record("NewConversation") }
func (r *recordingControl) SaveSession()                  { r.record("SaveSession") }
func (r *recordingControl) SetWorkflow(wf *workflow.WorkflowState) {
	r.record("SetWorkflow")
}
func (r *recordingControl) StartSideQuestion(ctx context.Context, q string) context.CancelFunc {
	r.record("StartSideQuestion")
	return func() {}
}
func (r *recordingControl) AppendSystemMessage(m proxy.Message) { r.record("AppendSystemMessage") }
func (r *recordingControl) ConsumeStartupNote() string          { r.record("ConsumeStartupNote"); return "" }
func (r *recordingControl) SaveRepoState(f func(*agent.RepoState)) {
	r.record("SaveRepoState")
}
func (r *recordingControl) SetInfoPanelOpen(open bool) { r.record("SetInfoPanelOpen") }

// recordingApply implements agent.StateApply.
type recordingApply struct {
	calls []string
}

func (r *recordingApply) record(name string) { r.calls = append(r.calls, name) }

func (r *recordingApply) SetCtxLimit(lim agent.ContextLimit) { r.record("SetCtxLimit") }
func (r *recordingApply) SetModelList(models []string)       { r.record("SetModelList") }
func (r *recordingApply) SetTools(tools []proxy.Tool)        { r.record("SetTools") }
func (r *recordingApply) ReplacePendingImages(imgs []proxy.ImagePart) {
	r.record("ReplacePendingImages")
}
func (r *recordingApply) AddPendingImage(img proxy.ImagePart) { r.record("AddPendingImage") }
func (r *recordingApply) ClearPendingImages()                 { r.record("ClearPendingImages") }

func (r *recordingControl) saw(name string) bool {
	for _, c := range r.calls {
		if c == name {
			return true
		}
	}
	return false
}

func (r *recordingApply) saw(name string) bool {
	for _, c := range r.calls {
		if c == name {
			return true
		}
	}
	return false
}

// TestControlRouting_AutoRevoke proves /auto OFF→ON→OFF mid-turn routes the
// revoke through m.control.RevokeAuto, not m.app directly. We drive the
// mid-turn /auto path (the only TUI code path that calls RevokeAuto).
func TestControlRouting_AutoRevoke(t *testing.T) {
	m := newTestTUI(t)
	// Turn consent ON so the second /auto is a revoke, and bind a fake control.
	m.app.SetConsent(agent.ConsentSnapshot{AutoApprove: true, AllowDestructive: true})
	fake := &recordingControl{}
	m.control = fake
	m.state = stateStreaming

	m = midTurnEnter(m, "/auto", stateStreaming)

	if !fake.saw("RevokeAuto") {
		t.Fatalf("expected control.RevokeAuto to be called; got %v", fake.calls)
	}
	if !m.app.Consent().AutoApprove {
		t.Error("real App consent should be unchanged by the no-op fake (routing proof: the seam, not m.app, is the mutation path)")
	}
}

// TestControlRouting_InfoPanelToggle proves the info-panel toggle routes
// SetInfoPanelOpen through m.control, and the real App mirror is untouched.
func TestControlRouting_InfoPanelToggle(t *testing.T) {
	m := newTestTUI(t)
	m.app.InfoPanelOpen = false
	fake := &recordingControl{}
	m.control = fake

	m = m.toggleInfoPanel()

	if !fake.saw("SetInfoPanelOpen") {
		t.Fatalf("expected control.SetInfoPanelOpen; got %v", fake.calls)
	}
	if m.app.InfoPanelOpen {
		t.Error("real App.InfoPanelOpen should be unchanged (routing proof)")
	}
}

// TestStateApplyRouting_ClipboardImage proves the clipboard-image handler
// routes the pending-image append through apply.AddPendingImage.
func TestStateApplyRouting_ClipboardImage(t *testing.T) {
	m := newTestTUI(t)
	fake := &recordingApply{}
	m.apply = fake

	m = step(m, clipboardImageMsg{Img: proxy.ImagePart{Path: "x.png", MIME: "image/png"}})

	if !fake.saw("AddPendingImage") {
		t.Fatalf("expected apply.AddPendingImage; got %v", fake.calls)
	}
	if len(m.app.PendingImages) != 0 {
		t.Error("real App.PendingImages should be unchanged (routing proof)")
	}
}

// TestStateApplyRouting_CtxLimit proves the BackendCtxLimitMsg handler routes
// through apply.SetCtxLimit and does not touch the real App field.
func TestStateApplyRouting_CtxLimit(t *testing.T) {
	m := newTestTUI(t)
	fake := &recordingApply{}
	m.apply = fake

	m = step(m, agent.BackendCtxLimitMsg{Limit: agent.ContextLimit{NCtx: 12345}})

	if !fake.saw("SetCtxLimit") {
		t.Fatalf("expected apply.SetCtxLimit; got %v", fake.calls)
	}
	if m.app.CtxLimit.NCtx != 0 {
		t.Errorf("real App.CtxLimit should be unchanged (routing proof); got %d", m.app.CtxLimit.NCtx)
	}
}
