package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/core/event"
)

// TestE2EGrantAtWriteFileApproval drives a write_file tool call through
// the real TUI approval gate and grants it with "g". The durable
// approval_resolved must carry outcome "granted", the file must be
// written, and the follow-up model request must carry a tool result that
// does not decline the write.
//
// Before the fix the durable event log rejected the outcome "granted", the turn failed and the file was not written.
func TestE2EGrantAtWriteFileApproval(t *testing.T) {
	e := startE2E(t, e2eOpts{
		script: [][]string{
			e2eToolCallFrames("call_1", "write_file", `{"path":"answer.txt","content":"hello"}`),
			{e2eContentChunk("done")},
		},
	})
	// Wait for the real model: the input placeholder is rendered only
	// once the swap installs the real TUI model.
	e.waitScreen("type a task", 30*time.Second)
	e.waitKind(event.KindSessionCreated, 1, 10*time.Second)

	e.submit("write the answer")
	// The prompt text comes from fmtConfirmBlock
	// (internal/tui/tui_msgs.go:104) and carries the grant choice when
	// the call is grant-eligible.
	e.waitScreen("[g] grant for this session", 30*time.Second)
	e.waitKind(event.KindApprovalRequested, 1, 10*time.Second)
	e.key("g")
	e.waitTurnDone(30 * time.Second)

	// The granted write must really land in the temp workdir.
	data, err := os.ReadFile(filepath.Join(e.workdir, "answer.txt"))
	if err != nil {
		t.Fatalf("answer.txt not written: %v\n%s", err, e.tail(2000))
	}
	if string(data) != "hello" {
		t.Fatalf("answer.txt = %q, want %q", data, "hello")
	}

	evs, err := e.events()
	if err != nil {
		t.Fatalf("events() read error (a finding, not swallowed): %v", err)
	}
	var resolved []event.ApprovalResolved
	for _, ev := range evs {
		switch ev.Kind {
		case event.KindApprovalResolved:
			resolved = append(resolved, ev.Payload.(event.ApprovalResolved))
		case event.KindSessionError:
			t.Fatalf("unexpected session_error event: %+v", ev.Payload)
		}
	}
	if len(resolved) != 1 {
		t.Fatalf("approval_resolved count = %d, want exactly 1\n%s", len(resolved), e.tail(2000))
	}
	if resolved[0].Outcome != "granted" {
		t.Fatalf("approval_resolved outcome = %q, want \"granted\"", resolved[0].Outcome)
	}
	if scr := e.screen(); strings.Contains(scr, "durable emit failed") || strings.Contains(scr, "internal error") {
		t.Fatalf("screen shows a durable/turn error:\n%s", e.tail(2000))
	}

	// The second model request must carry the tool result, and it must
	// not say the write was declined ("[declined by user]" is the
	// decline wording for tool results).
	e.waitRequests(2, 10*time.Second)
	reqs := e.requests()
	var body struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(reqs[1], &body); err != nil {
		t.Fatalf("second request body is not JSON: %v", err)
	}
	toolText := ""
	for _, m := range body.Messages {
		if m.Role != "tool" {
			continue
		}
		var s string
		if err := json.Unmarshal(m.Content, &s); err != nil {
			s = string(m.Content)
		}
		toolText = s
	}
	if toolText == "" {
		t.Fatalf("second request has no tool-role message:\n%s", reqs[1])
	}
	if strings.Contains(toolText, "[declined by user]") {
		t.Fatalf("tool result declines the granted write: %q", toolText)
	}
}
