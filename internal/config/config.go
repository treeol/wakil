package config

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Endpoint kinds. "openai" is a plain OpenAI-compatible chat-completions
// server (llama.cpp server, OpenRouter, vLLM…); "ilm-proxy" is the ilm proxy
// with its memory/grounding extensions (X-Ilm-* headers, metadata routing,
// model aliasing).
const (
	EndpointKindOpenAI   = "openai"
	EndpointKindIlmProxy = "ilm-proxy"
)

// EndpointConfig is one named endpoint in the "endpoints" block.
//
// Kind defaults to "openai" when omitted. Model is REQUIRED for kind "openai"
// (it is the literal model string sent in every request); for kind "ilm-proxy"
// it defaults to the proxy alias "ilm". The sampling fields are pointers so
// "unset" is distinguishable from zero: unset fields are omitted from the
// request body entirely and the server's own defaults stay authoritative —
// Wakil never invents sampling defaults.
type EndpointConfig struct {
	Kind        string   `json:"kind,omitempty"`        // "openai" (default) | "ilm-proxy"
	BaseURL     string   `json:"base_url"`              // full URL, e.g. "http://llama-host:11400"
	Model       string   `json:"model,omitempty"`       // required for kind=openai; defaults to "ilm" for kind=ilm-proxy
	AuthHeader  string   `json:"auth_header,omitempty"` // verbatim Authorization value; empty = fall back to api_key
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	// MaxTokens is unset by default; kind=openai then falls back to a
	// client-side default (see proxy.Client.Stream) sized for reasoning
	// models. Set explicitly for models that need more (extended-thinking
	// hosted models can exhaust a low budget on thinking alone and return
	// finish_reason="length" with no content) or less (a small-context
	// local server).
	MaxTokens *int `json:"max_tokens,omitempty"`

	// CachePrompt is llama.cpp's non-standard "cache_prompt" hint, sent
	// verbatim when set. Not gated on Kind — kind=openai also covers
	// OpenRouter/vLLM, which don't understand this field — so it is an
	// explicit per-endpoint opt-in, pointer-typed like the sampling fields
	// above: unset omits the field entirely, never sends a literal false.
	CachePrompt *bool `json:"cache_prompt,omitempty"`

	// CacheControl enables Anthropic-style prompt caching via cache_control
	// breakpoints on message content parts. When set, Stream injects two
	// ephemeral breakpoints on the wire copy of the request: one on
	// messages[0] (the day-stable preamble) and one on the last non-null-
	// content message. The stored transcript is never modified — decoration
	// is transient and per-request. Independent of CachePrompt (different
	// mechanisms, different providers); both may be set on the same endpoint.
	CacheControl *bool `json:"cache_control,omitempty"`

	// AppReferer, AppTitle, and AppCategories are OpenRouter app attribution
	// headers (HTTP-Referer, X-Title, and X-OpenRouter-Categories).
	// Pointer-typed: nil = apply defaults for openrouter.ai hosts, non-nil =
	// use verbatim (including empty string to opt out of the header
	// entirely). Only sent for kind=openai endpoints; ilm-proxy is untouched.
	AppReferer    *string `json:"app_referer,omitempty"`
	AppTitle      *string `json:"app_title,omitempty"`
	AppCategories *string `json:"app_categories,omitempty"`
}

