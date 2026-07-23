// Package browser provides headless-browser-backed tools for visual verification,
// DOM inspection, and interaction testing via chromedp (Chrome DevTools Protocol).
//
// It mirrors the lsp_enabled pattern: off by default, gated behind
// browser_enabled in config.json. When enabled, the browser manager creates
// a headless Chromium context (launched eagerly at startup) and exposes
// browser_* tools to the agent.
//
// In docker exec mode, Chromium is launched INSIDE the sandbox container (via
// docker exec) and controlled from the host via chromedp.NewRemoteAllocator
// (CDP over WebSocket). This reuses the chromium installed in the Docker image.
// In direct mode, Chromium is launched as a child of the wakil process on the
// host via chromedp.NewExecAllocator.
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
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
	"github.com/treeol/wakil/internal/safe"
)

// SandboxExecutor is the minimal subset of exec.Executor that the browser
// manager needs. Defined here (not imported from internal/exec) to avoid a
// circular dependency. In docker mode, RunShell executes inside the container,
// ContainerName returns the container name, and CDPPort returns the host-side
// port published for the container's CDP endpoint. In direct mode,
// ContainerName returns "" and CDPPort returns 0.
type SandboxExecutor interface {
	RunShell(ctx context.Context, command string) (string, error)
	ContainerName() string
	CDPPort() int
}

// defaultTimeout is the per-operation timeout for browser actions. Without
// this, a navigation to a slow/dead server or a WaitVisible on a missing
// element would hang forever (the caller's ctx is honored too, but this
// prevents indefinite hangs when the caller has no deadline).
const defaultTimeout = 30 * time.Second

// Manager owns a headless Chromium browser context and provides the tool
// handlers for browser_* operations. It is nil when BrowserEnabled is false
// or when browser launch failed at startup.
//
// The manager eagerly launches Chromium in NewManager (see the doc comment
// there for why the first chromedp.Run must use m.ctx directly). All
// operations reuse the default tab; the agent can set viewport, emulate
// media features, click, evaluate JS, extract text/HTML, and capture
// screenshots.
type Manager struct {
	// allocCtx is the browser allocator context (one browser process).
	allocCtx context.Context
	// allocCancel cancels the allocator (shuts down the browser).
	allocCancel context.CancelFunc

	// ctx is the default browser context. All operations run against the
	// tab/target this context represents.
	ctx       context.Context
	ctxCancel context.CancelFunc

	// userDataDir is the writable temp directory passed to Chromium via
	// --user-data-dir. Required on a --read-only sandbox rootfs (see
	// NewManager). Removed on Close.
	userDataDir string

	// dockerExe and dockerContainer are set when the browser runs inside a
	// Docker container. Close() uses them to kill the container-side chromium
	// process (NewRemoteAllocator does not own the process lifecycle).
	dockerExe       SandboxExecutor
	dockerContainer string
}

// NewManager creates a browser Manager and eagerly launches a headless Chromium
// instance. In docker mode (exe.ContainerName() != ""), Chromium is launched
// INSIDE the container via docker exec, using the image's installed chromium.
// In direct mode (ContainerName() == ""), Chromium is launched as a child of
// the wakil process on the host.
//
// The browserPath argument, when non-empty, overrides the Chromium binary
// location. In docker mode this is passed to the container's chromium invocation.
// In direct mode it is passed to chromedp.ExecPath.
//
// Returns an error if Chromium cannot start (missing binary, missing shared
// libraries, etc.) so the caller can log a clear message at startup instead
// of masking the failure as "context canceled" on every tool call.
func NewManager(exe SandboxExecutor, browserPath string) (*Manager, error) {
	if exe != nil && exe.ContainerName() != "" {
		return newDockerManager(exe, browserPath)
	}
	return newLocalManager(browserPath)
}

