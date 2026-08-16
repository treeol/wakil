package tools

import (
	"strings"
	"testing"
)

// Parser unit tests for the structured git tools (card #137). Fixtures mirror
// real `git --porcelain=v1 -z`, `git log --format=`, and `git blame --porcelain`
// output so the parsers are exercised against machine-format quirks (NUL
// framing, rename records, the branch header, 2-field blame headers).

func TestParseGitStatusBranchAndEntries(t *testing.T) {
	// Simulate: "## main...origin/main [ahead 1, behind 2]\0M  a.txt\0?? untracked.go\0"
	out := "## main...origin/main [ahead 1, behind 2]\x00M  a.txt\x00?? untracked.go\x00"
	res := ParseGitStatus(out)
	if res.Branch.Branch != "main" {
		t.Errorf("branch = %q, want main", res.Branch.Branch)
	}
	if res.Branch.Upstream != "origin/main" {
		t.Errorf("upstream = %q, want origin/main", res.Branch.Upstream)
	}
	if res.Branch.Ahead != 1 || res.Branch.Behind != 2 {
		t.Errorf("ahead/behind = %d/%d, want 1/2", res.Branch.Ahead, res.Branch.Behind)
	}
	if len(res.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(res.Entries))
	}
	if res.Entries[0].XY != "M " || res.Entries[0].Path != "a.txt" {
		t.Errorf("entry[0] = %+v", res.Entries[0])
	}
	if res.Entries[1].XY != "??" || res.Entries[1].Path != "untracked.go" {
		t.Errorf("entry[1] = %+v", res.Entries[1])
	}
}

func TestParseGitStatusRename(t *testing.T) {
	// Rename: "R  new.go\0old.go\0"
	out := "## master\x00R  new.go\x00old.go\x00"
	res := ParseGitStatus(out)
	if len(res.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(res.Entries))
	}
	e := res.Entries[0]
	if e.XY != "R " || e.Path != "new.go" || e.OrigPath != "old.go" {
		t.Errorf("rename entry = %+v", e)
	}
}

func TestParseGitStatusDetachedAndUnborn(t *testing.T) {
	det := ParseGitStatus("## HEAD (no branch)\x00")
	if det.Branch.Branch != "HEAD" {
		t.Errorf("detached branch = %q, want HEAD", det.Branch.Branch)
	}
	unborn := ParseGitStatus("## No commits yet on main\x00")
	if unborn.Branch.Branch != "main" {
		t.Errorf("unborn branch = %q, want main", unborn.Branch.Branch)
	}
}

func TestParseGitStatusEmpty(t *testing.T) {
	res := ParseGitStatus("")
	if len(res.Entries) != 0 {
		t.Errorf("entries = %d, want 0", len(res.Entries))
	}
}

func TestParseGitLog(t *testing.T) {
	// NUL-framed fields, RS-framed records (the real format).
	out := strings.Join([]string{
		"abc123\x00def456\x00Alice\x00a@x\x002026-01-01T00:00:00+00:00\x00fix: thing\x1e",
		"def456\x00\x00Bob\x00b@x\x002025-12-31T00:00:00+00:00\x00root commit\x1e",
	}, "")
	entries := ParseGitLog(out)
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	if entries[0].Hash != "abc123" || entries[0].Author != "Alice" || entries[0].Subject != "fix: thing" {
		t.Errorf("entry[0] = %+v", entries[0])
	}
	if entries[1].Parents != "" {
		t.Errorf("root commit parents = %q, want empty", entries[1].Parents)
	}
}

func TestParseGitLogSurvivesQuotesAndNewlines(t *testing.T) {
	// The NUL framing makes a subject with quotes/backslashes collision-proof
	// (this is the regression the old JSON-line format failed: %s is NOT
	// JSON-escaped by git, so a quoted subject could break the line or spoof a
	// hash key).
	out := `abc123` + "\x00" + "\x00" + `A"uthor` + "\x00" + `a@x` + "\x00" + `2026-01-01T00:00:00+00:00` + "\x00" + `fix "quoted" \ path` + "\x1e"
	entries := ParseGitLog(out)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0].Subject != `fix "quoted" \ path` {
		t.Errorf("subject = %q, want the quoted subject intact", entries[0].Subject)
	}
	if entries[0].Hash != "abc123" {
		t.Errorf("hash = %q, want abc123", entries[0].Hash)
	}
}

func TestParseGitLogToleratesGarbage(t *testing.T) {
	// A trailing incomplete record (no RS terminator) or a too-few-fields
	// record must be skipped, not error.
	out := "short\x00record\x1e" + "abc123\x00\x00A\x00a@x\x002026-01-01T00:00:00+00:00\x00ok\x1e"
	entries := ParseGitLog(out)
	if len(entries) != 1 || entries[0].Hash != "abc123" {
		t.Errorf("entries = %+v, want just the abc123 commit", entries)
	}
}

func TestParseGitBlame(t *testing.T) {
	// Two commits: first line from hash A, next two from hash B. Continuation
	// headers use the 2-field form (no num-lines).
	out := strings.Join([]string{
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa 1 1 1",
		"author Alice",
		"author-time 1000000000",
		"summary first",
		"\tline one",
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb 2 2 2",
		"author Bob",
		"author-time 2000000000",
		"summary second",
		"\tline two",
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb 3 3",
		"\tline three",
	}, "\n")
	hunks := ParseGitBlame(out)
	if len(hunks) != 2 {
		t.Fatalf("hunks = %d, want 2: %+v", len(hunks), hunks)
	}
	if hunks[0].Hash != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || hunks[0].Author != "Alice" || hunks[0].StartLine != 1 || hunks[0].NumLines != 1 {
		t.Errorf("hunk[0] = %+v", hunks[0])
	}
	if hunks[1].Hash != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" || hunks[1].Author != "Bob" || hunks[1].StartLine != 2 || hunks[1].NumLines != 2 {
		t.Errorf("hunk[1] = %+v", hunks[1])
	}
}

func TestParseGitBlameNonContiguousSameCommit(t *testing.T) {
	// Commit A owns lines 1 and 3 (non-adjacent); metadata is emitted only the
	// first time A appears. The second hunk for A must still carry the author.
	out := strings.Join([]string{
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa 1 1 1",
		"author Alice",
		"summary first",
		"\tline one (A)",
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb 2 2 1",
		"author Bob",
		"summary second",
		"\tline two (B)",
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa 3 3", // A again, no metadata
		"\tline three (A)",
	}, "\n")
	hunks := ParseGitBlame(out)
	if len(hunks) != 3 {
		t.Fatalf("hunks = %d, want 3: %+v", len(hunks), hunks)
	}
	if hunks[0].Author != "Alice" {
		t.Errorf("hunk[0] author = %q, want Alice", hunks[0].Author)
	}
	if hunks[2].Author != "Alice" {
		t.Errorf("hunk[2] author = %q, want Alice (metadata restored from cache)", hunks[2].Author)
	}
}

func TestGitLogFormatUsesNulFraming(t *testing.T) {
	// The format template must use NUL/RS framing (not JSON-line) — the
	// regression guard against the log spoofing bug.
	if !strings.Contains(GitLogFormat(), "%x00") || !strings.Contains(GitLogFormat(), "%x1e") {
		t.Errorf("GitLogFormat must be NUL/RS-framed, got: %q", GitLogFormat())
	}
	if strings.Contains(GitLogFormat(), "{") {
		t.Errorf("GitLogFormat must not emit inline JSON: %q", GitLogFormat())
	}
}
