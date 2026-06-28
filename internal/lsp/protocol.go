// LSP protocol type structs — sourced from gopls v0.22.0 internal/protocol
// (generated from the LSP 3.17 meta-model). Only the subset needed for the
// MVP's ~7 methods is included. See:
//   golang.org/x/tools/gopls@v0.22.0/internal/protocol/tsprotocol.go

package lsp

import (
	"encoding/json"
	"fmt"
)

// PositionEncodingKind is the negotiated position encoding.
type PositionEncodingKind string

const (
	UTF8  PositionEncodingKind = "utf-8"
	UTF16 PositionEncodingKind = "utf-16"
)

// FileChangeType describes how a watched file changed.
type FileChangeType int

const (
	FileChanged  FileChangeType = 2
	FileCreated  FileChangeType = 1
	FileDeleted  FileChangeType = 3
)

// TextDocumentSyncKind defines how documents are synced.
type TextDocumentSyncKind int

const (
	SyncNone        TextDocumentSyncKind = 0
	SyncFull        TextDocumentSyncKind = 1
	SyncIncremental TextDocumentSyncKind = 2
)

// SymbolKind — only the common ones; the LSP spec defines more.
type SymbolKind uint32

const (
	SymFile          SymbolKind = 1
	SymModule         SymbolKind = 2
	SymNamespace      SymbolKind = 3
	SymPackage        SymbolKind = 4
	SymClass         SymbolKind = 5
	SymMethod        SymbolKind = 6
	SymProperty      SymbolKind = 7
	SymField         SymbolKind = 8
	SymConstructor   SymbolKind = 9
	SymEnum          SymbolKind = 10
	SymInterface     SymbolKind = 11
	SymFunction      SymbolKind = 12
	SymVariable      SymbolKind = 13
	SymConstant      SymbolKind = 14
	SymStruct        SymbolKind = 23
	SymTypeParameter SymbolKind = 26
)

// ─── Core types ───────────────────────────────────────────────────────────────

// Position is a zero-based line/character position in a document.
// The character offset's encoding is determined by the negotiated
// PositionEncodingKind (UTF-8 or UTF-16).
type Position struct {
	Line      uint32 `json:"line"`
	Character uint32 `json:"character"`
}

// Range is a span in a text document.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Location is a URI + range.
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

// TextDocumentIdentifier identifies a document by URI.
type TextDocumentIdentifier struct {
	URI string `json:"uri"`
}

// VersionedTextDocumentIdentifier adds a version number.
type VersionedTextDocumentIdentifier struct {
	Version int32                  `json:"version"`
	TextDocumentIdentifier
}

// TextDocumentItem carries full document content (for didOpen).
type TextDocumentItem struct {
	URI       string `json:"uri"`
	LanguageID string `json:"languageId"`
	Version   int32  `json:"version"`
	Text      string `json:"text"`
}

// MarkupContent is markdown or plaintext.
type MarkupContent struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// ─── Initialize ────────────────────────────────────────────────────────────────

type WorkspaceFolder struct {
	URI  string `json:"uri"`
	Name string `json:"name"`
}

// GeneralClientCapabilities carries position encoding negotiation (LSP 3.17).
type GeneralClientCapabilities struct {
	PositionEncodings []PositionEncodingKind `json:"positionEncodings,omitempty"`
}

// WorkspaceClientCapabilities — only the fields we need.
type WorkspaceClientCapabilities struct {
	WorkspaceEdit   *WorkspaceEditClientCapabilities `json:"workspaceEdit,omitempty"`
	DidChangeWatchedFiles *DidChangeWatchedFilesClientCapabilities `json:"didChangeWatchedFiles,omitempty"`
}

type WorkspaceEditClientCapabilities struct {
	DocumentChanges   bool     `json:"documentChanges,omitempty"`
	ResourceOperations []string `json:"resourceOperations,omitempty"`
}

type DidChangeWatchedFilesClientCapabilities struct {
	DynamicRegistration bool `json:"dynamicRegistration,omitempty"`
}

// TextDocumentClientCapabilities — per-feature capabilities.
type TextDocumentClientCapabilities struct {
	Synchronization   *TextDocumentSyncClientCapabilities `json:"synchronization,omitempty"`
	Hover              *HoverClientCapabilities            `json:"hover,omitempty"`
	Definition         *DefinitionClientCapabilities       `json:"definition,omitempty"`
	References         *ReferenceClientCapabilities       `json:"references,omitempty"`
	DocumentSymbol     *DocumentSymbolClientCapabilities   `json:"documentSymbol,omitempty"`
}

