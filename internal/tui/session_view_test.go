package tui

import (
	"strings"
	"testing"

	agent "wakil/internal/agent"
	"wakil/internal/proxy"
)

func TestConvItemsFromRoundTrip(t *testing.T) {
	conv := []proxy.Message{
		{Role: "system", Content: agent.StrPtr("[Summary of earlier conversation]\n…")},
		{Role: "user", Content: agent.StrPtr("hi")},
		{Role: "assistant", Content: agent.StrPtr("hello"), ToolCalls: []proxy.ToolCall{{Function: proxy.FunctionCall{Name: "run_shell"}}}},
		{Role: "tool", Name: "run_shell", Content: agent.StrPtr("output")},
	}
	items := convItemsFrom(conv)
	kinds := []itemKind{iSys, iUser, iAsst, iSys}
	if len(items) != len(kinds) {
		t.Fatalf("got %d items, want %d", len(items), len(kinds))
	}
	for i, want := range kinds {
		if items[i].kind != want {
			t.Fatalf("item %d kind = %d, want %d", i, items[i].kind, want)
		}
	}
}

func TestConvItemsFrom(t *testing.T) {
	conv := []proxy.Message{
		{Role: "user", Content: agent.StrPtr("do it")},
		{Role: "assistant", Content: agent.StrPtr("working")},
		{Role: "assistant", Content: nil}, // tool-call-only turn: no visible item
		{Role: "tool", Name: "read_file", Content: agent.StrPtr("file body")},
		{Role: "system", Content: agent.StrPtr("[Summary]")},
	}
	items := convItemsFrom(conv)
	// user + assistant(text) + tool + system = 4 (the nil-content assistant is skipped)
	if len(items) != 4 {
		t.Fatalf("want 4 items, got %d: %+v", len(items), items)
	}
	if items[0].kind != iUser || !strings.Contains(items[0].text, "do it") {
		t.Errorf("first item should be the user echo; got %+v", items[0])
	}
}
