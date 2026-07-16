package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/proxy"
)

// TestDefaultTools verifies that DefaultTools returns the expected set of tools
// with valid schemas.
func TestDefaultTools(t *testing.T) {
	tools := DefaultTools("/work")
	if len(tools) == 0 {
		t.Fatal("DefaultTools returned empty slice")
	}

	expected := []string{
		"dispatch_subagent", "dispatch_subagents",
		"run_shell", "read_file", "read_file_full",
		"search_files", "find_files", "list_dir",
		"edit_file", "open_url", "write_file",
		"delete_file", "move_file",
		"run_background", "kill_process", "read_process_log",
	}
	// Plus staging and memory tools.
	names := toolNames(tools)
	for _, name := range expected {
		if _, ok := names[name]; !ok {
			t.Errorf("DefaultTools missing %q", name)
		}
	}
}

// TestDefaultTools_SchemasValid verifies every tool has parseable JSON schema.
func TestDefaultTools_SchemasValid(t *testing.T) {
	tools := DefaultTools("/work")
	for _, tool := range tools {
		if tool.Type != "function" {
			t.Errorf("tool %q type = %q, want %q", tool.Function.Name, tool.Type, "function")
		}
		if tool.Function.Name == "" {
			t.Error("tool with empty name")
		}
		if tool.Function.Description == "" {
			t.Errorf("tool %q has empty description", tool.Function.Name)
		}
		if len(tool.Function.Parameters) == 0 {
			t.Errorf("tool %q has empty parameters", tool.Function.Name)
		}
		var schema map[string]interface{}
		if err := json.Unmarshal(tool.Function.Parameters, &schema); err != nil {
			t.Errorf("tool %q parameters not valid JSON: %v", tool.Function.Name, err)
		}
		if schema["type"] != "object" {
			t.Errorf("tool %q schema type = %v, want %q", tool.Function.Name, schema["type"], "object")
		}
	}
}

// TestDefaultTools_CwdInDescription verifies the working directory note appears
// in file-related tool descriptions.
func TestDefaultTools_CwdInDescription(t *testing.T) {
	cwd := "/custom/path"
	tools := DefaultTools(cwd)
	for _, tool := range tools {
		// File-related tools should include the cwd note.
		if tool.Function.Name == "read_file" || tool.Function.Name == "write_file" {
			if !strings.Contains(tool.Function.Description, cwd) {
				t.Errorf("tool %q description missing cwd %q", tool.Function.Name, cwd)
			}
		}
	}
}

// TestGatedTool verifies which tools require human confirmation.
func TestGatedTool(t *testing.T) {
	gated := []string{"run_shell", "write_file", "edit_file", "delete_file", "move_file", "run_background", "kill_process"}
	for _, name := range gated {
		if !GatedTool(name) {
			t.Errorf("GatedTool(%q) = false, want true", name)
		}
	}
	ungated := []string{"read_file", "read_file_full", "list_dir", "search_files", "find_files", "read_process_log", "open_url"}
	for _, name := range ungated {
		if GatedTool(name) {
			t.Errorf("GatedTool(%q) = true, want false", name)
		}
	}
}

// TestValidCapability verifies capability validation.
func TestValidCapability(t *testing.T) {
	valid := []string{CapabilityDiscovery, CapabilityEdit, CapabilityTools}
	for _, cap := range valid {
		if !ValidCapability(cap) {
			t.Errorf("ValidCapability(%q) = false, want true", cap)
		}
	}
	invalid := []string{"", "admin", "root", "execute", "EDIT"}
	for _, cap := range invalid {
		if ValidCapability(cap) {
			t.Errorf("ValidCapability(%q) = true, want false", cap)
		}
	}
}

// TestIsEditTool verifies edit-tool classification.
func TestIsEditTool(t *testing.T) {
	editTools := []string{"write_file", "edit_file", "delete_file", "move_file"}
	for _, name := range editTools {
		if !IsEditTool(name) {
			t.Errorf("IsEditTool(%q) = false, want true", name)
		}
	}
	nonEdit := []string{"read_file", "run_shell", "search_files", "list_dir"}
	for _, name := range nonEdit {
		if IsEditTool(name) {
			t.Errorf("IsEditTool(%q) = true, want false", name)
		}
	}
}

// TestIsMashuraTool verifies mashura/oracle tool classification.
func TestIsMashuraTool(t *testing.T) {
	mashuraTools := []string{"mashura__review", "mashura__debug", "mashura__decide", "mashura__check", "oracle__ask"}
	for _, name := range mashuraTools {
		if !IsMashuraTool(name) {
			t.Errorf("IsMashuraTool(%q) = false, want true", name)
		}
	}
	nonMashura := []string{"run_shell", "read_file", "dispatch_subagent", "mashura_other"}
	for _, name := range nonMashura {
		if IsMashuraTool(name) {
			t.Errorf("IsMashuraTool(%q) = true, want false", name)
		}
	}
}

