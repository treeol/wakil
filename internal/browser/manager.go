// Package browser provides headless-browser-backed tools for visual verification,
// DOM inspection, and interaction testing via chromedp (Chrome DevTools Protocol).
//
// It mirrors the lsp_enabled pattern: off by default, gated behind
// browser_enabled in config.json. When enabled, the browser manager exposes
// browser_* tools to the agent. Chromium is NOT started with the container or
// at startup: it is launched on the first browser tool call, stopped after
// browser_idle_timeout_seconds without use, and on /new, chat end and exit.
//
// In docker exec mode, Chromium is launched INSIDE the sandbox container as a
// direct child of a `docker exec` session (own process group, per-launch
// profile dir) and controlled from the host via remote.NewAllocator (CDP over
// WebSocket) through a loopback-only stdio tunnel; nothing listens on a
// non-loopback address. In direct mode, Chromium is launched as a child of the
// wakil process on the host via chromedp.NewExecAllocator.
//
// Navigation is unrestricted — the browser can reach any URL accessible from
// its network namespace (localhost dev servers, file:// paths, or external URLs
// if egress is not blocked). The agent is responsible for navigating to
// appropriate URLs; no localhost-only allowlist is enforced.
package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
	"github.com/treeol/wakil/internal/safe"
)

// SandboxExecutor is the minimal subset of exec.Executor that the browser
// manager needs. Defined here (not imported from internal/exec) to keep the
// package light. In docker mode, RunShell executes inside the container and
// ContainerName returns the container name; in direct mode ContainerName
// returns "".
type SandboxExecutor interface {
	RunShell(ctx context.Context, command string) (string, error)
	ContainerName() string
}

// defaultTimeout is the per-operation timeout for browser actions. Without
// this, a navigation to a slow/dead server or a WaitVisible on a missing
// element would hang forever (the caller's ctx is honored too, but this
// prevents indefinite hangs when the caller has no deadline).
const defaultTimeout = 30 * time.Second

// DefaultIdleTimeout is how long the browser may sit unused before it is
// stopped (browser_idle_timeout_seconds, default 600).
const DefaultIdleTimeout = 10 * time.Minute

// Manager owns a lazily launched headless Chromium and provides the tool
// handlers for browser_* operations. It is nil when BrowserEnabled is false
// or when the browser cannot be used at all (preflight failed).
//
// The browser is launched by the first operation (beginOp), reused by later
// ones, and stopped by the idle timer, Stop, or Close; the next operation after
// a stop launches a fresh one in a new profile dir. All operations reuse the
// default tab; the agent can set viewport, emulate media features, click,
// evaluate JS, extract text/HTML, and capture screenshots.
type Manager struct {
	// lifeMu serialises launch and stop. Operations hold it only while they are
	// admitted (launch included), never while they run.
	lifeMu sync.Mutex

	// mu guards everything below.
	mu       sync.Mutex
	sess     *session
	ctx      context.Context // the running browser's context; nil when stopped
	inflight int             // operations currently running
	idle     *time.Timer
	closed   bool

	launcher    launcher
	idleTimeout time.Duration

	// screenshots tracks temp screenshot files for cleanup on Close.
	screenshots  []string
	screenshotMu sync.Mutex
}

// Option configures NewManager.
type Option func(*Manager)

// WithIdleTimeout sets how long the browser may sit unused before it is
// stopped. d <= 0 selects DefaultIdleTimeout.
func WithIdleTimeout(d time.Duration) Option {
	return func(m *Manager) {
		if d > 0 {
			m.idleTimeout = d
		}
	}
}

// NewManager creates a browser Manager. Nothing is launched: Chromium starts on
// the first tool call. It only checks that a browser can plausibly be launched
// (docker mode: chromium exists in the container; direct mode: a Chrome/
// Chromium binary is on PATH or at browserPath), so a missing browser still
// disables the tools at startup instead of failing every call.
//
// The browserPath argument, when non-empty, overrides the Chromium binary
// location (docker: the in-container command; direct: the host path).
func NewManager(exe SandboxExecutor, browserPath string, opts ...Option) (*Manager, error) {
	// An executor that is neither a container nor the host (a sandbox backend
	// such as hisn) must never fall through to the host launcher below: that
	// would run Chromium outside the sandbox.
	if g, ok := exe.(interface{ HostBrowserAllowed() bool }); ok && !g.HostBrowserAllowed() {
		return nil, fmt.Errorf("browser tools are not supported by this sandbox backend")
	}
	m := &Manager{idleTimeout: DefaultIdleTimeout}
	for _, o := range opts {
		o(m)
	}
	if exe != nil && exe.ContainerName() != "" {
		l, err := newDockerLauncher(exe, browserPath)
		if err != nil {
			return nil, err
		}
		m.launcher = l
		return m, nil
	}
	l, err := newLocalLauncher(browserPath)
	if err != nil {
		return nil, err
	}
	m.launcher = l
	return m, nil
}

