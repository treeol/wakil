package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/treeol/wakil/internal/proxy"
)

// LSPTools returns the LSP tool definitions for the MVP.
// Only called when cfg.LSPEnabled is true.
// Per-tool capability gating happens at call time (the tool is declared
// statically, but the handler checks capability + returns the failure contract
// if the server doesn't support the operation).
func LSPTools(cwd string) []proxy.Tool {
	cwdNote := fmt.Sprintf("Working directory: %s — prefer relative paths.", cwd)
	strProp := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "string", "description": desc}
	}
	intProp := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "integer", "description": desc}
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
			Name: "lsp_definition",
			Description: "Find the definition of a symbol. Pass the file path, the 1-based line number " +
				"(as shown by read_file), and the symbol name. The client resolves the position. " +
				"Returns file:line:col locations. Read-only — no confirmation needed. " + cwdNote,
			Parameters: obj(map[string]interface{}{
				"path":   strProp("Path to the source file (relative paths resolve from the working directory)"),
				"line":   intProp("1-based line number (as shown by read_file)"),
				"symbol": strProp("The symbol name to find the definition of"),
			}, "path", "line", "symbol"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "lsp_references",
			Description: "Find all references to a symbol. Pass the file path, the 1-based line number, " +
				"and the symbol name. Returns references clustered by file. Read-only. " + cwdNote,
			Parameters: obj(map[string]interface{}{
				"path":   strProp("Path to the source file"),
				"line":   intProp("1-based line number"),
				"symbol": strProp("The symbol name to find references for"),
			}, "path", "line", "symbol"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "lsp_hover",
			Description: "Get type/signature/doc info for a symbol. Pass the file path, 1-based line, " +
				"and symbol name. Returns plain-text hover info. Read-only. " + cwdNote,
			Parameters: obj(map[string]interface{}{
				"path":   strProp("Path to the source file"),
				"line":   intProp("1-based line number"),
				"symbol": strProp("The symbol name to hover"),
			}, "path", "line", "symbol"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "lsp_symbols",
			Description: "Search for symbols across the entire workspace by name. Returns matching symbols " +
				"with file, kind, and line. Read-only. " + cwdNote,
			Parameters: obj(map[string]interface{}{
				"query": strProp("Symbol name or partial name to search for"),
			}, "query"),
		}},
	}
}

// ─── Capability gating ────────────────────────────────────────────────────────
//
// At tool-declaration time (BuildTools), the config gate decides whether LSP
// tools are declared at all. At CALL time, the handler checks whether the
// specific server supports the operation. If not, returns the failure contract:
//
//	[lsp: "gopls" does not support <operation>. Fell back to text search: no]
//
// Per-tool drop: a server missing one capability drops EXACTLY that tool's
// calls, not the whole group. The model sees the tool declared (it may have
// been declared when the server was up), but calling it returns the failure
// contract.

// CapabilityForTool returns the ServerCapabilities field name that the tool
// requires, or "" if the tool doesn't need a specific capability.
func CapabilityForTool(toolName string) string {
	switch toolName {
	case "lsp_definition":
		return "definitionProvider"
	case "lsp_references":
		return "referencesProvider"
	case "lsp_hover":
		return "hoverProvider"
	case "lsp_symbols":
		return "workspaceSymbolProvider"
	}
	return ""
}

// ─── Dispatch handlers ────────────────────────────────────────────────────────