// Config is resolved with precedence: defaults < config file < env < flags.
// base_url takes precedence over host+port; if only host (and optionally port)
// are set, the URL is constructed as http://{host}:{port}.
type Config struct {
	BaseURL string `json:"base_url"` // full URL, e.g. "http://proxy-host:11400"
	Host    string `json:"host"`     // alternative to base_url: just the hostname/IP
	Port    int    `json:"port"`     // port for host (default 11400)

	// Endpoints is the named-endpoint block. When present it supersedes the
	// legacy top-level base_url/model fields: DefaultEndpoint selects the
	// active entry. When absent, a single "ilm-proxy" endpoint is synthesized
	// from the legacy fields so existing configs behave exactly as before.
	// ILM_MODEL / --model and ILM_BASE_URL / --base-url, when explicitly set,
	// override the selected endpoint's model / base_url.
	Endpoints       map[string]EndpointConfig `json:"endpoints,omitempty"`
	DefaultEndpoint string                    `json:"default_endpoint,omitempty"`

	// Endpoint is the resolved active endpoint (runtime-only, never serialized).
	// Populated by LoadConfig; hand-built Configs should go through
	// ActiveEndpoint(), which falls back to the legacy fields when this is empty.
	Endpoint     EndpointConfig `json:"-"`
	EndpointName string         `json:"-"`

	APIKey       string `json:"api_key"` // sent as "Authorization: Bearer <key>"
	Model        string `json:"model"`
	ExecMode     string `json:"exec_mode"`               // "docker" (default) | "direct"
	Image        string `json:"image"`                   // container image for docker mode
	WorkDir      string `json:"work_dir"`                // working dir inside the container
	HostWorkDir  string `json:"host_work_dir,omitempty"` // host path mounted into container (files appear here)
	DockerSocket bool   `json:"docker_socket,omitempty"` // bind-mount the host docker socket into the sandbox (drive host docker from inside)
	// Docker hardening (defense-in-depth for the sandbox container).
	// DockerCaps re-adds specific capabilities after --cap-drop=ALL.
	// Empty = no caps re-added (strictest). Add "CHOWN" if go build fails.
	DockerCaps []string `json:"docker_caps,omitempty"`
	// DockerMemory limits container memory (e.g. "4g"). Empty = no limit.
	// Must be >= DockerTmpfsSize in practice — tmpfs pages count against the
	// container memory cgroup, so a tmpfs larger than the memory limit will
	// OOM before filling.
	DockerMemory string `json:"docker_memory,omitempty"`
	// DockerPidsLimit caps the number of processes. 0 = no limit.
	DockerPidsLimit int `json:"docker_pids_limit,omitempty"`
	// DockerTmpfsSize sets the /tmp tmpfs size (e.g. "4g"). Empty = use the
	// built-in default (currently "4g" — see defaultDockerTmpfsSize in
	// internal/exec). /tmp must stay writable under --read-only, so the flag
	// is always emitted — empty never means "omit".
	DockerTmpfsSize string `json:"docker_tmpfs_size,omitempty"`
	SSHSigning      string `json:"ssh_signing,omitempty"` // SSH commit signing in the sandbox: "off" (default) | "auto" (detect from host git config) | path to a .pub key

	// kvr staging store (sandbox-local ephemeral KV).
	// KVRDisabled opts out of the staging store (default: false = enabled).
	// When disabled, no kvr-server is started and staging tools report
	// "staging unavailable". Also auto-disabled in direct mode.
	KVRDisabled             bool `json:"kvr_disabled,omitempty"`
	KVRMaxEntries           int  `json:"kvr_max_entries,omitempty"`            // default 100000
	KVRSweepIntervalSecs    int  `json:"kvr_sweep_interval_secs,omitempty"`    // default 30
	KVRSnapshotIntervalSecs int  `json:"kvr_snapshot_interval_secs,omitempty"` // default 300

	KeepBytes      int `json:"keep_bytes"`       // max bytes of verbatim turns kept after compaction; default 120000
	SummaryBytes   int `json:"summary_bytes"`    // cap on the running summary; re-summarize if exceeded; default 20000; 0=unlimited
	HardMaxBytes   int `json:"hard_max_bytes"`   // unconditional ctx ceiling; compact+drop oldest until under; 0=disabled; default 160000
	TurnToolBudget int `json:"turn_tool_budget"` // per-turn cumulative tool output budget; reduced slice once exceeded; default 40000
	MaxChars       int `json:"max_chars"`        // transcript-byte display ceiling for the hist line / compaction fallback
	CompactAt      int `json:"compact_at"`       // trigger compaction at this size; 0 → use max_chars

	// Relative context guards — computed as fractions of the live backend's
	// usable context window (ContextLimit.Usable() × 4 chars/token). When n_ctx
	// is known these override the absolute values above, automatically scaling
	// with the window. When n_ctx is unknown the absolute values are the fallback.
	// All three must satisfy: 0 < KeepBytesFrac < CompactAtFrac < HardMaxFrac ≤ 1.
	CompactAtFrac float64 `json:"compact_at_frac"` // fraction of usable context to trigger compaction; default 0.75
	KeepBytesFrac float64 `json:"keep_bytes_frac"` // fraction of usable context to keep verbatim after compaction; default 0.60
	HardMaxFrac   float64 `json:"hard_max_frac"`   // fraction of usable context as unconditional ceiling; default 0.95

	// ContextCapacityFrac is headroom below the literal context ceiling.
	// effective_ctx = usable_ctx × context_capacity_frac, then CompactAtFrac /
	// KeepBytesFrac / HardMaxFrac are applied on top of effective_ctx. This is
	// deliberate — it gives the model working room below the proxy-reported
	// usable_ctx, which already has reasoning+answer margins subtracted.
	// Default 0.80 (80% of the proxy's usable budget). Set to 1.0 to use the
	// full ceiling with no additional headroom.
	ContextCapacityFrac float64 `json:"context_capacity_frac,omitempty"`

	// EffectiveCtxMaxChars is an absolute cap (in chars) on the effective context
	// used to compute compaction thresholds. When the backend reports a large
	// context window (e.g. 1M tokens → ~3.2M chars effective), models often
	// become unreliable past ~200k chars regardless of their theoretical limit.
	// This cap is applied as min(computed_effective_chars, cap) inside
	// activeThresholds(), before the fractions — so the keepBytes < compactAt <
	// hardMax hierarchy is preserved automatically. 0 = disabled (use full
	// backend-reported context). Can be overridden at runtime via /maxctx.
	EffectiveCtxMaxChars int `json:"effective_ctx_max_chars,omitempty"`

	// Backend-truth context sizing. The authoritative per-slot context window
	// (n_ctx, in tokens) is fetched from the backend at startup (see ctxlimit.go);
	// these are the headroom reservations and the fallback used only when that
	// fetch fails. All token-valued.
	ReasoningBudgetTokens int `json:"reasoning_budget_tokens,omitempty"` // tokens reserved for extended thinking; default 4096
	AnswerMarginTokens    int `json:"answer_margin_tokens,omitempty"`    // tokens reserved for the final answer; default 4096
	ContextTokensFallback int `json:"context_tokens_fallback,omitempty"` // n_ctx assumed when the backend fetch fails; default 131072
	ToolResultCap         int `json:"tool_result_cap"`                   // max chars kept in ctx per tool result; 0 = unlimited; default 8000
	ToolResultTTL         int `json:"tool_result_ttl"`                   // evict large tool results after N completed turns; -1 = never; default 1
	MaxToolIterations     int `json:"max_tool_iterations"`               // hard cap on tool round-trips per turn; on the last iteration tools are dropped to force a wrap-up answer; 0 = unlimited (parent default)

	// SubagentMaxToolIter caps tool round-trips per subagent dispatch. 0 = use
	// the built-in default (30). Unlike the parent's MaxToolIterations (0 =
	// unlimited), subagents always get a finite cap — they're autonomous workers
	// with no human gate.
	SubagentMaxToolIter int `json:"subagent_max_tool_iterations,omitempty"`

	// SubagentTurnToolBudget overrides the per-turn cumulative tool output
	// budget for subagents. 0 = use the built-in default. Automatically clamped
	// to 35% of the active hardMax at dispatch time, so it's safe to set high
	// even on small-context backends.
	SubagentTurnToolBudget int `json:"subagent_turn_tool_budget,omitempty"`

	// SubagentToolResultCap overrides the per-result char cap for subagents.
	// 0 = use the built-in default (12,000).
	SubagentToolResultCap int `json:"subagent_tool_result_cap,omitempty"`

	ReadFileSizeLimit int               `json:"read_file_size_limit,omitempty"` // max bytes read_file accepts before refusing; default 1048576 (1 MB); 0 = use default
	MaxFullReadBytes  int               `json:"max_full_read_bytes,omitempty"`  // max bytes read_file_full accepts before refusing; default 262144 (256 KB); 0 = use default
	MaxRequestBytes   int               `json:"max_request_bytes,omitempty"`    // pre-send byte guard: trim largest tool results if request exceeds this; default 8388608 (8 MB); 0 = disabled
	SearXngURL        string            `json:"searxng_url,omitempty"`          // native searxng_search tool if set
	GoogleAPIKey      string            `json:"google_api_key,omitempty"`       // Google Custom Search API key (enables native google_search tool)
	GoogleCX          string            `json:"google_cx,omitempty"`            // Google Programmable Search Engine ID
	MentionBase       string            `json:"mention_base,omitempty"`         // base dir for "@" file mentions (default: launch cwd)
	MCPServers        []MCPServerConfig `json:"mcp_servers,omitempty"`

	// AttachImage is the path to an image file to attach to the first user
	// message at startup. Set via --attach-image flag. The image is loaded
	// and encoded as an OpenAI-compatible image_url content block. Multiple
	// paths can be comma-separated; empty = no image attached.
	AttachImage string `json:"-"`

	// Mashūra: counsel tool that calls an external AI API for a second opinion.
	// The API key is read from OracleAPIKeyEnv at call time — never stored here or
	// in session files. "mashura_*" is the canonical config spelling; the legacy
	// "oracle_*" keys remain accepted and, when both are present, the mashura_*
	// value wins (merged in LoadConfig). The Go field names keep the Oracle prefix
	// to avoid churn — only the JSON wire spelling and user-facing strings changed.
	OracleEnabled        bool   `json:"oracle_enabled"`                   // gate: tool only declared when true AND key present
	OracleModel          string `json:"oracle_model"`                     // Anthropic model ID sent in the request
	OracleMaxTokens      int    `json:"oracle_max_tokens"`                // max_tokens for the mashūra response
	OracleAPIKeyEnv      string `json:"oracle_api_key_env"`               // env var that holds the API key
	OracleEndpoint       string `json:"oracle_endpoint,omitempty"`        // override API endpoint (tests / proxies; empty = Anthropic)
	OracleTimeoutSeconds int    `json:"oracle_timeout_seconds,omitempty"` // HTTP timeout for mashūra calls; 0 = use default (300s)
	// OpenRouterAPIKeyEnv is the env var name for the OpenRouter API key.
	// Defaults to "OPENROUTER_API_KEY". Used by mashura panel calls that route
	// through OpenRouter (provider prefix "openrouter:") and by Fusion mode.
	OpenRouterAPIKeyEnv string `json:"openrouter_api_key_env,omitempty"` // env var that holds the OpenRouter API key

	// Canonical mashura_* aliases. Empty/zero means "unset, fall back to oracle_*".
	// Merged into the Oracle* fields in LoadConfig so the rest of the code reads
	// one set of fields.
	MashuraMaxTokens      int    `json:"mashura_max_tokens,omitempty"`      // canonical alias of oracle_max_tokens
	MashuraTimeoutSeconds int    `json:"mashura_timeout_seconds,omitempty"` // canonical alias of oracle_timeout_seconds
	MashuraMode           string `json:"mashura_mode,omitempty"`            // canonical alias of wf_oracle_mode

	// MashuraToolMaxTokens overrides max_tokens per counsel tool, keyed by short
	// name ("review"|"debug"|"decide"|"check"). Unset tools fall back to a tool
	// default (check is lighter) then to the shared base above.
	MashuraToolMaxTokens map[string]int `json:"mashura_max_tokens_by_tool,omitempty"`

	// AutoCounsel controls the TUI session counsel mode: "suggest" (default,
	// hint-only), "auto" (auto-fire mashura__debug up to CounselMaxPerSession
	// calls per user turn), or "off" (detect but print no hint). Empty = "suggest".
	AutoCounsel string `json:"auto_counsel,omitempty"`

	// CounselMaxPerSession caps auto-counsel calls per user turn in "auto" mode.
	// Default 3 when auto_counsel is "auto" and this is unset.
	CounselMaxPerSession int `json:"counsel_max_per_session,omitempty"`

	// MashuraPanels defines named model panels. Each panel is a named group of
	// prefixed model strings ("anthropic:model-id" or "openrouter:model-id") with
	// an execution mode ("panel" = query all, collect all; "fallback" = try in
	// order, stop on first success). The "default" panel is used when no panel
	// override is given. Example JSON:
	//   "mashura_panels": {
	//     "default":   {"models": ["anthropic:claude-opus-4-8"]},
	//     "review":    {"models": ["anthropic:claude-opus-4-8", "openrouter:google/gemini-2.5-pro"], "mode": "panel"},
	//     "resilient": {"models": ["anthropic:claude-opus-4-8", "openrouter:anthropic/claude-3.7-sonnet"], "mode": "fallback"}
	//   }
	MashuraPanels map[string]MashuraPanelConfig `json:"mashura_panels,omitempty"`

	// MashuraToolPanels maps each mashura tool short name ("review"|"debug"|
	// "decide"|"check") to a named panel. Unmapped tools use the "default" panel.
	// TODO(per-tool-briefing): per-member briefing customization (e.g. language
	// hint, persona) is a deliberate future feature — do not add it here.
	MashuraToolPanels map[string]string `json:"mashura_tool_panels,omitempty"`

	// Workflow settings.
	WFOracleMode       string `json:"wf_oracle_mode,omitempty"`        // default oracle consult schedule: "every-step" | "on-deviation" | "phases-only"
	WFFinalReview      bool   `json:"wf_final_review"`                 // run closing oracle check after the last step (default true)
	WFBriefingMaxBytes int    `json:"wf_briefing_max_bytes,omitempty"` // max bytes for final-review briefing; 0 = 16384

	// Verify holds explicit verification commands for the workflow verification
	// runner. When non-empty, these commands are used instead of auto-detection.
	// When empty (default), the runner detects commands from project manifests
	// (go.mod, package.json, Cargo.toml, pyproject.toml). Commands run after
	// all implementation steps complete, before the final oracle review; a
	// non-zero exit fails verification and keeps the workflow open (exit code
	// ExitGaps in headless mode). See internal/verify/ for detection details.
	Verify []string `json:"verify,omitempty"`

	// Proxy backend selection. Backend is the default backend name attached as
	// X-Ilm-Backend on every chat request. Empty = let the proxy choose its default
	// (no header sent). ExternalBackends lists backend names known to route to
	// external/cloud providers; used as a fallback for the egress consent gate when
	// the proxy doesn't expose /v1/ilm/backends.
	Backend          string   `json:"backend,omitempty"`
	ExternalBackends []string `json:"external_backends,omitempty"`

	// AuxModel pins the X-Ilm-Aux-Model header sent on every chat request.
	// Empty (default) = send the main model so aux always follows main.
	// Set to a specific model ID (e.g. "anthropic/claude-haiku-4-5-20251001")
	// to use a cheaper model for the proxy's auxiliary (memory/compose) calls.
	AuxModel string `json:"aux_model,omitempty"`

	// SubagentBackend controls which backend dispatch_subagent uses.
	//   "inherit" (or ""): subagent uses the main session's current backend —
	//     least surprise; if you're on OR the subagent is too.
	//   "default": proxy default regardless of main (no X-Ilm-Backend header).
	//   "<name>": pinned backend — e.g. "llama" for the cheap-labor pattern:
	//     heavy reasoning on an external backend, sub-task data stays local.
	//     Note: subagent_backend="llama" while main is external *reduces* egress,
	//     which is intentional.
	// Default: "inherit".
	SubagentBackend string `json:"subagent_backend,omitempty"`

	// SubagentEndpoint controls which endpoint (from the "endpoints" block)
	// dispatch_subagent uses. Orthogonal to SubagentBackend: SubagentBackend
	// selects a *backend* within an ilm-proxy endpoint, while SubagentEndpoint
	// selects which endpoint the subagent talks to in the first place.
	// SubagentBackend only applies when the child's resolved endpoint is kind
	// "ilm-proxy" — a kind "openai" endpoint has no backend concept.
	//   "" or "inherit" (default): the child follows the parent's live
	//     endpoint (kind, base_url, model, auth, sampling) exactly — including
	//     mid-session /backend or /model switches. This is the pre-existing
	//     behavior and remains the default.
	//   "<name>": a key into Endpoints — the child always targets that named
	//     endpoint regardless of the parent's current selection.
	// Validated at config load: a value other than "", "inherit", or a key
	// present in Endpoints fails LoadConfig with an error naming the missing key.
	SubagentEndpoint string `json:"subagent_endpoint,omitempty"`

	// MaxParallelSubagents bounds how many dispatch_subagent workers may run
	// concurrently when the model emits several dispatches in one turn (or via
	// the dispatch_subagents batch tool). Values ≤ 1 mean sequential execution
	// (the pre-parallelism behavior); the default is 2. Raising this only helps
	// when the backend serves concurrent requests.
	MaxParallelSubagents int `json:"max_parallel_subagents,omitempty"`

	// SubagentMCPServers is the allowlist of MCP server names that the "tools"
	// capability tier may expose to subagents. An empty/absent list means no MCP
	// tools are available to subagents (default-deny). Only servers listed here
	// have their tools included in the tools-tier subagent's toolset; the model
	// cannot call an MCP tool from a server not in this list.
	//
	// This is the consent surface for subagent MCP access: the user explicitly
	// opts in each server by name. IsMCPReadTool (the read-keyword allowlist)
	// is NOT used as a security input for subagents — it stays for parent
	// UX only.
	SubagentMCPServers []string `json:"subagent_mcp_servers,omitempty"`

	// AgentPromptPath is the file loaded once at startup to supply the agent
	// operating instructions (system message). Default: agent.txt next to the
	// config file. If missing, the built-in fallback prompt is used.
	AgentPromptPath string `json:"agent_prompt_path,omitempty"`

	// Costs holds per-source pricing for the cost-tracking sidebar. All rates
	// default to zero (unpriced): an unpriced source shows its call/token counts
	// but "—" for cost rather than a misleading "$0.00".
	Costs CostsConfig `json:"costs,omitempty"`

	// BackendMaxRetries is the maximum number of retry attempts for transient
	// backend failures (5xx, connection drops, timeouts) in unattended runs
	// (auto-approve or headless). Each retry is preceded by exponential backoff
	// (1s, 2s, 4s…). Default 3. 0 disables automatic retrying.
	BackendMaxRetries int `json:"backend_max_retries,omitempty"`

	// Trace capture. TraceSessions enables a rich JSONL trace store for every TUI
	// session; TraceDir is where trace files are written. Both are config-file
	// fields so "always trace" can be set once rather than on every invocation.
	// The trace store is separate from the normal session store and gates all
	// content with sft_eligible:false — capture ≠ consent-to-train.
	TraceSessions bool   `json:"trace_sessions"`      // trace every TUI session (default false)
	TraceDir      string `json:"trace_dir,omitempty"` // trace file directory; default ~/.local/share/wakil/traces

	// LSP: native language-server-backed code intelligence tools.
	LSPEnabled             bool                 `json:"lsp_enabled,omitempty"`
	LSPServers             map[string]LSPServer `json:"lsp_servers,omitempty"`
	LSPIdleTimeoutSeconds  int                  `json:"lsp_idle_timeout_seconds,omitempty"`  // default 1800 (30 min)
	LSPIndexTimeoutSeconds int                  `json:"lsp_index_timeout_seconds,omitempty"` // default 30

	// Browser: native headless-browser-backed tools (chromedp + Chromium) for
	// visual verification, DOM inspection, interaction testing. Off by default.
	// When enabled, chromium must be installed in the sandbox image.
	BrowserEnabled bool `json:"browser_enabled,omitempty"`

	// Runtime-only flags (never read from / written to the JSON config file).
	Resume       bool   `json:"-"` // resume the most recent session
	ResumeID     string `json:"-"` // resume a session by chat_id or unique prefix
	ListSessions bool   `json:"-"` // list saved sessions and exit
	AllSessions  bool   `json:"-"` // --all: ignore workspace scoping for --resume/--list-sessions
	AutoApprove  bool   `json:"-"` // skip all confirmation prompts
	Trace        bool   `json:"-"` // tracing enabled for this run (TraceSessions || --trace flag)

	// ModelExplicit and AutoExplicit record whether THIS run's invocation
	// explicitly set --model/ILM_MODEL or --auto, as opposed to picking up a
	// default. Used by the per-repo terminal-settings restore feature
	// (RestoreRepoState) to decide whether a remembered folder preference
	// may apply: an explicit flag or env var for this run always wins over a
	// restored value. Computed once in LoadConfig from the same flagsSet map
	// resolveEndpoint already uses for model/base-url override precedence.
	ModelExplicit bool `json:"-"`
	AutoExplicit  bool `json:"-"`
}

