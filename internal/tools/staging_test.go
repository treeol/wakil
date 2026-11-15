package tools

import (
	"testing"
)

func TestStagingTools_Detailed(t *testing.T) {
	tools := StagingTools()
	if len(tools) != 5 {
		t.Fatalf("expected 5 staging tools, got %d", len(tools))
	}
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.Function.Name] = true
	}
	expected := []string{"staging_put", "staging_get", "staging_delete", "staging_list", "staging_get_many"}
	for _, n := range expected {
		if !names[n] {
			t.Errorf("missing tool: %s", n)
		}
	}
}
