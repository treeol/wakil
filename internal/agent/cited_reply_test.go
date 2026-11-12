package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// writeCitedReply answers like a diligent reviewer: it reads the request the
// fake oracle received, finds the last heading / "── label ──" block of the
// document and ends its text with the mandatory "LAST SECTION READ:" citation.
// A fake that returns fixed text without it is (correctly) VOID.
//
// The response is sent as an Anthropic SSE stream (message_start,
// content_block_delta, message_delta, message_stop) because
// callOracleWithReasoning now sends stream:true.
func writeCitedReply(w http.ResponseWriter, r *http.Request, text, usageJSON string) {
	raw, _ := io.ReadAll(r.Body)
	var body struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(raw, &body)
	user := ""
	if len(body.Messages) > 0 {
		// The reviewer payload is the last user message (Messages[0] is the
		// system prompt on the single-transport wire shape).
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
	// Parse the usage JSON to extract input/output tokens for the SSE events.
	var usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	}
	_ = json.Unmarshal([]byte(usageJSON), &usage)

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	// OpenRouter chat.completions streaming shape (the single transport reads
	// this): content chunks, a finish chunk, then the trailing usage.
	fmt.Fprintf(w, "data: %s\n\n", mustJSONChunk(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": text}, "finish_reason": nil}}}))
	fmt.Fprintf(w, "data: %s\n\n", mustJSONChunk(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}}}))
	fmt.Fprintf(w, "data: %s\n\n", mustJSONChunk(map[string]any{"choices": []any{}, "usage": map[string]any{"prompt_tokens": usage.InputTokens, "completion_tokens": usage.OutputTokens}}))
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func mustJSONChunk(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
