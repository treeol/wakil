package browser

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// dispatch_test.go — tests for the dispatchToolCall seam using a fake
// browserOps. No Chrome is launched. These tests cover argument parsing,
// validation, error propagation, and output formatting for all 8 browser tools.

// fakeBrowserOps implements browserOps for testing. It records calls and
// returns configurable results/errors.
type fakeBrowserOps struct {
	// Call counters — distinguish "not called" from "called with zero value"
	navCalls        int
	screenshotCalls int
	viewportCalls   int
	reducedCalls    int
	clickCalls      int
	evalCalls       int
	textCalls       int
	htmlCalls       int

	// Recorded calls
	navURL       string
	screenshotFP bool
	viewportW    int
	viewportH    int
	reducedM     bool
	clickSel     string
	evalExpr     string
	textSel      string
	htmlSel      string

	// Configurable returns
	navTitle       string
	navFinalURL    string
	navErr         error
	screenshotPath string
	screenshotErr  error
	viewportErr    error
	reducedErr     error
	clickErr       error
	evalResult     string
	evalErr        error
	textResult     string
	textErr        error
	htmlResult     string
	htmlErr        error
}

func (f *fakeBrowserOps) Navigate(ctx context.Context, url string) (string, string, error) {
	f.navCalls++
	f.navURL = url
	return f.navTitle, f.navFinalURL, f.navErr
}
func (f *fakeBrowserOps) Screenshot(ctx context.Context, fullPage bool) (string, error) {
	f.screenshotCalls++
	f.screenshotFP = fullPage
	return f.screenshotPath, f.screenshotErr
}
func (f *fakeBrowserOps) SetViewport(ctx context.Context, w, h int) error {
	f.viewportCalls++
	f.viewportW = w
	f.viewportH = h
	return f.viewportErr
}
func (f *fakeBrowserOps) EmulateReducedMotion(ctx context.Context, enable bool) error {
	f.reducedCalls++
	f.reducedM = enable
	return f.reducedErr
}
func (f *fakeBrowserOps) Click(ctx context.Context, selector string) error {
	f.clickCalls++
	f.clickSel = selector
	return f.clickErr
}
func (f *fakeBrowserOps) EvalJS(ctx context.Context, expr string) (string, error) {
	f.evalCalls++
	f.evalExpr = expr
	return f.evalResult, f.evalErr
}
func (f *fakeBrowserOps) GetText(ctx context.Context, selector string) (string, error) {
	f.textCalls++
	f.textSel = selector
	return f.textResult, f.textErr
}
func (f *fakeBrowserOps) GetHTML(ctx context.Context, selector string) (string, error) {
	f.htmlCalls++
	f.htmlSel = selector
	return f.htmlResult, f.htmlErr
}

var _ browserOps = (*fakeBrowserOps)(nil)

// ─── Navigate ─────────────────────────────────────────────────────────

func TestDispatchNavigateSuccess(t *testing.T) {
	f := &fakeBrowserOps{navTitle: "Test Page", navFinalURL: "http://final"}
	result := dispatchToolCall(f, context.Background(), "browser_navigate", `{"url":"http://localhost:3000"}`)
	if !strings.Contains(result, "navigated to: http://localhost:3000") {
		t.Fatalf("unexpected result: %s", result)
	}
	if !strings.Contains(result, "title: Test Page") {
		t.Fatalf("missing title: %s", result)
	}
	if !strings.Contains(result, "final URL: http://final") {
		t.Fatalf("missing final URL: %s", result)
	}
	if f.navURL != "http://localhost:3000" {
		t.Fatalf("ops called with url=%q", f.navURL)
	}
}

func TestDispatchNavigateEmptyURL(t *testing.T) {
	f := &fakeBrowserOps{}
	result := dispatchToolCall(f, context.Background(), "browser_navigate", `{"url":""}`)
	if !strings.Contains(result, "ERROR: url is required") {
		t.Fatalf("expected url required error, got: %s", result)
	}
	if f.navCalls != 0 {
		t.Fatal("ops should not be called on validation error")
	}
}

func TestDispatchNavigateBadJSON(t *testing.T) {
	f := &fakeBrowserOps{}
	result := dispatchToolCall(f, context.Background(), "browser_navigate", `{invalid}`)
	if !strings.Contains(result, "ERROR: could not parse arguments") {
		t.Fatalf("expected parse error, got: %s", result)
	}
}

