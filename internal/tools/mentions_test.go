package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
