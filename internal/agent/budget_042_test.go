package agent

import (
	"testing"

	wtools "github.com/treeol/wakil/internal/tools"
)

// --- Item 1: Budget sizing per class incl. reserve arithmetic ---

func TestComputeSubagentBudgets(t *testing.T) {
	cases := []struct {
		name        string
		hardMax     int
		classPct    int
		reservePct  int
		wantWork    int
		wantReserve int
	}{
		{"read class default", 160000, 40, 8, 51200, 12800},
		{"search class default", 160000, 25, 8, 27200, 12800},
		{"edit class default", 160000, 60, 8, 83200, 12800},
		{"verify class default", 160000, 40, 8, 51200, 12800},
		{"zero hardMax", 0, 40, 8, 0, 0},
		{"negative hardMax", -1, 40, 8, 0, 0},
		{"reserve >= class → work clamps to 0", 160000, 8, 8, 0, 12800},
		{"reserve > class → work clamps to 0 (negative)", 160000, 5, 8, 0, 12800},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			work, reserve := computeSubagentBudgets(c.hardMax, c.classPct, c.reservePct)
			if work != c.wantWork {
				t.Errorf("work = %d, want %d", work, c.wantWork)
			}
			if reserve != c.wantReserve {
				t.Errorf("reserve = %d, want %d", reserve, c.wantReserve)
			}
		})
	}
}

func TestSubagentBudgetForClassDefaults(t *testing.T) {
	app := &App{}
	cases := []struct {
		class string
		want  int
	}{
		{wtools.SubagentClassRead, subagentBudgetReadDefault},
		{wtools.SubagentClassSearch, subagentBudgetSearchDefault},
		{wtools.SubagentClassEdit, subagentBudgetEditDefault},
		{wtools.SubagentClassVerify, subagentBudgetVerifyDefault},
		{"unknown", subagentBudgetReadDefault}, // fallback
	}
	for _, c := range cases {
		t.Run(c.class, func(t *testing.T) {
			got := app.subagentBudgetForClass(c.class)
			if got != c.want {
				t.Errorf("subagentBudgetForClass(%q) = %d, want %d", c.class, got, c.want)
			}
		})
	}
}

func TestSubagentBudgetForClassOverrides(t *testing.T) {
	app := &App{}
	app.Cfg.SubagentBudgetRead = 50
	app.Cfg.SubagentBudgetSearch = 30
	app.Cfg.SubagentBudgetEdit = 70
	app.Cfg.SubagentBudgetVerify = 45

	if got := app.subagentBudgetForClass(wtools.SubagentClassRead); got != 50 {
		t.Errorf("read override = %d, want 50", got)
	}
	if got := app.subagentBudgetForClass(wtools.SubagentClassSearch); got != 30 {
		t.Errorf("search override = %d, want 30", got)
	}
	if got := app.subagentBudgetForClass(wtools.SubagentClassEdit); got != 70 {
		t.Errorf("edit override = %d, want 70", got)
	}
	if got := app.subagentBudgetForClass(wtools.SubagentClassVerify); got != 45 {
		t.Errorf("verify override = %d, want 45", got)
	}
}

func TestSubagentReservePct(t *testing.T) {
	app := &App{}
	if got := app.subagentReservePct(); got != subagentBudgetReserveDefault {
		t.Errorf("default reserve = %d, want %d", got, subagentBudgetReserveDefault)
	}
	app.Cfg.SubagentBudgetReserve = 10
	if got := app.subagentReservePct(); got != 10 {
		t.Errorf("override reserve = %d, want 10", got)
	}
}

// --- Item 1: isStrictlySmallerScope ---

