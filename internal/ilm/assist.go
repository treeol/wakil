package ilm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// assistTimeout is the maximum time to wait for a /v1/assist response.
// Per C1: 800 ms. Any non-200, timeout, or abstain → proceed as without assist.
const assistTimeout = 800 * time.Millisecond

// AssistRequest is the POST /v1/assist request body (per the served contract).
type AssistRequest struct {
	SessionID string `json:"session_id"`
	Seq       int    `json:"seq"`
}

// AssistResponse is the POST /v1/assist response body (per the served contract).
type AssistResponse struct {
	Action              json.RawMessage `json:"action"` // null when abstain
	Abstain             bool            `json:"abstain"`
	Reason              string          `json:"reason,omitempty"`
	GateProbability     float64         `json:"gate_probability"`
	DeciderProbability  *float64        `json:"decider_probability"`
	CandidateProvenance json.RawMessage `json:"candidate_provenance"`
	DeciderID           string          `json:"decider_id,omitempty"`
	GateSHA256          string          `json:"gate_sha256,omitempty"`
	K                   int             `json:"k,omitempty"`
	Threshold           float64         `json:"threshold,omitempty"`
	LatencyMS           float64         `json:"latency_ms,omitempty"`
}

// AssistAction is the parsed action from the response (when action is non-null).
type AssistAction struct {
	Kind string          `json:"b"`   // must be "tool"
	Name string          `json:"name"` // tool name, e.g. "read_file"
	Args json.RawMessage `json:"args"` // tool arguments
}

// AssistClient posts to /v1/assist. It is separate from the Emitter because
// the assist call is synchronous and on the critical path (unlike the
// emitter's async event pipeline). A nil AssistClient is a no-op.
type AssistClient struct {
	endpoint string // base URL, e.g. "http://smaragd.local:8400"
	token    string // bearer token
	http     *http.Client
}

// NewAssistClient creates an AssistClient. endpoint is the ilm-stack base URL.
func NewAssistClient(endpoint, token string) *AssistClient {
	return &AssistClient{
		endpoint: endpoint,
		token:    token,
		http: &http.Client{
			Timeout: assistTimeout,
		},
	}
}

// Query POSTs to /v1/assist with the given session_id and seq. Returns the
// parsed response and nil error on 200; returns nil and an error on any
// non-200, timeout, or transport failure (C1: proceed as without assist).
func (c *AssistClient) Query(ctx context.Context, sessionID string, seq int) (*AssistResponse, error) {
	if c == nil {
		return nil, fmt.Errorf("assist: client is nil")
	}

	body, err := json.Marshal(AssistRequest{
		SessionID: sessionID,
		Seq:       seq,
	})
	if err != nil {
		return nil, fmt.Errorf("assist: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.endpoint+"/v1/assist", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("assist: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("assist: request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("assist: HTTP %d", resp.StatusCode)
	}

	// Limit the response body to prevent unbounded reads (defense in depth).
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB max
	if err != nil {
		return nil, fmt.Errorf("assist: read body: %w", err)
	}

	var ar AssistResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		return nil, fmt.Errorf("assist: parse response: %w", err)
	}

	return &ar, nil
}

// ParseAction extracts the AssistAction from an AssistResponse when action
// is non-null. Returns nil when the action is null (abstain).
func (r *AssistResponse) ParseAction() (*AssistAction, error) {
	if r == nil || r.Abstain || len(r.Action) == 0 || string(r.Action) == "null" {
		return nil, nil
	}
	var a AssistAction
	if err := json.Unmarshal(r.Action, &a); err != nil {
		return nil, fmt.Errorf("assist: parse action: %w", err)
	}
	// Validate the action kind — only "tool" actions are supported.
	if a.Kind != "tool" {
		return nil, fmt.Errorf("assist: unexpected action kind %q (expected \"tool\")", a.Kind)
	}
	if a.Name == "" {
		return nil, fmt.Errorf("assist: action has empty tool name")
	}
	return &a, nil
}
