package agent

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/treeol/wakil/internal/proxy"
)

// maxFindingsBytes caps the "Findings so far" part of a collapse checkpoint.
// The archive line is the one exception: it is never cut.
const maxFindingsBytes = 1500

// findingsSaidBytes caps one of the agent's own notes in the findings part.
const findingsSaidBytes = 160

// findingsHeader opens the part; a second collapse recomputes the part from
// the raw conversation, so exactly one such header is ever in a checkpoint.
const findingsHeader = "Findings so far:\n"

// execFailRe matches the structural prefix of a failed shell result
// ("ERROR: exit status N", see formatResult and the background path). Only
// the captured integer is ever used; nothing after the prefix is read.
var execFailRe = regexp.MustCompile(`^ERROR: exit status ([0-9]{1,3})(?:\s|$)`)

// findingEntry is one line item of the findings part. seq orders entries by
// age across categories (higher = newer): the oldest are cut first.
type findingEntry struct {
	cat  int // findCat*
	seq  int
	text string
}

const (
	findCatRead = iota
	findCatWrote
	findCatFailed
	findCatSaid
	findCatCount
)

// readRange is a requested line range of one file read; hi == math.MaxInt is
// "to the end of the file".
type readRange struct{ lo, hi int }

// buildFindings renders the "Findings so far" part of a collapse checkpoint
// for the range w (the messages being archived) and the archive path. It is
// deterministic: the same range gives the same bytes. It reads only what the
// agent itself wrote (tool-call arguments, assistant text) plus the integer
// of a "ERROR: exit status N" result prefix. It never copies tool-result
// content, file contents or environment values: tool output can hold secrets
// and this text stays in the context and on disk. Assistant text can quote
// tool output; that is the agent's own wording, cut to findingsSaidBytes.
//
// It returns "" when the range yields nothing (the note's "full record:" line
// already points at the archive in that case).
func buildFindings(w []proxy.Message, archive string) string {
	results := map[string]string{} // tool_call_id -> result text (prefix use only)
	for _, m := range w {
		if m.Role == "tool" && m.ToolCallID != "" && m.Content != nil {
			if _, dup := results[m.ToolCallID]; !dup {
				results[m.ToolCallID] = *m.Content
			}
		}
	}
	// resultOK: the call's result exists and is neither an error nor a decline.
	resultOK := func(id string) bool {
		r, ok := results[id]
		if !ok {
			return false
		}
		return !strings.HasPrefix(r, "ERROR") && !strings.HasPrefix(strings.TrimSpace(r), "[declined")
	}

	reads := map[string][]readRange{}
	readSeq := map[string]int{}
	wrote := map[string]int{} // path -> seq
	var failed, said []findingEntry
	seq := 0
	for _, m := range w {
		if m.Role != "assistant" {
			continue
		}
		seq++
		if m.Content != nil {
			if t := foldFindingText(stripPruneTags(*m.Content), findingsSaidBytes); t != "" {
				said = append(said, findingEntry{cat: findCatSaid, seq: seq, text: t})
			}
		}
		for _, tc := range m.ToolCalls {
			if IsAutoRoundCallID(tc.ID) {
				continue
			}
			seq++
			name := tc.Function.Name
			switch {
			case name == "read_file" || name == "read_file_full":
				if !resultOK(tc.ID) {
					continue
				}
				p, rng, ok := parseReadCall(name, tc.Function.Arguments)
				if !ok {
					continue
				}
				reads[p] = append(reads[p], rng)
				readSeq[p] = seq
			case writeTargetTools[name]:
				if !resultOK(tc.ID) {
					continue
				}
				for _, p := range extractPaths(tc.Function.Arguments) {
					p = cleanFindingPath(p)
					if p == "" {
						continue
					}
					wrote[p] = seq
					// A later write changes the file: what was read before it
					// is no longer what is on disk.
					delete(reads, p)
					delete(readSeq, p)
				}
			case name == "run_shell":
				r, ok := results[tc.ID]
				if !ok {
					continue
				}
				mm := execFailRe.FindStringSubmatch(r)
				if mm == nil {
					continue
				}
				var args struct {
					Command string `json:"command"`
				}
				_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
				failed = append(failed, findingEntry{cat: findCatFailed, seq: seq,
					text: fmt.Sprintf("%s (exit %s)", commandWord(args.Command), mm[1])})
			}
		}
	}

	var entries []findingEntry
	paths := make([]string, 0, len(reads))
	for p := range reads {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		entries = append(entries, findingEntry{cat: findCatRead, seq: readSeq[p], text: p + formatRanges(mergeRanges(reads[p]))})
	}
	wpaths := make([]string, 0, len(wrote))
	for p := range wrote {
		wpaths = append(wpaths, p)
	}
	sort.Strings(wpaths)
	for _, p := range wpaths {
		entries = append(entries, findingEntry{cat: findCatWrote, seq: wrote[p], text: p})
	}
	// Failed and said: newest first.
	sort.SliceStable(failed, func(i, j int) bool { return failed[i].seq > failed[j].seq })
	sort.SliceStable(said, func(i, j int) bool { return said[i].seq > said[j].seq })
	entries = append(entries, failed...)
	entries = append(entries, said...)
	if len(entries) == 0 {
		return ""
	}
	return fitFindings(entries, archive)
}

