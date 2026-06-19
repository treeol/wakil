package config

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config is resolved with precedence: defaults < config file < env < flags.
// base_url takes precedence over host+port; if only host (and optionally port)
// are set, the URL is constructed as http://{host}:{port}.
type Config struct {
	BaseURL string `json:"base_url"` // full URL, e.g. "http://proxy-host:11400"
	Host    string `json:"host"`     // alternative to base_url: just the hostname/IP
	Port    int    `json:"port"`     // port for host (default 11400)

	APIKey         string `json:"api_key"` // sent as "Authorization: Bearer <key>"
	Model          string `json:"model"`
	ExecMode       string `json:"exec_mode"`               // "docker" (default) | "direct"
	Image          string `json:"image"`                   // container image for docker mode
	WorkDir        string `json:"work_dir"`                // working dir inside the container
	HostWorkDir    string `json:"host_work_dir,omitempty"` // host path mounted into container (files appear here)
	DockerSocket   bool   `json:"docker_socket,omitempty"` // bind-mount the host docker socket into the sandbox (drive host docker from inside)
	KeepBytes      int    `json:"keep_bytes"`              // max bytes of verbatim turns kept after compaction; default 120000
	SummaryBytes   int    `json:"summary_bytes"`           // cap on the running summary; re-summarize if exceeded; default 20000; 0=unlimited
	HardMaxBytes   int    `json:"hard_max_bytes"`          // unconditional ctx ceiling; compact+drop oldest until under; 0=disabled; default 160000
	TurnToolBudget int    `json:"turn_tool_budget"`        // per-turn cumulative tool output budget; reduced slice once exceeded; default 40000
	MaxChars       int    `json:"max_chars"`               // transcript-byte display ceiling for the hist line / compaction fallback
	CompactAt      int    `json:"compact_at"`              // trigger compaction at this size; 0 → use max_chars

	// Relative context guards — computed as fractions of the live backend's
	// usable context window (ContextLimit.Usable() × 4 chars/token). When n_ctx
	// is known these override the absolute values above, automatically scaling
	// with the window. When n_ctx is unknown the absolute values are the fallback.
	// All three must satisfy: 0 < KeepBytesFrac < CompactAtFrac < HardMaxFrac ≤ 1.
	CompactAtFrac float64 `json:"compact_at_frac"` // fraction of usable context to trigger compaction; default 0.75
	KeepBytesFrac float64 `json:"keep_bytes_frac"` // fraction of usable context to keep verbatim after compaction; default 0.60
	HardMaxFrac   float64 `json:"hard_max_frac"`   // fraction of usable context as unconditional ceiling; default 0.95

	// Backend-truth context sizing. The authoritative per-slot context window
	// (n_ctx, in tokens) is fetched from the backend at startup (see ctxlimit.go);
	// these are the headroom reservations and the fallback used only when that
	// fetch fails. All token-valued.
	ReasoningBudgetTokens int               `json:"reasoning_budget_tokens,omitempty"` // tokens reserved for extended thinking; default 4096
	AnswerMarginTokens    int               `json:"answer_margin_tokens,omitempty"`    // tokens reserved for the final answer; default 4096
	ContextTokensFallback int               `json:"context_tokens_fallback,omitempty"` // n_ctx assumed when the backend fetch fails; default 131072
	ToolResultCap         int               `json:"tool_result_cap"`                   // max chars kept in ctx per tool result; 0 = unlimited; default 8000
	ToolResultTTL         int               `json:"tool_result_ttl"`                   // evict large tool results after N completed turns; -1 = never; default 1
	MaxToolIterations     int               `json:"max_tool_iterations"`               // hard cap on tool round-trips per turn; on the last iteration tools are dropped to force a wrap-up answer; 0 = unlimited (parent default)
	ReadFileSizeLimit     int               `json:"read_file_size_limit,omitempty"`    // max bytes read_file accepts before refusing; default 1048576 (1 MB); 0 = use default
	MaxRequestBytes       int               `json:"max_request_bytes,omitempty"`       // pre-send byte guard: trim largest tool results if request exceeds this; default 8388608 (8 MB); 0 = disabled
	SearXngURL            string            `json:"searxng_url,omitempty"`             // native searxng_search tool if set
	GoogleAPIKey          string            `json:"google_api_key,omitempty"`          // Google Custom Search API key (enables native google_search tool)
	GoogleCX              string            `json:"google_cx,omitempty"`               // Google Programmable Search Engine ID
	MentionBase           string            `json:"mention_base,omitempty"`            // base dir for "@" file mentions (default: launch cwd)
	MCPServers            []MCPServerConfig `json:"mcp_servers,omitempty"`

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

	// Runtime-only flags (never read from / written to the JSON config file).
	Resume       bool   `json:"-"` // resume the most recent session
	ResumeID     string `json:"-"` // resume a session by chat_id or unique prefix
	ListSessions bool   `json:"-"` // list saved sessions and exit
	AutoApprove  bool   `json:"-"` // skip all confirmation prompts
	Trace        bool   `json:"-"` // tracing enabled for this run (TraceSessions || --trace flag)
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

// ExternalInferenceCost returns the exact cost of one external inference call
// given the "backend/model" key's configured rate. priced is false when the
// key has no rate (or a zero rate), so the source renders "—".
func (c CostsConfig) ExternalInferenceCost(backendModel string, inTok, outTok int64) (usd float64, priced bool) {
	r, ok := c.InferenceBackends[backendModel]
	if !ok || (r.InputUSDPer1M == 0 && r.OutputUSDPer1M == 0) {
		return 0, false
	}
	usd = float64(inTok)/1e6*r.InputUSDPer1M + float64(outTok)/1e6*r.OutputUSDPer1M
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
	Models             []string `json:"models"`                       // prefixed model strings, or ~ models for fusion
	Mode               string   `json:"mode,omitempty"`               // "panel" (default) | "fallback" | "fusion"
	FusionJudge        string   `json:"fusion_judge,omitempty"`       // fusion: judge model; "" = OpenRouter default
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

func DefaultConfig() Config {
	return Config{
		Model:          "ilm",
		ExecMode:       "docker",
		Image:          "wakil-dev",
		DockerSocket:   true,
		KeepBytes:      120000, // keep ~120k of recent verbatim turns after compaction
		SummaryBytes:   20000,  // cap the running summary; re-condense if it grows past this
		HardMaxBytes:   160000, // unconditional ceiling; compact then drop until under
		TurnToolBudget: 40000,  // per-turn tool output budget; reduced slice once exceeded
		MaxChars:       512000, // transcript-byte ceiling (hist line + compaction fallback)
		CompactAt:      145000, // fire before reaching hard max (post-compact target ~140k)
		CompactAtFrac:  0.75,   // compact at 75% of usable context
		KeepBytesFrac:  0.60,   // keep 60% of usable context verbatim after compaction
		HardMaxFrac:    0.95,   // hard ceiling at 95% of usable context

		ReasoningBudgetTokens: 4096,   // headroom for extended thinking
		AnswerMarginTokens:    4096,   // headroom for the final answer
		ContextTokensFallback: 131072, // assumed n_ctx when the backend is unreachable
		ToolResultCap:         8000,      // keep first 8k chars in ctx; spill the rest to disk
		ToolResultTTL:         3,         // evict after 3 completed turns (longer window before re-reads are needed)
		ReadFileSizeLimit:     1 << 20,   // 1 MB: refuse larger reads at the tool layer
		MaxRequestBytes:       8 << 20,   // 8 MB: trim tool results before sending if over
		BackendMaxRetries:     3,
		OracleModel:           "claude-sonnet-4-6",
		OracleMaxTokens:       4096,
		OracleAPIKeyEnv:       "ANTHROPIC_API_KEY",
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

// LoadConfig resolves configuration from all sources.
func LoadConfig(argv []string) (Config, error) {
	cfg := DefaultConfig()

	// 1) config file (explicit --config handled by pre-scan; else default path)
	cfgPath := defaultConfigPath()
	for i := 0; i < len(argv); i++ {
		switch {
		case (argv[i] == "--config" || argv[i] == "-config") && i+1 < len(argv):
			cfgPath = argv[i+1]
		case strings.HasPrefix(argv[i], "--config="):
			cfgPath = argv[i][len("--config="):]
		case strings.HasPrefix(argv[i], "-config="):
			cfgPath = argv[i][len("-config="):]
		}
	}
	if cfgPath != "" {
		b, err := os.ReadFile(cfgPath)
		if err != nil {
			if !os.IsNotExist(err) {
				return cfg, fmt.Errorf("reading %s: %w", cfgPath, err)
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
	envStr(&cfg.BaseURL, "ILM_BASE_URL")
	envStr(&cfg.Host, "ILM_HOST")
	envInt(&cfg.Port, "ILM_PORT")
	envStr(&cfg.APIKey, "ILM_API_KEY")
	envStr(&cfg.Model, "ILM_MODEL")
	envStr(&cfg.ExecMode, "ILM_EXEC_MODE")
	envStr(&cfg.Image, "ILM_CONTAINER_IMAGE")
	envStr(&cfg.WorkDir, "ILM_WORKDIR")
	envStr(&cfg.HostWorkDir, "ILM_HOST_WORKDIR")
	envBool(&cfg.DockerSocket, "ILM_DOCKER_SOCKET")
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
	fs.StringVar(&cfg.WorkDir, "workdir", cfg.WorkDir, "working directory inside the container")
	fs.StringVar(&cfg.HostWorkDir, "host-workdir", cfg.HostWorkDir, "host path bind-mounted into container (files appear here locally)")
	fs.BoolVar(&cfg.DockerSocket, "docker-sock", cfg.DockerSocket, "pass host docker socket into the sandbox so the agent can start host containers (default: on; use --docker-sock=false to disable)")
	fs.String("config", cfgPath, "path to config file")
	fs.BoolVar(&cfg.Resume, "resume", false, "resume the most recent session")
	fs.StringVar(&cfg.ResumeID, "resume-id", "", "resume a session by chat_id (or unique prefix)")
	fs.BoolVar(&cfg.ListSessions, "list-sessions", false, "list saved sessions and exit")
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

	if cfg.BaseURL == "" {
		return cfg, fmt.Errorf("proxy address required — set base_url (or host+port) in config, ILM_BASE_URL, or --base-url")
	}
	if cfg.ExecMode != "docker" && cfg.ExecMode != "direct" {
		return cfg, fmt.Errorf("invalid exec mode %q (want docker|direct)", cfg.ExecMode)
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
		if n, err := fmt.Sscanf(v, "%d", dst); n != 1 || err != nil {
			// ignore malformed value
		}
	}
}

func (c Config) AuthHeader() string {
	if c.APIKey == "" {
		return ""
	}
	return "Bearer " + c.APIKey
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
	return nil
}
