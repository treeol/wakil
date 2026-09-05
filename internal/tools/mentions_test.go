package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadFileCappedTruncatesAndDetectsBinary verifies file content capping and
// binary detection.
func TestReadFileCappedTruncatesAndDetectsBinary(t *testing.T) {
	dir := t.TempDir()

	big := filepath.Join(dir, "big.txt")
	if err := os.WriteFile(big, []byte(strings.Repeat("x", maxMentionBytes+100)), 0o644); err != nil {
		t.Fatal(err)
	}
	_, truncated, binary, err := readFileCapped(big)
	if err != nil || binary || !truncated {
		t.Fatalf("big file: truncated=%v binary=%v err=%v", truncated, binary, err)
	}

	bin := filepath.Join(dir, "blob.bin")
	if err := os.WriteFile(bin, []byte{1, 2, 0, 3, 4}, 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, binary, err = readFileCapped(bin)
	if err != nil || !binary {
		t.Fatalf("binary detection failed: binary=%v err=%v", binary, err)
	}
}

// TestUserQueryText verifies extraction of original text from outgoing messages
// with injected "@" mention blocks.
func TestUserQueryText(t *testing.T) {
	t.Run("no_mention_block", func(t *testing.T) {
		if got := UserQueryText("just a plain message"); got != "just a plain message" {
			t.Errorf("UserQueryText = %q, want %q", got, "just a plain message")
		}
	})

	t.Run("with_mention_block", func(t *testing.T) {
		outgoing := "fix the bug in auth\n\n--- @src/main.go ---\n```go\npackage main\n```"
		got := UserQueryText(outgoing)
		if got != "fix the bug in auth" {
			t.Errorf("UserQueryText = %q, want %q", got, "fix the bug in auth")
		}
	})

	t.Run("empty_string", func(t *testing.T) {
		if got := UserQueryText(""); got != "" {
			t.Errorf("UserQueryText(\"\") = %q, want empty", got)
		}
	})
}

// TestResolveMentions verifies @path parsing, file injection, directory listing,
// and dedup.
func TestResolveMentions(t *testing.T) {
	dir := t.TempDir()

	// Create a test file.
	goFile := filepath.Join(dir, "main.go")
	if err := os.WriteFile(goFile, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create a subdirectory with a file.
	subDir := filepath.Join(dir, "pkg")
	if err := os.Mkdir(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "util.go"), []byte("package pkg\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create a binary file.
	binFile := filepath.Join(dir, "blob.bin")
	if err := os.WriteFile(binFile, []byte{0x00, 0x01, 0x02, 0x00}, 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("no_mentions", func(t *testing.T) {
		outgoing, refs := ResolveMentions("just text", dir)
		if outgoing != "just text" {
			t.Errorf("outgoing = %q, want %q", outgoing, "just text")
		}
		if refs != nil {
			t.Errorf("refs = %v, want nil", refs)
		}
	})

	t.Run("file_mention", func(t *testing.T) {
		outgoing, refs := ResolveMentions("look at @main.go", dir)
		if len(refs) != 1 {
			t.Fatalf("refs = %v, want 1 entry", refs)
		}
		if refs[0].Token != "main.go" {
			t.Errorf("token = %q, want %q", refs[0].Token, "main.go")
		}
		if !refs[0].Ok {
			t.Error("file mention should be Ok")
		}
		if !strings.Contains(outgoing, "```go") {
			t.Error("outgoing should contain go code block")
		}
		if !strings.Contains(outgoing, "package main") {
			t.Error("outgoing should contain file content")
		}
	})

	t.Run("directory_mention", func(t *testing.T) {
		outgoing, refs := ResolveMentions("list @pkg", dir)
		if len(refs) != 1 {
			t.Fatalf("refs = %v, want 1 entry", refs)
		}
		if !refs[0].Ok {
			t.Error("directory mention should be Ok")
		}
		if !strings.Contains(outgoing, "pkg/") {
			t.Error("outgoing should contain directory listing")
		}
		if !strings.Contains(outgoing, "util.go") {
			t.Error("outgoing should list directory contents")
		}
	})

	t.Run("binary_file_mention", func(t *testing.T) {
		_, refs := ResolveMentions("check @blob.bin", dir)
		if len(refs) != 1 {
			t.Fatalf("refs = %v, want 1 entry", refs)
		}
		if !refs[0].Ok {
			t.Error("binary file mention should be Ok (resolved)")
		}
		if refs[0].Note != "binary, skipped" {
			t.Errorf("note = %q, want %q", refs[0].Note, "binary, skipped")
		}
	})

	t.Run("not_found", func(t *testing.T) {
		_, refs := ResolveMentions("ref @nonexistent.xyz", dir)
		if len(refs) != 1 {
			t.Fatalf("refs = %v, want 1 entry", refs)
		}
		if refs[0].Ok {
			t.Error("non-existent file should not be Ok")
		}
		if refs[0].Note != "not found" {
			t.Errorf("note = %q, want %q", refs[0].Note, "not found")
		}
	})

	t.Run("dedup_duplicate_tokens", func(t *testing.T) {
		_, refs := ResolveMentions("see @main.go and @main.go again", dir)
		if len(refs) != 1 {
			t.Errorf("refs = %d entries, want 1 (dedup)", len(refs))
		}
	})

	t.Run("multiple_mentions", func(t *testing.T) {
		_, refs := ResolveMentions("see @main.go and @pkg", dir)
		if len(refs) != 2 {
			t.Errorf("refs = %d entries, want 2", len(refs))
		}
	})

	t.Run("trailing_punctuation_stripped", func(t *testing.T) {
		_, refs := ResolveMentions("look at @main.go.", dir)
		if len(refs) != 1 {
			t.Fatalf("refs = %d, want 1", len(refs))
		}
		if refs[0].Token != "main.go" {
			t.Errorf("token = %q, want %q (trailing dot stripped)", refs[0].Token, "main.go")
		}
	})
}

// TestLangForExt verifies file extension to language mapping.
func TestLangForExt(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"main.go", "go"},
		{"script.py", "python"},
		{"app.js", "javascript"},
		{"app.mjs", "javascript"},
		{"app.cjs", "javascript"},
		{"index.ts", "typescript"},
		{"main.rs", "rust"},
		{"foo.c", "c"},
		{"foo.h", "c"},
		{"bar.cpp", "cpp"},
		{"bar.cc", "cpp"},
		{"bar.hpp", "cpp"},
		{"Main.java", "java"},
		{"app.rb", "ruby"},
		{"run.sh", "bash"},
		{"run.bash", "bash"},
		{"config.json", "json"},
		{"config.yaml", "yaml"},
		{"config.yml", "yaml"},
		{"Cargo.toml", "toml"},
		{"README.md", "markdown"},
		{"index.html", "html"},
		{"style.css", "css"},
		{"query.sql", "sql"},
		{"Makefile", ""},
		{"unknown.xyz", ""},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			if got := langForExt(tc.path); got != tc.want {
				t.Errorf("langForExt(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// TestHumanSize verifies byte count formatting.
func TestHumanSize(t *testing.T) {
	cases := []struct {
		bytes int64
		want  string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1048576, "1.0 MB"},
		{1572864, "1.5 MB"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if got := HumanSize(tc.bytes); got != tc.want {
				t.Errorf("HumanSize(%d) = %q, want %q", tc.bytes, got, tc.want)
			}
		})
	}
}

// TestChipsLine verifies chip rendering for mention refs.
func TestChipsLine(t *testing.T) {
	t.Run("ok_ref", func(t *testing.T) {
		refs := []MentionRef{{Token: "main.go", Ok: true, Note: "512 B"}}
		line := ChipsLine(refs)
		if !strings.Contains(line, "📎") {
			t.Error("ok chip should contain 📎 icon")
		}
		if !strings.Contains(line, "main.go") {
			t.Error("chip line should contain token")
		}
		if !strings.Contains(line, "512 B") {
			t.Error("chip line should contain note")
		}
	})

	t.Run("failed_ref", func(t *testing.T) {
		refs := []MentionRef{{Token: "missing.go", Ok: false, Note: "not found"}}
		line := ChipsLine(refs)
		if !strings.Contains(line, "⚠") {
			t.Error("failed chip should contain ⚠ icon")
		}
	})

	t.Run("empty", func(t *testing.T) {
		line := ChipsLine(nil)
		if strings.Contains(line, "📎") || strings.Contains(line, "⚠") {
			t.Error("empty chips should not contain icons")
		}
	})
}
