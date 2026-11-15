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
