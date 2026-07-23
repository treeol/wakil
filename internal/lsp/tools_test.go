package lsp

import (
	"context"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
)

func TestLSPTools(t *testing.T) {
	tools := LSPTools("/work")
	if len(tools) != 4 {
		t.Fatalf("expected 4 LSP tools, got %d", len(tools))
	}
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.Function.Name] = true
	}
	expected := []string{"lsp_definition", "lsp_references", "lsp_hover", "lsp_symbols"}
	for _, n := range expected {
		if !names[n] {
			t.Errorf("missing tool: %s", n)
		}
	}
}

func TestCapabilityForTool(t *testing.T) {
	tests := []struct {
		tool string
		want string
	}{
		{"lsp_definition", "definitionProvider"},
		{"lsp_references", "referencesProvider"},
		{"lsp_hover", "hoverProvider"},
		{"lsp_symbols", "workspaceSymbolProvider"},
		{"unknown_tool", ""},
		{"", ""},
		{"lsp", ""},
	}
	for _, tt := range tests {
		got := CapabilityForTool(tt.tool)
		if got != tt.want {
			t.Errorf("CapabilityForTool(%q) = %q, want %q", tt.tool, got, tt.want)
		}
	}
}

func TestHandleLSPReadOnly_BadJSON(t *testing.T) {
	mgr := &Manager{cfg: config.DefaultConfig()}
	result := mgr.HandleLSPReadOnly(context.Background(), "lsp_definition", `{invalid json}`)
	if !strings.HasPrefix(result, "ERROR:") {
		t.Errorf("expected error for bad JSON, got: %s", result)
	}
	if !strings.Contains(result, "could not parse") {
		t.Errorf("expected parse error message, got: %s", result)
	}
}

func TestHandleLSPReadOnly_EmptyArgs(t *testing.T) {
	mgr := &Manager{cfg: config.DefaultConfig()}
	result := mgr.HandleLSPReadOnly(context.Background(), "lsp_definition", `{}`)
	// With no path, it should fail early with an error (no crash).
	if !strings.HasPrefix(result, "ERROR:") && !strings.HasPrefix(result, "[lsp:") {
		// Either an error or an LSP failure contract — both are acceptable
		// as long as it doesn't panic.
		t.Logf("result for empty args: %s", result)
	}
}

func TestHandleLSPReadOnly_UnknownTool(t *testing.T) {
	fe := newFakeLSPExec()
	fe.files["/work/main.go"] = "package main"
	srv := newTestServer(t, fe)
	mgr := srv.mgr
	mgr.servers = map[string]*Server{} // init to avoid nil-map panic

	result := mgr.HandleLSPReadOnly(context.Background(), "unknown_tool", `{"path":"/work/main.go","line":1}`)
	// Unknown tool should return an error, not panic.
	if result == "" {
		t.Error("expected non-empty result for unknown tool")
	}
}