func TestDispatchNavigateError(t *testing.T) {
	f := &fakeBrowserOps{navErr: errors.New("connection refused")}
	result := dispatchToolCall(f, context.Background(), "browser_navigate", `{"url":"http://fail"}`)
	if !strings.Contains(result, "[browser: navigate failed: connection refused]") {
		t.Fatalf("expected error message, got: %s", result)
	}
}

// ─── Screenshot ───────────────────────────────────────────────────────

func TestDispatchScreenshotViewport(t *testing.T) {
	f := &fakeBrowserOps{screenshotPath: "/tmp/shot.png"}
	result := dispatchToolCall(f, context.Background(), "browser_screenshot", `{}`)
	if !strings.Contains(result, "screenshot saved to: /tmp/shot.png (viewport)") {
		t.Fatalf("unexpected result: %s", result)
	}
	if f.screenshotFP {
		t.Fatal("expected fullPage=false")
	}
}

func TestDispatchScreenshotFullPage(t *testing.T) {
	f := &fakeBrowserOps{screenshotPath: "/tmp/shot.png"}
	result := dispatchToolCall(f, context.Background(), "browser_screenshot", `{"full_page":true}`)
	if !strings.Contains(result, "(full page)") {
		t.Fatalf("expected full page label, got: %s", result)
	}
	if !f.screenshotFP {
		t.Fatal("expected fullPage=true")
	}
}

func TestDispatchScreenshotError(t *testing.T) {
	f := &fakeBrowserOps{screenshotErr: errors.New("capture failed")}
	result := dispatchToolCall(f, context.Background(), "browser_screenshot", `{}`)
	if !strings.Contains(result, "[browser: screenshot failed: capture failed]") {
		t.Fatalf("expected error, got: %s", result)
	}
}

// ─── Viewport ─────────────────────────────────────────────────────────

func TestDispatchViewportSuccess(t *testing.T) {
	f := &fakeBrowserOps{}
	result := dispatchToolCall(f, context.Background(), "browser_viewport", `{"width":375,"height":812}`)
	if result != "viewport set to 375x812" {
		t.Fatalf("unexpected result: %s", result)
	}
	if f.viewportW != 375 || f.viewportH != 812 {
		t.Fatalf("ops called with %dx%d", f.viewportW, f.viewportH)
	}
}

func TestDispatchViewportNonPositive(t *testing.T) {
	f := &fakeBrowserOps{}
	result := dispatchToolCall(f, context.Background(), "browser_viewport", `{"width":0,"height":812}`)
	if !strings.Contains(result, "ERROR: width and height must be positive") {
		t.Fatalf("expected validation error, got: %s", result)
	}
	if f.viewportCalls != 0 {
		t.Fatal("ops should not be called on validation error")
	}
}

func TestDispatchViewportError(t *testing.T) {
	f := &fakeBrowserOps{viewportErr: errors.New("unsupported size")}
	result := dispatchToolCall(f, context.Background(), "browser_viewport", `{"width":100,"height":100}`)
	if !strings.Contains(result, "[browser: set viewport failed: unsupported size]") {
		t.Fatalf("expected error, got: %s", result)
	}
}

// ─── Click ───────────────────────────────────────────────────────────

func TestDispatchClickSuccess(t *testing.T) {
	f := &fakeBrowserOps{}
	result := dispatchToolCall(f, context.Background(), "browser_click", `{"selector":"button.submit"}`)
	if result != "clicked: button.submit" {
		t.Fatalf("unexpected result: %s", result)
	}
	if f.clickSel != "button.submit" {
		t.Fatalf("ops called with %q", f.clickSel)
	}
}

func TestDispatchClickEmptySelector(t *testing.T) {
	f := &fakeBrowserOps{}
	result := dispatchToolCall(f, context.Background(), "browser_click", `{"selector":""}`)
	if !strings.Contains(result, "ERROR: selector is required") {
		t.Fatalf("expected validation error, got: %s", result)
	}
	if f.clickCalls != 0 {
		t.Fatal("ops should not be called on validation error")
	}
}

func TestDispatchClickError(t *testing.T) {
	f := &fakeBrowserOps{clickErr: errors.New("element not found")}
	result := dispatchToolCall(f, context.Background(), "browser_click", `{"selector":"#missing"}`)
	if !strings.Contains(result, `[browser: click "#missing" failed: element not found]`) {
		t.Fatalf("expected error, got: %s", result)
	}
}

// ─── Eval ────────────────────────────────────────────────────────────