// fitFindings renders entries under maxFindingsBytes: while the part is too
// long the oldest entry (smallest seq) of any category is cut, and the line of
// its category shows a "(n more)" marker. The archive line is never cut.
func fitFindings(entries []findingEntry, archive string) string {
	hidden := [findCatCount]int{}
	for {
		out := renderFindings(entries, hidden, archive)
		if len(out) <= maxFindingsBytes {
			return out
		}
		if len(entries) == 0 {
			// Even the markers do not fit (a very long archive path): the
			// archive line alone, never cut.
			return renderFindings(nil, [findCatCount]int{}, archive)
		}
		oldest := 0
		for i, e := range entries {
			if e.seq < entries[oldest].seq {
				oldest = i
			}
		}
		hidden[entries[oldest].cat]++
		entries = append(entries[:oldest:oldest], entries[oldest+1:]...)
	}
}

func renderFindings(entries []findingEntry, hidden [findCatCount]int, archive string) string {
	var b strings.Builder
	b.WriteString(findingsHeader)
	labels := [findCatCount]string{"read (requested ranges)", "written or edited", "failed commands (newest first)", "my last notes (newest first)"}
	for cat := 0; cat < findCatCount; cat++ {
		var texts []string
		for _, e := range entries {
			if e.cat == cat {
				texts = append(texts, e.text)
			}
		}
		if len(texts) == 0 && hidden[cat] == 0 {
			continue
		}
		sep := "; "
		if cat == findCatSaid {
			sep = " | "
		}
		b.WriteString(labels[cat])
		b.WriteString(": ")
		b.WriteString(strings.Join(texts, sep))
		if hidden[cat] > 0 {
			if len(texts) > 0 {
				b.WriteString(" ")
			}
			fmt.Fprintf(&b, "(%d more)", hidden[cat])
		}
		b.WriteString("\n")
	}
	pj, _ := json.Marshal(archive)
	fmt.Fprintf(&b, "archive: %s — search it with search_files {\"pattern\":\"<term>\",\"path\":%s}\n", archive, pj)
	return b.String()
}

// parseReadCall extracts the path and the requested line range of a
// read_file / read_file_full call from its arguments.
func parseReadCall(name, args string) (string, readRange, bool) {
	var a struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if json.Unmarshal([]byte(args), &a) != nil {
		return "", readRange{}, false
	}
	p := cleanFindingPath(a.Path)
	if p == "" {
		return "", readRange{}, false
	}
	if name == "read_file_full" || (a.Offset <= 0 && a.Limit <= 0) {
		return p, readRange{lo: 1, hi: math.MaxInt}, true
	}
	lo := a.Offset
	if lo < 1 {
		lo = 1
	}
	hi := math.MaxInt
	if a.Limit > 0 && lo <= math.MaxInt-a.Limit {
		hi = lo + a.Limit - 1
	}
	return p, readRange{lo: lo, hi: hi}, true
}

