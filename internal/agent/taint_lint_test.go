package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoDirectAddGroundingInProductionCode is a lint-style test that enforces
// the convention that all AddGrounding calls in internal/agent/ production
// code go through App.addExternalGrounding() (the wrapper that eagerly sets
// the sticky taint flag). Calling a.Client.AddGrounding directly bypasses
// the latch and causes taint to undercount — a trust-model violation (A1).
//
// This test greps all .go files in internal/agent/ (excluding test files and
// the memory_tools.go wrapper itself) for direct AddGrounding calls and fails
// if it finds any.
func TestNoDirectAddGroundingInProductionCode(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// Walk all .go files in internal/agent/.
	var violations []string
	err = filepath.Walk(wd, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// Skip test files.
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// Skip the wrapper itself — it's the one place that calls
		// Client.AddGrounding legitimately.
		if strings.HasSuffix(path, "memory_tools.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		// Check for direct AddGrounding calls (a.Client.AddGrounding,
		// app.Client.AddGrounding, etc.). The wrapper uses a.Client.AddGrounding
		// internally, which is why memory_tools.go is excluded.
		if strings.Contains(string(data), "AddGrounding(") {
			rel, _ := filepath.Rel(wd, path)
			violations = append(violations, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) > 0 {
		t.Errorf("direct AddGrounding calls found in production code (must use App.addExternalGrounding instead):\n  %s\n"+
			"See the comment on Client.AddGrounding in internal/proxy/client.go for why.",
			strings.Join(violations, "\n  "))
	}
}