// CostsConfig is the [costs] pricing block consumed by the CostTracker. Rates
// are user-supplied estimates; see cost.go for how confidence tiers keep a
// modeled figure from being read as billed.
type CostsConfig struct {
	// Mashura (oracle) pricing keyed by oracle model ID — different models bill
	// at different rates (e.g. "claude-fable-5" vs "claude-opus-4-8"). Oracle
	// usage is exact (from the API response), so cost is exact given the rate.
	Mashura map[string]ModelRate `json:"mashura,omitempty"`

	// Inference is a single rate over local proxy tokens (main + aux). It models
	// a compute/electricity cost, NOT a billed amount; the default 0.0 leaves
	// inference unpriced — token counts still accrue, but cost shows "—".
	Inference InferenceRate `json:"inference,omitempty"`

	// InferenceBackends holds per-backend/model pricing for external inference
	// routed through the proxy (e.g. OpenRouter). Keys are "backend/model"
	// (e.g. "openrouter/openai/gpt-4o"); values are the real per-token rates
	// from the external provider. Used for ConfExact rows in the Costs sidebar.
	// Token counts come from the proxy (exact when the provider reports them).
	InferenceBackends map[string]ModelRate `json:"inference_backends,omitempty"`

	// Search is charged per query. Default 0.0 leaves search unpriced.
	Search SearchRate `json:"search,omitempty"`
}

