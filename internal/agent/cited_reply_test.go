package agent

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// writeCitedReply answers like a diligent reviewer: it reads the request the
// fake oracle received, finds the last heading / "── label ──" block of the
// document and ends its text with the mandatory "LAST SECTION READ:" citation.
// A fake that returns fixed text without it is (correctly) VOID.
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
	out, _ := json.Marshal(map[string]any{
		"content":     []map[string]string{{"type": "text", "text": text}},
		"stop_reason": "end_turn",
		"usage":       json.RawMessage(usageJSON),
	})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}
