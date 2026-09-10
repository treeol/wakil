package agent

import (
	"testing"

	"github.com/treeol/wakil/internal/proxy"
)

// TestSortToolsByName verifies tools are sorted by name.
func TestSortToolsByName(t *testing.T) {
	tools := []proxy.Tool{
		{Type: "function", Function: proxy.ToolFunction{Name: "zebra"}},
		{Type: "function", Function: proxy.ToolFunction{Name: "alpha"}},
		{Type: "function", Function: proxy.ToolFunction{Name: "mango"}},
	}
	sorted := SortToolsByName(tools)
	if sorted[0].Function.Name != "alpha" {
		t.Errorf("expected alpha first, got %s", sorted[0].Function.Name)
	}
	if sorted[1].Function.Name != "mango" {
		t.Errorf("expected mango second, got %s", sorted[1].Function.Name)
	}
	if sorted[2].Function.Name != "zebra" {
		t.Errorf("expected zebra third, got %s", sorted[2].Function.Name)
	}
}

// TestSortToolsByNameEmpty verifies empty input returns empty.
func TestSortToolsByNameEmpty(t *testing.T) {
	sorted := SortToolsByName(nil)
	if len(sorted) != 0 {
		t.Errorf("expected empty slice, got %d items", len(sorted))
	}
}

// TestSortToolsByNameSingle verifies a single tool is unchanged.
func TestSortToolsByNameSingle(t *testing.T) {
	tools := []proxy.Tool{
		{Type: "function", Function: proxy.ToolFunction{Name: "only"}},
	}
	sorted := SortToolsByName(tools)
	if len(sorted) != 1 || sorted[0].Function.Name != "only" {
		t.Errorf("expected single tool unchanged")
	}
}

// TestSortToolsByNameStable verifies the sort is stable (equal names preserve relative order).
func TestSortToolsByNameStable(t *testing.T) {
	tools := []proxy.Tool{
		{Type: "function", Function: proxy.ToolFunction{Name: "b", Description: "first"}},
		{Type: "function", Function: proxy.ToolFunction{Name: "a", Description: "second"}},
		{Type: "function", Function: proxy.ToolFunction{Name: "b", Description: "third"}},
	}
	sorted := SortToolsByName(tools)
	if sorted[0].Function.Name != "a" {
		t.Errorf("expected a first")
	}
	if sorted[0].Function.Description != "second" {
		t.Errorf("expected a to be 'second'")
	}
	// The two b's should preserve their relative order (stable sort).
	if sorted[1].Function.Description != "first" {
		t.Errorf("expected first b to be 'first'")
	}
	if sorted[2].Function.Description != "third" {
		t.Errorf("expected second b to be 'third'")
	}
}

// TestSortToolsByNameDoesNotMutateOriginal verifies the original slice is not modified.
func TestSortToolsByNameDoesNotMutateOriginal(t *testing.T) {
	tools := []proxy.Tool{
		{Type: "function", Function: proxy.ToolFunction{Name: "zebra"}},
		{Type: "function", Function: proxy.ToolFunction{Name: "alpha"}},
	}
	_ = SortToolsByName(tools)
	if tools[0].Function.Name != "zebra" {
		t.Errorf("original slice was mutated: expected zebra first, got %s", tools[0].Function.Name)
	}
}