// ModelRate is per-million-token input/output pricing for one oracle model.
type ModelRate struct {
	InputUSDPer1M  float64 `json:"input_usd_per_1m"`
	OutputUSDPer1M float64 `json:"output_usd_per_1m"`

	// CachedInputUSDPer1M is the discounted rate for cache-hit prompt tokens
	// (prompt_tokens_details.cached_tokens). Optional: 0 (the zero value, and
	// the default for every config that predates this field) means "no
	// discount configured" — cached tokens are billed at InputUSDPer1M like
	// any other input token, so cost arithmetic is byte-identical to before
	// this field existed. See CostsConfig.ExternalInferenceCost.
	CachedInputUSDPer1M float64 `json:"cached_input_usd_per_1m,omitempty"`

	// CacheWriteUSDPer1M is the rate for cache-write prompt tokens
	// (cache_creation_input_tokens). Optional: 0 (the zero value) means
	// "no separate write rate configured" — write tokens are billed at
	// InputUSDPer1M, so cost arithmetic is byte-identical to before this
	// field existed. Anthropic charges a 25% premium over base input for
	// cache writes; users who want that precision set this field explicitly.
	// No default multiplier is applied when unset.
	CacheWriteUSDPer1M float64 `json:"cache_write_usd_per_1m,omitempty"`
}

// InferenceRate is the single proxy-compute rate applied to main+aux tokens.
type InferenceRate struct {
	USDPer1MTokens float64 `json:"usd_per_1m_tokens"`
}

// SearchRate is the per-query search rate.
type SearchRate struct {
	USDPerQuery float64 `json:"usd_per_query"`
}

// mashuraCost returns the exact cost of one oracle call given the model's
// configured rate. priced is false when the model has no rate (or a zero rate),
// so the source renders "—" instead of "$0.00".
func (c CostsConfig) MashuraCost(model string, inTok, outTok int64) (usd float64, priced bool) {
	r, ok := c.Mashura[model]
	if !ok || (r.InputUSDPer1M == 0 && r.OutputUSDPer1M == 0) {
		return 0, false
	}
	usd = float64(inTok)/1e6*r.InputUSDPer1M + float64(outTok)/1e6*r.OutputUSDPer1M
	return usd, true
}

// inferenceCost returns the modeled cost for totalTok proxy tokens (input +
// output). priced is false when no inference rate is configured.
func (c CostsConfig) InferenceCost(totalTok int64) (usd float64, priced bool) {
	if c.Inference.USDPer1MTokens == 0 {
		return 0, false
	}
	return float64(totalTok) / 1e6 * c.Inference.USDPer1MTokens, true
}

// TokenDetail carries the per-call token breakdown that cost arithmetic
// consumes. It replaces the variadic cachedTok ...int64 parameter on
// ExternalInferenceCost and CostTracker.Record with a single typed struct,
// so new fields (e.g. CacheWriteTok) can be added without signature churn.
type TokenDetail struct {
	CachedTok     int64 // subset of InputTok served from the backend's prompt cache (cache reads)
	CacheWriteTok int64 // tokens written to the cache this turn (cache_creation_input_tokens)
}

// ExternalInferenceCost returns the exact cost of one external inference call
// given the "backend/model" key's configured rate. priced is false when the
// key has no rate (or a zero rate), so the source renders "—".
//
// detail carries cache-read and cache-write token counts. When a rate field
// is unset (0), the corresponding tokens are billed at InputUSDPer1M — so the
// returned usd is byte-identical to the pre-cache-accounting formula when no
// cache rates are configured, regardless of whether the caller passes real
// token counts. This is the golden "unconfigured stays unchanged" guarantee.
//
// Cache-write tokens are treated as additive (NOT inside prompt_tokens) —
// they are priced independently and added to the total. When CacheWriteUSDPer1M
// is unset, write tokens bill at InputUSDPer1M.
func (c CostsConfig) ExternalInferenceCost(backendModel string, inTok, outTok int64, detail TokenDetail) (usd float64, priced bool) {
	r, ok := c.InferenceBackends[backendModel]
	if !ok || (r.InputUSDPer1M == 0 && r.OutputUSDPer1M == 0) {
		return 0, false
	}
	cachedRate := r.CachedInputUSDPer1M
	if cachedRate == 0 {
		cachedRate = r.InputUSDPer1M
	}
	writeRate := r.CacheWriteUSDPer1M
	if writeRate == 0 {
		writeRate = r.InputUSDPer1M
	}
	cached := detail.CachedTok
	uncached := inTok - cached
	if uncached < 0 {
		uncached = 0
	}
	usd = float64(uncached)/1e6*r.InputUSDPer1M +
		float64(cached)/1e6*cachedRate +
		float64(detail.CacheWriteTok)/1e6*writeRate +
		float64(outTok)/1e6*r.OutputUSDPer1M
	return usd, true
}

// searchCost returns the modeled cost of one search query. priced is false when
// no per-query rate is configured.
func (c CostsConfig) SearchCost() (usd float64, priced bool) {
	if c.Search.USDPerQuery == 0 {
		return 0, false
	}
	return c.Search.USDPerQuery, true
}

// MashuraPanelConfig defines one named panel: a list of prefixed model strings
// and the panel execution mode.
//
// mode "fusion": a single OpenRouter Fusion call where Models becomes the
// analysis panel; FusionJudge is the judge that synthesizes their responses.
// All models use the ~ prefix syntax (e.g. "~anthropic/claude-opus-latest").
type MashuraPanelConfig struct {
	Models             []string `json:"models"`                          // prefixed model strings, or ~ models for fusion
	Mode               string   `json:"mode,omitempty"`                  // "panel" (default) | "fallback" | "fusion"
	FusionJudge        string   `json:"fusion_judge,omitempty"`          // fusion: judge model; "" = OpenRouter default
	FusionMaxToolCalls int      `json:"fusion_max_tool_calls,omitempty"` // fusion: 1–16 tool steps; 0 = default (8)
}

// MCPServerConfig declares one MCP server. Transport is "stdio" or "http".
type MCPServerConfig struct {
	Name      string            `json:"name"`
	Transport string            `json:"transport"`         // "stdio" | "http"
	Command   string            `json:"command,omitempty"` // stdio: executable
	Args      []string          `json:"args,omitempty"`    // stdio: arguments
	Env       map[string]string `json:"env,omitempty"`     // stdio: extra env vars
	URL       string            `json:"url,omitempty"`     // http: endpoint URL
}

// LSPServer declares one language server for the native LSP tools.
type LSPServer struct {
	Command     string                 `json:"command"`
	Args        []string               `json:"args,omitempty"`
	Env         map[string]string      `json:"env,omitempty"`
	InitOptions map[string]interface{} `json:"init_options,omitempty"`
}

func DefaultConfig() Config {
	return Config{
		Model:                   "ilm",
		ExecMode:                "docker",
		Image:                   "wakil-dev",
		DockerSocket:            false,
		DockerMemory:            "4g",
		DockerPidsLimit:         512,
		KVRMaxEntries:           100000,
		KVRSweepIntervalSecs:    30,
		KVRSnapshotIntervalSecs: 300,
		KeepBytes:               120000, // keep ~120k of recent verbatim turns after compaction
		SummaryBytes:            20000,  // cap the running summary; re-condense if it grows past this
		HardMaxBytes:            160000, // unconditional ceiling; compact then drop until under
		TurnToolBudget:          40000,  // per-turn tool output budget; reduced slice once exceeded
		MaxChars:                512000, // transcript-byte ceiling (hist line + compaction fallback)
		CompactAt:               145000, // fire before reaching hard max (post-compact target ~140k)
		CompactAtFrac:           0.75,   // compact at 75% of effective context
		KeepBytesFrac:           0.60,   // keep 60% of effective context verbatim after compaction
		HardMaxFrac:             0.95,   // hard ceiling at 95% of effective context
		ContextCapacityFrac:     0.80,   // use 80% of proxy's usable_ctx as the working budget

		ReasoningBudgetTokens: 4096,      // headroom for extended thinking
		AnswerMarginTokens:    4096,      // headroom for the final answer
		ContextTokensFallback: 131072,    // assumed n_ctx when the backend is unreachable
		ToolResultCap:         8000,      // keep first 8k chars in ctx; spill the rest to disk
		ToolResultTTL:         3,         // evict after 3 completed turns (longer window before re-reads are needed)
		ReadFileSizeLimit:     1 << 20,   // 1 MB: refuse larger reads at the tool layer
		MaxFullReadBytes:      256 << 10, // 256 KB: full-read ceiling (higher than ToolResultCap 8K, under MaxRequestBytes 8MB)
		MaxRequestBytes:       8 << 20,   // 8 MB: trim tool results before sending if over
		BackendMaxRetries:     3,
		MaxParallelSubagents:  2,
		OracleModel:           "claude-sonnet-4-6",
		OracleMaxTokens:       4096,
		OracleAPIKeyEnv:       "ANTHROPIC_API_KEY",
		OpenRouterAPIKeyEnv:   "OPENROUTER_API_KEY",
		OracleTimeoutSeconds:  300,
		WFFinalReview:         true,
	}
}

func defaultConfigPath() string {
	if x := os.Getenv("WAKIL_CONFIG"); x != "" {
		return x
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "wakil", "config.json")
}

