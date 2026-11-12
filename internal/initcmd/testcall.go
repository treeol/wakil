package initcmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/treeol/wakil/internal/proxy"
)

// mapTestFailure turns an HTTP failure of the test call into the one-line
// message init shows. key (may be "") is scrubbed from any server text.
func mapTestFailure(status int, body, key string) string {
	switch {
	case status == 401 || status == 403:
		return fmt.Sprintf("key rejected (HTTP %d)", status)
	case proxy.IsPrivacyPolicyError(status, body):
		return proxy.PrivacyPolicyMessage
	case status == 404:
		return "model not found (HTTP 404)"
	case status == 429:
		return "rate limited (HTTP 429; free models: 50 requests/day without credits)"
	}
	return fmt.Sprintf("HTTP %d: %s", status, oneLine(scrub(body, key), 200))
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// scrub removes every occurrence of the secret from s.
func scrub(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "[redacted]")
}

// testCall sends one chat request with a trivial tool definition and reports
// whether a tool call came back. msg is the one-line failure reason. Nothing
// the server says is shown without scrubbing the key from it.
//
// The request goes through the single transport (proxy.Exec), so the data
// policy applies to init-time test calls exactly as it does to live calls:
// with both switches off the wire bytes are identical to the old direct
// POST, and with zdr/region on the init test call is enforced (or refused
// before sending) like every other call.
func testCall(ctx context.Context, client *proxy.Client, baseURL, model, authHeader string) (ok bool, msg string) {
	key := strings.TrimPrefix(authHeader, "Bearer ")
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 512,
		"messages": []map[string]any{
			{"role": "user", "content": "Call the ping tool now. Do not answer in text."},
		},
		"stream": true, // the transport streams; a JSON-answering server is still handled (parseChatChunk)
		"tools": []map[string]any{{
			"type": "function",
			"function": map[string]any{
				"name":        "ping",
				"description": "Health check. Always call this tool.",
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
			},
		}},
	})
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	headers := map[string]string{}
	if authHeader != "" {
		headers["Authorization"] = authHeader
	}
	res, err := client.Exec(ctx, proxy.ChatRequest{
		BaseURL:      baseURL + "/v1/chat/completions",
		Model:        model,
		Body:         body,
		Headers:      headers,
		TotalTimeout: 90 * time.Second,
	})
	if err != nil {
		var hsErr *proxy.HTTPStatusError
		if errors.As(err, &hsErr) {
			return false, mapTestFailure(hsErr.StatusCode, hsErr.Body, key)
		}
		var pref *proxy.PolicyRefusedError
		if errors.As(err, &pref) {
			return false, "the data policy refused the request: " + pref.Err.Error()
		}
		return false, "cannot reach the endpoint: " + oneLine(scrub(err.Error(), key), 200)
	}
	if len(res.ToolCalls) > 0 {
		return true, ""
	}
	return false, "model did not call the tool; tool calling may be unsupported"
}
