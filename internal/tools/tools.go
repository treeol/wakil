package tools

import (
	"encoding/json"
	"fmt"

	"github.com/treeol/wakil/internal/proxy"
)

// DefaultTools returns the built-in tool set with descriptions that include
// the current working directory so the model prefers relative paths.
func DefaultTools(cwd string) []proxy.Tool {
	cwdNote := fmt.Sprintf("Working directory: %s — prefer relative paths (e.g. 'report.txt') unless an absolute path is explicitly needed.", cwd)
	strProp := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "string", "description": desc}
	}
	intProp := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "integer", "description": desc}
	}
	boolProp := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "boolean", "description": desc}
	}
	obj := func(props map[string]interface{}, required ...string) json.RawMessage {
		m := map[string]interface{}{"type": "object", "properties": props}
		// Only emit "required" when non-empty: a nil variadic marshals to
		// "required": null, which the backend's template parser rejects.
		if len(required) > 0 {
			m["required"] = required
		}
		b, _ := json.Marshal(m)
		return b
	}
	return []proxy.Tool{
		{Type: "function", Function: proxy.ToolFunction{
			Name: "dispatch_subagent",
			Description: "Dispatch a subagent for a bounded, single-objective task. " +
				"capability \"discovery\" (default) is read-only: navigate and read code, return a structured " +
				"JSON summary (findings with file:line locations, checked/skipped files, uncertainty). " +
				"capability \"edit\" adds write_file, edit_file, delete_file, and move_file for delegated " +
				"bounded implementation; requires session write consent (/auto or --auto). Multiple " +
				"independent dispatch_subagent calls emitted in the same turn run in parallel (bounded); " +
				"for several related tasks prefer dispatch_subagents (plural) which runs them concurrently by design.",
			Parameters: obj(map[string]interface{}{
				"task":       strProp("Specific discovery objective, e.g. 'find where ToolResultCap is configured across the repo'."),
				"capability": strProp("Capability tier: \"discovery\" (default, read-only) or \"edit\" (adds file mutation tools; requires /auto or --auto)."),
			}, "task"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "dispatch_subagents",
			Description: "Dispatch several subagents CONCURRENTLY, one per task (bounded by config). " +
				"Each task is a bounded, single-objective job, independent of the others. Returns a JSON " +
				"array of structured summaries in task order. Use for 2+ independent objectives — faster " +
				"than sequential dispatch_subagent calls. All tasks share the same capability tier.",
			Parameters: obj(map[string]interface{}{
				"tasks": map[string]interface{}{
					"type":        "array",
					"items":       strProp("One discovery objective."),
					"description": "Independent objectives (1–8), each handled by its own subagent.",
				},
				"capability": strProp("Capability tier for all tasks: \"discovery\" (default, read-only) or \"edit\" (adds file mutation tools; requires /auto or --auto)."),
			}, "tasks"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name:        "run_shell",
			Description: "Run a shell command in the working directory and return combined stdout/stderr. Requires user confirmation. " + cwdNote,
			Parameters: obj(map[string]interface{}{
				"command": strProp("The shell command to run"),
			}, "command"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "read_file",
			Description: "Read a file and return its contents with line numbers. Reads the whole file by default; " +
				"pass offset/limit to read only a line range (cheaper for large files). " + cwdNote,
			Parameters: obj(map[string]interface{}{
				"path":   strProp("Path to the file to read (relative paths resolve from the working directory)"),
				"offset": intProp("Optional 1-based line number to start reading from."),
				"limit":  intProp("Optional maximum number of lines to read from offset."),
			}, "path"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "read_file_full",
			Description: "Read an entire file in one call and return its complete contents with line numbers. " +
				"Use read_file_full when you need the complete contents of a normal source file (up to ~256 KB); " +
				"use read_file for large files or targeted ranges (offset/limit). " +
				"Prefer read_file_full over repeated read_file calls with different offsets on the same file. " + cwdNote,
			Parameters: obj(map[string]interface{}{
				"path": strProp("Path to the file to read (relative paths resolve from the working directory)"),
			}, "path"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "search_files",
			Description: "Search file contents for a pattern and return matching lines with file:line context. " +
				"Equivalent to grep -rn. " + cwdNote,
			Parameters: obj(map[string]interface{}{
				"pattern":          strProp("Search pattern (literal string or basic regex)."),
				"path":             strProp("File or directory to search."),
				"file_pattern":     strProp("Optional glob to restrict which files are searched, e.g. '*.go'."),
				"case_insensitive": boolProp("Case-insensitive search (default false)."),
			}, "pattern", "path"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "find_files",
			Description: "Find files by name recursively under a path. Equivalent to find -type f -name. " +
				"Use to locate files when you don't know where they live. " + cwdNote,
			Parameters: obj(map[string]interface{}{
				"pattern": strProp("Filename glob, e.g. '*.go' or 'config.*'."),
				"path":    strProp("Directory to search under (defaults to the working directory)."),
			}, "pattern"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name:        "list_dir",
			Description: "List the entries of a directory (names, with a trailing / on subdirectories). Use this to discover what exists before reading files. " + cwdNote,
			Parameters: obj(map[string]interface{}{
				"path": strProp("Directory to list (defaults to the working directory)"),
			}),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "edit_file",
			Description: "Replace an exact substring in a file. old_string must match the file's raw text " +
				"verbatim (including whitespace, and WITHOUT the line-number gutter that read_file shows) and " +
				"must be unique unless replace_all is set. Prefer this over write_file for changes to existing " +
				"files. Requires user confirmation. " + cwdNote,
			Parameters: obj(map[string]interface{}{
				"path":        strProp("Path to the file to edit (relative paths resolve from the working directory)"),
				"old_string":  strProp("Exact text to replace, copied verbatim from the file (no line-number prefix)."),
				"new_string":  strProp("Replacement text."),
				"replace_all": boolProp("Replace every occurrence instead of requiring a unique match (default false)."),
			}, "path", "old_string", "new_string"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name:        "open_url",
			Description: "Open a URL (or local file path) in the user's default browser/application on their HOST machine. Use this instead of running xdg-open/open via run_shell — shell commands may run inside a headless sandbox that cannot reach the host's desktop, whereas this always runs on the host.",
			Parameters: obj(map[string]interface{}{
				"url": strProp("The URL (e.g. http://localhost:23000) or file path to open"),
			}, "url"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name:        "write_file",
			Description: "Write content to a file, overwriting it if it exists. Requires user confirmation. " + cwdNote,
			Parameters: obj(map[string]interface{}{
				"path":    strProp("Path to the file to write (relative paths resolve from the working directory)"),
				"content": strProp("The full content to write to the file"),
			}, "path", "content"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "delete_file",
			Description: "Delete a file or empty directory. " +
				"Does NOT delete non-empty directories — use run_shell rm -r explicitly for that. " +
				"Path must be inside the workspace; traversal and symlink escapes outside the workspace are rejected. " +
				"Requires user confirmation. " + cwdNote,
			Parameters: obj(map[string]interface{}{
				"path": strProp("Path to the file or empty directory to delete."),
			}, "path"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "move_file",
			Description: "Rename or move a file or directory within the workspace. " +
				"Both src and dst must be inside the workspace. " +
				"Fails if dst already exists — delete it first if you intend to overwrite. " +
				"Does not create parent directories of dst — use run_shell mkdir first if needed. " +
				"Requires user confirmation. " + cwdNote,
			Parameters: obj(map[string]interface{}{
				"src": strProp("Source path (file or directory to move)."),
				"dst": strProp("Destination path. Must not already exist."),
			}, "src", "dst"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "run_background",
			Description: "Start a command in the background, detached from the current shell. " +
				"Returns an id and log path; use read_process_log to check output and kill_process to stop it. " +
				"Maximum 5 concurrent background processes. " +
				"Requires user confirmation.",
			Parameters: obj(map[string]interface{}{
				"command": strProp("Shell command to run in the background."),
				"label":   strProp("Short human-readable label, e.g. 'dev-server'. Shown in status messages."),
			}, "command", "label"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "kill_process",
			Description: "Stop a background process started with run_background. " +
				"Sends SIGTERM to the entire process group, then SIGKILL after 5 seconds if still alive. " +
				"Requires user confirmation.",
			Parameters: obj(map[string]interface{}{
				"id": strProp("Process id returned by run_background, e.g. 'bg1'."),
			}, "id"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "read_process_log",
			Description: "Read the tail of a background process's log (last 8 KB, hard cap). " +
				"Also reports whether the process is still running. " +
				"Does not require user confirmation.",
			Parameters: obj(map[string]interface{}{
				"id": strProp("Process id returned by run_background, e.g. 'bg1'."),
			}, "id"),
		}},
	}
}

// GatedTool reports whether a tool requires human confirmation before running.
func GatedTool(name string) bool {
	switch name {
	case "run_shell", "write_file", "edit_file",
		"delete_file", "move_file", "run_background", "kill_process":
		return true
	default:
		return false
	}
}

// DiscoveryTools is the read-only tool set for the subagent.
//
// run_shell is deliberately absent: isReadOnlyShell is defence-in-depth for
// the parent's UX (a human is still in the loop there), but the subagent has
// no confirm gate, which would make isReadOnlyShell the sole security wall —
// and the audit found that enumerate-the-bad never converges. Removing the
// capability is safer than trying to classify it correctly under all inputs.
//
// grep-style search is provided via the constrained search_files tool whose
// handler builds the shell command from structured JSON arguments rather than
// accepting a raw shell string from the model.
func DiscoveryTools(cwd string) []proxy.Tool {
	cwdNote := fmt.Sprintf("Working directory: %s — prefer relative paths.", cwd)
	strProp := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "string", "description": desc}
	}
	intProp := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "integer", "description": desc}
	}
	boolProp := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "boolean", "description": desc}
	}
	obj := func(props map[string]interface{}, required ...string) json.RawMessage {
		m := map[string]interface{}{"type": "object", "properties": props}
		if len(required) > 0 {
			m["required"] = required
		}
		b, _ := json.Marshal(m)
		return b
	}
	return []proxy.Tool{
		{Type: "function", Function: proxy.ToolFunction{
			Name: "read_file",
			Description: "Read a file and return its contents with line numbers. Reads the whole file by default; " +
				"pass offset/limit to read only a line range. " + cwdNote,
			Parameters: obj(map[string]interface{}{
				"path":   strProp("Path to the file to read."),
				"offset": intProp("Optional 1-based line number to start reading from."),
				"limit":  intProp("Optional maximum number of lines to read from offset."),
			}, "path"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "read_file_full",
			Description: "Read an entire file in one call and return its complete contents with line numbers. " +
				"Use read_file_full when you need the complete contents of a normal source file (up to ~256 KB); " +
				"use read_file for large files or targeted ranges (offset/limit). " +
				"Prefer read_file_full over repeated read_file calls with different offsets on the same file. " + cwdNote,
			Parameters: obj(map[string]interface{}{
				"path": strProp("Path to the file to read."),
			}, "path"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "search_files",
			Description: "Search for a pattern in files and return matching lines with file:line context. " +
				"Equivalent to grep -rn. " + cwdNote,
			Parameters: obj(map[string]interface{}{
				"pattern":          strProp("Search pattern (literal string or basic regex)."),
				"path":             strProp("File or directory to search."),
				"file_pattern":     strProp("Optional glob to restrict which files are searched, e.g. '*.go'."),
				"case_insensitive": boolProp("Case-insensitive search (default false)."),
			}, "pattern", "path"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "find_files",
			Description: "Find files by name recursively under a path. Equivalent to find -type f -name. " +
				"Use to locate files when you don't know where they live. " + cwdNote,
			Parameters: obj(map[string]interface{}{
				"pattern": strProp("Filename glob, e.g. '*.go' or 'config.*'."),
				"path":    strProp("Directory to search under (defaults to the working directory)."),
			}, "pattern"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "list_dir",
			Description: "List the entries of a directory (names, with a trailing / on subdirectories). " +
				"Use this to discover what files exist before reading them. " + cwdNote,
			Parameters: obj(map[string]interface{}{
				"path": strProp("Directory to list (defaults to the working directory)."),
			}),
		}},
	}
}

// CapabilityDiscovery is the default read-only subagent capability.
const CapabilityDiscovery = "discovery"

// CapabilityEdit adds file mutation tools (write_file, edit_file, delete_file,
// move_file) to the discovery set. Requires session write consent at dispatch time.
// exec tools (run_shell, run_background, kill_process) are deliberately excluded:
// run_shell has no path confinement by design, the shared Executor is read-safe
// only, and child bgProcs would orphan on child completion.
const CapabilityEdit = "edit"

// validCapabilities is the exhaustive set of accepted capability values.
var validCapabilities = map[string]bool{
	CapabilityDiscovery: true,
	CapabilityEdit:      true,
}

// ValidCapability reports whether capability is a recognized value.
func ValidCapability(capability string) bool {
	return validCapabilities[capability]
}

// EditTools returns the edit-tier tool set: DiscoveryTools' 5 read-only tools
// plus the 4 edit tools (write_file, edit_file, delete_file, move_file). Same
// deterministic-schema construction as DiscoveryTools — no interpolation, so
// all edit-tier dispatches share a byte-identical tool-schema prefix.
func EditTools(cwd string) []proxy.Tool {
	cwdNote := fmt.Sprintf("Working directory: %s — prefer relative paths.", cwd)
	strProp := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "string", "description": desc}
	}
	intProp := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "integer", "description": desc}
	}
	boolProp := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "boolean", "description": desc}
	}
	obj := func(props map[string]interface{}, required ...string) json.RawMessage {
		m := map[string]interface{}{"type": "object", "properties": props}
		if len(required) > 0 {
			m["required"] = required
		}
		b, _ := json.Marshal(m)
		return b
	}
	return []proxy.Tool{
		{Type: "function", Function: proxy.ToolFunction{
			Name: "read_file",
			Description: "Read a file and return its contents with line numbers. Reads the whole file by default; " +
				"pass offset/limit to read only a line range. " + cwdNote,
			Parameters: obj(map[string]interface{}{
				"path":   strProp("Path to the file to read."),
				"offset": intProp("Optional 1-based line number to start reading from."),
				"limit":  intProp("Optional maximum number of lines to read from offset."),
			}, "path"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "read_file_full",
			Description: "Read an entire file in one call and return its complete contents with line numbers. " +
				"Use read_file_full when you need the complete contents of a normal source file (up to ~256 KB); " +
				"use read_file for large files or targeted ranges (offset/limit). " +
				"Prefer read_file_full over repeated read_file calls with different offsets on the same file. " + cwdNote,
			Parameters: obj(map[string]interface{}{
				"path": strProp("Path to the file to read."),
			}, "path"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "search_files",
			Description: "Search for a pattern in files and return matching lines with file:line context. " +
				"Equivalent to grep -rn. " + cwdNote,
			Parameters: obj(map[string]interface{}{
				"pattern":          strProp("Search pattern (literal string or basic regex)."),
				"path":             strProp("File or directory to search."),
				"file_pattern":     strProp("Optional glob to restrict which files are searched, e.g. '*.go'."),
				"case_insensitive": boolProp("Case-insensitive search (default false)."),
			}, "pattern", "path"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "find_files",
			Description: "Find files by name recursively under a path. Equivalent to find -type f -name. " +
				"Use to locate files when you don't know where they live. " + cwdNote,
			Parameters: obj(map[string]interface{}{
				"pattern": strProp("Filename glob, e.g. '*.go' or 'config.*'."),
				"path":    strProp("Directory to search under (defaults to the working directory)."),
			}, "pattern"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "list_dir",
			Description: "List the entries of a directory (names, with a trailing / on subdirectories). " +
				"Use this to discover what files exist before reading them. " + cwdNote,
			Parameters: obj(map[string]interface{}{
				"path": strProp("Directory to list (defaults to the working directory)."),
			}),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "edit_file",
			Description: "Replace an exact substring in a file. old_string must match the file's raw text " +
				"verbatim (including whitespace, and WITHOUT the line-number gutter that read_file shows) and " +
				"must be unique unless replace_all is set. Prefer this over write_file for changes to existing " +
				"files. " + cwdNote,
			Parameters: obj(map[string]interface{}{
				"path":        strProp("Path to the file to edit."),
				"old_string":  strProp("Exact text to replace, copied verbatim from the file (no line-number prefix)."),
				"new_string":  strProp("Replacement text."),
				"replace_all": boolProp("Replace every occurrence instead of requiring a unique match (default false)."),
			}, "path", "old_string", "new_string"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name:        "write_file",
			Description: "Write content to a file, overwriting it if it exists. " + cwdNote,
			Parameters: obj(map[string]interface{}{
				"path":    strProp("Path to the file to write."),
				"content": strProp("The full content to write to the file"),
			}, "path", "content"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "delete_file",
			Description: "Delete a file or empty directory. " +
				"Does NOT delete non-empty directories — path must be inside the workspace. " +
				cwdNote,
			Parameters: obj(map[string]interface{}{
				"path": strProp("Path to the file or empty directory to delete."),
			}, "path"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "move_file",
			Description: "Rename or move a file or directory within the workspace. " +
				"Both src and dst must be inside the workspace. " +
				"Fails if dst already exists. " + cwdNote,
			Parameters: obj(map[string]interface{}{
				"src": strProp("Source path (file or directory to move)."),
				"dst": strProp("Destination path. Must not already exist."),
			}, "src", "dst"),
		}},
	}
}

// IsEditTool reports whether name is one of the edit-category tools that mutate
// files (write_file, edit_file, delete_file, move_file). Used by the files_changed
// recorder to decide which tool calls to track.
func IsEditTool(name string) bool {
	switch name {
	case "write_file", "edit_file", "delete_file", "move_file":
		return true
	}
	return false
}

// IsMashuraTool reports whether name is one of the mashūra counsel tools (the
// mashura__* family) or the legacy oracle__ask alias kept for back-compat. Every
// such tool goes through the confirm gate (auto-approved in /auto mode with a
// visible ⚡ auto note), and its response is kept in full (capOrStub never
// truncates it).
func IsMashuraTool(name string) bool {
	switch name {
	case "mashura__review", "mashura__debug", "mashura__decide", "mashura__check", "oracle__ask":
		return true
	}
	return false
}

// IsSubagentResult reports whether name is the dispatch_subagent tool or its
// batch variant dispatch_subagents. Their results are already structured JSON
// digests of dozens of internal tool iterations — re-truncating or stubbing a
// digest discards the work. Protected from cap/stub the same way mashūra
// responses are, for the same reason.
func IsSubagentResult(name string) bool {
	return name == "dispatch_subagent" || name == "dispatch_subagents"
}

// The mashūra counsel tools (mashura__review / debug / decide / check) are
// defined in mashura.go via mashuraToolDefs.