func TestDispatchEvalSuccess(t *testing.T) {
	f := &fakeBrowserOps{evalResult: "  100px  "}
	result := dispatchToolCall(f, context.Background(), "browser_eval", `{"expression":"getComputedStyle().height"}`)
	if result != "100px" {
		t.Fatalf("expected trimmed result, got: %q", result)
	}
}

func TestDispatchEvalEmptyResult(t *testing.T) {
	f := &fakeBrowserOps{evalResult: ""}
	result := dispatchToolCall(f, context.Background(), "browser_eval", `{"expression":"undefined"}`)
	if result != "(eval returned empty/undefined)" {
		t.Fatalf("expected empty message, got: %s", result)
	}
}

func TestDispatchEvalEmptyExpression(t *testing.T) {
	f := &fakeBrowserOps{}
	result := dispatchToolCall(f, context.Background(), "browser_eval", `{"expression":""}`)
	if !strings.Contains(result, "ERROR: expression is required") {
		t.Fatalf("expected validation error, got: %s", result)
	}
	if f.evalCalls != 0 {
		t.Fatal("ops should not be called on validation error")
	}
}

func TestDispatchEvalError(t *testing.T) {
	f := &fakeBrowserOps{evalErr: errors.New("syntax error")}
	result := dispatchToolCall(f, context.Background(), "browser_eval", `{"expression":"bad js"}`)
	if !strings.Contains(result, "[browser: eval failed: syntax error]") {
		t.Fatalf("expected error, got: %s", result)
	}
}

// ─── Text ────────────────────────────────────────────────────────────

func TestDispatchTextSuccess(t *testing.T) {
	f := &fakeBrowserOps{textResult: "Hello World"}
	result := dispatchToolCall(f, context.Background(), "browser_text", `{"selector":"h1"}`)
	if result != "Hello World" {
		t.Fatalf("unexpected result: %s", result)
	}
	if f.textSel != "h1" {
		t.Fatalf("ops called with %q", f.textSel)
	}
}

func TestDispatchTextEmptySelector(t *testing.T) {
	f := &fakeBrowserOps{}
	result := dispatchToolCall(f, context.Background(), "browser_text", `{"selector":""}`)
	if !strings.Contains(result, "ERROR: selector is required") {
		t.Fatalf("expected validation error, got: %s", result)
	}
	if f.textCalls != 0 {
		t.Fatal("ops should not be called on validation error")
	}
}

func TestDispatchTextError(t *testing.T) {
	f := &fakeBrowserOps{textErr: errors.New("no match")}
	result := dispatchToolCall(f, context.Background(), "browser_text", `{"selector":".missing"}`)
	if !strings.Contains(result, `[browser: get text ".missing" failed: no match]`) {
		t.Fatalf("expected error, got: %s", result)
	}
}

// ─── HTML ────────────────────────────────────────────────────────────

func TestDispatchHTMLSuccess(t *testing.T) {
	f := &fakeBrowserOps{htmlResult: "<div>content</div>"}
	result := dispatchToolCall(f, context.Background(), "browser_html", `{"selector":".main"}`)
	if result != "<div>content</div>" {
		t.Fatalf("unexpected result: %s", result)
	}
	if f.htmlSel != ".main" {
		t.Fatalf("ops called with %q", f.htmlSel)
	}
}

func TestDispatchHTMLEmptySelector(t *testing.T) {
	f := &fakeBrowserOps{htmlResult: "<html><body>full</body></html>"}
	result := dispatchToolCall(f, context.Background(), "browser_html", `{}`)
	if result != "<html><body>full</body></html>" {
		t.Fatalf("unexpected result: %s", result)
	}
	if f.htmlSel != "" {
		t.Fatalf("expected empty selector, got %q", f.htmlSel)
	}
}

func TestDispatchHTMLError(t *testing.T) {
	f := &fakeBrowserOps{htmlErr: errors.New("timeout")}
	result := dispatchToolCall(f, context.Background(), "browser_html", `{"selector":"#main"}`)
	if !strings.Contains(result, "[browser: get HTML failed: timeout]") {
		t.Fatalf("expected error, got: %s", result)
	}
}

// ─── ReducedMotion ───────────────────────────────────────────────────

func TestDispatchReducedMotionEnable(t *testing.T) {
	f := &fakeBrowserOps{}
	result := dispatchToolCall(f, context.Background(), "browser_reduced_motion", `{"emulate":true}`)
	if result != "prefers-reduced-motion emulation set to: reduce" {
		t.Fatalf("unexpected result: %s", result)
	}
	if !f.reducedM {
		t.Fatal("expected EmulateReducedMotion called with true")
	}
}

