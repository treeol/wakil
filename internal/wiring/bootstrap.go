// Package wiring — bootstrap surface (card #148 chunk 7, plan D19).
//
// This file owns *agent.App construction and resource lifecycle for BOTH
// entry points: the headless driver (see headless.go) and the TUI bootstrap in
// cmd/wakil/main.go. It is the only package that holds and imports *agent.App
// for construction, so cmd/wakil's headless path can stop importing internal/
// agent entirely (D12 / exit gate #1, headless half).
//
// Moved verbatim (exported, comments preserved) from cmd/wakil/app_builder.go
// and cmd/wakil/main.go: buildApp, appResources, closeResources, newHTTPClient,
// newExecutor, loadAgentPrompt.
package wiring

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/browser"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/exec"
	"github.com/treeol/wakil/internal/lsp"
	"github.com/treeol/wakil/internal/memory"
	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/sessionhistory"
	"github.com/treeol/wakil/internal/staging"
	"github.com/treeol/wakil/internal/trace"
	"github.com/treeol/wakil/prompts"
)

// BuildAppOpts carries the entry-point-specific settings that differ between
// the TUI (main.go) and headless (run.go) construction paths.
type BuildAppOpts struct {
	IsHeadless  bool
	AutoCounsel bool
	MaxCounsel  int
}

// AppResources holds the side-effect handles (closers) that the caller must
// defer-close after BuildApp returns. The App itself holds references to
// these resources but does not own their lifecycle — the caller does.
type AppResources struct {
	MCPMgr           *agent.MCPManager
	LSPMgr           *lsp.Manager
	BrowserMgr       *browser.Manager
	TraceStore       *trace.Store
	MemStore         *memory.Store
	SkillStore       *memory.Store
	SessionHistStore *sessionhistory.Store
}