// cdpReady checks if the CDP HTTP endpoint responds at the given URL.
func cdpReady(cdpURL string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(cdpURL + "/json/version")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

// newDockerManager launches Chromium inside the Docker container and connects
// to it via chromedp.NewRemoteAllocator (CDP over WebSocket). The container
// publishes port 9222 to an ephemeral host port (via -p 127.0.0.1::9222 at
// container creation), which the host-side browser manager connects to via
// localhost. This avoids container-IP routing issues (Docker Desktop, rootless
// Docker, firewalls) and works regardless of whether chromium honors
// --remote-debugging-address=0.0.0.0.
func newDockerManager(exe SandboxExecutor, browserPath string) (*Manager, error) {
	container := exe.ContainerName()
	hostPort := exe.CDPPort()
	if hostPort == 0 {
		return nil, fmt.Errorf("container %s did not publish CDP port 9222 — ensure BrowserEnabled is true in DockerOpts", container)
	}
	chromiumBin := browserPath
	if chromiumBin == "" {
		chromiumBin = "chromium"
	}

	// Preflight: verify the chromium binary exists inside the container.
	checkCtx, checkCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer checkCancel()
	if _, err := exe.RunShell(checkCtx, fmt.Sprintf("command -v %s", chromiumBin)); err != nil {
		return nil, fmt.Errorf("chromium binary %q not found in container %s — set browser_enabled:false or install chromium in the image", chromiumBin, container)
	}

	// Start chromium inside the container. Redirect stdout+stderr to a log file
	// so RunShell doesn't block on the pipe. Use --remote-debugging-port=9222
	// which is the in-container port published to the host.
	const cdpContainerPort = 9222
	const profileDir = "/tmp/wakil-chrome-profile"
	const logFile = "/tmp/wakil-chrome.log"
	// shellQuote escapes a string for safe use as a single shell argument.
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	startCmd := fmt.Sprintf(
		"HOME=/tmp %s --headless=new --no-sandbox --disable-gpu --disable-dev-shm-usage"+
			" --remote-debugging-port=%d"+
			" --user-data-dir=%s >%s 2>&1 &",
		quote(chromiumBin), cdpContainerPort, profileDir, logFile)
	if _, err := exe.RunShell(context.Background(), startCmd); err != nil {
		return nil, fmt.Errorf("cannot start Chromium in container %s (%w) — set browser_enabled:false or install chromium in the image", container, err)
	}

	// Connect via the published host port (127.0.0.1:<hostPort>). Poll the
	// CDP /json/version endpoint until it responds — chromium needs a moment
	// to start, and chromedp.Run fails fast on connection refused.
	cdpURL := fmt.Sprintf("http://127.0.0.1:%d", hostPort)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cdpReady(cdpURL) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !cdpReady(cdpURL) {
		// Chromium didn't start — fetch its logs to help diagnose.
		logOut, _ := exe.RunShell(context.Background(), "tail -20 "+logFile)
		return nil, fmt.Errorf("Chromium did not start in container %s (port %d not responding after 15s). Logs:\n%s", container, cdpContainerPort, strings.TrimSpace(logOut))
	}

	// Connect via chromedp.NewRemoteAllocator.
	allocCtx, allocCancel := chromedp.NewRemoteAllocator(context.Background(), cdpURL)
	browserCtx, browserCancel := chromedp.NewContext(allocCtx)

	m := &Manager{
		allocCtx:    allocCtx,
		allocCancel: allocCancel,
		ctx:         browserCtx,
		ctxCancel:   browserCancel,
		// Store the executor + container name so Close() can kill the
		// container-side chromium process.
		dockerExe:       exe,
		dockerContainer: container,
	}

	// Verify the CDP connection works by running a no-op action.
	launchDone := make(chan error, 1)
	safe.Go("browser-launch", func() { launchDone <- chromedp.Run(browserCtx) })
	select {
	case err := <-launchDone:
		if err != nil {
			m.Close()
			return nil, fmt.Errorf("cannot connect to Chromium in container %s at %s (%w) — ensure chromium is installed in the image", container, cdpURL, err)
		}
	case <-time.After(15 * time.Second):
		m.Close()
		return nil, fmt.Errorf("timed out connecting to Chromium in container %s at %s after 15s", container, cdpURL)
	}

	return m, nil
}

// newLocalManager launches Chromium as a child of the wakil process (host-side)
// using chromedp.NewExecAllocator. Used in direct mode or when no executor
// is available.
//
// Eager launch is critical: chromedp ties the browser process's lifetime to
// whichever context is used for the FIRST chromedp.Run call. If that first call
// uses a context derived with context.WithTimeout/WithCancel, canceling that
// derived context later kills the browser process outright. The eager launch
// below uses m.ctx DIRECTLY, with no derived timeout/cancel wrapping it.
//
// A writable user-data-dir + HOME are required on a --read-only sandbox rootfs:
// Chromium's crashpad crash-handler needs a writable directory for its database,
// and it reads $HOME for its own state. /tmp is writable (tmpfs) in the sandbox,
// so both point there. This was empirically verified against the sandbox's exact
// hardening flags (--read-only, --cap-drop=ALL, --security-opt=no-new-privileges,
// --tmpfs=/tmp, --tmpfs=/etc).
func newLocalManager(browserPath string) (*Manager, error) {
	userDataDir, err := os.MkdirTemp("", "wakil-chrome-profile-")
	if err != nil {
		return nil, fmt.Errorf("create user-data-dir: %w", err)
	}

	allocOpts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", "new"),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.UserDataDir(userDataDir),
		chromedp.Env("HOME="+os.TempDir()),
	)
	if browserPath != "" {
		allocOpts = append(allocOpts, chromedp.ExecPath(browserPath))
	}

	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), allocOpts...)
	ctx, ctxCancel := chromedp.NewContext(allocCtx)

	m := &Manager{
		allocCtx:    allocCtx,
		allocCancel: allocCancel,
		ctx:         ctx,
		ctxCancel:   ctxCancel,
		userDataDir: userDataDir,
	}

	// triedMsg returns a human-readable description of what binary was attempted.
	triedMsg := "PATH search (google-chrome, chromium, etc.)"
	if browserPath != "" {
		triedMsg = fmt.Sprintf("browser_path=%q", browserPath)
	}

	// Eager launch: run a no-op action to start the browser process now,
	// using m.ctx DIRECTLY (see the doc comment above — this is the critical
	// part, not just "eager"). The 15s timeout is enforced by racing against
	// a timer in a goroutine, NOT by deriving a cancelable context from ctx,
	// because canceling any context used for chromedp's first Run call kills
	// the browser process. On timeout or launch error, m.Close() tears down
	// the (failed or hung) allocator — that is a deliberate full abort, not
	// a mid-session op cancellation, so canceling ctx there is correct.
	launchDone := make(chan error, 1)
	safe.Go("browser-launch", func() { launchDone <- chromedp.Run(ctx) })
	select {
	case err := <-launchDone:
		if err != nil {
			m.Close()
			return nil, fmt.Errorf("cannot start Chromium, tried %s (%w) — set browser_path to a Chrome/Chromium binary, or set browser_enabled:false", triedMsg, err)
		}
	case <-time.After(15 * time.Second):
		m.Close()
		return nil, fmt.Errorf("timed out launching Chromium after 15s, tried %s — set browser_path to a Chrome/Chromium binary, or set browser_enabled:false", triedMsg)
	}

	return m, nil
}