// defaultConfigTemplate is the minimal config written on first run. It contains
// only _comment keys so built-in defaults stay authoritative until the user edits
// it. No placeholder endpoints or fake secrets — the user fills in their own.
// Without an "endpoints" block, resolveEndpoint falls back to the legacy path
// (base_url/model top-level fields or ILM_BASE_URL env var), so the user can
// either add an endpoints block or set ILM_BASE_URL — whichever they prefer.
const defaultConfigTemplate = `{
  "_comment": "Wakil config — edit this file to set your endpoint. You MUST provide at least one endpoint (see below); other fields are optional and use built-in defaults when unset. Precedence: defaults < this file < env < flags. See config.example.json in the repo for a fully commented reference.",

  "_comment_endpoints": "REQUIRED: set at least one endpoint. Either add an \"endpoints\" block here (see config.example.json) or set the ILM_BASE_URL env var. Example: \"endpoints\": {\"local\": {\"kind\": \"openai\", \"base_url\": \"http://localhost:8080\", \"model\": \"qwen3.6-35b\"}}, \"default_endpoint\": \"local\"",

  "_comment_exec": "exec_mode: \"docker\" (default, needs the wakil-dev image — build with: docker build -t wakil-dev .) or \"direct\" (bare-metal, no Docker)."
}
`

// maybeCreateDefaultConfig writes a minimal default config file to cfgPath when
// the file doesn't exist. It creates parent directories with 0o700 and the
// file with 0o600 (config can hold API keys). Uses O_CREATE|O_EXCL to avoid a
// race between concurrent first runs. Errors are printed to stderr and swallowed
// — the caller proceeds with built-in defaults regardless.
func maybeCreateDefaultConfig(cfgPath string) {
	dir := filepath.Dir(cfgPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "config: cannot create config dir %s: %v\n", dir, err)
		return
	}
	f, err := os.OpenFile(cfgPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		// Race: another process created it, or it appeared between the
		// IsNotExist check and here. Either way, not our file to write.
		if !os.IsExist(err) {
			fmt.Fprintf(os.Stderr, "config: cannot create default config %s: %v\n", cfgPath, err)
		}
		return
	}
	defer f.Close()
	if _, err := f.WriteString(defaultConfigTemplate); err != nil {
		fmt.Fprintf(os.Stderr, "config: cannot write default config %s: %v\n", cfgPath, err)
		return
	}
	fmt.Fprintf(os.Stderr, "config: created default config at %s — edit it to set your endpoint, then re-run.\n", cfgPath)
}

