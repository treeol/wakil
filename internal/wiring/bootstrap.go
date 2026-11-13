// Package wiring — bootstrap surface (chunk 7, plan D19).
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
	"sync"
	"time"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/browser"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/diag"
	"github.com/treeol/wakil/internal/exec"
	"github.com/treeol/wakil/internal/lsp"
	"github.com/treeol/wakil/internal/memory"
	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/sessionhistory"
	"github.com/treeol/wakil/internal/staging"
	"github.com/treeol/wakil/internal/styleguide"
	"github.com/treeol/wakil/internal/timeline"
	wtools "github.com/treeol/wakil/internal/tools"
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

	// DeferredStartup runs the startup work that BuildApp kept OFF the start
	// path: the three provider probes (model list, backend list, context
	// limit), the hisn orphan reconcile, and the staging/memory startup-note
	// scans. It returns a note to show the user (e.g. "memory: N proposals
	// pending") — empty for none. Headless callers invoke it immediately (no
	// prompt to return to); the TUI invokes it once after the first prompt, in
	// the background. nil when there is nothing to defer.
	DeferredStartup func(ctx context.Context) (note string)
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
//
// Returns an error if the workspace could not be resolved: the client needs a
// working directory to scope every request to the workspace it came from, and an
// unscoped request is rejected downstream, so failing here keeps the error next
// to its cause. Callers must treat this as fatal and close any executor they had
// already opened.
func BuildApp(cfg config.Config, exe exec.Executor, opts BuildAppOpts) (*agent.App, *AppResources, error) {
	defer timeline.Begin("build app")()
	var res AppResources

	// Validate user-owned style files before endpoint probes or tool managers.
	endGuides := timeline.Begin("style guides")
	guides, err := styleguide.NewCatalog(cfg.StyleGuidesEnabled, cfg.StyleGuidePaths)
	endGuides()
	if err != nil {
		return nil, nil, err
	}

	// The workspace the tools operate on (the executor's root), not the launch
	// directory: in docker mode this is the in-container /mnt/<basename>, in
	// direct mode the host path. Resolved once here rather than per request —
	// a session's workspace does not move, and re-reading it each turn would let
	// a mid-session chdir silently repoint the workspace.
	cwd, err := proxy.ResolveCwd(cfg.WorkDir)
	if err != nil {
		return nil, nil, err
	}

	ep := cfg.ActiveEndpoint()
	client := &proxy.Client{
		BaseURL:           strings.TrimRight(ep.BaseURL, "/"),
		Model:             ep.Model,
		Cwd:               cwd,
		Kind:              ep.Kind,
		ConfiguredModel:   ep.Model,
		Temperature:       ep.Temperature,
		TopP:              ep.TopP,
		MaxTokens:         ep.MaxTokens,
		CachePrompt:       ep.CachePrompt,
		ToolChoice:        ep.ToolChoice,
		CacheControl:      ep.CacheControl,
		AppTitle:          ep.AppTitle,
		ChatID:            agent.NewChatID(),
		AuthHeader:        cfg.AuthHeader(),
		HTTP:              newHTTPClient(),
		MaxRequestBytes:   cfg.MaxRequestBytes,
		StreamIdleTimeout: time.Duration(cfg.StreamIdleTimeoutSeconds) * time.Second,
		// Data policy: the effective switches (user config, overridable
		// per-repo via /zdr and /region) are enforced by the proxy client
		// itself on every outgoing request. ZDR defaults off everywhere,
		// so existing setups send byte-identical requests.
		ZDR:               cfg.ZDR,
		Region:            cfg.Region,
		TrustedLocalHosts: cfg.TrustedLocalHosts,
	}

	// MCP manager. Servers connect in the BACKGROUND (concurrently), so this
	// span measures only manager construction — the connects no longer block
	// the first prompt. Tools appear as each server becomes ready; a call to a
	// still-connecting server waits only on that server. See NewMCPManager.
	var mcpMgr *agent.MCPManager
	if len(cfg.MCPServers) > 0 {
		endMCP := timeline.Begin("mcp manager (background connect)")
		mcpMgr = agent.NewMCPManager(context.Background(), cfg.MCPServers)
		endMCP()
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
		var idleOpt browser.Option = browser.WithIdleTimeout(0)
		if cfg.BrowserIdleTimeoutSeconds > 0 {
			idleOpt = browser.WithIdleTimeout(time.Duration(cfg.BrowserIdleTimeoutSeconds) * time.Second)
		}
		endBr := timeline.Begin("browser manager (probe chromium)")
		mgr, err := browser.NewManager(exe, cfg.BrowserPath, idleOpt)
		endBr()
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

	// Provider probes (context limit, backend list, model list) are DEFERRED:
	// BuildApp leaves CtxLimit/BackendList/ModelList at zero and records a
	// closure that runs the three probes with the same 6s ceiling, applying
	// each result through the App setters. The TUI runs it after the first
	// prompt, so the probe round trip is off the start path. Headless callers
	// run it immediately (they have no prompt to return to).
	var appRef *agent.App
	runProbes := func(ctx context.Context) {
		// Capture the endpoint generation BEFORE probing; publish only if it is
		// unchanged, so a probe in flight during a /backend or /model switch
		// cannot overwrite the newer endpoint's resolved info with stale data.
		gen := appRef.EndpointGen()
		probeCtx, probeCancel := context.WithTimeout(ctx, 6*time.Second)
		defer probeCancel()
		var (
			ctxLimit    agent.ContextLimit
			backendList []agent.BackendInfo
			modelList   []string
		)
		var wg sync.WaitGroup
		wg.Add(3)
		endProbes := timeline.Begin("provider probes (3 in parallel, 6s ceiling)")
		go func() {
			defer wg.Done()
			defer timeline.BeginConcurrent("provider probe: context limit")()
			ctxLimit = agent.ResolveContextLimit(probeCtx, client.HTTP, cfg, os.Stderr)
		}()
		go func() {
			defer wg.Done()
			defer timeline.BeginConcurrent("provider probe: backend list")()
			backendList = agent.FetchBackendListWithFallback(probeCtx, client.HTTP, cfg, os.Stderr)
		}()
		go func() {
			defer wg.Done()
			defer timeline.BeginConcurrent("provider probe: model list")()
			modelList = agent.FetchModelListForEndpoint(probeCtx, client.HTTP, cfg)
		}()
		wg.Wait()
		endProbes()
		// Publish only if the endpoint did not change while probing.
		if appRef.EndpointGen() == gen {
			appRef.SetCtxLimit(ctxLimit)
			appRef.SetBackendList(backendList)
			appRef.SetModelList(modelList)
		}
	}

	// Load the agent prompt. The full prompt is used as AgentPrompt (the
	// preamble in full mode, or as the fallback when split prompts aren't
	// available). Split prompts (core + extension) are always loaded from the
	// binary embed below — they don't depend on whether the full prompt came
	// from a custom file or the embed.
	promptForApp, _ := loadAgentPrompt(cfg)

	app := &agent.App{
		Cfg:                          cfg,
		Client:                       client,
		Exec:                         exe,
		MCP:                          mcpMgr,
		LSP:                          lspMgr,
		Browser:                      browserMgr,
		Tools:                        agent.BuildTools(cfg, exe.Cwd(), mcpMgr, ""),
		AgentPrompt:                  promptForApp,
		SelectedBackend:              cfg.Backend,
		AgentPrefix:                  "main",
		Out:                          os.Stderr,
		Confirm:                      func(_, _, _ string, _ bool) bool { return false },
		InjectDate:                   true,
		EffectiveCtxMaxCharsOverride: -1, // -1 = not set → use config value
	}
	appRef = app
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
	app.SetStyleGuideCatalog(guides)
	// Data policy: wire the once-per-model zdr auto notice onto the
	// session transport. The effective switches were set into the client
	// at construction; RestoreRepoState later applies any per-repo
	// override on top.
	app.WireNoZDRNotice()
	// Bind the sandbox session to this conversation's chat (no-op for
	// executors without per-chat sessions). The session starts lazily.
	app.SetChatID(client.ChatID)
	// Bind the sandbox session to this conversation's chat (no-op for
	// executors without per-chat sessions). The session starts lazily.
	app.SetChatID(client.ChatID)

	// Initialize lifecycle hooks (nil when no hooks configured — hot path
	// skips all hook checks). Hooks run on the host, not the sandbox.
	app.Hooks = agent.NewHookEngine(cfg.Hooks, exe.Cwd())

	// Initialize consent state from the --auto flag. RestoreRepoState may
	// override this later (TUI path only); that uses SetAutoApprove too.
	// AllowDestructive and AllowReads start false — AllowDestructive is a
	// separate explicit opt-in (/auto destructive) and AllowReads is granted
	// mid-session via a confirm choice. Kept unexported: cmd/wakil cannot
	// set it directly, so the bridge is this exported setter.
	app.SetConsent(agent.ConsentSnapshot{AutoApprove: cfg.AutoApprove})

	// Initialize context mode from config. RestoreRepoState may override
	// this later (guarded by Cfg.ContextModeExplicit). Subagents force
	// standard regardless (see subagent construction).
	if err := app.SetContextMode(cfg.ContextMode); err != nil {
		// SetContextMode normalises via NormalizeContextMode, so an invalid
		// value here means the config itself is invalid — log and continue
		// with the default (standard) rather than crash startup.
		fmt.Fprintf(os.Stderr, "warning: invalid context_mode %q: %v\n", cfg.ContextMode, err)
	}

	// Split-prompt support: load the core and extension prompts unconditionally
	// so they're available when the mode changes at runtime (/prompt core) or
	// via repo-state restore. Only the embedded split prompts participate — a
	// custom agent_prompt_path file bypasses the feature (the full file is the
	// core; no extension is loaded), preserving backward compatibility with
	// user overrides.
	// RestoreRepoState may override the mode later (guarded by
	// Cfg.PromptModeExplicit).
	if err := app.SetPromptMode(cfg.PromptMode); err != nil {
		// SetPromptMode normalises via NormalizePromptMode — an invalid value
		// means invalid config. Log and continue with the default (full).
		fmt.Fprintf(os.Stderr, "warning: invalid prompt_mode %q: %v\n", cfg.PromptMode, err)
	}
	// Load the embedded core/extension split prompts unconditionally. They
	// are embedded in the binary and always available, independent of
	// whether the full AgentPrompt came from a custom file or the embed.
	// In full mode (default) AgentPrompt is used; in core mode AgentCorePrompt
	// is used as the lean preamble and AgentExtensionPrompt is lazy-loaded.
	// A custom agent_prompt_path only affects the full prompt — the split
	// prompts still come from the binary embed.
	app.AgentCorePrompt = strings.TrimRight(prompts.EmbeddedCorePrompt, "\n")
	app.AgentExtensionPrompt = strings.TrimRight(prompts.EmbeddedExtensionPrompt, "\n")

	// The sandbox's own risk statement (hisn: label + capabilities) is the first
	// thing the user sees in the session.
	appendStartupNote(app, SandboxBanner(exe))

	// Staging client
	if planned := plannedKVR(exe); planned != "" {
		// An in-sandbox store: its session starts lazily, so the client exists
		// from the plan and the guard (run before EVERY connect) starts the
		// session if needed, requires the service to be up, and vets the
		// socket (a directory the sandbox can write: no symlink swaps).
		app.StagingClient = staging.NewClient(planned)
		app.StagingClient.SetPathGuard(inSandboxKVRGuard(exe, planned))
	} else if kvrSocket := exe.KVRSocketPath(); kvrSocket != "" {
		app.StagingClient = staging.NewClient(kvrSocket)
	}

	// Memory store
	memDBPath := agent.MemoryDBPath(app.SessionWorkspace())
	if memDBPath != "" {
		endMem := timeline.Begin("open memory store")
		memStore, err := memory.Open(memDBPath, app.SessionWorkspace())
		endMem()
		if err != nil {
			fmt.Fprintln(os.Stderr, "memory: failed to open store:", err)
		} else {
			endLegacy := timeline.Begin("open legacy memory store (read-through)")
			app.LegacyMemory = agent.OpenLegacyMemory(app.Cfg, memDBPath)
			endLegacy()
			// Sweep runs in the background — it expires stale mid-tier
			// entries and is not on the critical path. The store is
			// usable immediately after Open; Sweep just reclaims space.
			// Errors go through diag (not os.Stderr) so they can't
			// garble the TUI alt-screen if the sweep finishes late.
			go func() {
				sweepCtx, sweepCancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer sweepCancel()
				if err := memStore.Sweep(sweepCtx); err != nil {
					diag.Printf("memory: sweep warning: %v", err)
				}
			}()
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
		endSk := timeline.Begin("open skill store")
		skillStore, err := memory.Open(skillDBPath, "")
		endSk()
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
		endSH := timeline.Begin("open session-history store")
		shStore, err := sessionhistory.Open(sessionHistPath)
		endSH()
		if err != nil {
			fmt.Fprintln(os.Stderr, "session history: failed to open store:", err)
		} else {
			app.SessionHistory = shStore
			res.SessionHistStore = shStore
		}
	}

	// Trace store
	if cfg.Trace {
		endTr := timeline.Begin("open trace store")
		ts, err := trace.Open(cfg.TraceDir, client.ChatID, client.Model, app.Exec.WorkspaceRoot())
		endTr()
		if err != nil {
			fmt.Fprintln(os.Stderr, "trace: failed to open store:", err)
		} else {
			app.Trace = ts
			res.TraceStore = ts
		}
	}

	// Deferred startup: everything BuildApp kept off the start path. Order:
	// probes first (they populate the model/backend/context info the TUI
	// shows), then the deferred hisn orphan reconcile. The startup-note scans
	// (AppResources closure set in BootstrapTUI) chain on top of this one. It
	// returns a note for the user (empty for none); the closure captures
	// app/exe/cfg.
	res.DeferredStartup = func(ctx context.Context) (note string) {
		runProbes(ctx)
		if be, ok := exe.(interface {
			RunDeferredReconcile(context.Context)
		}); ok {
			be.RunDeferredReconcile(ctx)
		}
		return ""
	}

	return app, &res, nil
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
	defer timeline.Begin("close resources")()
	app.OnStop()
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
		if cfg.DockerAllowUserns {
			fmt.Fprintln(os.Stderr, "warning: docker_allow_userns is set but exec_mode is direct — the setting has no effect in direct mode")
		}
		return exec.NewDirectExecutor(cfg.WorkDir)
	default:
		if cfg.Sandbox.Backend == config.SandboxBackendHisn {
			return newHisnExecutor(cfg)
		}
		if cfg.DockerAllowUserns {
			fmt.Fprintln(os.Stderr, "warning: docker sandbox isolation reduced: user namespaces enabled (sandbox.docker.allow_userns)")
		}
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
		// Keyed by agent.ProjectDir (0.5.0), the same identity as memory,
		// history and session-host storage.
		var stagingMount string
		kvrEnabled := !cfg.KVRDisabled
		if kvrEnabled {
			var err error
			stagingMount, err = agent.EnsureStagingDir(agent.ProjectDir(cfg))
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
			DockerAllowUserns:       cfg.DockerAllowUserns,
			DockerStartTimeout:      cfg.DockerStartTimeoutSec,
			DockerStopTimeout:       cfg.DockerStopTimeoutSec,
		})
	}
}

