package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/counsel"
	"github.com/treeol/wakil/internal/exec"
	"github.com/treeol/wakil/internal/lsp"
	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/trace"
	"github.com/treeol/wakil/internal/tui"

	tea "github.com/charmbracelet/bubbletea"
)

// globalProg is the running tea.Program. Set once in main() before Run().
// Agent goroutines use it to post tea.Msgs without threading the pointer
// through every call site. Safe: only written once before any goroutine reads.
var globalProg *tea.Program

func main() {
	// "wakil run" subcommand: headless, non-interactive, exits with a code.
	if len(os.Args) >= 2 && os.Args[1] == "run" {
		cfg, err := config.LoadConfig(nil) // flags after "run" are for RunHeadless, not LoadConfig
		if err != nil {
			fmt.Fprintln(os.Stderr, "config error:", err)
			os.Exit(ExitError)
		}
		os.Exit(RunHeadless(cfg, os.Args[2:]))
	}

	// --list-sessions short-circuits before config resolution so it works even
	// without a configured proxy. Scoped to the launch cwd by default (no config
	// has been loaded yet, so cwd is the only workspace identity available);
	// --all lists every session regardless of folder.
	listAll := false
	wantList := false
	for _, a := range os.Args[1:] {
		switch a {
		case "--list-sessions", "-list-sessions":
			wantList = true
		case "--all", "-all":
			listAll = true
		}
	}
	if wantList {
		cwd, _ := os.Getwd()
		agent.PrintSessions(os.Stdout, cwd, listAll)
		return
	}

	cfg, err := config.LoadConfig(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		os.Exit(2)
	}
	fmt.Fprintf(os.Stderr, "ctx limits: compactAt=%d hardMax=%d keep=%d summary=%d\n",
		cfg.CompactAt, cfg.HardMaxBytes, cfg.KeepBytes, cfg.SummaryBytes)

	ep := cfg.ActiveEndpoint()
	client := &proxy.Client{
		BaseURL:         strings.TrimRight(ep.BaseURL, "/"),
		Model:           ep.Model,
		Kind:            ep.Kind,
		ConfiguredModel: ep.Model,
		Temperature:     ep.Temperature,
		TopP:            ep.TopP,
		MaxTokens:       ep.MaxTokens,
		CachePrompt:     ep.CachePrompt,
		CacheControl:    ep.CacheControl,
		ChatID:          agent.NewChatID(),
		AuthHeader:      cfg.AuthHeader(),
		HTTP:            newHTTPClient(),
		MaxRequestBytes: cfg.MaxRequestBytes,
	}

	// Resume a saved session: reload its transcript and re-attach its chat_id so
	// the proxy's server-side memory for that conversation continues.
	//
	// --resume-id (an explicit id/prefix) always searches globally — the same
	// rule the TUI's /resume <id> follows — so a hint like "resume with <id>"
	// works from any directory. Bare --resume (no id) defaults to the most
	// recent session in the CURRENT workspace, resolved the same way
	// App.SessionWorkspace() would (host path in docker mode, work dir in
	// direct mode) — computed here directly since App doesn't exist yet.
	// --all overrides this to search every folder.
	var resumed *agent.Session
	if cfg.Resume || cfg.ResumeID != "" {
		ws := cfg.WorkDir
		if cfg.ExecMode != "direct" {
			ws = cfg.HostWorkDir
		}
		s, err := agent.LoadSessionScoped(cfg.ResumeID, agent.SessionScope{Workspace: ws, All: cfg.AllSessions})
		if err != nil {
			fmt.Fprintln(os.Stderr, "resume error:", err)
			os.Exit(1)
		}
		resumed = s
		client.ChatID = s.ChatID
	}

	exe, err := newExecutor(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "executor error:", err)
		os.Exit(1)
	}

	// Connect MCP servers before starting the TUI so tools are ready immediately.
	var mcpMgr *agent.MCPManager
	if len(cfg.MCPServers) > 0 {
		mcpMgr = agent.NewMCPManager(context.Background(), cfg.MCPServers)
	}

	tools := agent.BuildTools(cfg, exe.Cwd(), mcpMgr)

	// Initialize the LSP manager when enabled. The manager owns language server
	// processes (gopls) for code-intelligence tools. nil when LSPEnabled is false.
	var lspMgr *lsp.Manager
	if cfg.LSPEnabled {
		rootURI := "file://" + exe.Cwd()
		lspMgr = lsp.NewManager(exe, cfg, rootURI)
	}

	agentPrompt := loadAgentPrompt(cfg)

	// Backend-truth context sizing: ask the backend (through the proxy) for its
	// real per-slot n_ctx and cache it for the process. On failure this prints a
	// loud fallback note to stderr.
	ctxLimit := agent.ResolveContextLimit(context.Background(), client.HTTP, cfg, os.Stderr)

	// Prime the OpenRouter model-context cache in the background when any
	// mashura panel routes through OpenRouter. ResolveContextLength never
	// fetches on its own (oracle calls must not block on cold-cache network
	// I/O), so without this warm-up OpenRouter models silently get the
	// conservative fallback context length. Best-effort; errors are ignored.
	if panelsUseOpenRouter(cfg) {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_, _ = counsel.FetchModelContextLimits(ctx)
		}()
	}

	// Backend list: fetch /v1/ilm/backends; fall back to config external_backends.
	backendList := agent.FetchBackendListWithFallback(context.Background(), client.HTTP, cfg, os.Stderr)

	// Model list for /model completion: /v1/ilm/models (proxy) or /v1/models
	// (openai kind); silently empty on failure.
	modelList := agent.FetchModelListForEndpoint(context.Background(), client.HTTP, cfg)

	counselMode := cfg.AutoCounsel
	if counselMode == "" {
		counselMode = "suggest"
	}
	counselMax := cfg.CounselMaxPerSession
	if counselMode == "auto" && counselMax == 0 {
		counselMax = 3
	}

	app := &agent.App{
		Cfg:             cfg,
		Client:          client,
		Exec:            exe,
		MCP:             mcpMgr,
		LSP:             lspMgr,
		Tools:           tools,
		CtxLimit:        ctxLimit,
		AgentPrompt:     agentPrompt,
		BackendList:     backendList,
		ModelList:       modelList,
		SelectedBackend: cfg.Backend,
		CounselMode:     counselMode,
		MaxCounsel:      counselMax,
		// Out and Confirm are injected per-turn by runTurn.
		Out:         os.Stderr,
		Confirm:     func(_, _, _ string, _ bool) bool { return false },
		InjectDate:  true,
		AutoApprove: cfg.AutoApprove,
		Costs:       proxy.NewCostTracker(),
	}

	if resumed != nil {
		app.Conv = resumed.Conv
		app.Session = resumed
		app.Workflow = resumed.SavedWorkflow
	} else {
		app.Session = &agent.Session{
			ChatID:    client.ChatID,
			Model:     client.Model,
			Created:   time.Now(),
			Workspace: app.SessionWorkspace(),
		}
		// Per-repo terminal settings restore: only on a fresh conversation.
		// A resumed session's model/backend must never be silently changed by
		// a remembered folder preference. TUI-only — cmd/wakil/run.go (the
		// headless path) never calls this, since App.AutoApprove has no
		// effect on headless tool confirmation (see repostate.go doc comment).
		result := agent.RestoreRepoState(app)
		if result.Note != "" {
			app.StartupNote = result.Note
		}
		// Re-resolve context limits using the literal restored strings —
		// mirrors resolveBackendCtxCmd's own calling convention, avoiding the
		// empty-SelectedModel trap ApplyModelOverride leaves for openai-kind
		// endpoints (reading app.SelectedModel back here would be wrong).
		if result.Model != "" || result.Backend != "" {
			ctxLimit = agent.ResolveContextLimitForBackendModel(context.Background(), client.HTTP, cfg, result.Backend, result.Model, os.Stderr)
			app.CtxLimit = ctxLimit
		}
	}

	// Open the P38 trace store when tracing is enabled (trace_sessions:true or
	// --trace flag). Non-fatal: a failure prints a warning and continues without
	// tracing so a misconfigured trace_dir never prevents a session from starting.
	if cfg.Trace {
		ts, err := trace.Open(cfg.TraceDir, client.ChatID, client.Model, app.SessionWorkspace())
		if err != nil {
			fmt.Fprintln(os.Stderr, "trace: failed to open store:", err)
		} else {
			app.Trace = ts
			defer ts.Close()
		}
	}

	model := tui.NewTUIModel(app)
	prog := tea.NewProgram(model,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)
	globalProg = prog
	app.EventSink = func(msg interface{}) { globalProg.Send(msg) }

	if _, err := prog.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "tui error:", err)
		app.StopAllBackgroundProcs()
		exe.Close()
		if mcpMgr != nil {
			mcpMgr.Close()
		}
		os.Exit(1)
	}

	app.StopAllBackgroundProcs()
	exe.Close()
	if mcpMgr != nil {
		mcpMgr.Close()
	}
	if lspMgr != nil {
		lspMgr.Shutdown()
	}
}