// LoadConfig resolves configuration from all sources.
func LoadConfig(argv []string) (Config, error) {
	cfg := DefaultConfig()

	// 1) config file (explicit --config handled by pre-scan; else default path)
	cfgPath := defaultConfigPath()
	cfgPathExplicit := false // true when --config flag was used
	for i := 0; i < len(argv); i++ {
		switch {
		case (argv[i] == "--config" || argv[i] == "-config") && i+1 < len(argv):
			cfgPath = argv[i+1]
			cfgPathExplicit = true
		case strings.HasPrefix(argv[i], "--config="):
			cfgPath = argv[i][len("--config="):]
			cfgPathExplicit = true
		case strings.HasPrefix(argv[i], "-config="):
			cfgPath = argv[i][len("-config="):]
			cfgPathExplicit = true
		}
	}
	if cfgPath != "" {
		b, err := os.ReadFile(cfgPath)
		if err != nil {
			if !os.IsNotExist(err) {
				return cfg, fmt.Errorf("reading %s: %w", cfgPath, err)
			}
			// First run: the default config file doesn't exist. Create a minimal
			// default so the user has a starting point to edit. The file contains
			// only _comment keys — built-in defaults from DefaultConfig() remain
			// authoritative until the user edits it.
			//
			// Only auto-create for the default config path. If the user passed
			// --config explicitly, the path might be a typo — erroring is better
			// than silently creating a file at the wrong location.
			if !cfgPathExplicit {
				maybeCreateDefaultConfig(cfgPath)
			}
		} else if err := json.Unmarshal(b, &cfg); err != nil {
			return cfg, fmt.Errorf("parsing %s: %w", cfgPath, err)
		}
	}

	// Canonical mashura_* keys win over the legacy oracle_* spelling when both are
	// set. After this merge the rest of the code reads only the Oracle* fields.
	if cfg.MashuraMaxTokens != 0 {
		cfg.OracleMaxTokens = cfg.MashuraMaxTokens
	}
	if cfg.MashuraTimeoutSeconds != 0 {
		cfg.OracleTimeoutSeconds = cfg.MashuraTimeoutSeconds
	}
	if cfg.MashuraMode != "" {
		cfg.WFOracleMode = cfg.MashuraMode
	}

	// 2) environment
	envStr(&cfg.SearXngURL, "SEARXNG_URL")
	envStr(&cfg.GoogleAPIKey, "GOOGLE_API_KEY")
	envStr(&cfg.GoogleCX, "GOOGLE_CX")
	envStr(&cfg.MentionBase, "WAKIL_MENTION_BASE")
	// ILM_* env vars are legacy aliases. WAKIL_* is the preferred namespace.
	// ILM_* takes precedence (checked first) for backward compatibility.
	envStr(&cfg.BaseURL, "ILM_BASE_URL")
	envStr(&cfg.BaseURL, "WAKIL_BASE_URL")
	envStr(&cfg.Host, "ILM_HOST")
	envStr(&cfg.Host, "WAKIL_HOST")
	envInt(&cfg.Port, "ILM_PORT")
	envInt(&cfg.Port, "WAKIL_PORT")
	envStr(&cfg.APIKey, "ILM_API_KEY")
	envStr(&cfg.APIKey, "WAKIL_API_KEY")
	envStr(&cfg.Model, "ILM_MODEL")
	envStr(&cfg.Model, "WAKIL_MODEL")
	envStr(&cfg.ExecMode, "ILM_EXEC_MODE")
	envStr(&cfg.ExecMode, "WAKIL_EXEC_MODE")
	envStr(&cfg.Image, "ILM_CONTAINER_IMAGE")
	envStr(&cfg.Image, "WAKIL_IMAGE")
	envStr(&cfg.WorkDir, "ILM_WORKDIR")
	envStr(&cfg.WorkDir, "WAKIL_WORKDIR")
	envStr(&cfg.HostWorkDir, "ILM_HOST_WORKDIR")
	envStr(&cfg.HostWorkDir, "WAKIL_HOST_WORKDIR")
	envBool(&cfg.DockerSocket, "ILM_DOCKER_SOCKET")
	envBool(&cfg.DockerSocket, "WAKIL_DOCKER_SOCKET")
	envStr(&cfg.SSHSigning, "ILM_SSH_SIGNING")
	envStr(&cfg.SSHSigning, "WAKIL_SSH_SIGNING")
	envBool(&cfg.KVRDisabled, "WAKIL_KVR_DISABLED")
	envInt(&cfg.KVRMaxEntries, "WAKIL_KVR_MAX_ENTRIES")
	envInt(&cfg.KVRSweepIntervalSecs, "WAKIL_KVR_SWEEP_INTERVAL_SECS")
	envInt(&cfg.KVRSnapshotIntervalSecs, "WAKIL_KVR_SNAPSHOT_INTERVAL_SECS")
	envBool(&cfg.TraceSessions, "WAKIL_TRACE_SESSIONS")
	envStr(&cfg.TraceDir, "WAKIL_TRACE_DIR")

	// 3) flags (highest precedence)
	fs := flag.NewFlagSet("wakil", flag.ContinueOnError)
	fs.StringVar(&cfg.BaseURL, "base-url", cfg.BaseURL, "ilm proxy base URL (overrides host/port)")
	fs.StringVar(&cfg.SearXngURL, "searxng-url", cfg.SearXngURL, "SearXNG base URL (enables native searxng_search tool)")
	fs.StringVar(&cfg.GoogleCX, "google-cx", cfg.GoogleCX, "Google Programmable Search Engine ID (pair with GOOGLE_API_KEY env)")
	fs.StringVar(&cfg.MentionBase, "mention-base", cfg.MentionBase, "base directory for @ file mentions (default: current directory)")
	fs.StringVar(&cfg.Host, "host", cfg.Host, "ilm proxy host (alternative to base-url)")
	fs.IntVar(&cfg.Port, "port", cfg.Port, "ilm proxy port (used with --host, default 11400)")
	fs.StringVar(&cfg.APIKey, "api-key", cfg.APIKey, "API key (sent as Bearer token)")
	fs.StringVar(&cfg.Model, "model", cfg.Model, "model name")
	fs.StringVar(&cfg.ExecMode, "exec", cfg.ExecMode, "execution mode: docker|direct")
	fs.StringVar(&cfg.Image, "image", cfg.Image, "container image (docker mode)")
	fs.StringVar(&cfg.AttachImage, "attach-image", cfg.AttachImage, "path to an image file to attach to the first message (png, jpeg, gif, webp; comma-separate for multiple)")
	fs.StringVar(&cfg.WorkDir, "workdir", cfg.WorkDir, "working directory inside the container")
	fs.StringVar(&cfg.HostWorkDir, "host-workdir", cfg.HostWorkDir, "host path bind-mounted into container (files appear here locally)")
	fs.BoolVar(&cfg.DockerSocket, "docker-sock", cfg.DockerSocket, "pass host docker socket into the sandbox so the agent can start host containers (default: off; use --docker-sock=true to enable)")
	fs.StringVar(&cfg.SSHSigning, "ssh-signing", cfg.SSHSigning, "SSH commit signing in the sandbox: off|auto|<path to .pub> (auto reads the host git config; agent socket is passed through, the private key never enters the sandbox)")
	fs.String("config", cfgPath, "path to config file")
	fs.BoolVar(&cfg.Resume, "resume", false, "resume the most recent session in this workspace")
	fs.StringVar(&cfg.ResumeID, "resume-id", "", "resume a session by chat_id (or unique prefix) — searches all workspaces")
	fs.BoolVar(&cfg.ListSessions, "list-sessions", false, "list saved sessions for this workspace and exit")
	fs.BoolVar(&cfg.AllSessions, "all", false, "with --resume/--list-sessions: ignore workspace scoping, use every folder")
	fs.BoolVar(&cfg.AutoApprove, "auto", false, "auto-approve all tool calls without prompting")
	var traceFlag bool
	fs.BoolVar(&traceFlag, "trace", false, "enable rich JSONL trace capture for this session (overrides trace_sessions config)")
	fs.StringVar(&cfg.TraceDir, "trace-dir", cfg.TraceDir, "directory for trace files (default ~/.local/share/wakil/traces)")
	fs.Usage = func() {
		out := fs.Output()
		fmt.Fprintln(out, "Usage: wakil [flags] [workspace-path]")
		fmt.Fprintln(out, "\n  workspace-path  directory to mount into the sandbox (docker mode)")
		fmt.Fprintln(out, "                  or work in (direct mode); defaults to the current directory.")
		fmt.Fprintln(out, "\nFlags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(argv); err != nil {
		return cfg, err
	}

	// Record which flags were explicitly passed — needed to distinguish
	// "user set --model/--base-url" (overrides the selected endpoint) from
	// "flag default carried the config value through".
	flagsSet := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { flagsSet[f.Name] = true })

	// Record explicit-this-run overrides for the per-repo settings restore
	// feature (RestoreRepoState): a --model/ILM_MODEL or --auto set for THIS
	// invocation always wins over a remembered folder preference.
	cfg.ModelExplicit = flagsSet["model"] || os.Getenv("ILM_MODEL") != ""
	cfg.AutoExplicit = flagsSet["auto"]

	// Resolve trace: --trace flag enables for this run; config trace_sessions makes
	// it the standing default. Either way cfg.Trace is the single bit callers check.
	cfg.Trace = cfg.TraceSessions || traceFlag

	// Derive a default trace directory when tracing is enabled but none was set.
	// Follows the same XDG convention as the session store.
	if cfg.Trace && cfg.TraceDir == "" {
		if x := os.Getenv("XDG_DATA_HOME"); x != "" {
			cfg.TraceDir = filepath.Join(x, "wakil", "traces")
		} else if home, err := os.UserHomeDir(); err == nil {
			cfg.TraceDir = filepath.Join(home, ".local", "share", "wakil", "traces")
		}
	}

	// Expand a leading ~ in trace_dir from any source (config file, env, flag).
	// Go does not expand tildes; a literal "~/.local/…" in config.json would
	// create a directory named "~" under the data home. The auto-default above
	// uses os.UserHomeDir() correctly, but explicitly set values do not.
	cfg.TraceDir = expandTilde(cfg.TraceDir)

	// Construct base_url from host+port if not set directly.
	if cfg.BaseURL == "" && cfg.Host != "" {
		port := cfg.Port
		if port == 0 {
			port = 11400
		}
		cfg.BaseURL = fmt.Sprintf("http://%s:%d", cfg.Host, port)
	}

	// Resolve the active endpoint. With an endpoints block, the selected entry
	// governs; without one, a single ilm-proxy endpoint is synthesized from the
	// legacy fields (exact pre-endpoints behavior). Env/flag overrides for
	// model and base_url apply on top of the selected endpoint.
	if err := resolveEndpoint(&cfg, flagsSet); err != nil {
		return cfg, err
	}
	if err := validateSubagentEndpoint(cfg); err != nil {
		return cfg, err
	}
	if cfg.ExecMode != "docker" && cfg.ExecMode != "direct" {
		return cfg, fmt.Errorf("invalid exec mode %q (want docker|direct)", cfg.ExecMode)
	}

	// Resolve kvr effective state. KVRDisabled is the single opt-out
	// (default: false = enabled). Also auto-disabled in direct mode
	// (staging is docker-only in this ticket).
	if cfg.KVRDisabled {
		// explicit opt-out
	} else if cfg.ExecMode != "docker" {
		cfg.KVRDisabled = true
	}
	if err := validateContextLimits(cfg); err != nil {
		return cfg, err
	}

	// Positional arg: the workspace directory. In docker mode it is bind-mounted
	// into the container; in direct mode it is the working directory. Precedence:
	// positional arg > configured/flagged path > current working directory.
	workspace := ""
	if rest := fs.Args(); len(rest) > 0 {
		workspace = rest[0]
	}
	if workspace != "" {
		abs, err := filepath.Abs(workspace)
		if err != nil {
			return cfg, fmt.Errorf("resolving workspace path %q: %w", workspace, err)
		}
		if info, statErr := os.Stat(abs); statErr != nil || !info.IsDir() {
			return cfg, fmt.Errorf("workspace path %q is not a directory", workspace)
		}
		workspace = abs
	}

	switch cfg.ExecMode {
	case "docker":
		if workspace != "" {
			cfg.HostWorkDir = workspace
		}
		if cfg.HostWorkDir == "" {
			if wd, err := os.Getwd(); err == nil {
				cfg.HostWorkDir = wd // default: mount the current directory
			}
		}
		// Derive the in-container path from the host directory name so the agent
		// always sees the project at a predictable, descriptive location.
		if cfg.WorkDir == "" && cfg.HostWorkDir != "" {
			cfg.WorkDir = "/mnt/" + filepath.Base(cfg.HostWorkDir)
		}
		if cfg.WorkDir == "" {
			cfg.WorkDir = "/mnt/work"
		}
	case "direct":
		if workspace != "" {
			cfg.WorkDir = workspace
		}
		// An empty WorkDir falls back to the cwd inside NewDirectExecutor.
	}

	// "@" mention base: default to the launch cwd; always resolve to absolute.
	if cfg.MentionBase == "" {
		if wd, err := os.Getwd(); err == nil {
			cfg.MentionBase = wd
		}
	}
	if cfg.MentionBase != "" {
		if abs, err := filepath.Abs(cfg.MentionBase); err == nil {
			cfg.MentionBase = abs
		}
	}

	// Derive the default agent prompt path: agent.txt next to the config file.
	// Only set the default when cfgPath is non-empty and no explicit value was
	// supplied via the config file, env, or flags.
	if cfg.AgentPromptPath == "" && cfgPath != "" {
		cfg.AgentPromptPath = filepath.Join(filepath.Dir(cfgPath), "agent.txt")
	}

	if err := validateEnums(cfg); err != nil {
		return cfg, err
	}
	if err := validateTimeouts(cfg); err != nil {
		return cfg, err
	}
	if err := validateDockerConfig(cfg); err != nil {
		return cfg, err
	}
	if err := validateURLs(cfg); err != nil {
		return cfg, err
	}
	if err := validateExternalCommands(cfg); err != nil {
		return cfg, err
	}

	return cfg, nil
}

// expandTilde replaces a leading "~/" (or bare "~") with the user's home
// directory. Any other path is returned unchanged. Used for trace_dir so a
// config value of "~/.local/share/wakil/traces" expands correctly.
func expandTilde(p string) string {
	if p == "" {
		return p
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		return filepath.Join(home, p[1:])
	}
	return p
}

func envStr(dst *string, key string) {
	if v := os.Getenv(key); v != "" {
		*dst = v
	}
}

func envBool(dst *bool, key string) {
	switch strings.ToLower(os.Getenv(key)) {
	case "1", "true", "yes", "on":
		*dst = true
	case "0", "false", "no", "off":
		*dst = false
	}
}

func envInt(dst *int, key string) {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			log.Printf("config: invalid %s=%q: %v (ignored)", key, v, err)
			return
		}
		*dst = n
	}
}

