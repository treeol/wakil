package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wakil/internal/tui"


	"wakil/internal/tools"
)

func setupMentionTree(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "pkg", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "pkg", "util.go"), []byte("package pkg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return base
}

func TestResolveMentionsFile(t *testing.T) {
	base := setupMentionTree(t)
	out, refs := tools.ResolveMentions("explain @main.go please", base)
	if len(refs) != 1 || !refs[0].Ok || refs[0].Token != "main.go" {
		t.Fatalf("refs = %+v", refs)
	}
	if !strings.Contains(out, "explain @main.go please") {
		t.Fatal("typed text should be preserved")
	}
	if !strings.Contains(out, "--- @main.go") || !strings.Contains(out, "func main()") {
		t.Fatalf("file content not injected:\n%s", out)
	}
	if !strings.Contains(out, "```go") {
		t.Fatal("expected go fence")
	}
}

func TestResolveMentionsFolder(t *testing.T) {
	base := setupMentionTree(t)
	out, refs := tools.ResolveMentions("look at @pkg", base)
	if len(refs) != 1 || !refs[0].Ok {
		t.Fatalf("refs = %+v", refs)
	}
	if !strings.Contains(out, "(directory)") || !strings.Contains(out, "util.go") {
		t.Fatalf("folder listing not injected:\n%s", out)
	}
	if strings.Contains(out, "package pkg") {
		t.Fatal("folder injection must not include file contents")
	}
}

func TestResolveMentionsMissing(t *testing.T) {
	base := setupMentionTree(t)
	out, refs := tools.ResolveMentions("@nope.go", base)
	if len(refs) != 1 || refs[0].Ok || refs[0].Note != "not found" {
		t.Fatalf("refs = %+v", refs)
	}
	if out != "@nope.go" {
		t.Fatalf("nothing should be injected for a miss, got %q", out)
	}
}

func TestResolveMentionsNone(t *testing.T) {
	base := setupMentionTree(t)
	out, refs := tools.ResolveMentions("an email a@b.com is not a mention", base)
	if refs != nil {
		t.Fatalf("mid-word @ should not match: %+v", refs)
	}
	if out != "an email a@b.com is not a mention" {
		t.Fatal("input should be unchanged")
	}
}

func TestSplitMentionQuery(t *testing.T) {
	cases := map[string][2]string{
		"ma":     {"", "ma"},
		"src/ma": {"src/", "ma"},
		"a/b/c":  {"a/b/", "c"},
		"src/":   {"src/", ""},
	}
	for in, want := range cases {
		d, l := tui.SplitMentionQuery(in)
		if d != want[0] || l != want[1] {
			t.Errorf("tui.SplitMentionQuery(%q) = (%q,%q), want (%q,%q)", in, d, l, want[0], want[1])
		}
	}
}