// Close shuts down the browser process, releases all resources, and removes
// the temporary user-data-dir. For docker-mode managers, also kills the
// container-side chromium process (NewRemoteAllocator does not own the process
// lifecycle, so canceling the chromedp context only closes the WebSocket —
// the chromium process would otherwise leak until the container is destroyed).
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	if m.ctxCancel != nil {
		m.ctxCancel()
	}
	if m.allocCancel != nil {
		m.allocCancel()
	}
	if m.userDataDir != "" {
		os.RemoveAll(m.userDataDir)
	}
	// Docker mode: kill the chromium process inside the container.
	if m.dockerExe != nil && m.dockerContainer != "" {
		killCtx, killCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer killCancel()
		_, _ = m.dockerExe.RunShell(killCtx, "pkill -f 'remote-debugging-port=9222' 2>/dev/null; rm -rf /tmp/wakil-chrome-profile /tmp/wakil-chrome.log")
	}
	return nil
}

// opCtx derives a per-operation context from the caller's ctx and the
// manager's browser ctx. The caller's ctx provides cancellation (e.g., user
// cancels the turn); the timeout prevents indefinite hangs on slow pages or
// missing elements. The browser ctx is the parent so chromedp can track the
// operation against the right browser target.
//
// The returned cancel function cancels BOTH the timeout and the merged
// context, ensuring the goroutine that bridges caller cancellation exits
// cleanly (no leak).
func (m *Manager) opCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	parent := m.ctx
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