// quoteSh single-quotes s for use as one shell word.
func quoteSh(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

// begin admits one operation: it launches the browser if it is not running
// (first call, or first call after an idle/`/new` stop), pins it against the
// idle timer, and returns the operation context. The returned cancel ends the
// operation (and re-arms the idle timer).
func (m *Manager) begin(ctx context.Context) (context.Context, context.CancelFunc, error) {
	parent, end, err := m.beginOp()
	if err != nil {
		return nil, nil, err
	}
	octx, cancel := deriveOpCtx(parent, ctx)
	return octx, func() { cancel(); end() }, nil
}

// deriveOpCtx builds the per-operation context from the browser context
// (parent) and the caller's ctx. The caller's cancellation (user cancels the
// turn) is bridged in; defaultTimeout bounds slow pages and missing elements.
// The browser ctx is the parent so chromedp tracks the operation against the
// right browser target. The returned cancel ends the timeout and the merged
// context, so the bridging goroutine exits (no leak).
func deriveOpCtx(parent, ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		// No caller ctx — just use a timeout on the browser ctx.
		return context.WithTimeout(parent, defaultTimeout)
	}
	// Merge: use the browser ctx as parent (so chromedp targets the right
	// browser), but cancel when the caller's ctx is done. The goroutine
	// bridges caller cancellation into the merged context.
	merged, mergedCancel := context.WithCancel(parent)
	safe.Go("browser-ctx-bridge", func() {
		select {
		case <-ctx.Done():
			mergedCancel()
		case <-merged.Done():
		}
	})
	// Derive a timeout from merged. The returned cancel cancels both so
	// the goroutine exits and no resources leak.
	tctx, tcancel := context.WithTimeout(merged, defaultTimeout)
	return tctx, func() {
		tcancel()
		mergedCancel()
	}
}

// Close stops the browser, removes its profile dir and any screenshot temp
// files, and makes the manager refuse further operations. It is the Wakil-exit
// and chat-end hook. Safe on a nil Manager and idempotent.
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	err := m.stop(true)
	m.screenshotMu.Lock()
	for _, p := range m.screenshots {
		_ = os.Remove(p)
	}
	m.screenshots = nil
	m.screenshotMu.Unlock()
	return err
}

// Navigate opens a URL in the browser and waits for the page to load.
// Returns the page title and final URL (after redirects).
func (m *Manager) Navigate(ctx context.Context, url string) (title, finalURL string, err error) {
	if m == nil {
		return "", "", fmt.Errorf("browser: manager is not initialized")
	}
	octx, cancel, err := m.begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer cancel()
	if err := chromedp.Do(octx,
		chromedp.Navigate(url),
		chromedp.WaitReady("body"),
	); err != nil {
		return "", "", err
	}
	title, err = chromedp.Run(octx, chromedp.Title())
	if err != nil {
		return "", "", err
	}
	finalURL, err = chromedp.Run(octx, chromedp.Location())
	if err != nil {
		return "", "", err
	}
	return title, finalURL, nil
}

// Screenshot captures a screenshot of the current page and saves it to a
// temporary file. Returns the file path so the agent can reference it.
// If fullPage is true, captures the entire scrollable page.
func (m *Manager) Screenshot(ctx context.Context, fullPage bool) (string, error) {
	if m == nil {
		return "", fmt.Errorf("browser: manager is not initialized")
	}
	octx, cancel, err := m.begin(ctx)
	if err != nil {
		return "", err
	}
	defer cancel()
	var buf []byte
	if fullPage {
		buf, err = chromedp.Run(octx, chromedp.FullScreenshot(90))
		if err != nil {
			return "", err
		}
	} else {
		buf, err = chromedp.Run(octx, chromedp.CaptureScreenshot())
		if err != nil {
			return "", err
		}
	}
	// Save to a temp file so the agent can reference it. The file is in the
	// system temp dir; the caller (agent) can read it with read_file if the
	// endpoint supports vision, or just know the capture succeeded.
	tmpFile, err := os.CreateTemp("", "wakil-screenshot-*.png")
	if err != nil {
		return "", fmt.Errorf("browser: create screenshot file: %w", err)
	}
	if _, err := tmpFile.Write(buf); err != nil {
		tmpFile.Close()
		_ = os.Remove(tmpFile.Name())
		return "", fmt.Errorf("browser: write screenshot file: %w", err)
	}
	tmpFile.Close()
	// Track for cleanup on Close.
	m.screenshotMu.Lock()
	m.screenshots = append(m.screenshots, tmpFile.Name())
	m.screenshotMu.Unlock()
	return tmpFile.Name(), nil
}