// HandleLSPReadOnly dispatches a read-only LSP tool call (definition, references,
// hover, symbols). Returns the result string for the model.
func (m *Manager) HandleLSPReadOnly(ctx context.Context, toolName string, argsJSON string) string {
	var args struct {
		Path   string `json:"path"`
		Line   int    `json:"line"`
		Symbol string `json:"symbol"`
		Query  string `json:"query"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}

	// lsp_symbols is workspace-wide — no file/line/symbol needed.
	if toolName == "lsp_symbols" {
		return m.handleSymbols(ctx, args.Query)
	}

	// Positional tools: need path + line + symbol.
	if args.Path == "" || args.Line == 0 {
		return "[lsp: path and line are required for " + toolName + ". Pass the file path and 1-based line number as shown by read_file.]"
	}

	// Detect language and ensure server.
	lang := detectLanguage(args.Path)
	if lang == "" {
		return fmt.Sprintf("[lsp: unsupported file type for %s. Configure lsp_servers for this language.]", args.Path)
	}

	srv, err := m.EnsureServer(ctx, lang)
	if err != nil {
		return m.failureContract(toolName, lang, err, "")
	}

	// Check capability.
	capName := CapabilityForTool(toolName)
	if capName != "" && !srv.CapabilitySupported(capName) {
		return fmt.Sprintf("[lsp: %q does not support %s. Fell back to text search: no]", lang, toolName)
	}

	// Resolve position.
	pos, ambiguous, err := m.resolveToPosition(ctx, srv, args.Path, args.Line, args.Symbol)
	if err != nil {
		return m.failureContract(toolName, lang, err, "resolve")
	}
	if ambiguous != nil {
		// Multi-match: return candidate list.
		var b strings.Builder
		fmt.Fprintf(&b, "[lsp: symbol %q appears %d times on line %d. Disambiguate:\n", args.Symbol, len(ambiguous.Candidates), args.Line)
		for i, c := range ambiguous.Candidates {
			fmt.Fprintf(&b, "  %d. col %d: %s\n", i+1, c.Column, c.Snippet)
		}
		fmt.Fprintf(&b, "Re-call with more context (e.g., the receiver type).]")
		return b.String()
	}

	// Build the LSP request params. Route through HostPathToURI which resolves
	// relative paths against hostMount and translates to the container-visible
	// URI gopls expects. The leak assertion is inside HostPathToURI — a host
	// path that can't be translated returns an error here, not a silent empty
	// URI to gopls.
	uri, err := m.exec.HostPathToURI(args.Path)
	if err != nil {
		return m.failureContract(toolName, lang, fmt.Errorf("URI translation for %q: %w", args.Path, err), "resolve")
	}
	params := TextDocumentPositionParams{
		TextDocument: TextDocumentIdentifier{URI: uri},
		Position:     pos,
	}

	// Dispatch by tool name.
	switch toolName {
	case "lsp_definition":
		return m.handleDefinition(ctx, srv, params)
	case "lsp_references":
		return m.handleReferences(ctx, srv, params)
	case "lsp_hover":
		return m.handleHover(ctx, srv, params)
	}

	return fmt.Sprintf("[lsp: unknown tool %q]", toolName)
}

func (m *Manager) handleDefinition(ctx context.Context, srv *Server, params TextDocumentPositionParams) string {
	reqParams := DefinitionParams{TextDocumentPositionParams: params}
	raw, err := srv.Call(ctx, "textDocument/definition", reqParams)
	if err != nil {
		return m.failureContract("lsp_definition", srv.lang, err, "call")
	}

	locs, err := DecodeDefinition(raw)
	if err != nil {
		return fmt.Sprintf("[lsp: error decoding definition result: %v]", err)
	}
	return RenderDefinition(locs)
}

func (m *Manager) handleReferences(ctx context.Context, srv *Server, params TextDocumentPositionParams) string {
	reqParams := ReferenceParams{
		TextDocumentPositionParams: params,
		Context:                    ReferenceContext{IncludeDeclaration: false},
	}
	raw, err := srv.Call(ctx, "textDocument/references", reqParams)
	if err != nil {
		return m.failureContract("lsp_references", srv.lang, err, "call")
	}

	var locs []Location
	if err := json.Unmarshal(raw, &locs); err != nil {
		// Could be null (genuine empty).
		if string(raw) != "null" {
			return fmt.Sprintf("[lsp: error decoding references result: %v]", err)
		}
	}
	return RenderReferences(locs)
}

func (m *Manager) handleHover(ctx context.Context, srv *Server, params TextDocumentPositionParams) string {
	reqParams := HoverParams{TextDocumentPositionParams: params}
	raw, err := srv.Call(ctx, "textDocument/hover", reqParams)
	if err != nil {
		return m.failureContract("lsp_hover", srv.lang, err, "call")
	}

	if len(raw) == 0 || string(raw) == "null" {
		return "[lsp: no hover information available for this symbol.]"
	}

	var hover Hover
	if err := json.Unmarshal(raw, &hover); err != nil {
		return fmt.Sprintf("[lsp: error decoding hover result: %v]", err)
	}
	return RenderHover(&hover)
}

func (m *Manager) handleSymbols(ctx context.Context, query string) string {
	if query == "" {
		return "[lsp: query is required for lsp_symbols.]"
	}

	// lsp_symbols needs a server — use "go" as the default (the only configured
	// language for the MVP). In the future, the model could specify a language.
	srv, err := m.EnsureServer(ctx, "go")
	if err != nil {
		return m.failureContract("lsp_symbols", "go", err, "")
	}

	if !srv.CapabilitySupported("workspaceSymbolProvider") {
		return fmt.Sprintf("[lsp: %q does not support lsp_symbols. Fell back to text search: no]", "go")
	}

	params := WorkspaceSymbolParams{Query: query}
	raw, err := srv.Call(ctx, "workspace/symbol", params)
	if err != nil {
		return m.failureContract("lsp_symbols", "go", err, "call")
	}

	// Decode defensively: workspace/symbol can return SymbolInformation[] or
	// the newer WorkspaceSymbol[] (3.17) with a different location shape.
	// Try SymbolInformation[] first (the common/legacy shape gopls uses).
	var symInfos []SymbolInformation
	if err := json.Unmarshal(raw, &symInfos); err != nil {
		// Try the newer WorkspaceSymbol[] shape — it has the same Name/Kind
		// but location may be a URI-only or a different struct.
		// As a fallback, decode into a generic shape and extract what we can.
		var genericSyms []struct {
			Name     string `json:"name"`
			Kind     uint32 `json:"kind"`
			Location struct {
				URI   string `json:"uri"`
				Range struct {
					Start struct {
						Line      uint32 `json:"line"`
						Character uint32 `json:"character"`
					} `json:"start"`
				} `json:"range"`
			} `json:"location"`
		}
		if err2 := json.Unmarshal(raw, &genericSyms); err2 != nil {
			return fmt.Sprintf("[lsp: error decoding symbols result: %v]", err)
		}
		// Convert generic shape to SymbolInformation.
		symInfos = make([]SymbolInformation, 0, len(genericSyms))
		for _, gs := range genericSyms {
			symInfos = append(symInfos, SymbolInformation{
				Name: gs.Name,
				Kind: SymbolKind(gs.Kind),
				Location: Location{
					URI: gs.Location.URI,
					Range: Range{
						Start: Position{
							Line:      gs.Location.Range.Start.Line,
							Character: gs.Location.Range.Start.Character,
						},
					},
				},
			})
		}
	}

	symbols := make([]SymbolInfo, 0, len(symInfos))
	for _, s := range symInfos {
		symbols = append(symbols, SymbolInfo{
			Name:     s.Name,
			Kind:     s.Kind,
			Location: s.Location,
		})
	}
	return RenderSymbols(symbols)
}

// ─── Failure contract (Decision 8) ────────────────────────────────────────────

// failureContract returns the truthful status string for a failure path.
// The "Fell back to text search: no" field is always present — the model
// makes the explicit choice to use search_files.
func (m *Manager) failureContract(toolName, lang string, err error, phase string) string {
	// Classify the error.
	errStr := err.Error()

	// Binary not found.
	if strings.Contains(errStr, "spawn failed") || strings.Contains(errStr, "not found in PATH") || strings.Contains(errStr, "no such file") {
		return fmt.Sprintf("[lsp: server binary for %q not found or spawn failed — %v. Install it or use --exec direct. Fell back to text search: no]", lang, err)
	}

	// Initialize failed.
	if strings.Contains(errStr, "initialize failed") {
		return fmt.Sprintf("[lsp: server %q failed to initialize — %v. Fell back to text search: no]", lang, err)
	}

	// Still indexing.
	if strings.Contains(errStr, "still indexing") || strings.Contains(errStr, "indexing") {
		return fmt.Sprintf("[lsp: server %q is still indexing — try again in a moment. Fell back to text search: no]", lang)
	}

	// Server died.
	if strings.Contains(errStr, "server died") || strings.Contains(errStr, "crash") || strings.Contains(errStr, "connection closed") {
		return fmt.Sprintf("[lsp: server %q crashed and could not restart — %v. Fell back to text search: no]", lang, err)
	}

	// Generic.
	return fmt.Sprintf("[lsp: %s failed for %q — %v. Fell back to text search: no]", toolName, lang, err)
}

// _ = config to avoid unused import if config is not directly used in this file.