type TextDocumentSyncClientCapabilities struct {
	DidSave bool `json:"didSave,omitempty"`
}

type HoverClientCapabilities struct {
	ContentFormat []string `json:"contentFormat,omitempty"`
}

type DefinitionClientCapabilities struct {
	LinkSupport bool `json:"linkSupport,omitempty"` // we do NOT set this — we want Location[], not LocationLink[]
}

type ReferenceClientCapabilities struct{}

type DocumentSymbolClientCapabilities struct {
	HierarchicalDocumentSymbolSupport bool `json:"hierarchicalDocumentSymbolSupport,omitempty"`
}

// WindowClientCapabilities — for $/progress.
type WindowClientCapabilities struct {
	WorkDoneProgress bool `json:"workDoneProgress,omitempty"`
}

// ClientCapabilities is the full client capability set sent in initialize.
type ClientCapabilities struct {
	Workspace     *WorkspaceClientCapabilities     `json:"workspace,omitempty"`
	TextDocument  *TextDocumentClientCapabilities  `json:"textDocument,omitempty"`
	Window        *WindowClientCapabilities        `json:"window,omitempty"`
	General       *GeneralClientCapabilities       `json:"general,omitempty"`
}

// InitializeParams is the initialize request payload.
type InitializeParams struct {
	ProcessID           *int32              `json:"processId"`
	ClientInfo          *ClientInfo         `json:"clientInfo,omitempty"`
	RootURI             *string             `json:"rootUri"` // can be null
	Capabilities        ClientCapabilities  `json:"capabilities"`
	InitializationOptions any               `json:"initializationOptions,omitempty"`
	WorkspaceFolders    []WorkspaceFolder   `json:"workspaceFolders,omitempty"`
}

type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// ServerCapabilities is the server's advertised capabilities (subset).
type ServerCapabilities struct {
	PositionEncoding     *PositionEncodingKind `json:"positionEncoding,omitempty"`
	TextDocumentSync     any                   `json:"textDocumentSync,omitempty"` // can be TextDocumentSyncKind or TextDocumentSyncOptions
	DefinitionProvider   any                   `json:"definitionProvider,omitempty"`
	ReferencesProvider   any                   `json:"referencesProvider,omitempty"`
	HoverProvider        any                   `json:"hoverProvider,omitempty"`
	DocumentSymbolProvider any                 `json:"documentSymbolProvider,omitempty"`
	WorkspaceSymbolProvider any                `json:"workspaceSymbolProvider,omitempty"`
	RenameProvider       any                   `json:"renameProvider,omitempty"`
}

// InitializeResult is the initialize response.
type InitializeResult struct {
	Capabilities ServerCapabilities `json:"capabilities"`
	ServerInfo   *ServerInfo        `json:"serverInfo,omitempty"`
}

type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// ─── Request params ─────────────────────────────────────────────────────────────

// TextDocumentPositionParams is the shared base for positional requests.
type TextDocumentPositionParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
	Position      Position               `json:"position"`
}

// DefinitionParams — textDocument/definition.
type DefinitionParams struct {
	TextDocumentPositionParams
}

// ReferenceContext carries includeDeclaration (mandatory).
type ReferenceContext struct {
	IncludeDeclaration bool `json:"includeDeclaration"`
}

// ReferenceParams — textDocument/references.
type ReferenceParams struct {
	TextDocumentPositionParams
	Context ReferenceContext `json:"context"`
}

// HoverParams — textDocument/hover.
type HoverParams struct {
	TextDocumentPositionParams
}

// DocumentSymbolParams — textDocument/documentSymbol.
type DocumentSymbolParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
}

// WorkspaceSymbolParams — workspace/symbol.
type WorkspaceSymbolParams struct {
	Query string `json:"query"`
}

// RenameParams — textDocument/rename.
type RenameParams struct {
	TextDocumentPositionParams
	NewName string `json:"newName"`
}

// ─── Result types ───────────────────────────────────────────────────────────────

// Hover is the result of textDocument/hover.
type Hover struct {
	Contents MarkupContent `json:"contents"`
	Range    *Range        `json:"range,omitempty"`
}

// DocumentSymbol is a hierarchical symbol (functions, types, vars).
type DocumentSymbol struct {
	Name           string           `json:"name"`
	Detail         string           `json:"detail,omitempty"`
	Kind           SymbolKind       `json:"kind"`
	Range          Range            `json:"range"`
	SelectionRange Range            `json:"selectionRange"`
	Children       []DocumentSymbol `json:"children,omitempty"`
}

