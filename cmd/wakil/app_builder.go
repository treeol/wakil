package main

// app_builder.go contains the shared App construction logic extracted from
// main.go and run.go (WP-6.7). Both entry points (TUI and headless) call
// buildApp to construct the *agent.App, avoiding drift between the two
// construction sites.
//
// Differences between the two entry points are handled via buildAppOpts:
//   - main.go (TUI): sets up resume session, RestoreRepoState, counsel mode
//     defaults, OpenRouter cache priming — all done by the caller AFTER
//     buildApp returns.
//   - run.go (headless): sets IsHeadless, AutoCounsel, MaxCounsel — passed
//     via opts.

import (
	"context"
	"fmt"
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
	"github.com/treeol/wakil/internal/staging"
	"github.com/treeol/wakil/internal/trace"
)

// buildAppOpts carries the entry-point-specific settings that differ between
// the TUI (main.go) and headless (run.go) construction paths.
type buildAppOpts struct {
	IsHeadless  bool
	AutoCounsel bool
	MaxCounsel  int
}

// appResources holds the side-effect handles (closers) that the caller must
// defer-close after buildApp returns. The App itself holds references to
// these resources but does not own their lifecycle — the caller does.
type appResources struct {
	mcpMgr     *agent.MCPManager
	lspMgr     *lsp.Manager
	browserMgr *browser.Manager
	traceStore *trace.Store
	memStore   *memory.Store
}

// buildApp constructs a *agent.App from config, executor, and entry-point
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
//   - Creating the executor (newExecutor)
//   - Resume/RestoreRepoState (main.go only)
//   - Counsel mode defaults (main.go only)
//   - OpenRouter cache priming (main.go only)
//   - Setting Out/Confirm/EventSink (done per-turn or at run time)
//   - Closing resources (exe, mcpMgr, lspMgr, traceStore, memStore)
//
// Returns the App and an appResources struct holding the closable resources.
func buildApp(cfg config.Config, exe exec.Executor, opts buildAppOpts) (*agent.App, appResources) {
	var res appResources

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
		AppReferer:      ep.AppReferer,
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
	res.mcpMgr = mcpMgr

	// LSP manager
	var lspMgr *lsp.Manager
	if cfg.LSPEnabled {
		rootURI := "file://" + exe.Cwd()
		lspMgr = lsp.NewManager(exe, cfg, rootURI)
	}
	res.lspMgr = lspMgr

	// Browser manager
	var browserMgr *browser.Manager
	if cfg.BrowserEnabled {
		mgr, err := browser.NewManager()
		if err != nil {
			fmt.Fprintln(os.Stderr, "browser:", err)
		} else {
			browserMgr = mgr
		}
	}
	res.browserMgr = browserMgr

	// Context limit resolution
	ctxLimit := agent.ResolveContextLimit(context.Background(), client.HTTP, cfg, os.Stderr)

	// Backend list
	backendList := agent.FetchBackendListWithFallback(context.Background(), client.HTTP, cfg, os.Stderr)

	// Model list
	modelList := agent.FetchModelListForEndpoint(context.Background(), client.HTTP, cfg)

	app := &agent.App{
		Cfg:             cfg,
		Client:          client,
		Exec:            exe,
		MCP:             mcpMgr,
		LSP:             lspMgr,
		Browser:         browserMgr,
		Tools:           agent.BuildTools(cfg, exe.Cwd(), mcpMgr),
		CtxLimit:        ctxLimit,
		AgentPrompt:     loadAgentPrompt(cfg),
		BackendList:     backendList,
		ModelList:       modelList,
		SelectedBackend: cfg.Backend,
		AgentPrefix:     "main",
		Out:             os.Stderr,
		Confirm:         func(_, _, _ string, _ bool) bool { return false },
		InjectDate:      true,
		AutoApprove:     cfg.AutoApprove,
		IsHeadless:      opts.IsHeadless,
		AutoCounsel:     opts.AutoCounsel,
		MaxCounsel:      opts.MaxCounsel,
		Costs:           proxy.NewCostTracker(),
	}

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
			res.memStore = memStore
		}
	}

	// Trace store
	if cfg.Trace {
		ts, err := trace.Open(cfg.TraceDir, client.ChatID, client.Model, app.Exec.WorkspaceRoot())
		if err != nil {
			fmt.Fprintln(os.Stderr, "trace: failed to open store:", err)
		} else {
			app.Trace = ts
			res.traceStore = ts
		}
	}

	return app, res
}