// TestIsSubagentResult verifies subagent tool classification.
func TestIsSubagentResult(t *testing.T) {
	if !IsSubagentResult("dispatch_subagent") {
		t.Error("IsSubagentResult(dispatch_subagent) should be true")
	}
	if !IsSubagentResult("dispatch_subagents") {
		t.Error("IsSubagentResult(dispatch_subagents) should be true")
	}
	if IsSubagentResult("run_shell") {
		t.Error("IsSubagentResult(run_shell) should be false")
	}
}

// TestDiscoveryTools verifies the discovery (read-only) tool set.
func TestDiscoveryTools(t *testing.T) {
	tools := DiscoveryTools("/work")
	names := toolNames(tools)

	// Must have read-only tools.
	required := []string{"read_file", "read_file_full", "search_files", "find_files", "list_dir"}
	for _, name := range required {
		if _, ok := names[name]; !ok {
			t.Errorf("DiscoveryTools missing %q", name)
		}
	}
	// Must NOT have mutation tools.
	forbidden := []string{"run_shell", "write_file", "edit_file", "delete_file", "move_file", "run_background", "kill_process"}
	for _, name := range forbidden {
		if _, ok := names[name]; ok {
			t.Errorf("DiscoveryTools must NOT include %q", name)
		}
	}
}

// TestEditTools verifies the edit-tier tool set.
func TestEditTools(t *testing.T) {
	tools := EditTools("/work")
	names := toolNames(tools)

	// Must have read-only tools.
	for _, name := range []string{"read_file", "search_files", "list_dir"} {
		if _, ok := names[name]; !ok {
			t.Errorf("EditTools missing %q", name)
		}
	}
	// Must have edit tools.
	for _, name := range []string{"write_file", "edit_file", "delete_file", "move_file"} {
		if _, ok := names[name]; !ok {
			t.Errorf("EditTools missing %q", name)
		}
	}
	// Must NOT have exec tools.
	for _, name := range []string{"run_shell", "run_background", "kill_process"} {
		if _, ok := names[name]; ok {
			t.Errorf("EditTools must NOT include %q", name)
		}
	}
}

// TestStagingTools verifies staging tools are present.
func TestStagingTools(t *testing.T) {
	tools := StagingTools()
	if len(tools) == 0 {
		t.Fatal("StagingTools returned empty slice")
	}
	names := toolNames(tools)
	expected := []string{"staging_put", "staging_get", "staging_delete", "staging_list", "staging_get_many"}
	for _, name := range expected {
		if _, ok := names[name]; !ok {
			t.Errorf("StagingTools missing %q", name)
		}
	}
}

// TestMemoryTools verifies memory tools are present.
func TestMemoryTools(t *testing.T) {
	tools := MemoryTools()
	if len(tools) == 0 {
		t.Fatal("MemoryTools returned empty slice")
	}
	names := toolNames(tools)
	// At minimum, memory_put and memory_get should exist.
	if _, ok := names["memory_put"]; !ok {
		t.Error("MemoryTools missing memory_put")
	}
	if _, ok := names["memory_get"]; !ok {
		t.Error("MemoryTools missing memory_get")
	}
}

// TestDefaultTools_NoNilParameters verifies no tool has nil parameters.
func TestDefaultTools_NoNilParameters(t *testing.T) {
	tools := DefaultTools("/work")
	for _, tool := range tools {
		if tool.Function.Parameters == nil {
			t.Errorf("tool %q has nil Parameters", tool.Function.Name)
		}
	}
}

// TestDefaultTools_RequiredFields verifies that tools with required fields
// have them properly set (e.g. run_shell requires "command").
func TestDefaultTools_RequiredFields(t *testing.T) {
	tools := DefaultTools("/work")
	for _, tool := range tools {
		var schema map[string]interface{}
		if err := json.Unmarshal(tool.Function.Parameters, &schema); err != nil {
			continue
		}
		if req, ok := schema["required"]; ok {
			if req == nil {
				t.Errorf("tool %q has required: null, should be omitted", tool.Function.Name)
			}
		}
	}
}

// toolNames extracts tool names from a slice into a set.
func toolNames(tools []proxy.Tool) map[string]bool {
	names := make(map[string]bool, len(tools))
	for _, tool := range tools {
		names[tool.Function.Name] = true
	}
	return names
}