// SymbolInformation is a flat workspace symbol.
type SymbolInformation struct {
	Name          string   `json:"name"`
	Kind          SymbolKind `json:"kind"`
	Location      Location `json:"location"`
	ContainerName string   `json:"containerName,omitempty"`
}

// TextEdit is a single edit.
type TextEdit struct {
	Range   Range  `json:"range"`
	NewText string `json:"newText"`
}

// WorkspaceEdit is the result of rename — changes OR documentChanges.
type WorkspaceEdit struct {
	Changes        map[string][]TextEdit  `json:"changes,omitempty"`
	DocumentChanges []DocumentChange       `json:"documentChanges,omitempty"`
}

// DocumentChange is a union: TextDocumentEdit | CreateFile | RenameFile | DeleteFile.
// Sourced from gopls's tsdocument_changes.go.
type DocumentChange struct {
	TextDocumentEdit *TextDocumentEdit
	CreateFile       *CreateFile
	RenameFile       *RenameFile
	DeleteFile       *DeleteFile
}

type OptionalVersionedTextDocumentIdentifier struct {
	Version int32 `json:"version"`
	TextDocumentIdentifier
}

type TextDocumentEdit struct {
	TextDocument OptionalVersionedTextDocumentIdentifier `json:"textDocument"`
	Edits        []TextEdit                                `json:"edits"`
}

type CreateFile struct {
	Kind string `json:"kind"`
	URI  string `json:"uri"`
}

type RenameFile struct {
	Kind  string `json:"kind"`
	OldURI string `json:"oldUri"`
	NewURI string `json:"newUri"`
}

type DeleteFile struct {
	Kind string `json:"kind"`
	URI  string `json:"uri"`
}

// UnmarshalJSON for DocumentChange — discriminates by "kind" or "textDocument".
// Sourced from gopls's tsdocument_changes.go.
func (d *DocumentChange) UnmarshalJSON(data []byte) error {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	if _, ok := m["textDocument"]; ok {
		d.TextDocumentEdit = &TextDocumentEdit{}
		return json.Unmarshal(data, d.TextDocumentEdit)
	}
	kind, _ := m["kind"].(string)
	switch kind {
	case "create":
		d.CreateFile = &CreateFile{}
		return json.Unmarshal(data, d.CreateFile)
	case "rename":
		d.RenameFile = &RenameFile{}
		return json.Unmarshal(data, d.RenameFile)
	case "delete":
		d.DeleteFile = &DeleteFile{}
		return json.Unmarshal(data, d.DeleteFile)
	}
	return fmt.Errorf("DocumentChange: unexpected kind: %q", kind)
}

// ─── Notification params ─────────────────────────────────────────────────────────

// DidOpenTextDocumentParams — textDocument/didOpen.
type DidOpenTextDocumentParams struct {
	TextDocument TextDocumentItem `json:"textDocument"`
}

// DidChangeTextDocumentParams — textDocument/didChange (full sync).
type DidChangeTextDocumentParams struct {
	TextDocument   VersionedTextDocumentIdentifier     `json:"textDocument"`
	ContentChanges []TextDocumentContentChangeEvent    `json:"contentChanges"`
}

// TextDocumentContentChangeEvent — for full sync, just {text: ...}.
type TextDocumentContentChangeEvent struct {
	Text string `json:"text"`
}

// DidChangeWatchedFilesParams — workspace/didChangeWatchedFiles.
type DidChangeWatchedFilesParams struct {
	Changes []FileEvent `json:"changes"`
}

type FileEvent struct {
	URI  string          `json:"uri"`
	Type FileChangeType `json:"type"`
}

// ─── Progress ───────────────────────────────────────────────────────────────────

// ProgressParams is the $/progress notification.
type ProgressParams struct {
	Token any `json:"token"`
	Value any `json:"value"`
}

// WorkDoneProgressBegin is the start of a progress operation.
type WorkDoneProgressBegin struct {
	Kind   string `json:"kind"`
	Title  string `json:"title"`
	Message string `json:"message,omitempty"`
}

// WorkDoneProgressEnd signals completion of a progress operation.
type WorkDoneProgressEnd struct {
	Kind    string `json:"kind"`
	Message string `json:"message,omitempty"`
}

// WorkDoneProgressCreateParams — window/workDoneProgress/create (server→client request).
type WorkDoneProgressCreateParams struct {
	Token any `json:"token"`
}

// ─── WorkspaceFolder ─────────────────────────────────────────────────────────────

// WorkspaceFoldersInitializeParams is embedded into InitializeParams.
type WorkspaceFoldersInitializeParams struct {
	WorkspaceFolders []WorkspaceFolder `json:"workspaceFolders,omitempty"`
}
