package tools

// Tests for the per-chat archive index (findable archives — Block 1):
// one append-only file per chat dir recording every handed-out spill
// path with its kind and a short label, written before hand-out.

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func indexTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv("WAKIL_SESSIONS_DIR", "")
	t.Setenv("XDG_DATA_HOME", t.TempDir())
}

func readIndex(t *testing.T, chatID string) string {
	t.Helper()
	b, err := os.ReadFile(IndexPathFor(chatID))
	if err != nil {
		return ""
	}
	return string(b)
}

// TestIndexWrittenBeforeHandout pins the structural guarantee: when
// SpillToCache returns, the index already names the path. Also pins
// the line shape: "<kind>\t<abs path>\t<label>" with the kind from
// the fixed mapping table.
func TestIndexWrittenBeforeHandout(t *testing.T) {
	indexTestEnv(t)
	p := SpillToCache("chat-idx1", "dispatch_subagent", `{"objective":"x"}`)
	if p == "" {
		t.Fatal("SpillToCache returned empty path")
	}
	ix := readIndex(t, "chat-idx1")
	if !strings.Contains(ix, p) {
		t.Fatalf("index does not name the handed-out path %q; index = %q", p, ix)
	}
	lines := strings.Split(strings.Trim(ix, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("index must have exactly 1 record line; got %d: %q", len(lines), ix)
	}
	fields := strings.Split(lines[0], "\t")
	if len(fields) != 3 {
		t.Fatalf("record must be kind<TAB>path<TAB>label; got %q", lines[0])
	}
	if fields[0] != "subagent-summary" {
		t.Errorf("kind for dispatch_subagent = %q, want subagent-summary", fields[0])
	}
	if fields[1] != p {
		t.Errorf("path column = %q, want %q", fields[1], p)
	}
	if fields[2] == "" {
		t.Error("label column must not be empty")
	}
	if !strings.Contains(fields[2], "#1") {
		t.Errorf("label must carry the sequence number; got %q", fields[2])
	}
}

// TestIndexCompletenessAcrossKinds pins that every wrapper that spills
// also indexes: CapToolResult, StubToolResult, SpillFullResult and
// SpillToCache with various tool names all land in the chat's index.
func TestIndexCompletenessAcrossKinds(t *testing.T) {
	indexTestEnv(t)
	big := strings.Repeat("x", 5000)

	capped := CapToolResult(big, "search_files", "chat-idx2", 200)
	cp := ExtractSpillPath(capped)
	if cp == "" {
		t.Fatal("CapToolResult spill missing")
	}
	stubbed := StubToolResult(big, "read_file", "chat-idx2")
	sp := ExtractSpillPath(stubbed)
	if sp == "" {
		t.Fatal("StubToolResult spill missing")
	}
	full := SpillFullResult(big, "read_file_full", "chat-idx2")
	fp := ExtractSpillPath(full)
	if fp == "" {
		t.Fatal("SpillFullResult spill missing")
	}
	tp := SpillToCache("chat-idx2", "turn-1-part", "collapse archive body")
	if tp == "" {
		t.Fatal("SpillToCache returned empty")
	}
	mp := SpillToCache("chat-idx2", "some_unknown_tool", "misc body")
	if mp == "" {
		t.Fatal("SpillToCache (unknown tool) returned empty")
	}

	ix := readIndex(t, "chat-idx2")
	for _, p := range []string{cp, sp, fp, tp, mp} {
		if !strings.Contains(ix, p) {
			t.Errorf("index missing handed-out path %q; index = %q", p, ix)
		}
	}
	for _, want := range []string{"tool-result", "collapse-spill", "misc"} {
		if !strings.Contains(ix, want) {
			t.Errorf("index must carry kind %q; index = %q", want, ix)
		}
	}
}

// TestIndexLabelHygiene pins the label contract: capped at 80 runes,
// no tabs/newlines/CRs, no content beyond the first line, no tool
// arguments — a long first line and a secret-looking second line must
// not leak.
func TestIndexLabelHygiene(t *testing.T) {
	indexTestEnv(t)
	content := strings.Repeat("A", 500) + "\nSECRET-TOKEN-ABC-123\nmore"
	p := SpillToCache("chat-idx3", "search_files", content)
	if p == "" {
		t.Fatal("spill failed")
	}
	ix := readIndex(t, "chat-idx3")
	lines := strings.Split(strings.Trim(ix, "\n"), "\n")
	fields := strings.Split(lines[0], "\t")
	if len(fields) != 3 {
		t.Fatalf("record shape broken: %q", lines[0])
	}
	label := fields[2]
	if len([]rune(label)) > 83 { // 80 content runes + " #N" suffix
		t.Errorf("label runes = %d, want <= 83 (80 content runes + sequence suffix): %q", len([]rune(label)), label)
	}
	if strings.ContainsAny(label, "\t\n\r") {
		t.Errorf("label contains tab/newline/CR: %q", label)
	}
	if strings.Contains(label, "SECRET-TOKEN") {
		t.Errorf("label leaks content beyond the first line: %q", label)
	}
	if strings.Contains(label, strings.Repeat("A", 100)) {
		t.Errorf("label is not truncated: %q", label)
	}
}

// TestIndexAppendFailureIsObservable pins the failure contract: a
// failed index append (here: a symlink planted at the index path, so
// appends are refused) must not lose the spill, and must be counted.
func TestIndexAppendFailureIsObservable(t *testing.T) {
	indexTestEnv(t)
	chatDir := toolCacheDir("chat-idx4")
	if err := os.MkdirAll(chatDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "elsewhere.txt")
	if err := os.WriteFile(target, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, IndexPathFor("chat-idx4")); err != nil {
		t.Fatal(err)
	}
	before := FailureCountFor("chat-idx4")
	p := SpillToCache("chat-idx4", "search_files", "body text")
	if p == "" {
		t.Fatal("spill must succeed even when the index append is refused")
	}
	if got := FailureCountFor("chat-idx4"); got != before+1 {
		t.Errorf("failure counter = %d, want %d (append refused must be counted)", got, before+1)
	}
	// The symlink target must be untouched: no write escaped.
	b, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "precious" {
		t.Errorf("symlink target was modified: %q — the index append followed the symlink", string(b))
	}
}

// TestIndexConcurrentSpills pins append discipline under parallel
// dispatch: line count equals spill count and every listed path exists.
func TestIndexConcurrentSpills(t *testing.T) {
	indexTestEnv(t)
	const n = 24
	paths := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			paths[i] = SpillToCache("chat-idx5", "dispatch_subagent", "body")
		}()
	}
	wg.Wait()
	ix := readIndex(t, "chat-idx5")
	lines := strings.Split(strings.Trim(ix, "\n"), "\n")
	if len(lines) != n {
		t.Fatalf("index lines = %d, want %d (one per spill); index = %q", len(lines), n, ix)
	}
	for i, p := range paths {
		if p == "" {
			t.Fatalf("spill %d returned empty", i)
		}
		if !strings.Contains(ix, p) {
			t.Errorf("index missing path %q", p)
		}
		if _, err := os.Stat(p); err != nil {
			t.Errorf("indexed path %q does not exist: %v", p, err)
		}
	}
	for _, line := range lines {
		fields := strings.Split(line, "\t")
		if len(fields) != 3 || fields[0] == "" || fields[1] == "" {
			t.Errorf("malformed index line: %q", line)
		}
		if !strings.HasPrefix(fields[1], chatDirOf(t, "chat-idx5")+string(filepath.Separator)) {
			t.Errorf("path column outside the chat dir: %q", line)
		}
	}
}

func chatDirOf(t *testing.T, chatID string) string {
	t.Helper()
	d := toolCacheDir(chatID)
	if d == "" {
		t.Fatal("toolCacheDir empty")
	}
	return d
}

// TestIndexSelfFileNotIndexed: the index file must not name itself.
func TestIndexSelfFileNotIndexed(t *testing.T) {
	indexTestEnv(t)
	if p := SpillToCache("chat-idx6", "search_files", "body"); p == "" {
		t.Fatal("spill failed")
	}
	ix := readIndex(t, "chat-idx6")
	if strings.Contains(ix, IndexPathFor("chat-idx6")) {
		t.Errorf("index names itself: %q", ix)
	}
	// The index file itself must exist as a regular file.
	fi, err := os.Lstat(IndexPathFor("chat-idx6"))
	if err != nil {
		t.Fatalf("index file missing: %v", err)
	}
	if !fi.Mode().IsRegular() {
		t.Errorf("index path is not a regular file: %v", fi.Mode())
	}
}