func TestDispatchReducedMotionDisable(t *testing.T) {
	f := &fakeBrowserOps{}
	result := dispatchToolCall(f, context.Background(), "browser_reduced_motion", `{"emulate":false}`)
	if result != "prefers-reduced-motion emulation set to: no-preference" {
		t.Fatalf("unexpected result: %s", result)
	}
	if f.reducedM {
		t.Fatal("expected EmulateReducedMotion called with false")
	}
}

func TestDispatchReducedMotionMissingEmulate(t *testing.T) {
	// Missing "emulate" key defaults to false (Go bool zero value).
	f := &fakeBrowserOps{}
	result := dispatchToolCall(f, context.Background(), "browser_reduced_motion", `{}`)
	if result != "prefers-reduced-motion emulation set to: no-preference" {
		t.Fatalf("expected no-preference for missing emulate, got: %s", result)
	}
}

func TestDispatchReducedMotionError(t *testing.T) {
	f := &fakeBrowserOps{reducedErr: errors.New("not supported")}
	result := dispatchToolCall(f, context.Background(), "browser_reduced_motion", `{"emulate":true}`)
	if !strings.Contains(result, "[browser: emulate reduced motion failed: not supported]") {
		t.Fatalf("expected error, got: %s", result)
	}
}

// ─── Unknown tool ────────────────────────────────────────────────────

func TestDispatchUnknownTool(t *testing.T) {
	f := &fakeBrowserOps{}
	result := dispatchToolCall(f, context.Background(), "browser_bogus", `{}`)
	if !strings.Contains(result, `[browser: unknown tool "browser_bogus"]`) {
		t.Fatalf("expected unknown tool message, got: %s", result)
	}
	// No ops should be called for an unknown tool.
	if f.navCalls+f.screenshotCalls+f.viewportCalls+f.reducedCalls+
		f.clickCalls+f.evalCalls+f.textCalls+f.htmlCalls != 0 {
		t.Fatal("ops should not be called for unknown tool")
	}
}

// ─── Tool-list drift test ────────────────────────────────────────────

// TestToolListConsistency verifies that BrowserTools(), IsBrowserTool(), and
// dispatchToolCall all reference the same set of tool names — three
// hand-maintained lists that can drift.
func TestToolListConsistency(t *testing.T) {
	tools := BrowserTools()
	toolNames := make(map[string]bool)
	for _, tool := range tools {
		toolNames[tool.Function.Name] = true
		if !IsBrowserTool(tool.Function.Name) {
			t.Errorf("tool %q in BrowserTools() but not in IsBrowserTool()", tool.Function.Name)
		}
	}
	// Verify IsBrowserTool doesn't accept names not in BrowserTools.
	knownFalse := []string{"browser_bogus", "run_shell", "lsp_definition", ""}
	for _, name := range knownFalse {
		if IsBrowserTool(name) {
			t.Errorf("IsBrowserTool(%q) = true, expected false", name)
		}
	}
	// Verify dispatchToolCall doesn't return unknown for any registered tool.
	f := &fakeBrowserOps{screenshotPath: "/tmp/s.png", htmlResult: "<html></html>"}
	for name := range toolNames {
		result := dispatchToolCall(f, context.Background(), name, `{}`)
		if strings.Contains(result, "[browser: unknown tool") {
			t.Errorf("tool %q dispatched as unknown: %s", name, result)
		}
	}
}

// ─── Bad JSON shared across tools ────────────────────────────────────

func TestDispatchBadJSON(t *testing.T) {
	f := &fakeBrowserOps{}
	// Tools with required args should return parse errors.
	for _, tool := range []string{"browser_navigate", "browser_viewport", "browser_click", "browser_eval", "browser_text", "browser_reduced_motion"} {
		result := dispatchToolCall(f, context.Background(), tool, `{invalid json}`)
		if !strings.Contains(result, "ERROR: could not parse arguments") {
			t.Errorf("%s: expected parse error, got: %s", tool, result)
		}
	}
	// Tools with optional args should ignore parse errors and proceed.
	for _, tool := range []string{"browser_screenshot", "browser_html"} {
		result := dispatchToolCall(f, context.Background(), tool, `{invalid json}`)
		if strings.Contains(result, "ERROR: could not parse") {
			t.Errorf("%s: should not error on bad JSON (optional args), got: %s", tool, result)
		}
	}
}
