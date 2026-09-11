package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/treeol/wakil/internal/config"
)

// HookTiming constants define when hooks fire.
const (
	hookPreTool      = "pre_tool"
	hookPostTool     = "post_tool"
	hookSessionStart = "session_start"
	hookSessionEnd   = "session_end"
	hookOnStop       = "on_stop"
)

// hookTimeout is the default timeout for hook command execution.
const hookTimeout = 30 * time.Second

// hookContext carries information about the triggering event to the hook.
type hookContext struct {
	toolName string
	toolArgs string
	filePath string // extracted from tool args when available
	cwd      string
}

// hookResult is the outcome of running a hook.
type hookResult struct {
	output   string // stdout+stderr, capped
	blocked  bool   // pre-hook: non-zero exit blocks the tool
	blockMsg string // human-readable reason when blocked
}

// HookEngine manages hook configuration and execution. Exported because it
// is set on App by the wiring layer.
type HookEngine struct {
	config  config.HooksConfig
	cwd     string
	timeout time.Duration
}

// NewHookEngine creates a hook engine from config. Returns nil if no hooks
// are configured (so the hot path skips all hook checks).
func NewHookEngine(cfg config.HooksConfig, cwd string) *HookEngine {
	if !hasHooks(cfg) {
		return nil
	}
	return &HookEngine{config: cfg, cwd: cwd, timeout: hookTimeout}
}

// SetTimeout overrides the hook execution timeout (for tests).
func (h *HookEngine) SetTimeout(d time.Duration) { h.timeout = d }

func hasHooks(cfg config.HooksConfig) bool {
	return len(cfg.PreTool) > 0 || len(cfg.PostTool) > 0 ||
		len(cfg.SessionStart) > 0 || len(cfg.SessionEnd) > 0 ||
		len(cfg.OnStop) > 0
}

// RunPreToolHooks runs all matching pre_tool hooks. If any returns non-zero,
// the tool is blocked. Hooks run in config order. A blocked hook stops
// further hooks for this tool.
func (h *HookEngine) RunPreToolHooks(ctx context.Context, toolName, toolArgs, cwd string) hookResult {
	hc := hookContext{
		toolName: toolName,
		toolArgs: toolArgs,
		filePath: extractFilePath(toolName, toolArgs),
		cwd:      cwd,
	}
	var result hookResult
	for _, hk := range h.config.PreTool {
		if !toolMatches(hk.Tool, hc.toolName) {
			continue
		}
		r := h.runHook(ctx, hk, hc)
		if r.blocked {
			return r
		}
		result.output += r.output
	}
	return result
}

// RunPostToolHooks runs all matching post_tool hooks. All matching hooks
// run regardless of individual exit codes — a post-hook cannot block.
// Output is concatenated and returned for injection into context.
func (h *HookEngine) RunPostToolHooks(ctx context.Context, toolName, toolArgs, cwd string) hookResult {
	hc := hookContext{
		toolName: toolName,
		toolArgs: toolArgs,
		filePath: extractFilePath(toolName, toolArgs),
		cwd:      cwd,
	}
	var result hookResult
	for _, hk := range h.config.PostTool {
		if !toolMatches(hk.Tool, hc.toolName) {
			continue
		}
		r := h.runHook(ctx, hk, hc)
		result.output += r.output
	}
	return result
}

// RunSessionHooks runs session_start, session_end, or on_stop hooks.
func (h *HookEngine) RunSessionHooks(ctx context.Context, timing string) {
	var hooks []config.HookConfig
	switch timing {
	case hookSessionStart:
		hooks = h.config.SessionStart
	case hookSessionEnd:
		hooks = h.config.SessionEnd
	case hookOnStop:
		hooks = h.config.OnStop
	}
	hc := hookContext{cwd: h.cwd}
	for _, hk := range hooks {
		h.runHook(ctx, hk, hc)
	}
}

// runHook executes a single hook command. The command runs via the host
// shell (not the sandbox executor) — hooks are user-configured, run with
// user privileges, and operate on the host filesystem. Environment
// variables WAKIL_TOOL_NAME, WAKIL_FILE_PATH, and WAKIL_CWD are set
// IN ADDITION to the inherited environment (PATH, HOME, etc.).
func (h *HookEngine) runHook(ctx context.Context, hk config.HookConfig, hc hookContext) hookResult {
	if hk.Command == "" {
		return hookResult{}
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	cmd := exec.CommandContext(timeoutCtx, "sh", "-c", hk.Command)
	cmd.Dir = h.cwd
	if cmd.Dir == "" {
		cmd.Dir = "."
	}

	// Inherit the host environment and ADD Wakil-specific vars.
	// This ensures PATH, HOME, and other essentials are available.
	cmd.Env = os.Environ()
	cmd.Env = append(cmd.Env,
		"WAKIL_TOOL_NAME="+hc.toolName,
		"WAKIL_FILE_PATH="+hc.filePath,
		"WAKIL_CWD="+hc.cwd,
	)

	// Process group: when the timeout fires, kill the entire process group
	// (sh + any children like sleep), not just the sh process.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Use Cancel + WaitDelay (Go 1.20+) for coordinated timeout:
	// When the context expires, Cmd sends SIGKILL to the process, and
	// WaitDelay forces the process to terminate within 2s of the signal.
	// Combined with Setpgid, this kills the whole process group.
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second

	out, err := cmd.CombinedOutput()
	trimmed := strings.TrimSpace(string(out))
	if len(trimmed) > 2000 {
		trimmed = trimmed[:2000] + "… (truncated)"
	}
	// Note: blocked is also set for post-tool hooks on timeout/error, but
	// RunPostToolHooks ignores r.blocked (post hooks cannot block), so this
	// is harmless — the field just carries diagnostic info in the result.
	if err != nil {
		return hookResult{
			output:   trimmed,
			blocked:  true,
			blockMsg: fmt.Sprintf("hook '%s' blocked: %s", hk.Command, err.Error()),
		}
	}
	return hookResult{output: trimmed}
}

// toolMatches checks if a hook's tool pattern matches the actual tool name.
// A hook with no tool pattern ("") matches all tools. Patterns support
// pipe-separated alternatives: "write_file|edit_file" matches either.
func toolMatches(pattern, toolName string) bool {
	if pattern == "" {
		return true
	}
	for _, p := range strings.Split(pattern, "|") {
		p = strings.TrimSpace(p)
		if p == toolName {
			return true
		}
	}
	return false
}

// extractFilePath pulls the file path from tool arguments JSON for hooks
// that need $WAKIL_FILE_PATH (e.g. post_tool gofmt). Uses encoding/json
// for correct parsing.
func extractFilePath(toolName, args string) string {
	var fields map[string]any
	if err := json.Unmarshal([]byte(args), &fields); err != nil {
		return ""
	}
	switch toolName {
	case "write_file", "write_binary_file", "delete_file", "read_file", "read_file_full", "edit_file",
		"replace", "multi_edit", "list_dir", "find_files", "search_files":
		// These tools all use a top-level "path" JSON field. For directory-
		// scoped tools (list_dir, find_files, search_files) the value may
		// be a directory, not an individual file.
		if v, ok := fields["path"].(string); ok {
			return v
		}
	case "move_file":
		if v, ok := fields["src"].(string); ok {
			return v
		}
		if v, ok := fields["dst"].(string); ok {
			return v
		}
	}
	return ""
}
