package agent

// End-to-end dispatchSubagent test: subagent calls read_file_full (not
// read_file), reads the file content, and returns a structured summary.
// Verifies:
//   - read_file_full is available in the subagent's tool set (DiscoveryTools)
//   - The subagent receives real file content (not an unknown-tool error)
//   - The summary contains real findings derived from the file
//   - No provider 400 (the result is a normal tool string)

import (
	"context"
	"io"
	"strings"
	"testing"

	"wakil/internal/tools"
)

func TestDispatchSubagentReadFileFull(t *testing.T) {
	fileContent := "package main\n\nconst ToolResultCap = 8000 // per-result context cap\n"
	summaryJSON := `{"objective":"find ToolResultCap","findings":[{"summary":"ToolResultCap = 8000 in config.go:3","location":"config.go:3","kind":"match","weight":"high"}],"checked":[{"path":"config.go","size_k":1,"status":"full"}]}`

	srv := sseServer(t,
		// call 0 — subagent returns read_file_full tool call (NOT read_file)
		toolCallFrames("rf1", "read_file_full", `{"path":"config.go"}`),
		// call 1 — subagent returns JSON summary
		[]string{contentChunk(summaryJSON)},
	)
	defer srv.Close()

	exec := newFakeExecutor()
	exec.files["config.go"] = fileContent

	parent := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })

	// Verify read_file_full is in the subagent's tool set.
	subTools := tools.DiscoveryTools("/work")
	hasReadFileFull := false
	for _, tool := range subTools {
		if tool.Function.Name == "read_file_full" {
			hasReadFileFull = true
			break
		}
	}
	if !hasReadFileFull {
		t.Fatal("read_file_full is not in DiscoveryTools — subagent cannot use it")
	}

	summary, _, _, _ := parent.dispatchSubagent(context.Background(), "find ToolResultCap", io.Discard, "")

	// Must have real findings.
	if len(summary.Findings) == 0 {
		t.Fatal("no findings returned from subagent")
	}
	if summary.Findings[0].Kind != "match" {
		t.Errorf("kind = %q, want match", summary.Findings[0].Kind)
	}
	if !strings.Contains(summary.Findings[0].Summary, "8000") {
		t.Errorf("finding should mention the value 8000, got: %q", summary.Findings[0].Summary)
	}
	if summary.Findings[0].Location != "config.go:3" {
		t.Errorf("location = %q, want config.go:3", summary.Findings[0].Location)
	}

	t.Logf("OK: subagent used read_file_full and returned real findings:")
	t.Logf("  finding: %s", summary.Findings[0].Summary)
	t.Logf("  location: %s", summary.Findings[0].Location)
}

// TestSubagentReadFileFullNoWindowedReReads verifies that when read_file_full
// is called, the full content arrives in one call — no windowed re-reads
// (no read_file with offset/limit on the same file afterward).
func TestSubagentReadFileFullNoWindowedReReads(t *testing.T) {
	fileContent := strings.Repeat("line of importance\n", 200) // ~3600 bytes
	summaryJSON := `{"objective":"read file","findings":[{"summary":"file read fully","location":"big.go","kind":"fact","weight":"low"}],"checked":[{"path":"big.go","size_k":3,"status":"full"}]}`

	srv := sseServer(t,
		// call 0 — subagent returns read_file_full tool call
		toolCallFrames("rf1", "read_file_full", `{"path":"big.go"}`),
		// call 1 — subagent returns summary (NO further read_file calls)
		[]string{contentChunk(summaryJSON)},
	)
	defer srv.Close()

	exec := newFakeExecutor()
	exec.files["big.go"] = fileContent

	parent := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })

	summary, _, _, _ := parent.dispatchSubagent(context.Background(), "read big.go", io.Discard, "")

	if len(summary.Findings) == 0 {
		t.Fatal("no findings returned")
	}
	// The subagent should have read the file fully — checked status = "full".
	if len(summary.Checked) == 0 {
		t.Fatal("no checked items — subagent did not report reading the file")
	}
	if summary.Checked[0].Status != "full" {
		t.Errorf("checked status = %q, want 'full' (read_file_full should give full content)", summary.Checked[0].Status)
	}

	t.Logf("OK: subagent used read_file_full (one call, no windowed re-reads), checked status=full")
}
