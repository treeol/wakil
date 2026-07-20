package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/treeol/wakil/internal/proxy"
)

// BrowserTools returns the headless browser tool definitions. Only called
// when cfg.BrowserEnabled is true. The tools are read-only/interactive —
// they navigate, inspect DOM, capture screenshots, and emulate media features.
// No confirmation needed (same as lsp_* — the browser runs inside the sandbox
// and cannot write to the filesystem or execute arbitrary commands).
func BrowserTools() []proxy.Tool {
	strProp := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "string", "description": desc}
	}
	intProp := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "integer", "description": desc}
	}
	boolProp := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "boolean", "description": desc}
	}
	obj := func(props map[string]interface{}, required ...string) json.RawMessage {
		m := map[string]interface{}{"type": "object", "properties": props}
		if len(required) > 0 {
			m["required"] = required
		}
		b, _ := json.Marshal(m)
		return b
	}

	return []proxy.Tool{
		{Type: "function", Function: proxy.ToolFunction{
			Name: "browser_navigate",
			Description: "Navigate the headless browser to a URL and wait for the page to load. " +
				"Returns the page title and final URL (after redirects). " +
				"Use for: loading a local dev server (http://localhost:PORT), " +
				"opening a file:// URL, or checking a deployed page. " +
				"No confirmation needed — the browser runs inside the sandbox.",
			Parameters: obj(map[string]interface{}{
				"url": strProp("URL to navigate to (e.g. http://localhost:3000, file:///path/to/index.html)"),
			}, "url"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "browser_screenshot",
			Description: "Capture a screenshot of the current page. Saves the PNG to a temp file and returns the path. " +
				"Set full_page=true to capture the entire scrollable page (default: viewport only). " +
				"Use for: visual verification, layout checks, capturing error states. " +
				"No confirmation needed — the browser runs inside the sandbox.",
			Parameters: obj(map[string]interface{}{
				"full_page": boolProp("Capture the entire scrollable page (default: false, viewport only)"),
			}),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "browser_viewport",
			Description: "Set the browser viewport dimensions for responsive testing. " +
				"After calling this, subsequent screenshots and DOM queries use the new size. " +
				"Common sizes: 375x812 (mobile), 768x1024 (tablet), 1280x720 (desktop), 1920x1080 (full HD). " +
				"No confirmation needed — the browser runs inside the sandbox.",
			Parameters: obj(map[string]interface{}{
				"width":  intProp("Viewport width in pixels (e.g. 375 for mobile)"),
				"height": intProp("Viewport height in pixels (e.g. 812 for mobile)"),
			}, "width", "height"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "browser_click",
			Description: "Click an element matching the CSS selector. Waits for the element to be visible before clicking. " +
				"Use for: interaction testing, button/form interaction, carousel advancement, tab switching. " +
				"Note: clicking can trigger page side effects (form submission, navigation, state changes). " +
				"No confirmation needed — the browser runs inside the sandbox.",
			Parameters: obj(map[string]interface{}{
				"selector": strProp("CSS selector for the element to click (e.g. \"button.submit\", \".carousel-next\", \"#tab-2\")"),
			}, "selector"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "browser_eval",
			Description: "Evaluate a JavaScript expression in the page and return the result (as a string or JSON). " +
				"Use for: DOM inspection, extracting computed styles, checking element visibility, " +
				"reading window state, verifying runtime behavior (e.g. transition-duration, scroll position). " +
				"Note: JS can trigger side effects (network requests, DOM mutation, state changes). " +
				"No confirmation needed — the browser runs inside the sandbox.",
			Parameters: obj(map[string]interface{}{
				"expression": strProp("JavaScript expression to evaluate (e.g. \"getComputedStyle(document.querySelector('.ring')).transitionDuration\")"),
			}, "expression"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "browser_text",
			Description: "Extract the text content of an element matching the CSS selector. " +
				"Use for: verifying rendered content, checking for error messages, extracting labels. " +
				"No confirmation needed — the browser runs inside the sandbox.",
			Parameters: obj(map[string]interface{}{
				"selector": strProp("CSS selector for the element (e.g. \"h1\", \".error-message\", \"#status\")"),
			}, "selector"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "browser_html",
			Description: "Get the outerHTML of an element, or the full document HTML if no selector is given. " +
				"Output is capped at 50KB to prevent context blowout. " +
				"Use for: inspecting DOM structure, verifying rendered markup, checking attribute values. " +
				"No confirmation needed — the browser runs inside the sandbox.",
			Parameters: obj(map[string]interface{}{
				"selector": strProp("CSS selector (optional — omit for full document HTML)"),
			}),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "browser_reduced_motion",
			Description: "Emulate the prefers-reduced-motion media query. " +
				"Set emulate=true to make the browser report prefers-reduced-motion: reduce to the page, " +
				"allowing verification that transitions/animations are actually disabled (not just branched-on in code). " +
				"Set emulate=false to restore the default (no-preference). " +
				"No confirmation needed — the browser runs inside the sandbox.",
			Parameters: obj(map[string]interface{}{
				"emulate": boolProp("true = emulate prefers-reduced-motion: reduce; false = restore no-preference"),
			}, "emulate"),
		}},
	}
}

// IsBrowserTool reports whether name is one of the browser_* tools.
func IsBrowserTool(name string) bool {
	switch name {
	case "browser_navigate", "browser_screenshot", "browser_viewport",
		"browser_click", "browser_eval", "browser_text", "browser_html",
		"browser_reduced_motion":
		return true
	}
	return false
}

// browserOps is the interface that *Manager satisfies. It covers only the
// operations dispatched by tool handlers — Close is lifecycle, not a tool op.
// This seam allows testing dispatch + argument validation with a fake,
// without launching Chrome.
type browserOps interface {
	Navigate(ctx context.Context, url string) (title, finalURL string, err error)
	Screenshot(ctx context.Context, fullPage bool) (string, error)
	SetViewport(ctx context.Context, width, height int) error
	EmulateReducedMotion(ctx context.Context, enable bool) error
	Click(ctx context.Context, selector string) error
	EvalJS(ctx context.Context, expr string) (string, error)
	GetText(ctx context.Context, selector string) (string, error)
	GetHTML(ctx context.Context, selector string) (string, error)
}

// Compile-time assertion that *Manager satisfies browserOps.
var _ browserOps = (*Manager)(nil)

// HandleToolCall dispatches a browser_* tool call to the appropriate manager
// method. Returns the result string for the model.
func (m *Manager) HandleToolCall(ctx context.Context, toolName string, argsJSON string) string {
	if m == nil {
		return "[browser: browser tools are not enabled. Configure browser_enabled in config.]"
	}
	return dispatchToolCall(m, ctx, toolName, argsJSON)
}

// dispatchToolCall is the testable dispatch seam: it parses args, validates,
// calls the appropriate browserOps method, and formats the result string.
// Called by (*Manager).HandleToolCall; also used directly in tests with a fake.
func dispatchToolCall(ops browserOps, ctx context.Context, toolName string, argsJSON string) string {
	switch toolName {
	case "browser_navigate":
		return handleNavigate(ops, ctx, argsJSON)
	case "browser_screenshot":
		return handleScreenshot(ops, ctx, argsJSON)
	case "browser_viewport":
		return handleViewport(ops, ctx, argsJSON)
	case "browser_click":
		return handleClick(ops, ctx, argsJSON)
	case "browser_eval":
		return handleEval(ops, ctx, argsJSON)
	case "browser_text":
		return handleText(ops, ctx, argsJSON)
	case "browser_html":
		return handleHTML(ops, ctx, argsJSON)
	case "browser_reduced_motion":
		return handleReducedMotion(ops, ctx, argsJSON)
	}
	return fmt.Sprintf("[browser: unknown tool %q]", toolName)
}

func handleNavigate(ops browserOps, ctx context.Context, argsJSON string) string {
	var args struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	if args.URL == "" {
		return "ERROR: url is required"
	}
	title, finalURL, err := ops.Navigate(ctx, args.URL)
	if err != nil {
		return fmt.Sprintf("[browser: navigate failed: %v]", err)
	}
	return fmt.Sprintf("navigated to: %s\ntitle: %s\nfinal URL: %s", args.URL, title, finalURL)
}

func handleScreenshot(ops browserOps, ctx context.Context, argsJSON string) string {
	var args struct {
		FullPage bool `json:"full_page"`
	}
	_ = json.Unmarshal([]byte(argsJSON), &args) // full_page is optional
	path, err := ops.Screenshot(ctx, args.FullPage)
	if err != nil {
		return fmt.Sprintf("[browser: screenshot failed: %v]", err)
	}
	return fmt.Sprintf("screenshot saved to: %s (%s)", path, func() string {
		if args.FullPage {
			return "full page"
		}
		return "viewport"
	}())
}

func handleViewport(ops browserOps, ctx context.Context, argsJSON string) string {
	var args struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	if args.Width <= 0 || args.Height <= 0 {
		return "ERROR: width and height must be positive integers"
	}
	if err := ops.SetViewport(ctx, args.Width, args.Height); err != nil {
		return fmt.Sprintf("[browser: set viewport failed: %v]", err)
	}
	return fmt.Sprintf("viewport set to %dx%d", args.Width, args.Height)
}

func handleClick(ops browserOps, ctx context.Context, argsJSON string) string {
	var args struct {
		Selector string `json:"selector"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	if args.Selector == "" {
		return "ERROR: selector is required"
	}
	if err := ops.Click(ctx, args.Selector); err != nil {
		return fmt.Sprintf("[browser: click %q failed: %v]", args.Selector, err)
	}
	return fmt.Sprintf("clicked: %s", args.Selector)
}

func handleEval(ops browserOps, ctx context.Context, argsJSON string) string {
	var args struct {
		Expression string `json:"expression"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	if args.Expression == "" {
		return "ERROR: expression is required"
	}
	result, err := ops.EvalJS(ctx, args.Expression)
	if err != nil {
		return fmt.Sprintf("[browser: eval failed: %v]", err)
	}
	if result == "" {
		return "(eval returned empty/undefined)"
	}
	return strings.TrimSpace(result)
}

func handleText(ops browserOps, ctx context.Context, argsJSON string) string {
	var args struct {
		Selector string `json:"selector"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	if args.Selector == "" {
		return "ERROR: selector is required"
	}
	text, err := ops.GetText(ctx, args.Selector)
	if err != nil {
		return fmt.Sprintf("[browser: get text %q failed: %v]", args.Selector, err)
	}
	return text
}

func handleHTML(ops browserOps, ctx context.Context, argsJSON string) string {
	var args struct {
		Selector string `json:"selector"`
	}
	_ = json.Unmarshal([]byte(argsJSON), &args) // selector is optional
	html, err := ops.GetHTML(ctx, args.Selector)
	if err != nil {
		return fmt.Sprintf("[browser: get HTML failed: %v]", err)
	}
	return html
}

func handleReducedMotion(ops browserOps, ctx context.Context, argsJSON string) string {
	var args struct {
		Emulate bool `json:"emulate"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	if err := ops.EmulateReducedMotion(ctx, args.Emulate); err != nil {
		return fmt.Sprintf("[browser: emulate reduced motion failed: %v]", err)
	}
	state := "no-preference"
	if args.Emulate {
		state = "reduce"
	}
	return fmt.Sprintf("prefers-reduced-motion emulation set to: %s", state)
}