// LoadAgentPrompt reads the agent operating instructions from cfg.AgentPromptPath.
// On success it logs the byte count and returns the content. On any failure
// (missing file, read error, empty file, or no path configured) it logs a
// warning and returns the full embedded prompt from prompts/agent.txt so the
// process always has a complete system prompt — the bare binary is self-contained.
func LoadAgentPrompt(cfg config.Config) string { prompt, _ := loadAgentPrompt(cfg); return prompt }

// loadAgentPrompt reads the agent operating instructions from cfg.AgentPromptPath.
// On success it logs the byte count and returns the content. On any failure
// (missing file, read error, empty file, or no path configured) it logs a
// warning and returns the full embedded prompt from prompts/agent.txt so the
// process always has a complete system prompt — the bare binary is self-contained.
// usingEmbedded reports whether the returned content came from the embedded
// prompt (not a custom file). When true, the caller also loads the embedded
// core/extension split prompts so /prompt core works even when AgentPromptPath
// was auto-set to a non-existent file (the common case with a config.json).
func loadAgentPrompt(cfg config.Config) (prompt string, usingEmbedded bool) {
	embedded := strings.TrimRight(prompts.EmbeddedAgentPrompt, "\n")
	path := cfg.AgentPromptPath
	if path == "" {
		return embedded, true
	}
	b, err := os.ReadFile(path)
	if err != nil {
		// Not-found is the normal case (file is optional — embedded prompt is
		// the default). Only warn on real errors (permissions, I/O, etc.).
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "agent prompt: warning: cannot read %s (%v) — using embedded prompt\n", path, err)
		}
		return embedded, true
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		fmt.Fprintf(os.Stderr, "agent prompt: warning: %s is empty — using embedded prompt\n", path)
		return embedded, true
	}
	prompt = strings.TrimRight(string(b), "\n")
	fmt.Fprintf(os.Stderr, "agent prompt: loaded %d bytes from %s\n", len(b), path)
	return prompt, false
}

var toolCacheSweep sync.Once

// SweepToolCacheOnce prunes stale per-chat tool-cache dirs at most once per
// process, in the background (never on the critical path). It is called by
// the long-lived entry points (interactive TUI, daemon) — not by BuildApp, so
// tests and per-conversation rebuilds never touch the real data dir.
// retentionDays <= 0 disables it. Live chats are protected by the newest-file
// mtime rule (they are being written to); keepChatIDs adds explicit
// exemptions. Errors go through diag so a late finish cannot garble the TUI.
func SweepToolCacheOnce(retentionDays int, keepChatIDs ...string) {
	if retentionDays <= 0 {
		return
	}
	toolCacheSweep.Do(func() {
		keep := map[string]bool{}
		for _, id := range keepChatIDs {
			if id != "" {
				keep[id] = true
			}
		}
		go func() {
			n, err := wtools.SweepToolCache(wtools.ToolCacheRoot(),
				time.Duration(retentionDays)*24*time.Hour, time.Now(), keep)
			if err != nil {
				diag.Printf("toolcache: sweep warning: %v", err)
			} else if n > 0 {
				diag.Printf("toolcache: pruned %d stale chat dir(s) older than %d days", n, retentionDays)
			}
		}()
	})
}