// mergeRanges sorts ranges and merges overlapping or adjacent ones.
func mergeRanges(rs []readRange) []readRange {
	out := append([]readRange(nil), rs...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].lo != out[j].lo {
			return out[i].lo < out[j].lo
		}
		return out[i].hi < out[j].hi
	})
	merged := out[:0]
	for _, r := range out {
		if n := len(merged); n > 0 && (merged[n-1].hi == math.MaxInt || r.lo <= merged[n-1].hi+1) {
			if r.hi > merged[n-1].hi {
				merged[n-1].hi = r.hi
			}
			continue
		}
		merged = append(merged, r)
	}
	return merged
}

// formatRanges renders merged ranges as ":10-59,80-" or "" for a whole file.
func formatRanges(rs []readRange) string {
	if len(rs) == 1 && rs[0].lo == 1 && rs[0].hi == math.MaxInt {
		return ""
	}
	parts := make([]string, 0, len(rs))
	for _, r := range rs {
		if r.hi == math.MaxInt {
			parts = append(parts, strconv.Itoa(r.lo)+"-")
		} else {
			parts = append(parts, strconv.Itoa(r.lo)+"-"+strconv.Itoa(r.hi))
		}
	}
	return ":" + strings.Join(parts, ",")
}

// cleanFindingPath keeps a path printable on one line, at most 200 bytes.
func cleanFindingPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == ';' || r == '|' {
			return '?'
		}
		return r
	}, p)
	return truncateUTF8(p, 200)
}

// foldFindingText folds whitespace to single spaces and cuts to n bytes
// (UTF-8 safe, with an ellipsis).
func foldFindingText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return ""
	}
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	return truncateBytes(s, n)
}

// commandWord returns the name of the command a shell line runs: leading
// "VAR=value" words and "cd DIR &&" prefixes are skipped, the word is reduced
// to its base name, and only [A-Za-z0-9._+-] up to 40 bytes is kept ("?"
// otherwise). The arguments are never returned: they may hold secrets.
func commandWord(cmd string) string {
	fields := strings.Fields(cmd)
	i := 0
	for i < len(fields) {
		f := fields[i]
		switch {
		case isAssignmentWord(f):
			// A value with a quote, a backslash, a backtick or a "$" can hold
			// spaces that Fields split on, which would make a piece of the
			// value look like the command word: no command word then.
			if strings.ContainsAny(f, "'\"\\`$") {
				return "?"
			}
			i++ // VAR=value prefix
		case f == "cd":
			// Skip "cd [-options] DIR" up to the && or ; that follows it; no
			// such separator means no command word (the directory is an
			// argument and is never returned).
			i++
			for i < len(fields) && fields[i] != "&&" && fields[i] != ";" {
				i++
			}
			if i >= len(fields) {
				return "?"
			}
			i++
		default:
			goto found
		}
	}
	return "?"
found:
	w := fields[i]
	if j := strings.LastIndex(w, "/"); j >= 0 {
		w = w[j+1:]
	}
	if w == "" || len(w) > 40 {
		return "?"
	}
	for _, r := range w {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._+-", r)) {
			return "?"
		}
	}
	return w
}

// isAssignmentWord reports whether w is NAME=value with a shell variable name.
func isAssignmentWord(w string) bool {
	k := strings.Index(w, "=")
	if k <= 0 {
		return false
	}
	for j, r := range w[:k] {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || j > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// withFindings appends the findings part to a note, making sure a newline
// separates them.
func withFindings(note, findings string) string {
	if findings != "" && note != "" && !strings.HasSuffix(note, "\n") {
		note += "\n"
	}
	return note + findings
}