// Navigate opens a URL in the browser and waits for the page to load.
// Returns the page title and final URL (after redirects).
func (m *Manager) Navigate(ctx context.Context, url string) (title, finalURL string, err error) {
	if m == nil {
		return "", "", fmt.Errorf("browser: manager is not initialized")
	}
	octx, cancel := m.opCtx(ctx)
	defer cancel()
	err = chromedp.Run(octx,
		chromedp.Navigate(url),
		chromedp.WaitReady("body"),
		chromedp.Title(&title),
		chromedp.Location(&finalURL),
	)
	return title, finalURL, err
}

// Screenshot captures a screenshot of the current page and saves it to a
// temporary file. Returns the file path so the agent can reference it.
// If fullPage is true, captures the entire scrollable page.
func (m *Manager) Screenshot(ctx context.Context, fullPage bool) (string, error) {
	if m == nil {
		return "", fmt.Errorf("browser: manager is not initialized")
	}
	octx, cancel := m.opCtx(ctx)
	defer cancel()
	var buf []byte
	if fullPage {
		if err := chromedp.Run(octx, chromedp.FullScreenshot(&buf, 90)); err != nil {
			return "", err
		}
	} else {
		if err := chromedp.Run(octx, chromedp.CaptureScreenshot(&buf)); err != nil {
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
	return tmpFile.Name(), nil
}

// SetViewport sets the browser viewport dimensions for responsive testing.
// After calling this, subsequent screenshots and DOM queries use the new size.
func (m *Manager) SetViewport(ctx context.Context, width, height int) error {
	if m == nil {
		return fmt.Errorf("browser: manager is not initialized")
	}
	octx, cancel := m.opCtx(ctx)
	defer cancel()
	return chromedp.Run(octx,
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
	octx, cancel := m.opCtx(ctx)
	defer cancel()
	return chromedp.Run(octx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			return emulation.SetEmulatedMedia().
				WithMedia("").
				WithFeatures([]*emulation.MediaFeature{
					{Name: "prefers-reduced-motion", Value: value},
				}).Do(ctx)
		}),
	)
}

// Click performs a mouse click on the element matching the CSS selector.
// Waits for the element to be visible before clicking.
func (m *Manager) Click(ctx context.Context, selector string) error {
	if m == nil {
		return fmt.Errorf("browser: manager is not initialized")
	}
	octx, cancel := m.opCtx(ctx)
	defer cancel()
	return chromedp.Run(octx,
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
	octx, cancel := m.opCtx(ctx)
	defer cancel()
	// Evaluate into any to handle arbitrary return types (string, number,
	// boolean, object, null). chromedp JSON-decodes the CDP result.
	var result any
	err := chromedp.Run(octx,
		chromedp.Evaluate(expr, &result),
	)
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
	octx, cancel := m.opCtx(ctx)
	defer cancel()
	var text string
	err := chromedp.Run(octx,
		chromedp.Text(selector, &text),
	)
	return text, err
}

// GetHTML returns the outerHTML of the element matching the selector, or the
// full document HTML if selector is empty. Output is capped at 50KB to
// prevent context blowout on large pages.
func (m *Manager) GetHTML(ctx context.Context, selector string) (string, error) {
	if m == nil {
		return "", fmt.Errorf("browser: manager is not initialized")
	}
	octx, cancel := m.opCtx(ctx)
	defer cancel()
	var html string
	target := "html"
	if selector != "" {
		target = selector
	}
	err := chromedp.Run(octx, chromedp.OuterHTML(target, &html))
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