// BuildApp constructs a *agent.App from config, executor, and entry-point
// options. It performs the shared construction steps that were previously
// duplicated between main.go and run.go:
//   - proxy.Client creation
//   - MCP manager init
//   - LSP manager init
//   - context limit resolution
//   - backend/model list fetching
//   - staging client init
//   - memory store init
//   - trace store init
//
// The caller is responsible for:
//   - Creating the executor (NewExecutor)
//   - Resume/RestoreRepoState (TUI only)
//   - Counsel mode defaults (TUI only)
//   - OpenRouter cache priming (TUI only)
//   - Setting Out/Confirm/EventSink (done per-turn or at run time)
//   - Closing resources (exe, mcpMgr, lspMgr, browserMgr, traceStore, memStore, skillStore)
//
// Returns the App and an AppResources struct holding the closable resources.
func BuildApp(cfg config.Config, exe exec.Executor, opts BuildAppOpts) (*agent.App, *AppResources) {
	var res AppResources

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
		AppTitle:        ep.AppTitle,
		ChatID:          agent.NewChatID(),
		AuthHeader:      cfg.AuthHeader(),
		HTTP:            newHTTPClient(),
		MaxRequestBytes: cfg.MaxRequestBytes,
	}

	// MCP manager
	var mcpMgr *agent.MCPManager
	if len(cfg.MCPServers) > 0 {
		mcpMgr = agent.NewMCPManager(context.Background(), cfg.MCPServers)
	}
	res.MCPMgr = mcpMgr

	// LSP manager
	var lspMgr *lsp.Manager
	if cfg.LSPEnabled {
		rootURI := "file://" + exe.Cwd()
		lspMgr = lsp.NewManager(exe, cfg, rootURI)
	}
	res.LSPMgr = lspMgr

	// Browser manager
	var browserMgr *browser.Manager
	if cfg.BrowserEnabled {
		mgr, err := browser.NewManager(exe, cfg.BrowserPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "browser:", err)
			// Disable browser tools so BuildTools doesn't advertise them
			// when the manager failed to init — otherwise the model sees
			// browser tools but every call returns an error.
			cfg.BrowserEnabled = false
		} else {
			browserMgr = mgr
		}
	}
	res.BrowserMgr = browserMgr

	// Context limit resolution
	ctxLimit := agent.ResolveContextLimit(context.Background(), client.HTTP, cfg, os.Stderr)

	// Backend list
	backendList := agent.FetchBackendListWithFallback(context.Background(), client.HTTP, cfg, os.Stderr)

	// Model list
	modelList := agent.FetchModelListForEndpoint(context.Background(), client.HTTP, cfg)

	app := &agent.App{
		Cfg:                          cfg,
		Client:                       client,
		Exec:                         exe,
		MCP:                          mcpMgr,
		LSP:                          lspMgr,
		Browser:                      browserMgr,
		Tools:                        agent.BuildTools(cfg, exe.Cwd(), mcpMgr),
		CtxLimit:                     ctxLimit,
		AgentPrompt:                  loadAgentPrompt(cfg),
		BackendList:                  backendList,
		ModelList:                    modelList,
		SelectedBackend:              cfg.Backend,
		AgentPrefix:                  "main",
		Out:                          os.Stderr,
		Confirm:                      func(_, _, _ string, _ bool) bool { return false },
		InjectDate:                   true,
		EffectiveCtxMaxCharsOverride: -1, // -1 = not set → use config value
	}
	// Deferred fields (slated for sub-struct extraction) are set via options,
	// not composite-literal keys — this is the WP-6.3-followup construction API.
	// Once all cross-package construction goes through options/setters, these
	// fields can be unexported into costState/turnState without breaking callers.
	app.ApplyOptions(
		agent.WithHeadless(opts.IsHeadless),
		agent.WithAutoCounsel(opts.AutoCounsel, opts.MaxCounsel),
		agent.WithCosts(proxy.NewCostTracker()),
		agent.WithVerifyEnabled(len(cfg.Verify) > 0),
	)

	// Initialize consent state from the --auto flag. RestoreRepoState may
	// override this later (TUI path only); that uses SetAutoApprove too.
	// AllowDestructive and AllowReads start false — AllowDestructive is a
	// separate explicit opt-in (/auto destructive) and AllowReads is granted
	// mid-session via a confirm choice. Kept unexported: cmd/wakil cannot
	// set it directly, so the bridge is this exported setter.
	app.SetConsent(agent.ConsentSnapshot{AutoApprove: cfg.AutoApprove})

	// Staging client
	if kvrSocket := exe.KVRSocketPath(); kvrSocket != "" {
		app.StagingClient = staging.NewClient(kvrSocket)
	}

	// Memory store
	memDBPath := agent.MemoryDBPath(app.SessionWorkspace())
	if memDBPath != "" {
		memStore, err := memory.Open(memDBPath, app.SessionWorkspace())
		if err != nil {
			fmt.Fprintln(os.Stderr, "memory: failed to open store:", err)
		} else {
			sweepCtx, sweepCancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := memStore.Sweep(sweepCtx); err != nil {
				fmt.Fprintln(os.Stderr, "memory: sweep warning:", err)
			}
			sweepCancel()
			app.MemoryStore = memStore
			res.MemStore = memStore
		}
	}

	// Skill store — GLOBAL (not workspace-keyed). Same memory.Store engine
	// opened at a shared path so skills are available across every session
	// and every project. No Sweep call: skills are always durable (no TTL),
	// so sweep is a no-op and would only churn. Anchors are disabled for
	// skills — there is no stable workspace root to anchor against, so we
	// pass "" as the workspace root.
	skillDBPath := agent.SkillDBPath()
	if skillDBPath != "" {
		skillStore, err := memory.Open(skillDBPath, "")
		if err != nil {
			fmt.Fprintln(os.Stderr, "skills: failed to open store:", err)
		} else {
			app.SkillStore = agent.NewSkillsProfile(skillStore)
			res.SkillStore = skillStore
		}
	}

	// Session-history index — WORKSPACE-KEYED, host-side, disposable (derived
	// from the session JSON files). Best-effort: a failure means recall and
	// indexing are unavailable, never that the session breaks.
	sessionHistPath := agent.SessionHistoryDBPath(app.SessionWorkspace())
	if sessionHistPath != "" {
		shStore, err := sessionhistory.Open(sessionHistPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "session history: failed to open store:", err)
		} else {
			app.SessionHistory = shStore
			res.SessionHistStore = shStore
		}
	}

	// Trace store
	if cfg.Trace {
		ts, err := trace.Open(cfg.TraceDir, client.ChatID, client.Model, app.Exec.WorkspaceRoot())
		if err != nil {
			fmt.Fprintln(os.Stderr, "trace: failed to open store:", err)
		} else {
			app.Trace = ts
			res.TraceStore = ts
		}
	}

	return app, &res
}