// resolveEndpoint populates cfg.Endpoint/EndpointName from the endpoints block,
// or synthesizes a legacy ilm-proxy endpoint when no block exists. It then
// applies explicit env/flag overrides (ILM_MODEL/--model, ILM_BASE_URL/
// --base-url) and mirrors the result into the legacy cfg.BaseURL/cfg.Model
// fields so existing call sites keep reading one source of truth.
func resolveEndpoint(cfg *Config, flagsSet map[string]bool) error {
	modelOverridden := flagsSet["model"] || os.Getenv("ILM_MODEL") != ""
	baseURLOverridden := flagsSet["base-url"] || os.Getenv("ILM_BASE_URL") != ""

	if len(cfg.Endpoints) == 0 {
		// Legacy config: synthesize a single ilm-proxy endpoint from the
		// top-level fields. Behavior must be byte-identical to pre-endpoints
		// Wakil, including the original missing-URL error.
		if cfg.BaseURL == "" {
			return fmt.Errorf("proxy address required — set base_url (or host+port) in config, ILM_BASE_URL, or --base-url")
		}
		cfg.Endpoint = EndpointConfig{
			Kind:    EndpointKindIlmProxy,
			BaseURL: cfg.BaseURL,
			Model:   cfg.Model, // default "ilm", possibly overridden by file/env/flag
		}
		cfg.EndpointName = "ilm"
		return nil
	}

	// Endpoints block present: select and validate.
	name := cfg.DefaultEndpoint
	if name == "" {
		if len(cfg.Endpoints) == 1 {
			for n := range cfg.Endpoints {
				name = n
			}
		} else {
			return fmt.Errorf("endpoints block has %d entries — set default_endpoint to choose one", len(cfg.Endpoints))
		}
	}
	ep, ok := cfg.Endpoints[name]
	if !ok {
		return fmt.Errorf("default_endpoint %q not found in endpoints block", name)
	}
	if ep.Kind == "" {
		ep.Kind = EndpointKindOpenAI
	}
	switch ep.Kind {
	case EndpointKindOpenAI:
		if ep.Model == "" {
			return fmt.Errorf("endpoint %q: model is required for kind %q — set the literal model string the server expects (e.g. \"qwen3.6-35b\")", name, EndpointKindOpenAI)
		}
	case EndpointKindIlmProxy:
		if ep.Model == "" {
			ep.Model = "ilm" // proxy alias routing, current behavior
		}
	default:
		return fmt.Errorf("endpoint %q: unknown kind %q (want %q or %q)", name, ep.Kind, EndpointKindOpenAI, EndpointKindIlmProxy)
	}
	if ep.BaseURL == "" {
		return fmt.Errorf("endpoint %q: base_url is required", name)
	}

	// Explicit env/flag overrides win over the endpoint entry.
	if modelOverridden {
		ep.Model = cfg.Model
	}
	if baseURLOverridden {
		ep.BaseURL = cfg.BaseURL
	}

	cfg.Endpoint = ep
	cfg.EndpointName = name
	// Mirror into the legacy fields — the rest of the code reads one set.
	cfg.BaseURL = ep.BaseURL
	cfg.Model = ep.Model
	return nil
}

// validateSubagentEndpoint checks that subagent_endpoint, when set to
// something other than "" or "inherit", names a key present in Endpoints.
// Called after resolveEndpoint so Endpoints is fully populated by the time
// this runs (env/flag overrides don't touch Endpoints, so ordering relative
// to those doesn't matter).
func validateSubagentEndpoint(cfg Config) error {
	switch cfg.SubagentEndpoint {
	case "", "inherit":
		return nil
	default:
		if _, ok := cfg.Endpoints[cfg.SubagentEndpoint]; !ok {
			return fmt.Errorf("subagent_endpoint %q not found in endpoints block", cfg.SubagentEndpoint)
		}
		return nil
	}
}

// ActiveEndpoint returns the resolved endpoint. For Configs built by hand
// (tests, subagents) that never went through LoadConfig, it synthesizes the
// legacy ilm-proxy shape from the top-level fields — preserving pre-endpoints
// behavior for every existing construction path.
func (c Config) ActiveEndpoint() EndpointConfig {
	if c.Endpoint.Kind != "" {
		return c.Endpoint
	}
	return EndpointConfig{
		Kind:    EndpointKindIlmProxy,
		BaseURL: c.BaseURL,
		Model:   c.Model,
	}
}

func (c Config) AuthHeader() string {
	return c.AuthHeaderFor(c.Endpoint)
}

// AuthHeaderFor returns the Authorization header value for an arbitrary
// endpoint entry: the endpoint's own auth_header (verbatim Authorization
// value) wins over the legacy api_key ("Bearer <key>") fallback. Mirrors
// AuthHeader() but works for any EndpointConfig, not just the currently
// active one — used to resolve a named endpoint that may differ from
// c.Endpoint (e.g. a subagent_endpoint override).
func (c Config) AuthHeaderFor(ep EndpointConfig) string {
	if ep.AuthHeader != "" {
		return ep.AuthHeader
	}
	if c.APIKey == "" {
		return ""
	}
	return "Bearer " + c.APIKey
}

// NormalizeEndpoint resolves and validates a named entry from the Endpoints
// block, applying the same defaulting rules used when an endpoint becomes
// active (resolveEndpoint at config load, handleEndpointSwitch on /backend):
// Kind defaults to "openai" when empty; kind=ilm-proxy defaults Model to
// "ilm" when empty; kind=openai requires Model; base_url is always required.
// Unlike ActiveEndpoint(), this works for any key in Endpoints, not just the
// currently active one — used to resolve a subagent_endpoint override that
// may name a different entry than the session's current endpoint.
func (c Config) NormalizeEndpoint(name string) (EndpointConfig, error) {
	ep, ok := c.Endpoints[name]
	if !ok {
		return EndpointConfig{}, fmt.Errorf("endpoint %q not found in endpoints block", name)
	}
	if ep.Kind == "" {
		ep.Kind = EndpointKindOpenAI
	}
	switch ep.Kind {
	case EndpointKindOpenAI:
		if ep.Model == "" {
			return EndpointConfig{}, fmt.Errorf("endpoint %q: model is required for kind %q", name, EndpointKindOpenAI)
		}
	case EndpointKindIlmProxy:
		if ep.Model == "" {
			ep.Model = "ilm"
		}
	default:
		return EndpointConfig{}, fmt.Errorf("endpoint %q: unknown kind %q (want %q or %q)", name, ep.Kind, EndpointKindOpenAI, EndpointKindIlmProxy)
	}
	if ep.BaseURL == "" {
		return EndpointConfig{}, fmt.Errorf("endpoint %q: base_url is required", name)
	}
	return ep, nil
}

// validateContextLimits checks the four context-management sizing fields for
// internal consistency. Called at the end of LoadConfig so any source
// (file/env/flag) is covered.
func validateContextLimits(cfg Config) error {
	if cfg.CompactAt <= 0 {
		return fmt.Errorf("compact_at must be > 0 (got %d)", cfg.CompactAt)
	}
	if cfg.HardMaxBytes <= 0 {
		return fmt.Errorf("hard_max_bytes must be > 0 (got %d)", cfg.HardMaxBytes)
	}
	if cfg.KeepBytes <= 0 {
		return fmt.Errorf("keep_bytes must be > 0 (got %d)", cfg.KeepBytes)
	}
	if cfg.SummaryBytes <= 0 {
		return fmt.Errorf("summary_bytes must be > 0 (got %d)", cfg.SummaryBytes)
	}
	if cfg.KeepBytes+cfg.SummaryBytes >= cfg.CompactAt {
		return fmt.Errorf(
			"keep_bytes (%d) + summary_bytes (%d) = %d must be < compact_at (%d) — "+
				"otherwise compaction cannot reduce the transcript below its own trigger",
			cfg.KeepBytes, cfg.SummaryBytes, cfg.KeepBytes+cfg.SummaryBytes, cfg.CompactAt)
	}
	if cfg.CompactAt >= cfg.HardMaxBytes {
		return fmt.Errorf("compact_at (%d) must be < hard_max_bytes (%d)", cfg.CompactAt, cfg.HardMaxBytes)
	}
	// Backend-truth context sizing: the fallback ceiling must leave a positive
	// prompt budget after the reasoning + answer reservations are removed.
	if cfg.ContextTokensFallback <= 0 {
		return fmt.Errorf("context_tokens_fallback must be > 0 (got %d)", cfg.ContextTokensFallback)
	}
	if cfg.ReasoningBudgetTokens < 0 {
		return fmt.Errorf("reasoning_budget_tokens must be >= 0 (got %d)", cfg.ReasoningBudgetTokens)
	}
	if cfg.AnswerMarginTokens < 0 {
		return fmt.Errorf("answer_margin_tokens must be >= 0 (got %d)", cfg.AnswerMarginTokens)
	}
	if cfg.ReasoningBudgetTokens+cfg.AnswerMarginTokens >= cfg.ContextTokensFallback {
		return fmt.Errorf(
			"reasoning_budget_tokens (%d) + answer_margin_tokens (%d) must be < context_tokens_fallback (%d) — "+
				"otherwise no prompt budget remains under the fallback ceiling",
			cfg.ReasoningBudgetTokens, cfg.AnswerMarginTokens, cfg.ContextTokensFallback)
	}
	// Relative context guard fractions — only validated when at least one is set.
	if cfg.CompactAtFrac != 0 || cfg.KeepBytesFrac != 0 || cfg.HardMaxFrac != 0 {
		if cfg.CompactAtFrac <= 0 || cfg.CompactAtFrac >= 1.0 {
			return fmt.Errorf("compact_at_frac must be in (0,1), got %g", cfg.CompactAtFrac)
		}
		if cfg.KeepBytesFrac <= 0 || cfg.KeepBytesFrac >= cfg.CompactAtFrac {
			return fmt.Errorf("keep_bytes_frac (%g) must be > 0 and < compact_at_frac (%g)",
				cfg.KeepBytesFrac, cfg.CompactAtFrac)
		}
		if cfg.HardMaxFrac <= cfg.CompactAtFrac || cfg.HardMaxFrac > 1.0 {
			return fmt.Errorf("hard_max_frac (%g) must be in (compact_at_frac=%g, 1.0]",
				cfg.HardMaxFrac, cfg.CompactAtFrac)
		}
	}
	// context_capacity_frac: headroom fraction below the proxy's usable_ctx.
	// Valid range is [0, 1.0]; 0 = use default (DefaultConfig supplies 0.80).
	if cfg.ContextCapacityFrac < 0 || cfg.ContextCapacityFrac > 1.0 {
		return fmt.Errorf("context_capacity_frac must be in [0, 1.0], got %g (0 = use default)", cfg.ContextCapacityFrac)
	}
	// effective_ctx_max_chars: absolute cap on effective context. Negative is
	// invalid; 0 = disabled (use full backend-reported context).
	if cfg.EffectiveCtxMaxChars < 0 {
		return fmt.Errorf("effective_ctx_max_chars must be >= 0 (got %d; 0 = disabled)", cfg.EffectiveCtxMaxChars)
	}
	// Subagent budget overrides — reject negative values. Zero means "use default".
	if cfg.SubagentMaxToolIter < 0 {
		return fmt.Errorf("subagent_max_tool_iterations must be >= 0 (got %d; 0 = use default)", cfg.SubagentMaxToolIter)
	}
	if cfg.SubagentTurnToolBudget < 0 {
		return fmt.Errorf("subagent_turn_tool_budget must be >= 0 (got %d; 0 = use default)", cfg.SubagentTurnToolBudget)
	}
	if cfg.SubagentToolResultCap < 0 {
		return fmt.Errorf("subagent_tool_result_cap must be >= 0 (got %d; 0 = use default)", cfg.SubagentToolResultCap)
	}
	return nil
}