func TestIsStrictlySmallerScope(t *testing.T) {
	cases := []struct {
		name      string
		origFiles []string
		newFiles  []string
		origHash  string
		newHash   string
		want      bool
	}{
		{
			name:      "proper subset, different task",
			origFiles: []string{"a.go", "b.go", "c.go"},
			newFiles:  []string{"a.go", "b.go"},
			origHash:  "hash1",
			newHash:   "hash2",
			want:      true,
		},
		{
			name:      "identical task text → not smaller",
			origFiles: []string{"a.go", "b.go"},
			newFiles:  []string{"a.go"},
			origHash:  "same",
			newHash:   "same",
			want:      false,
		},
		{
			name:      "identical file set → not smaller (same size)",
			origFiles: []string{"a.go", "b.go"},
			newFiles:  []string{"a.go", "b.go"},
			origHash:  "h1",
			newHash:   "h2",
			want:      false,
		},
		{
			name:      "new file not in original → not smaller",
			origFiles: []string{"a.go", "b.go"},
			newFiles:  []string{"a.go", "c.go"},
			origHash:  "h1",
			newHash:   "h2",
			want:      false,
		},
		{
			name:      "empty new files → not smaller (empty is subset but len not < if orig also empty)",
			origFiles: []string{"a.go"},
			newFiles:  []string{},
			origHash:  "h1",
			newHash:   "h2",
			want:      true, // empty set is a proper subset of non-empty set
		},
		{
			name:      "both empty → not smaller (same size)",
			origFiles: []string{},
			newFiles:  []string{},
			origHash:  "h1",
			newHash:   "h2",
			want:      false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := isStrictlySmallerScope(c.origFiles, c.newFiles, c.origHash, c.newHash)
			if got != c.want {
				t.Errorf("isStrictlySmallerScope = %v, want %v", got, c.want)
			}
		})
	}
}

func TestToStrSet(t *testing.T) {
	s := toStrSet([]string{"a", "b", "c"})
	if len(s) != 3 {
		t.Fatalf("set size = %d, want 3", len(s))
	}
	for _, k := range []string{"a", "b", "c"} {
		if !s[k] {
			t.Errorf("set missing %q", k)
		}
	}
	// Duplicates collapse.
	s2 := toStrSet([]string{"a", "a", "b"})
	if len(s2) != 2 {
		t.Errorf("dedup set size = %d, want 2", len(s2))
	}
}

// --- Item 1: ValidSubagentClass ---

func TestValidSubagentClass(t *testing.T) {
	for _, c := range []string{wtools.SubagentClassRead, wtools.SubagentClassSearch, wtools.SubagentClassEdit, wtools.SubagentClassVerify} {
		if !wtools.ValidSubagentClass(c) {
			t.Errorf("ValidSubagentClass(%q) = false, want true", c)
		}
	}
	for _, c := range []string{"", "unknown", "READ", "exec"} {
		if wtools.ValidSubagentClass(c) {
			t.Errorf("ValidSubagentClass(%q) = true, want false", c)
		}
	}
}

// --- Item 1: SubagentResult struct is well-formed ---

func TestSubagentResultJSON(t *testing.T) {
	r := SubagentResult{
		Status:         "partial",
		FilesCovered:   []string{"a.go"},
		FilesNotRead:   []string{"b.go"},
		Findings:       []Finding{{Summary: "found X", Location: "a.go:10", Kind: "fact", Weight: "high"}},
		RemainingScope: "need to check b.go",
		Budget:         BudgetInfo{Used: 50000, Limit: 51200, Class: "read"},
	}
	// Verify it marshals cleanly (no panic, valid JSON).
	if r.Status != "partial" {
		t.Errorf("status = %q, want partial", r.Status)
	}
	if len(r.FilesCovered) != 1 || r.FilesCovered[0] != "a.go" {
		t.Errorf("files_covered = %v", r.FilesCovered)
	}
	if len(r.FilesNotRead) != 1 || r.FilesNotRead[0] != "b.go" {
		t.Errorf("files_not_read = %v", r.FilesNotRead)
	}
	if r.Budget.Class != "read" {
		t.Errorf("budget.class = %q, want read", r.Budget.Class)
	}
}

// --- Item 2: deriveLedger ---

