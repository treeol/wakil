// Package browser provides headless-browser-backed tools for visual verification,
// DOM inspection, and interaction testing via chromedp (Chrome DevTools Protocol).
//
// It mirrors the lsp_enabled pattern: off by default, gated behind
// browser_enabled in config.json. When enabled, the browser manager creates
// a headless Chromium context (launched lazily on first use by chromedp) and
// exposes browser_* tools to the agent.
//
// The browser runs inside the sandbox container (chromium is installed in the
// Dockerfile image). Navigation is unrestricted — the browser can reach any
// URL the container's network namespace allows (localhost dev servers,
// file:// paths, or external URLs if egress is not blocked). The agent is
// responsible for navigating to appropriate URLs; no localhost-only allowlist
// is enforced.
package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
)

// defaultTimeout is the per-operation timeout for browser actions. Without
// this, a navigation to a slow/dead server or a WaitVisible on a missing
// element would hang forever (the caller's ctx is honored too, but this
// prevents indefinite hangs when the caller has no deadline).
const defaultTimeout = 30 * time.Second

// Manager owns a headless Chromium browser context and provides the tool
// handlers for browser_* operations. It is nil when BrowserEnabled is false.
//
// The manager creates a single browser context (lazily — chromedp starts the
// Chromium process on the first chromedp.Run call, not at construction).
// All operations reuse the default tab; the agent can set viewport, emulate
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
}

// NewManager creates a new browser Manager and eagerly launches the Chromium
// process. The browser runs with --no-sandbox (required inside a Docker
// container that drops all capabilities) and --disable-gpu. Call Close when
// the session ends to release the process.
//
// Eager launch is critical: if the first chromedp.Run happens inside a
// per-operation context (which has a timeout and gets canceled after the
// op), chromedp ties the browser process lifecycle to that context. When the
// per-op context is canceled, the browser dies and m.ctx is permanently
// canceled — every subsequent op fails with "context canceled". By launching
// with the long-lived m.ctx here, the browser process survives across ops.
//
// Returns an error if Chromium cannot start (missing binary, missing shared
// libraries, etc.) so the caller can log a clear message at startup instead
// of masking the failure as "context canceled" on every tool call.
func NewManager() (*Manager, error) {
	allocOpts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", "new"),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("disable-dev-shm-usage", true),
	)

	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), allocOpts...)
	ctx, ctxCancel := chromedp.NewContext(allocCtx)

	m := &Manager{
		allocCtx:    allocCtx,
		allocCancel: allocCancel,
		ctx:         ctx,
		ctxCancel:   ctxCancel,
	}

	// Eager launch: run a no-op action to start the browser process now,
	// tied to the long-lived m.ctx (not a per-op context). This surfaces
	// launch failures (missing binary, missing libs) immediately with a
	// real error instead of "context canceled" on every subsequent call.
	launchCtx, launchCancel := context.WithTimeout(ctx, 15*time.Second)
	defer launchCancel()
	if err := chromedp.Run(launchCtx); err != nil {
		m.Close()
		return nil, fmt.Errorf("browser: failed to launch Chromium (check that chromium is installed and has all shared libraries — try 'chromium --headless=new --no-sandbox --dump-dom about:blank' in the container): %w", err)
	}

	return m, nil
}

// Close shuts down the browser process and releases all resources.
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
	go func() {
		select {
		case <-ctx.Done():
			mergedCancel()
		case <-merged.Done():
		}
	}()
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
		os.Remove(tmpFile.Name())
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
