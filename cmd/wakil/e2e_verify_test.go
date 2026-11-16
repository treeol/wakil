package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// verifyToolCall is one decoded tool call in a recorded request.
type verifyToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// verifyMessage is one decoded message in a recorded request.
type verifyMessage struct {
	Role       string
	Content    string
	ToolCalls  []verifyToolCall
	ToolCallID string
}

// verifyContentText extracts text from a raw message content value.
func verifyContentText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, part := range parts {
			b.WriteString(part.Text)
		}
		if b.Len() > 0 {
			return b.String()
		}
	}
	return string(raw)
}

// verifyDecodeMessages decodes the messages of one recorded request body.
func verifyDecodeMessages(t *testing.T, body []byte) []verifyMessage {
	t.Helper()
	var req struct {
		Messages []struct {
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	out := make([]verifyMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		msg := verifyMessage{Role: m.Role, Content: verifyContentText(m.Content), ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			msg.ToolCalls = append(msg.ToolCalls, verifyToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
		}
		out = append(out, msg)
	}
	return out
}

// verifyUserContent returns the first user message content.
func verifyUserContent(msgs []verifyMessage) (string, bool) {
	for _, m := range msgs {
		if m.Role == "user" {
			return m.Content, true
		}
	}
	return "", false
}

// TestE2EHandoffKeepsTaskText checks a handoff rotation keeps the task text.
func TestE2EHandoffKeepsTaskText(t *testing.T) {
	const taskText = "Please write a summary of this task into summary.txt in one line."
	turns := map[string][][]string{
		"write a summary": {
			e2eToolCallFrames("s1", "write_file", `{"path":"summary.txt","content":"The task was to write a summary."}`),
			{e2eContentChunk("done")},
		},
		"continue writing": {
			{e2eContentChunk("continued")},
		},
	}
	e := startE2E(t, e2eOpts{Turns: turns})
	e2eReady(t, e)
	e.submit(taskText)
	e2eAnswerApprovals(t, e, 1)
	e.waitTurnDone(30 * time.Second)

	beforeHandoff := len(e.requests())
	e.submitRotation("/handoff")
	var ids []string
	rotated := false
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		got, err := e.sessionIDs()
		if err == nil && len(got) >= 2 {
			ids = got
			rotated = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !rotated {
		ids, _ = e.sessionIDs()
		t.Logf("handoff did not rotate to a second session (sessions=%v)\n%s", ids, e.tail(3000))
		t.Fatalf("handoff did not create a second session in this run (sessions=%v); the handoff command behaved differently than expected", ids)
	}
	e.submit("continue writing")
	e.waitTurns(2, 30*time.Second)

	post := e.requests()[beforeHandoff:]
	carrying := 0
	firstWithout := -1
	firstPostHasTask := false
	for i, body := range post {
		msgs := verifyDecodeMessages(t, body)
		contains := false
		for _, m := range msgs {
			if strings.Contains(m.Content, taskText) {
				contains = true
				break
			}
		}
		if contains {
			carrying++
		}
		if i == 0 {
			firstPostHasTask = contains
		}
		if !contains && firstWithout < 0 {
			firstWithout = i
		}
	}
	t.Logf("request bodies after handoff carrying exact task text: %d of %d examined, first post-handoff request has task text: %v, first without task text at index %d", carrying, len(post), firstPostHasTask, firstWithout)
	if carrying < 1 {
		t.Logf("screen tail after handoff:\n%s", e.tail(3000))
		t.Fatalf("no request after handoff carries the task text verbatim (sessions=%v, examined=%d); the handoff did not preserve the task in this run", ids, len(post))
	}
	if len(post) == 0 || !firstPostHasTask {
		t.Fatalf("first request after /handoff does not contain the exact task text verbatim (sessions=%v, examined=%d)", ids, len(post))
	}
}
