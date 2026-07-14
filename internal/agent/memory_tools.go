package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/treeol/wakil/internal/memory"
	"github.com/treeol/wakil/internal/proxy"
)

// memoryUnavailable is the response for all memory tools when the store
// is not available (init failed, no workspace, read-only filesystem).
const memoryUnavailable = "memory unavailable (could not open store)"

// TTL bounds: mid-tier requires 1h–7d.
const (
	memoryTTMin = 3600   // 1 hour
	memoryTTMax = 604800 // 7 days
)

// getMemoryStore returns the memory store, or nil if unavailable.
func (a *App) getMemoryStore() *memory.Store {
	return a.MemoryStore
}

// memoryMainOnlyDenied is the error returned when a subagent calls a
// main-agent-only memory tool. The error message is stable for tests.
const memoryMainOnlyDenied = "ERROR: this memory tool is main-agent only — subagents can propose but not promote, reject, forget, or bridge from staging."

// handleMemoryPut handles memory_put: mid-tier (TTL) or durable-tier (proposed).
func (a *App) handleMemoryPut(ctx context.Context, tc proxy.ToolCall) string {
	s := a.getMemoryStore()
	if s == nil {
		return memoryUnavailable
	}
	var args struct {
		Key        string   `json:"key"`
		Value      string   `json:"value"`
		Kind       string   `json:"kind"`
		TTLSeconds *int     `json:"ttl_seconds,omitempty"`
		Anchors    []string `json:"anchors,omitempty"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	if args.Key == "" {
		return "ERROR: key is required"
	}
	if args.Value == "" {
		return "ERROR: value is required"
	}
	if args.Kind == "" {
		return "ERROR: kind is required"
	}

	tainted := a.computeTainted()

	if args.TTLSeconds != nil {
		// Mid-tier: TTL required, bounds enforced.
		if *args.TTLSeconds < memoryTTMin || *args.TTLSeconds > memoryTTMax {
			return fmt.Sprintf("ERROR: ttl_seconds must be between %d and %d (1h–7d)", memoryTTMin, memoryTTMax)
		}
		expiresAt := time.Now().Add(time.Duration(*args.TTLSeconds) * time.Second).UnixMilli()
		entry, err := s.PutActive(ctx, args.Key, args.Value, args.Kind, memory.TierMid,
			a.AgentPrefix, a.chatID(), tainted, &expiresAt, args.Anchors, "")
		if err != nil {
			return fmt.Sprintf("ERROR: memory put: %v", err)
		}
		return fmt.Sprintf("stored (mid-tier, expires in %ds): %s [id: %d]\n%s",
			*args.TTLSeconds, args.Key, entry.ID, renderProvenance(entry))
	}

	// Durable-tier: always proposed, for everyone.
	entry, err := s.PutProposed(ctx, args.Key, args.Value, args.Kind,
		a.AgentPrefix, a.chatID(), tainted, args.Anchors, "")
	if err != nil {
		return fmt.Sprintf("ERROR: memory put: %v", err)
	}
	return fmt.Sprintf("proposed (durable, awaiting promotion): %s [id: %d]\n%s",
		entry.Key, entry.ID, renderProvenance(entry))
}

// handleMemoryPromote handles memory_promote: main-agent-only.
func (a *App) handleMemoryPromote(ctx context.Context, tc proxy.ToolCall) string {
	if a.IsSubagent {
		return memoryMainOnlyDenied
	}
	s := a.getMemoryStore()
	if s == nil {
		return memoryUnavailable
	}
	var args struct {
		ID          int64   `json:"id"`
		EditedValue *string `json:"edited_value,omitempty"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}

	entry, err := s.Promote(ctx, args.ID, args.EditedValue, a.AgentPrefix)
	if err != nil {
		return fmt.Sprintf("ERROR: memory promote: %v", err)
	}
	return fmt.Sprintf("promoted to active: %s\n%s\n%s",
		entry.Key, renderProvenance(entry), entry.Value)
}

