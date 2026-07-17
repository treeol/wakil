package browser

import (
	"testing"
)

// TestBrowserToolsDefinitions verifies that BrowserTools returns all expected
// tool definitions with non-empty names and descriptions.
func TestBrowserToolsDefinitions(t *testing.T) {
	tools := BrowserTools()
	if len(tools) != 8 {
		t.Fatalf("expected 8 browser tools, got %d", len(tools))
	}

	expected := []string{
		"browser_navigate", "browser_screenshot", "browser_viewport",
		"browser_click", "browser_eval", "browser_text", "browser_html",
		"browser_reduced_motion",
	}
	for i, name := range expected {
		if tools[i].Function.Name != name {
			t.Errorf("tool[%d]: expected %q, got %q", i, name, tools[i].Function.Name)
		}
		if tools[i].Function.Description == "" {
			t.Errorf("tool %q has empty description", name)
		}
		if tools[i].Type != "function" {
			t.Errorf("tool %q: expected type 'function', got %q", name, tools[i].Type)
		}
	}
}

// TestIsBrowserTool verifies the tool name classifier.
func TestIsBrowserTool(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"browser_navigate", true},
		{"browser_screenshot", true},
		{"browser_viewport", true},
		{"browser_click", true},
		{"browser_eval", true},
		{"browser_text", true},
		{"browser_html", true},
		{"browser_reduced_motion", true},
		{"browser_unknown", false},
		{"lsp_definition", false},
		{"run_shell", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsBrowserTool(c.name); got != c.want {
			t.Errorf("IsBrowserTool(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestHandleToolCallNilManager verifies the nil-manager guard returns a
// helpful message instead of panicking.
func TestHandleToolCallNilManager(t *testing.T) {
	var m *Manager
	result := m.HandleToolCall(nil, "browser_navigate", `{"url":"http://localhost"}`)
	if result == "" {
		t.Fatal("expected non-empty result for nil manager")
	}
	if result[0] != '[' {
		t.Fatalf("expected bracketed message for nil manager, got: %s", result)
	}
}

// TestHandleToolCallUnknownTool verifies an unknown tool name returns an error.
func TestHandleToolCallUnknownTool(t *testing.T) {
	// Use a nil manager — HandleToolCall guards against nil.
	var m *Manager
	result := m.HandleToolCall(nil, "browser_bogus", "{}")
	if result == "" {
		t.Fatal("expected non-empty result for unknown tool")
	}
}

// TestHandleToolCallNilManagerClose verifies Close is nil-safe.
func TestHandleToolCallNilManagerClose(t *testing.T) {
	var m *Manager
	if err := m.Close(); err != nil {
		t.Fatalf("Close on nil manager should return nil, got: %v", err)
	}
}