// newHTTPClient returns an HTTP client suitable for SSE streaming. It sets only
// ResponseHeaderTimeout so stalls before the first response byte are caught, but
// a live stream can run as long as needed — the per-turn ctx handles cancellation.
func newHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 30 * time.Second
	return &http.Client{Transport: tr}
}

// panelsUseOpenRouter reports whether any configured mashura panel routes at
// least one model through OpenRouter ("openrouter:..." prefix or "~..." fusion
// syntax). Used to decide whether priming the OpenRouter model-context cache
// is worthwhile at startup.
func panelsUseOpenRouter(cfg config.Config) bool {
	for _, panel := range cfg.MashuraPanels {
		if panel.Mode == "fusion" {
			return true
		}
		for _, m := range panel.Models {
			if strings.HasPrefix(m, "openrouter:") || strings.HasPrefix(m, "~") {
				return true
			}
		}
	}
	return false
}

func newExecutor(cfg config.Config) (exec.Executor, error) {
	switch cfg.ExecMode {
	case "direct":
		return exec.NewDirectExecutor(cfg.WorkDir)
	default:
		// Resolve SSH commit signing on the host before the container starts.
		// Best-effort: a skip reason is logged, never fatal.
		signing, skip := exec.DetectSigning(cfg.SSHSigning, cfg.HostWorkDir)
		if skip != "" {
			fmt.Fprintln(os.Stderr, "signing disabled —", skip)
		}
		if signing.Enabled {
			fmt.Fprintf(os.Stderr, "ssh signing: active (agent %s, key %.24s…, autosign=%v)\n",
				signing.AgentSock, signing.PublicKey, signing.AutoSign)
		}

		// Staging dir: per-repo, host-side. Reuses workspaceKey via the
		// exported agent.StagingPath helper (same identity as repo-state).
		var stagingMount string
		if cfg.KVREnabled {
			var err error
			stagingMount, err = agent.EnsureStagingDir(cfg.HostWorkDir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "kvr: staging dir error (staging unavailable): %v\n", err)
				cfg.KVREnabled = false
			}
		}

		return exec.NewDockerExecutor(exec.DockerOpts{
			Image:                 cfg.Image,
			Workdir:               cfg.WorkDir,
			HostMount:             cfg.HostWorkDir,
			DockerSock:            cfg.DockerSocket,
			Signing:               signing,
			StagingMount:          stagingMount,
			KVREnabled:            cfg.KVREnabled,
			KVRMaxEntries:         cfg.KVRMaxEntries,
			KVRSweepIntervalSecs:  cfg.KVRSweepIntervalSecs,
			KVRSnapshotIntervalSecs: cfg.KVRSnapshotIntervalSecs,
		})
	}
}

// defaultAgentPrompt is the built-in fallback used when agent_prompt_path is
// missing or unreadable. Intentionally minimal — the real instructions live in
// the agent.txt file shipped alongside the config.
const defaultAgentPrompt = "You are Wakil, a terminal coding agent. Complete the stated task with the fewest correct actions, then report what you actually did. You are not done until the result is verified."

// loadAgentPrompt reads the agent operating instructions from cfg.AgentPromptPath.
// On success it logs the byte count and returns the content. On any failure it
// logs a warning and returns the built-in fallback so the process always has a
// usable system prompt.
func loadAgentPrompt(cfg config.Config) string {
	path := cfg.AgentPromptPath
	if path == "" {
		fmt.Fprintln(os.Stderr, "agent prompt: no path configured, using built-in fallback")
		return defaultAgentPrompt
	}
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent prompt: warning: cannot read %s (%v) — using built-in fallback\n", path, err)
		return defaultAgentPrompt
	}
	prompt := strings.TrimRight(string(b), "\n")
	fmt.Fprintf(os.Stderr, "agent prompt: loaded %d bytes from %s\n", len(b), path)
	return prompt
}
