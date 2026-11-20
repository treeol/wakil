package agent

import (
	"bufio"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// readonly_corpus_test.go — regression corpus for the shell classifier.
//
// testdata/shell_classify_v085.jsonl records, for 1125 generated command
// lines, how the v0.8.5 classifier answered (v085_ro, v085_d). The corpus was
// generated from a pristine v0.8.5 checkout; it is data, not a regenerated
// fixture. The test fails when a command that v0.8.5 called destructive is
// read-only or not destructive now, unless it is one of the intended
// relaxations in intendedRelaxation below.

type corpusRow struct {
	Cmd    string `json:"cmd"`
	V085RO bool   `json:"v085_ro"`
	V085D  bool   `json:"v085_d"`
}

func loadCorpus(t *testing.T) []corpusRow {
	t.Helper()
	f, err := os.Open("testdata/shell_classify_v085.jsonl")
	if err != nil {
		t.Fatalf("open corpus: %v", err)
	}
	defer f.Close()
	var rows []corpusRow
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r corpusRow
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("corpus line: %v", err)
		}
		rows = append(rows, r)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	if len(rows) < 300 {
		t.Fatalf("corpus has %d rows, want at least 300", len(rows))
	}
	return rows
}

// The intended relaxations, in the order the task lists them. Each is a
// narrow predicate, not a pattern on the whole command, so it cannot bless
// an unrelated write.
var (
	reDevNullRedirect = regexp.MustCompile(`(^|[\s;&|(])(&|[0-9]*|2>>)?>{1,2}\s*/dev/null($|[\s;|&)])`)
	reFDDup           = regexp.MustCompile(`(^|[\s;&|(])[0-9]*>&([0-9]+|-)($|[\s;|&)])`)
)

// intendedRelaxation reports whether a command that v0.8.5 called destructive
// may now be read-only or non-destructive, and which relaxation covers it.
// Only the listed relaxations count. A "$(...)" or a loop is NOT a blanket
// excuse: those are covered only when the classifier itself judges the parts
// read-only, which the regression test checks separately.
func intendedRelaxation(cmd string) (string, bool) {
	// Only the relaxation token itself may be excused. Remove the one matched
	// token, then the rest of the command must contain no further redirect
	// that is not itself a listed relaxation.
	if strings.Contains(cmd, ">>&") {
		return "", false
	}
	if loc := reFDDup.FindStringIndex(cmd); loc != nil {
		rest := cmd[:loc[0]] + " " + cmd[loc[1]:]
		if !hasOtherRedirect(rest) {
			return "fd duplication", true
		}
	}
	if loc := reDevNullRedirect.FindStringIndex(cmd); loc != nil {
		rest := cmd[:loc[0]] + " " + cmd[loc[1]:]
		if !hasOtherRedirect(rest) {
			return "redirect to /dev/null", true
		}
	}
	return "", false
}

// hasOtherRedirect reports whether s still contains a '>' or '<' redirect
// after the relaxation token has been removed.
func hasOtherRedirect(s string) bool {
	return strings.ContainsAny(s, "<>")
}

// innerSubstitutionsReadOnly reports whether every "$(...)" in cmd has an
// inner command the classifier itself calls read-only. It is the check behind
// the "$(...) whose inner and outer are both read-only" relaxation.
func innerSubstitutionsReadOnly(cmd string) bool {
	_, inners, ok := substituteDollarParen(cmd)
	if !ok || len(inners) == 0 {
		return false
	}
	for _, in := range inners {
		if !IsReadOnlyShell(in) {
			return false
		}
	}
	return true
}

// TestShellClassifierCorpusNoRegression fails when a v0.8.5-destructive
// command is read-only or non-destructive now, outside the intended
// relaxations. The read-only relaxation for "$(...)" and for complete
// one-line loops is checked by the classifier's own inner and outer verdicts,
// so those commands are not excused here.
func TestShellClassifierCorpusNoRegression(t *testing.T) {
	for _, r := range loadCorpus(t) {
		ro := IsReadOnlyShell(r.Cmd)
		d := IsDestructiveShell(r.Cmd)
		if ro && d {
			t.Errorf("%q is both read-only and destructive", r.Cmd)
		}
		if !r.V085D {
			continue
		}
		// v0.8.5 destructive: it may only lose the destructive flag or become
		// read-only through a listed relaxation. A "$(...)" that is read-only
		// now must have every inner command read-only too: that is the
		// relaxation, checked by the classifier, not by the string.
		if ro || !d {
			if _, ok := intendedRelaxation(r.Cmd); ok {
				continue
			}
			if ro && innerSubstitutionsReadOnly(r.Cmd) {
				continue
			}
			t.Errorf("%q: v0.8.5 destructive; now ro=%v d=%v (no intended relaxation)", r.Cmd, ro, d)
		}
	}
}