// SetViewport sets the browser viewport dimensions for responsive testing.
// After calling this, subsequent screenshots and DOM queries use the new size.
func (m *Manager) SetViewport(ctx context.Context, width, height int) error {
	if m == nil {
		return fmt.Errorf("browser: manager is not initialized")
	}
	octx, cancel, err := m.begin(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	return chromedp.Do(octx,
		chromedp.EmulateViewport(int64(width), int64(height)),
	)
}

// EmulateReducedMotion emulates the prefers-reduced-motion: reduce media query.
// When enabled, the browser reports reduced-motion preference to the page,
// allowing verification that transitions are actually disabled.
func (m *Manager) EmulateReducedMotion(ctx context.Context, enable bool) error {
	if m == nil {
		return fmt.Errorf("browser: manager is not initialized")
	}
	value := "no-preference"
	if enable {
		value = "reduce"
	}
	octx, cancel, err := m.begin(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	return chromedp.Do(octx,
		chromedp.Func(func(ctx context.Context, t *chromedp.Target) error {
			_, err := chromedp.Call(ctx, emulation.SetEmulatedMedia, emulation.SetEmulatedMediaParams{
				Media: "",
				Features: []*emulation.MediaFeature{
					{Name: "prefers-reduced-motion", Value: value},
				},
			})
			return err
		}),
	)
}

// Click performs a mouse click on the element matching the CSS selector.
// Waits for the element to be visible before clicking.
func (m *Manager) Click(ctx context.Context, selector string) error {
	if m == nil {
		return fmt.Errorf("browser: manager is not initialized")
	}
	octx, cancel, err := m.begin(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	return chromedp.Do(octx,
		chromedp.WaitVisible(selector),
		chromedp.Click(selector),
	)
}

// EvalJS evaluates a JavaScript expression in the page and returns the result
// as a JSON string. Handles non-string results (numbers, booleans, objects,
// null) by JSON-encoding them.
func (m *Manager) EvalJS(ctx context.Context, expr string) (string, error) {
	if m == nil {
		return "", fmt.Errorf("browser: manager is not initialized")
	}
	octx, cancel, err := m.begin(ctx)
	if err != nil {
		return "", err
	}
	defer cancel()
	// Evaluate into any to handle arbitrary return types (string, number,
	// boolean, object, null). chromedp JSON-decodes the CDP result.
	result, err := chromedp.Run(octx, chromedp.Evaluate[any](expr))
	if err != nil {
		return "", err
	}
	if result == nil {
		return "null", nil
	}
	// If it's already a string, return it directly (common case for JS
	// expressions that return strings like computed styles).
	if s, ok := result.(string); ok {
		return s, nil
	}
	// JSON-encode non-string results for structured output.
	b, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", result), nil
	}
	return string(b), nil
}

// GetText extracts the text content of the element matching the CSS selector.
func (m *Manager) GetText(ctx context.Context, selector string) (string, error) {
	if m == nil {
		return "", fmt.Errorf("browser: manager is not initialized")
	}
	octx, cancel, err := m.begin(ctx)
	if err != nil {
		return "", err
	}
	defer cancel()
	return chromedp.Run(octx, chromedp.Text(selector))
}

// GetHTML returns the outerHTML of the element matching the selector, or the
// full document HTML if selector is empty. Output is capped at 50KB to
// prevent context blowout on large pages.
func (m *Manager) GetHTML(ctx context.Context, selector string) (string, error) {
	if m == nil {
		return "", fmt.Errorf("browser: manager is not initialized")
	}
	octx, cancel, err := m.begin(ctx)
	if err != nil {
		return "", err
	}
	defer cancel()
	target := "html"
	if selector != "" {
		target = selector
	}
	html, err := chromedp.Run(octx, chromedp.OuterHTML(target))
	if err != nil {
		return "", err
	}
	// Cap at 50KB to prevent context blowout.
	const maxHTMLLen = 50 * 1024
	if len(html) > maxHTMLLen {
		html = html[:maxHTMLLen] + "\n... (truncated at 50KB — use a more specific selector for full content)"
	}
	return html, nil
}

// ScreenshotDir returns the default directory for saved screenshots.
// Currently the system temp dir; could be configured in the future.
func ScreenshotDir() string {
	return filepath.Join(os.TempDir())
}
