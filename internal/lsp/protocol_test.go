package lsp

import (
	"encoding/json"
	"testing"
)

func TestDocumentChange_UnmarshalJSON_TextDocumentEdit(t *testing.T) {
	data := `{"textDocument":{"version":1,"uri":"file:///work/main.go"},"edits":[{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":5}},"newText":"hello"}]}`
	var dc DocumentChange
	if err := json.Unmarshal([]byte(data), &dc); err != nil {
		t.Fatalf("unmarshal TextDocumentEdit: %v", err)
	}
	if dc.TextDocumentEdit == nil {
		t.Fatal("TextDocumentEdit is nil")
	}
	if dc.CreateFile != nil || dc.RenameFile != nil || dc.DeleteFile != nil {
		t.Error("only TextDocumentEdit should be set")
	}
	if dc.TextDocumentEdit.TextDocument.Version != 1 {
		t.Errorf("version = %d, want 1", dc.TextDocumentEdit.TextDocument.Version)
	}
}

func TestDocumentChange_UnmarshalJSON_CreateFile(t *testing.T) {
	data := `{"kind":"create","uri":"file:///work/new.go"}`
	var dc DocumentChange
	if err := json.Unmarshal([]byte(data), &dc); err != nil {
		t.Fatalf("unmarshal CreateFile: %v", err)
	}
	if dc.CreateFile == nil {
		t.Fatal("CreateFile is nil")
	}
	if dc.CreateFile.Kind != "create" {
		t.Errorf("kind = %q, want 'create'", dc.CreateFile.Kind)
	}
	if dc.CreateFile.URI != "file:///work/new.go" {
		t.Errorf("uri = %q", dc.CreateFile.URI)
	}
}

func TestDocumentChange_UnmarshalJSON_RenameFile(t *testing.T) {
	data := `{"kind":"rename","oldUri":"file:///work/old.go","newUri":"file:///work/new.go"}`
	var dc DocumentChange
	if err := json.Unmarshal([]byte(data), &dc); err != nil {
		t.Fatalf("unmarshal RenameFile: %v", err)
	}
	if dc.RenameFile == nil {
		t.Fatal("RenameFile is nil")
	}
	if dc.RenameFile.OldURI != "file:///work/old.go" {
		t.Errorf("oldUri = %q", dc.RenameFile.OldURI)
	}
	if dc.RenameFile.NewURI != "file:///work/new.go" {
		t.Errorf("newUri = %q", dc.RenameFile.NewURI)
	}
}

func TestDocumentChange_UnmarshalJSON_DeleteFile(t *testing.T) {
	data := `{"kind":"delete","uri":"file:///work/deleted.go"}`
	var dc DocumentChange
	if err := json.Unmarshal([]byte(data), &dc); err != nil {
		t.Fatalf("unmarshal DeleteFile: %v", err)
	}
	if dc.DeleteFile == nil {
		t.Fatal("DeleteFile is nil")
	}
	if dc.DeleteFile.URI != "file:///work/deleted.go" {
		t.Errorf("uri = %q", dc.DeleteFile.URI)
	}
}

func TestDocumentChange_UnmarshalJSON_UnknownKind(t *testing.T) {
	data := `{"kind":"unknown","uri":"file:///work/x.go"}`
	var dc DocumentChange
	if err := json.Unmarshal([]byte(data), &dc); err == nil {
		t.Fatal("expected error for unknown kind, got nil")
	}
}

func TestDocumentChange_UnmarshalJSON_InvalidJSON(t *testing.T) {
	data := `{invalid json}`
	var dc DocumentChange
	if err := json.Unmarshal([]byte(data), &dc); err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}