// validateEnums checks that string-enum config fields have valid values.
// Unknown values are startup errors, not silent degradation.
func validateEnums(cfg Config) error {
	switch cfg.AutoCounsel {
	case "", "suggest", "auto", "off":
	default:
		return fmt.Errorf("auto_counsel must be one of: suggest, auto, off (got %q)", cfg.AutoCounsel)
	}
	switch cfg.WFOracleMode {
	case "", "every-step", "on-deviation", "phases-only":
	default:
		return fmt.Errorf("wf_oracle_mode must be one of: every-step, on-deviation, phases-only (got %q)", cfg.WFOracleMode)
	}
	switch cfg.SubagentBackend {
	case "", "inherit", "default":
	default:
		// Any other value is treated as a pinned backend name — valid if it
		// matches a known endpoint, but we don't verify that here (endpoints
		// are resolved later). Just reject obviously invalid patterns.
		if strings.ContainsAny(cfg.SubagentBackend, " \t\n") {
			return fmt.Errorf("subagent_backend must not contain whitespace (got %q)", cfg.SubagentBackend)
		}
	}
	return nil
}

// validateDockerConfig checks Docker-related fields for syntactic validity.
// Only validates non-empty fields — empty means "use default" or "no limit".
func validateDockerConfig(cfg Config) error {
	// DockerCaps: normalize to uppercase, strip optional CAP_ prefix.
	// Syntactic check only — we don't maintain an allowlist because
	// kernel/Docker versions add new capabilities over time.
	for i, cap := range cfg.DockerCaps {
		cap = strings.TrimSpace(cap)
		if cap == "" {
			return fmt.Errorf("docker_caps[%d]: empty capability name", i)
		}
		cap = strings.ToUpper(cap)
		cap = strings.TrimPrefix(cap, "CAP_")
		if !regexp.MustCompile(`^[A-Z_]+$`).MatchString(cap) {
			return fmt.Errorf("docker_caps[%d]: invalid capability name %q (must be alphanumeric + underscore)", i, cfg.DockerCaps[i])
		}
		cfg.DockerCaps[i] = cap
	}

	// Docker memory size: digits with optional decimal and unit (b/k/m/g).
	// Case-insensitive — Docker accepts both "4g" and "4G".
	dockerSizeRe := regexp.MustCompile(`^[0-9]+(\.[0-9]+)?[bBkKmMgG]?$`)
	if cfg.DockerMemory != "" {
		mem := strings.TrimSpace(cfg.DockerMemory)
		if !dockerSizeRe.MatchString(mem) {
			return fmt.Errorf("docker_memory: invalid size %q (expected e.g. \"4g\", \"512m\", \"1024\")", cfg.DockerMemory)
		}
	}
	if cfg.DockerTmpfsSize != "" {
		tmpfs := strings.TrimSpace(cfg.DockerTmpfsSize)
		if !dockerSizeRe.MatchString(tmpfs) {
			return fmt.Errorf("docker_tmpfs_size: invalid size %q (expected e.g. \"4g\", \"512m\", \"1024\")", cfg.DockerTmpfsSize)
		}
	}
	return nil
}

// validateURLs checks that non-empty URL fields are absolute HTTP(S) URLs
// with a non-empty host. Only validates the active resolved endpoint,
// not inactive entries in the endpoints block.
func validateURLs(cfg Config) error {
	// Active endpoint BaseURL.
	if cfg.Endpoint.BaseURL != "" {
		if err := validateHTTPURL(cfg.Endpoint.BaseURL, "base_url"); err != nil {
			return err
		}
	}

	// SearXngURL.
	if cfg.SearXngURL != "" {
		if err := validateHTTPURL(cfg.SearXngURL, "searxng_url"); err != nil {
			return err
		}
	}

	// MCP HTTP server URLs.
	for _, srv := range cfg.MCPServers {
		if srv.Transport == "http" && srv.URL != "" {
			field := fmt.Sprintf("mcp_servers[%s].url", srv.Name)
			if err := validateHTTPURL(srv.URL, field); err != nil {
				return err
			}
		}
	}

	return nil
}

// validateHTTPURL checks that a URL is an absolute HTTP(S) URL with a host.
func validateHTTPURL(raw, fieldName string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("%s: invalid URL %q: %v", fieldName, raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s: must be http or https URL (got %q)", fieldName, raw)
	}
	if u.Host == "" {
		return fmt.Errorf("%s: URL must have a host (got %q)", fieldName, raw)
	}
	return nil
}

// validateExternalCommands checks that MCP and LSP server configs have
// the required fields for their transport type. Only validates when servers
// are configured.
func validateExternalCommands(cfg Config) error {
	// MCP servers: stdio requires Command, http requires URL.
	for _, srv := range cfg.MCPServers {
		srvName := srv.Name
		if srvName == "" {
			srvName = "(unnamed)"
		}
		transport := srv.Transport
		if transport == "" {
			transport = "stdio" // default
		}
		switch transport {
		case "stdio":
			if strings.TrimSpace(srv.Command) == "" {
				return fmt.Errorf("mcp_servers[%s]: command is required for stdio transport", srvName)
			}
		case "http":
			if strings.TrimSpace(srv.URL) == "" {
				return fmt.Errorf("mcp_servers[%s]: url is required for http transport", srvName)
			}
		default:
			return fmt.Errorf("mcp_servers[%s]: unknown transport %q (want stdio or http)", srvName, transport)
		}
	}

	// LSP servers: validate only when LSP is enabled.
	if cfg.LSPEnabled {
		for name, srv := range cfg.LSPServers {
			if strings.TrimSpace(srv.Command) == "" {
				return fmt.Errorf("lsp_servers[%s]: command is required when lsp_enabled is true", name)
			}
		}
	}

	return nil
}

// validateTimeouts rejects negative timeout values that would cause
// immediate failures or undefined behavior.
func validateTimeouts(cfg Config) error {
	if cfg.OracleTimeoutSeconds < 0 {
		return fmt.Errorf("oracle_timeout_seconds must be >= 0 (got %d; 0 = use default 300s)", cfg.OracleTimeoutSeconds)
	}
	if cfg.LSPIdleTimeoutSeconds < 0 {
		return fmt.Errorf("lsp_idle_timeout_seconds must be >= 0 (got %d)", cfg.LSPIdleTimeoutSeconds)
	}
	if cfg.LSPIndexTimeoutSeconds < 0 {
		return fmt.Errorf("lsp_index_timeout_seconds must be >= 0 (got %d)", cfg.LSPIndexTimeoutSeconds)
	}
	return nil
}
