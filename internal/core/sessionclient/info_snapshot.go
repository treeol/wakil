// InfoSnapshot: the narrow, immutable DTO the TUI's info panel and status
// line read instead of deep *agent.App internals (card #148 chunk 7b3 m4,
// per the review-panel guidance: "no vague Info() — a narrow, immutable
// InfoSnapshot() DTO with defensive copies").
//
// Every field here is a display string or plain value the render path needs.
// It is fetched on-demand (info panel open / status line render), NOT folded
// into ClientSnapshot — the fields are cheap but numerous, and ClientSnapshot
// is re-fetched on every event batch.

package sessionclient

import (
	"github.com/treeol/wakil/internal/proxy"
)

// InfoSnapshot is the immutable view of the deep session state the TUI's info
// panel (F2 / ctrl+o) and status line render. Constructed by the facade on
// demand; all slices are defensive copies.
type InfoSnapshot struct {
	// Identity / endpoint.
	ChatID      string // proxy chat ID (display only; ShortID'd by the TUI)
	BaseURL     string // backend endpoint URL
	LastBackend string // backend actually used by the last response
	Cwd         string // working directory inside the executor
	ExecMode    string // executor description ("direct", "docker", …)

	// LastLatencyMs is the time-to-first-byte latency from the most recent
	// Stream call. 0 = no measurement yet. Shown in the status line.
	LastLatencyMs int64

	// Model / backend selection (status line).
	SelectedBackend string
	ConfigBackend   string // the config-level default backend
	EffectiveModel  string
	SubagentModel   string

	// Prompt / config bits the panel displays.
	PromptNote  string   // agent prompt note (file the system prompt came from)
	Image       string   // container image name (docker mode)
	OracleLabel string   // Mashūra panel label, or "no key"
	OracleOn    bool     // oracle enabled
	SearXngURL  string   // SearXNG search endpoint ("" = default)
	MentionBase string   // @-mention root directory
	Endpoints   []string // endpoint names for /endpoint completion ("inherit" first)

	// Context gauge (status line ctx segment).
	ContextLimit ContextLimit
	ContextUsed  int  // tokens used by the assembled prompt
	ContextExact bool // whether Used is exact (vs an estimate)

	// Transcript stats.
	ConvLen        int // number of messages in the conversation
	TranscriptSize int // total transcript size (bytes, display-scaled by TUI)

	// Workflow sidebar label ("" when no workflow is active).
	WorkflowLabel string

	// InfoPanelOpen reports the persisted info-panel toggle (restored
	// per-session; the TUI seeds its toggle state from it).
	InfoPanelOpen bool

	// MCP servers: name → status string ("up", "down", …) plus tool count.
	MCPServers []MCPServerInfo

	// SearXNG tools available (for the tools segment).
	SearxngTools []string

	// Grounding entries (display): type + label pairs.
	Grounding []GroundingEntry

	// Costs is the live cost tracker (pointer — it is a synchronized object;
	// the TUI reads a split snapshot from it).
	Costs *proxy.CostTracker
}

// MCPServerInfo is one MCP server entry in the info panel.
type MCPServerInfo struct {
	Name   string
	Status string
	ToolN  int
}

// GroundingEntry is one grounding-entry display row.
type GroundingEntry struct {
	Type  string // "web", "oracle", "file", …
	Label string
}
