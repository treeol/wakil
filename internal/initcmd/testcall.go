package initcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/treeol/wakil/internal/proxy"
)

// mapTestFailure turns an HTTP failure of the test call into the one-line
// message init shows. key (may be "") is scrubbed from any server text.
func mapTestFailure(status int, body, key string) string {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fmt.Sprintf("key rejected (HTTP %d)", status)
	case proxy.IsPrivacyPolicyError(status, body):
		return proxy.PrivacyPolicyMessage
	case status == http.StatusNotFound:
		return "model not found (HTTP 404)"
	case status == http.StatusTooManyRequests:
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
func testCall(ctx context.Context, hc *http.Client, baseURL, model, authHeader string) (ok bool, msg string) {
	key := strings.TrimPrefix(authHeader, "Bearer ")
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 512,
		"messages": []map[string]any{
			{"role": "user", "content": "Call the ping tool now. Do not answer in text."},
		},
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return false, "cannot build the request: " + oneLine(scrub(err.Error(), key), 200)
	}
	req.Header.Set("Content-Type", "application/json")
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return false, "cannot reach the endpoint: " + oneLine(scrub(err.Error(), key), 200)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return false, mapTestFailure(resp.StatusCode, string(raw), key)
	}
	var out struct {
		Choices []struct {
			Message struct {
				ToolCalls []json.RawMessage `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return false, "the endpoint did not answer with an OpenAI-style JSON reply"
	}
	if len(out.Choices) == 0 && out.Error != nil {
		return false, "the provider returned an error: " + oneLine(scrub(out.Error.Message, key), 200)
	}
	for _, c := range out.Choices {
		if len(c.Message.ToolCalls) > 0 {
			return true, ""
		}
	}
	return false, "model did not call the tool; tool calling may be unsupported"
}
