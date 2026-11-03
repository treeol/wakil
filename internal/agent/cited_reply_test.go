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
		user = body.Messages[0].Content
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
	fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":%d,\"output_tokens\":0}}}\n\n", usage.InputTokens)
	fmt.Fprintf(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"content_block\":{\"type\":\"text\"}}\n\n")
	fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", text)
	fmt.Fprintf(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\"}\n\n")
	fmt.Fprintf(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":%d}}\n\n", usage.OutputTokens)
	fmt.Fprintf(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
}