// handleMemoryReject handles memory_reject: main-agent-only.
func (a *App) handleMemoryReject(ctx context.Context, tc proxy.ToolCall) string {
	if a.IsSubagent {
		return memoryMainOnlyDenied
	}
	s := a.getMemoryStore()
	if s == nil {
		return memoryUnavailable
	}
	var args struct {
		ID     int64  `json:"id"`
		Reason string `json:"reason,omitempty"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}

	if err := s.Reject(ctx, args.ID, args.Reason); err != nil {
		return fmt.Sprintf("ERROR: memory reject: %v", err)
	}
	return fmt.Sprintf("rejected: entry %d", args.ID)
}

// handleMemoryGet handles memory_get: available to all tiers.
func (a *App) handleMemoryGet(ctx context.Context, tc proxy.ToolCall) string {
	s := a.getMemoryStore()
	if s == nil {
		return memoryUnavailable
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

	entry, err := s.Get(ctx, args.Key)
	if err == memory.ErrNotFound {
		return "not found: " + args.Key
	}
	if err != nil {
		return fmt.Sprintf("ERROR: memory get: %v", err)
	}
	history := renderSupersedesHistory(ctx, s, entry)
	return fmt.Sprintf("%s [id: %d]\n%s%s\n%s", args.Key, entry.ID, renderProvenance(entry), history, entry.Value)
}

// handleMemorySearch handles memory_search: available to all tiers.
func (a *App) handleMemorySearch(ctx context.Context, tc proxy.ToolCall) string {
	s := a.getMemoryStore()
	if s == nil {
		return memoryUnavailable
	}
	var args struct {
		Query           string `json:"query"`
		Tier            string `json:"tier,omitempty"`
		IncludeProposed any    `json:"include_proposed,omitempty"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	if args.Query == "" {
		return "ERROR: query is required"
	}

	// Accept include_proposed as boolean (true/false) or string ("true").
	includeProposed := false
	switch v := args.IncludeProposed.(type) {
	case bool:
		includeProposed = v
	case string:
		includeProposed = strings.EqualFold(v, "true")
	}
	entries, err := s.Search(ctx, args.Query, args.Tier, includeProposed)
	if err != nil {
		return fmt.Sprintf("ERROR: memory search: %v", err)
	}
	if len(entries) == 0 {
		return "(no matches)"
	}

	var b strings.Builder
	for i, e := range entries {
		if i > 0 {
			b.WriteString("\n---\n")
		}
		fmt.Fprintf(&b, "%s [id: %d]\n%s\n%s", e.Key, e.ID, renderProvenance(e), e.Value)
	}
	if len(entries) >= 20 {
		// Only show truncation marker if we might have more results.
		fmt.Fprintf(&b, "\n... (capped at %d results — narrow your query for more)", 20)
	}
	return b.String()
}

// handleMemoryList handles memory_list: available to all tiers.
func (a *App) handleMemoryList(ctx context.Context, tc proxy.ToolCall) string {
	s := a.getMemoryStore()
	if s == nil {
		return memoryUnavailable
	}
	var args struct {
		Prefix string `json:"prefix,omitempty"`
		Tier   string `json:"tier,omitempty"`
		Status string `json:"status,omitempty"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}

	entries, err := s.List(ctx, args.Prefix, args.Tier, args.Status)
	if err != nil {
		return fmt.Sprintf("ERROR: memory list: %v", err)
	}
	if len(entries) == 0 {
		return "(no entries found)"
	}

	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "[id: %d] %s %s\n", e.ID, e.Key, renderProvenance(e))
	}
	return strings.TrimRight(b.String(), "\n")
}

// handleMemoryForget handles memory_forget: main-agent-only.
func (a *App) handleMemoryForget(ctx context.Context, tc proxy.ToolCall) string {
	if a.IsSubagent {
		return memoryMainOnlyDenied
	}
	s := a.getMemoryStore()
	if s == nil {
		return memoryUnavailable
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

	if err := s.Forget(ctx, args.Key); err != nil {
		return fmt.Sprintf("ERROR: memory forget: %v", err)
	}
	return "forgotten: " + args.Key
}

// handleMemoryPromoteFromStaging handles memory_promote_from_staging: main-agent-only.
func (a *App) handleMemoryPromoteFromStaging(ctx context.Context, tc proxy.ToolCall) string {
	if a.IsSubagent {
		return memoryMainOnlyDenied
	}
	s := a.getMemoryStore()
	if s == nil {
		return memoryUnavailable
	}
	sc := a.getStagingClient()
	if sc == nil {
		return stagingUnavailable
	}
	var args struct {
		StagingKey string   `json:"staging_key"`
		Key        string   `json:"key"`
		Kind       string   `json:"kind"`
		Anchors    []string `json:"anchors,omitempty"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	if args.StagingKey == "" || args.Key == "" || args.Kind == "" {
		return "ERROR: staging_key, key, and kind are required"
	}

	// Read the value from staging.
	value, ok, err := sc.Get(ctx, args.StagingKey)
	if err != nil {
		return fmt.Sprintf("ERROR: staging get: %v", err)
	}
	if !ok {
		return "ERROR: staging key not found: " + args.StagingKey
	}

	// Extract the original writer from the staging key's prefix.
	// The staging key is "<prefix>/<key>", where prefix is "main" or "sub-<id>".
	writer := "unknown"
	if idx := strings.Index(args.StagingKey, "/"); idx > 0 {
		writer = args.StagingKey[:idx]
	}

	// Staging-promoted entries are always tainted=unknown (staging values
	// carry no taint metadata — the original writer's grounding state is lost).
	entry, err := s.PutProposed(ctx, args.Key, value, args.Kind,
		writer, a.chatID(), memory.TaintUnknown, args.Anchors,
		fmt.Sprintf("promoted from staging key %s by %s", args.StagingKey, a.AgentPrefix))
	if err != nil {
		return fmt.Sprintf("ERROR: memory put (from staging): %v", err)
	}
	return fmt.Sprintf("proposed from staging (writer=%s, promoted_by=%s): %s\n%s",
		writer, a.AgentPrefix, renderProvenance(entry), value)
}

// ─── Rendering ─────────────────────────────────────────────────────────────

// renderSupersedesHistory follows the supersedes chain and renders a brief
// history line. Handles dangling references gracefully ("history unavailable"
// when a referenced entry has been hard-deleted past the auditability window).
func renderSupersedesHistory(ctx context.Context, s *memory.Store, e *memory.Entry) string {
	if e.Supersedes == nil {
		return ""
	}
	prev, err := s.GetByID(ctx, *e.Supersedes)
	if err == memory.ErrNotFound {
		return "\n  supersedes: entry #" + fmt.Sprintf("%d", *e.Supersedes) + " (history unavailable — hard-deleted past audit window)"
	}
	if err != nil {
		return ""
	}
	return fmt.Sprintf("\n  supersedes: #%d (%s, by %s, %s)",
		prev.ID, prev.Status, prev.Writer,
		time.UnixMilli(prev.CreatedAt).UTC().Format("2006-01-02"))
}

// renderProvenance renders the one-line provenance header for an entry.
// Compact, always present, shows: tier, writer, created date, expiry,
// taint status, anchor staleness, promotion info.
//
// Examples:
//
//	[mid-tier | sub-4f2a91c3 | 2026-07-14 | expires 2026-07-16 | tainted]
//	[durable | main | promoted 2026-07-10 | anchors: 1 stale of 2]
//	[durable | sub-abc12345 | proposed | taint-unknown]
func renderProvenance(e *memory.Entry) string {
	var parts []string

	parts = append(parts, e.Tier+"-tier")

	parts = append(parts, e.Writer)

	// Created date.
	createdStr := time.UnixMilli(e.CreatedAt).UTC().Format("2006-01-02")
	parts = append(parts, createdStr)

	// Status (show non-active statuses explicitly).
	if e.Status != memory.StatusActive {
		parts = append(parts, e.Status)
	}

	// Expiry (mid-tier only).
	if e.ExpiresAt != nil {
		expiresStr := time.UnixMilli(*e.ExpiresAt).UTC().Format("2006-01-02")
		parts = append(parts, "expires "+expiresStr)
	}

	// Promotion info. Note: for edited-value promotion, CreatedAt is the
	// promotion time (a new entry is created). For in-place promotion,
	// CreatedAt is the original proposal time — so we show "promoted by X"
	// without a date to avoid implying the creation date is the promotion date.
	if e.PromotedBy != "" {
		parts = append(parts, "promoted by "+e.PromotedBy)
	}

	// Taint flag.
	switch e.Tainted {
	case memory.TaintTrue:
		parts = append(parts, "tainted")
	case memory.TaintUnknown:
		parts = append(parts, "taint-unknown")
	}

	// Anchor staleness.
	if e.TotalAnchors > 0 && e.StaleAnchors > 0 {
		parts = append(parts, fmt.Sprintf("anchors: %d stale of %d", e.StaleAnchors, e.TotalAnchors))
	}

	return "[" + strings.Join(parts, " | ") + "]"
}

// ─── Taint signal ──────────────────────────────────────────────────────────

// addExternalGrounding is the single entry point for recording external
// content exposure. It calls Client.AddGrounding AND eagerly sets the sticky
// taint flag (A1: session-cumulative). This must be used instead of
// Client.AddGrounding directly for any web/oracle/MCP grounding, so the
// flag latches at exposure time — not lazily at memory_put time.
//
// The flag is never reset for the App's lifetime. ResetGrounding clears the
// Client's per-turn grounding slice but does NOT clear this sticky flag.
// Subagent Apps start fresh (their own exposure only); the main App latches
// for the session.
func (a *App) addExternalGrounding(e proxy.GroundingEntry) {
	if a.Client != nil {
		a.Client.AddGrounding(e)
	}
	if e.Type == "web" || e.Type == "oracle" {
		a.touchedExternal = true
	}
}

// computeTainted returns the taint value for a memory write based on the
// App's session-cumulative external exposure (A1).
//
// The flag is set eagerly by addExternalGrounding at exposure time, not
// scanned here. This means that if the agent fetched web content in turn 1,
// and ResetGrounding cleared the per-turn grounding slice at the start of
// turn 2, the flag is still set when memory_put is called in turn 2.
//
// Returns:
//   - memory.TaintTrue (1) if the agent has touched external content this session
//   - memory.TaintUnknown (2) otherwise (absence of web/oracle grounding
//     doesn't prove no untrusted content was touched — file-read injection
//     is not captured by this signal)
func (a *App) computeTainted() int {
	if a.touchedExternal {
		return memory.TaintTrue
	}
	return memory.TaintUnknown
}
