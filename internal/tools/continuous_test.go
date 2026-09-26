package tools

import (
	"testing"

	"github.com/treeol/wakil/internal/proxy"
)

func TestContinuousToolsAreExplicitAndExcludedFromDefault(t *testing.T) {
	if hasContinuousTool(DefaultTools("/work"), "finalize_goal") {
		t.Fatal("finalize_goal must not be part of ordinary default tools")
	}
	if hasContinuousTool(DiscoveryTools("/work"), "finalize_goal") {
		t.Fatal("finalize_goal must not be available to discovery subagents")
	}
	if !hasContinuousTool(ContinuousTools(), "finalize_goal") {
		t.Fatal("continuous tools must include finalize_goal")
	}
}

func hasContinuousTool(list []proxy.Tool, name string) bool {
	for _, tool := range list {
		if tool.Function.Name == name {
			return true
		}
	}
	return false
}