// CloseResources drains async work and closes all AppResources. It is used on
// the os.Exit error paths, where Go defers cannot fire (os.Exit skips them), so
// every resource built by BuildApp is released before the process exits. It
// does NOT close the executor — the caller owns exe and closes it after this
// call so that lspMgr/browserMgr (which hold exe) are shut down first. It is
// safe on a freshly built App with no async ops or background processes (both
// Stop methods early-return on an empty registry).
//
// NOTE: this helper is intentionally only used before os.Exit. The success
// paths and RunHeadless keep their defer-based cleanup, so CloseResources is
// never followed by a normal return that would fire resource defers again —
// avoiding any double-close.
func CloseResources(app *agent.App, res *AppResources) {
	app.StopAllAsyncOps()
	app.StopAllBackgroundProcs()
	if res.MemStore != nil {
		res.MemStore.Close()
	}
	if res.SkillStore != nil {
		res.SkillStore.Close()
	}
	if res.SessionHistStore != nil {
		res.SessionHistStore.Close()
	}
	if res.MCPMgr != nil {
		res.MCPMgr.Close()
	}
	if res.LSPMgr != nil {
		res.LSPMgr.Shutdown()
	}
	if res.BrowserMgr != nil {
		res.BrowserMgr.Close()
	}
	if res.TraceStore != nil {
		res.TraceStore.Close()
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

// NewExecutor builds the sandbox executor from config. It is the single
// construction site shared by the TUI and headless entry points (moved from
// cmd/wakil/main.go).
func NewExecutor(cfg config.Config) (exec.Executor, error) {
	switch cfg.ExecMode {
	case "direct":
		if cfg.DockerIOUring {
			fmt.Fprintln(os.Stderr, "warning: docker_io_uring is set but exec_mode is direct — io_uring setting has no effect in direct mode")
		}
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
		// Uses cfg.WorkspacePath() (respects WAKIL_WORKSPACE_PATH) for the key
		// so staging matches memory/history/session-host storage identity.
		var stagingMount string
		kvrEnabled := !cfg.KVRDisabled
		if kvrEnabled {
			var err error
			stagingMount, err = agent.EnsureStagingDir(cfg.WorkspacePath())
			if err != nil {
				fmt.Fprintf(os.Stderr, "kvr: staging dir error (staging unavailable): %v\n", err)
				kvrEnabled = false
			}
		}

		return exec.NewDockerExecutor(exec.DockerOpts{
			Image:                   cfg.Image,
			Workdir:                 cfg.WorkDir,
			HostMount:               cfg.HostWorkDir,
			DockerSock:              cfg.DockerSocket,
			Signing:                 signing,
			StagingMount:            stagingMount,
			KVREnabled:              kvrEnabled,
			KVRMaxEntries:           cfg.KVRMaxEntries,
			KVRSweepIntervalSecs:    cfg.KVRSweepIntervalSecs,
			KVRSnapshotIntervalSecs: cfg.KVRSnapshotIntervalSecs,
			DockerCaps:              cfg.DockerCaps,
			DockerMemory:            cfg.DockerMemory,
			DockerPidsLimit:         cfg.DockerPidsLimit,
			DockerTmpfsSize:         cfg.DockerTmpfsSize,
			DockerIOUring:           cfg.DockerIOUring,
			BrowserEnabled:          cfg.BrowserEnabled,
		})
	}
}

// LoadAgentPrompt reads the agent operating instructions from cfg.AgentPromptPath.
// On success it logs the byte count and returns the content. On any failure
// (missing file, read error, empty file, or no path configured) it logs a
// warning and returns the full embedded prompt from prompts/agent.txt so the
// process always has a complete system prompt — the bare binary is self-contained.
func LoadAgentPrompt(cfg config.Config) string { return loadAgentPrompt(cfg) }

// loadAgentPrompt reads the agent operating instructions from cfg.AgentPromptPath.
// On success it logs the byte count and returns the content. On any failure
// (missing file, read error, empty file, or no path configured) it logs a
// warning and returns the full embedded prompt from prompts/agent.txt so the
// process always has a complete system prompt — the bare binary is self-contained.
func loadAgentPrompt(cfg config.Config) string {
	embedded := strings.TrimRight(prompts.EmbeddedAgentPrompt, "\n")
	path := cfg.AgentPromptPath
	if path == "" {
		return embedded
	}
	b, err := os.ReadFile(path)
	if err != nil {
		// Not-found is the normal case (file is optional — embedded prompt is
		// the default). Only warn on real errors (permissions, I/O, etc.).
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "agent prompt: warning: cannot read %s (%v) — using embedded prompt\n", path, err)
		}
		return embedded
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		fmt.Fprintf(os.Stderr, "agent prompt: warning: %s is empty — using embedded prompt\n", path)
		return embedded
	}
	prompt := strings.TrimRight(string(b), "\n")
	fmt.Fprintf(os.Stderr, "agent prompt: loaded %d bytes from %s\n", len(b), path)
	return prompt
}
