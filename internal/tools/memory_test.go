package tools

import (
	"testing"
)

func TestMemoryTools_Detailed(t *testing.T) {
	tools := MemoryTools()
	if len(tools) != 8 {
		t.Fatalf("expected 8 memory tools, got %d", len(tools))
	}
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.Function.Name] = true
	}
	expected := []string{
		"memory_put", "memory_promote", "memory_reject", "memory_get",
		"memory_search", "memory_list", "memory_forget", "memory_promote_from_staging",
	}
	for _, n := range expected {
		if !names[n] {
			t.Errorf("missing tool: %s", n)
		}
	}
}

func TestIsMemoryTool(t *testing.T) {
	for _, name := range []string{
		"memory_put", "memory_promote", "memory_reject", "memory_get",
		"memory_search", "memory_list", "memory_forget", "memory_promote_from_staging",
	} {
		if !IsMemoryTool(name) {
			t.Errorf("IsMemoryTool(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", "memory", "staging_put", "google_search", "read_file"} {
		if IsMemoryTool(name) {
			t.Errorf("IsMemoryTool(%q) = true, want false", name)
		}
	}
}

func TestIsMemoryMainOnlyTool(t *testing.T) {
	// Main-only tools.
	for _, name := range []string{"memory_promote", "memory_reject", "memory_forget", "memory_promote_from_staging"} {
		if !IsMemoryMainOnlyTool(name) {
			t.Errorf("IsMemoryMainOnlyTool(%q) = false, want true", name)
		}
	}
	// Non-main-only tools.
	for _, name := range []string{"memory_put", "memory_get", "memory_search", "memory_list"} {
		if IsMemoryMainOnlyTool(name) {
			t.Errorf("IsMemoryMainOnlyTool(%q) = true, want false", name)
		}
	}
	// Non-memory tools.
	for _, name := range []string{"", "staging_put", "google_search"} {
		if IsMemoryMainOnlyTool(name) {
			t.Errorf("IsMemoryMainOnlyTool(%q) = true, want false", name)
		}
	}
}