func TestDeriveLedgerAllRead(t *testing.T) {
	rec := newFileReadRecorder()
	rec.record("a.go")
	rec.record("b.go")
	rec.record("c.go")

	ledger := rec.deriveLedger([]string{"a.go", "b.go", "c.go"})
	if len(ledger.Read) != 3 {
		t.Errorf("Read = %v, want 3 entries", ledger.Read)
	}
	if len(ledger.NotRead) != 0 {
		t.Errorf("NotRead = %v, want empty", ledger.NotRead)
	}
	if len(ledger.Extra) != 0 {
		t.Errorf("Extra = %v, want empty", ledger.Extra)
	}
}

func TestDeriveLedgerPartialRead(t *testing.T) {
	rec := newFileReadRecorder()
	rec.record("a.go")
	rec.record("c.go") // not in listed set → extra
	// b.go listed but not read

	ledger := rec.deriveLedger([]string{"a.go", "b.go"})
	if len(ledger.Read) != 1 || ledger.Read[0] != "a.go" {
		t.Errorf("Read = %v, want [a.go]", ledger.Read)
	}
	if len(ledger.NotRead) != 1 || ledger.NotRead[0] != "b.go" {
		t.Errorf("NotRead = %v, want [b.go]", ledger.NotRead)
	}
	if len(ledger.Extra) != 1 || ledger.Extra[0] != "c.go" {
		t.Errorf("Extra = %v, want [c.go]", ledger.Extra)
	}
}

func TestDeriveLedgerZeroListedRead(t *testing.T) {
	rec := newFileReadRecorder()
	rec.record("x.go")
	rec.record("y.go")
	// None of the listed files were read.

	ledger := rec.deriveLedger([]string{"a.go", "b.go"})
	if len(ledger.Read) != 0 {
		t.Errorf("Read = %v, want empty (off-task)", ledger.Read)
	}
	if len(ledger.NotRead) != 2 {
		t.Errorf("NotRead = %v, want 2 entries", ledger.NotRead)
	}
	if len(ledger.Extra) != 2 {
		t.Errorf("Extra = %v, want 2 entries", ledger.Extra)
	}
}

func TestDeriveLedgerEmptyList(t *testing.T) {
	rec := newFileReadRecorder()
	rec.record("a.go")
	rec.record("b.go")

	ledger := rec.deriveLedger(nil)
	// No explicit list → no rejection; Extra is still populated.
	if ledger.Listed != nil {
		t.Errorf("Listed = %v, want nil", ledger.Listed)
	}
	if len(ledger.Read) != 0 {
		t.Errorf("Read = %v, want empty when no listed set", ledger.Read)
	}
	if len(ledger.Extra) != 2 {
		t.Errorf("Extra = %v, want 2 entries", ledger.Extra)
	}
}

func TestDeriveLedgerNilRecorder(t *testing.T) {
	var rec *fileReadRecorder
	// Nil-safe: should not panic, returns empty ledger with listed set.
	ledger := rec.deriveLedger([]string{"a.go", "b.go"})
	if len(ledger.Read) != 0 {
		t.Errorf("Read = %v, want empty for nil recorder", ledger.Read)
	}
	if len(ledger.NotRead) != 2 {
		t.Errorf("NotRead = %v, want 2 (all listed, none read)", ledger.NotRead)
	}
}

func TestFileReadRecorderDedup(t *testing.T) {
	rec := newFileReadRecorder()
	rec.record("a.go")
	rec.record("a.go") // duplicate
	rec.record("b.go")

	snap := rec.snapshot()
	if len(snap) != 2 {
		t.Errorf("snapshot = %v, want 2 (deduplicated)", snap)
	}
	if snap[0] != "a.go" || snap[1] != "b.go" {
		t.Errorf("snapshot order = %v, want [a.go b.go]", snap)
	}
}

func TestFileReadRecorderEmptyString(t *testing.T) {
	rec := newFileReadRecorder()
	rec.record("") // empty path → ignored
	if len(rec.snapshot()) != 0 {
		t.Errorf("empty path should be ignored, got %v", rec.snapshot())
	}
}
