package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/staging"
)

// stagingListCap is the maximum number of keys returned by a single staging_list
// call. If more keys match, the response includes a truncation marker.
const stagingListCap = 200

// stagingUnavailable is the response for all staging tools when kvr is not
// available (disabled, direct mode, or failed readiness).
const stagingUnavailable = "staging unavailable (requires docker mode with kvr enabled)"

// getStagingClient returns the staging client, or nil if unavailable.
func (a *App) getStagingClient() *staging.Client {
	return a.StagingClient
}

// prefixedKey prepends the agent's prefix to the key. The tool layer
// UNCONDITIONALLY prepends — agents cannot write outside their prefix.
func (a *App) prefixedKey(key string) string {
	return a.AgentPrefix + "/" + key
}

// handleStagingPut handles staging_put: SET or SETX (when ttl_seconds present).
func (a *App) handleStagingPut(ctx context.Context, tc proxy.ToolCall) string {
	c := a.getStagingClient()
	if c == nil {
		return stagingUnavailable
	}
	var args struct {
		Key        string `json:"key"`
		Value      string `json:"value"`
		TTLSeconds *int   `json:"ttl_seconds,omitempty"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	if args.Key == "" {
		return "ERROR: key is required"
	}
	fullKey := a.prefixedKey(args.Key)
	if args.TTLSeconds != nil && *args.TTLSeconds > 0 {
		if err := c.SetEx(ctx, fullKey, args.Value, time.Duration(*args.TTLSeconds)*time.Second); err != nil {
			return fmt.Sprintf("ERROR: staging put (ttl): %v", err)
		}
		return fmt.Sprintf("staged (ttl=%ds): %s", *args.TTLSeconds, fullKey)
	}
	if err := c.Set(ctx, fullKey, args.Value); err != nil {
		return fmt.Sprintf("ERROR: staging put: %v", err)
	}
	return "staged: " + fullKey
}

// handleStagingGet handles staging_get: GET by full key (cross-prefix allowed).
func (a *App) handleStagingGet(ctx context.Context, tc proxy.ToolCall) string {
	c := a.getStagingClient()
	if c == nil {
		return stagingUnavailable
	}
	var args struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	if args.Key == "" {
		return "ERROR: key is required"
	}
	val, ok, err := c.Get(ctx, args.Key)
	if err != nil {
		return fmt.Sprintf("ERROR: staging get: %v", err)
	}
	if !ok {
		return "not found: " + args.Key
	}
	return val
}

// handleStagingDelete handles staging_delete: DEL by prefixed key.
func (a *App) handleStagingDelete(ctx context.Context, tc proxy.ToolCall) string {
	c := a.getStagingClient()
	if c == nil {
		return stagingUnavailable
	}
	var args struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	if args.Key == "" {
		return "ERROR: key is required"
	}
	fullKey := a.prefixedKey(args.Key)
	deleted, err := c.Del(ctx, fullKey)
	if err != nil {
		return fmt.Sprintf("ERROR: staging delete: %v", err)
	}
	if !deleted {
		return "not found: " + fullKey
	}
	return "deleted: " + fullKey
}

// handleStagingList handles staging_list: SCAN with optional prefix,
// paginated internally, capped at stagingListCap.
func (a *App) handleStagingList(ctx context.Context, tc proxy.ToolCall) string {
	c := a.getStagingClient()
	if c == nil {
		return stagingUnavailable
	}
	var args struct {
		Prefix string `json:"prefix,omitempty"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}

	// Paginate through SCAN results until we reach the cap or exhaust the store.
	var allKeys []string
	cursor := ""
	for len(allKeys) < stagingListCap {
		result, err := c.Scan(ctx, args.Prefix, stagingListCap-len(allKeys), cursor)
		if err != nil {
			return fmt.Sprintf("ERROR: staging list: %v", err)
		}
		allKeys = append(allKeys, result.Keys...)
		if !result.More || len(result.Keys) == 0 {
			break
		}
		cursor = result.Cursor
	}
	truncated := len(allKeys) >= stagingListCap
	var b strings.Builder
	for _, k := range allKeys {
		b.WriteString(k)
		b.WriteByte('\n')
	}
	if truncated {
		b.WriteString("... (truncated at 200 keys; use a more specific prefix to see more)\n")
	}
	if len(allKeys) == 0 {
		return "(no keys found" + maybePrefix(args.Prefix) + ")"
	}
	return strings.TrimSpace(b.String())
}

func maybePrefix(prefix string) string {
	if prefix == "" {
		return ""
	}
	return " matching prefix " + prefix
}

// handleStagingGetMany handles staging_get_many: MGET with a JSON array of keys.
func (a *App) handleStagingGetMany(ctx context.Context, tc proxy.ToolCall) string {
	c := a.getStagingClient()
	if c == nil {
		return stagingUnavailable
	}
	var args struct {
		Keys []string `json:"keys"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	if len(args.Keys) == 0 {
		return "ERROR: keys array is required"
	}
	values, err := c.MGet(ctx, args.Keys)
	if err != nil {
		return fmt.Sprintf("ERROR: staging get_many: %v", err)
	}
	var b strings.Builder
	for i, key := range args.Keys {
		val := values[i]
		if val == nil {
			b.WriteString(key)
			b.WriteString(": (not found)\n")
		} else {
			b.WriteString(key)
			b.WriteString(": ")
			b.Write(val)
			b.WriteByte('\n')
		}
	}
	return strings.TrimSpace(b.String())
}
