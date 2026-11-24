package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The resume benchmarks run on the 500-session fixture at realistic size
// (≈127 MB, generated in a temp dir). Run by hand:
//
//	go test -run '^$' -bench 'Resume' -benchtime 3x ./internal/agent/
//
// BenchmarkResumeListBaseline* split the pre-index listing path into its
// phases so the report can say what dominates: reading the files, decoding the
// JSON, deriving titles, and the scope filter.

func benchFixture(b *testing.B) resumeFixture {
	b.Helper()
	return genResumeFixture(b, 500, 1)
}

func BenchmarkResumeListBaselineReadFiles(b *testing.B) {
	fx := benchFixture(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ents, _ := os.ReadDir(fx.Dir)
		for _, e := range ents {
			if strings.HasSuffix(e.Name(), ".json") {
				if _, err := os.ReadFile(filepath.Join(fx.Dir, e.Name())); err != nil {
					b.Fatal(err)
				}
			}
		}
	}
}

func BenchmarkResumeListBaselineDecode(b *testing.B) {
	fx := benchFixture(b)
	ents, _ := os.ReadDir(fx.Dir)
	var raw [][]byte
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".json") {
			data, _ := os.ReadFile(filepath.Join(fx.Dir, e.Name()))
			raw = append(raw, data)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, data := range raw {
			var s Session
			if err := json.Unmarshal(data, &s); err != nil {
				b.Fatal(err)
			}
		}
	}
}

func BenchmarkResumeListBaselineTitles(b *testing.B) {
	fx := benchFixture(b)
	all, _ := ListSessions()
	if len(all) != fx.Count {
		b.Fatalf("listed %d, want %d", len(all), fx.Count)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, s := range all {
			SessionTurns(s)
		}
	}
}

// BenchmarkResumeListBaselineFirstScreen is the whole pre-index path the
// picker pays: the facade's ListSessions = ListSessionsScoped + ListLegacySessions.
func BenchmarkResumeListBaselineFirstScreen(b *testing.B) {
	fx := benchFixture(b)
	scope := SessionScope{Workspace: fx.Workspace, IncludeLegacy: true, LegacyWorkspace: "/nonexistent-legacy"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, _, err := ListSessionsScoped(scope)
		if err != nil || len(got) != fx.Count {
			b.Fatalf("scoped: %d sessions, err %v", len(got), err)
		}
		if _, err := ListLegacySessions(scope); err != nil {
			b.Fatal(err)
		}
	}
}

// After the index: first screen from the index alone, a warm reconcile (stat
// only), and a cold rebuild (index deleted, every file decoded once).

func BenchmarkResumeIndexOnly(b *testing.B) {
	fx := benchFixture(b)
	scope := SessionScope{Workspace: fx.Workspace, IncludeLegacy: true, LegacyWorkspace: "/nonexistent-legacy"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, _, _, err := ListResumeEntries(scope, false)
		if err != nil || len(got) != fx.Count {
			b.Fatalf("index-only: %d entries, err %v", len(got), err)
		}
	}
}

func BenchmarkResumeIndexWarmReconcile(b *testing.B) {
	fx := benchFixture(b)
	scope := SessionScope{Workspace: fx.Workspace, IncludeLegacy: true, LegacyWorkspace: "/nonexistent-legacy"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, _, _, err := ListResumeEntries(scope, true)
		if err != nil || len(got) != fx.Count {
			b.Fatalf("warm reconcile: %d entries, err %v", len(got), err)
		}
	}
}

func BenchmarkResumeIndexColdRebuild(b *testing.B) {
	fx := benchFixture(b)
	scope := SessionScope{Workspace: fx.Workspace, IncludeLegacy: true, LegacyWorkspace: "/nonexistent-legacy"}
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		_ = os.Remove(filepath.Join(fx.Dir, resumeIndexFile))
		b.StartTimer()
		got, _, _, err := ListResumeEntries(scope, true)
		if err != nil || len(got) != fx.Count {
			b.Fatalf("cold rebuild: %d entries, err %v", len(got), err)
		}
	}
}
