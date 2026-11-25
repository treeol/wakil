// Command gen generates the collapse-long-v3 fixture: 40 prose files of
// about 60 KB each, each with a dense ledger block past the ToolResultCap
// cut plus number-laden filler decoys around it. The seed is fixed, so the
// fixture is reproducible. It lives outside the task's repo/ so neither the
// generator nor the seed ever reaches the work folder or its git history;
// setup.sh runs it with the output directory as argument.
//
// v3 fixes the v2 defects (plan §12.3): every ledger entry line carries a
// visible per-file 1-based identifier ("entry 7 of orchard"); a withdrawal
// line names the identifier it amends, binds within its own file, and each
// entry has at most one withdrawal; the sentence templates never prefix
// "the " to an alias that already starts with "the " (v2 produced "The
// the", "the the miller"); no ledger line is emitted twice. The answer key
// (expected per-category sums) is written ONLY to the path given by the
// second argument (setup.sh passes a path in the task dir, outside the
// workdir), never next to the generated docs.
//
// The generator self-check (runSelfCheck) runs on every generation and
// fails it on: a ledger line appearing twice in a file, a doubled word in
// a ledger line, two withdrawals naming one entry, or an identifier that
// does not resolve within its own file.
//
// Determinism discipline: no map iteration in output paths (all selections
// come from slices in fixed order via the seeded RNG); two generations with
// the same seed are byte-identical (verified by the self-test).
package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var categories = []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta"}

var numberWords = []struct {
	n int
	w string
}{
	{1, "one"}, {2, "two"}, {3, "three"}, {4, "four"}, {5, "five"},
	{6, "six"}, {7, "seven"}, {8, "eight"}, {9, "nine"}, {10, "ten"},
	{11, "eleven"}, {12, "twelve"}, {13, "thirteen"}, {14, "fourteen"}, {15, "fifteen"},
	{16, "sixteen"}, {17, "seventeen"}, {18, "eighteen"}, {19, "nineteen"}, {20, "twenty"},
	{21, "twenty-one"}, {22, "twenty-two"}, {23, "twenty-three"}, {24, "twenty-four"}, {25, "twenty-five"},
	{26, "twenty-six"}, {27, "twenty-seven"}, {28, "twenty-eight"}, {29, "twenty-nine"}, {30, "thirty"},
}

// Aliases per entity: each entity has a base name and two aliases, so no
// single name match finds every instance. Aliases may start with "the ";
// the sentence templates never prefix "the " to an alias (the v2 doubling
// bug), they place the alias directly or use the base name.
var aliases = map[string][]string{
	"harbour":  {"the harbourmaster", "the port-keeper", "the quay ledger"},
	"ferry":    {"the ferry operator", "the ferryman", "the crossing house"},
	"kiln":     {"the kiln-house keeper", "the burner", "the firing shed"},
	"mill":     {"the miller", "the grinding shop", "the wheel-house"},
	"bakery":   {"the baker", "the bakehouse", "the oven room"},
	"market":   {"the stall-holder", "the market office", "the toll clerk"},
	"chapel":   {"the chapelwarden", "the vestry", "the small chapel"},
	"smithy":   {"the smith", "the forge", "the anvil bench"},
	"orchard":  {"the ortsman", "the cider press", "the orchard house"},
	"granary":  {"the granary guard", "the grain store", "the barn loft"},
}

var entities = []string{"harbour", "ferry", "kiln", "mill", "bakery", "market", "chapel", "smithy", "orchard", "granary"}

var fillers = []string{
	"The harbour notice warned that the tide tables for the next fortnight would be posted near the customs house, with about twenty entries each season.",
	"Paragraph after paragraph the clerkess summarised the parish registers, noting every baptism and burial, thirty in some years and a dozen in others.",
	"Nobody in the room could remember a winter quite so severe, when the river froze for roughly five weeks and the carts crossed with their loads.",
	"A small brass key, green with age, opened a drawer in which the town seal had been kept; there were always about twelve keys in that drawer across the years.",
	"The expedition's first camp was pitched above the tree line, where about fourteen men kept watch in relays of three and four.",
	"When the market bell rang, merchants from the inland villages brought their wool, salt, and iron — perhaps sixty sacks in a good week.",
	"The astronomer recorded each transit with the same patience, filling a cabinet of notebooks with dates, measures, and small marginal doubts.",
	"Children learned their letters from a battered primer that had passed through many generations of the same household.",
	"The bridge toll was collected in copper coin, and the keeper remembered every regular traveller by name, some forty of them.",
	"Chart, compass, and chronometer lay side by side on the chart table, ready for the evening's observations; the log held nineteen pages of soundings.",
	"The committee counted twelve barrels of ale for the feast, though the steward claimed fourteen had arrived in the last delivery.",
	"Nearly eight boats were seen beached at low water, and the warden recorded something like seventeen nets mended before dusk.",
}

type entry struct {
	entity   string
	aliasIdx int
	cat      string
	qty      int
}

func wordFor(n int) string {
	for _, e := range numberWords {
		if e.n == n {
			return e.w
		}
	}
	panic("no word")
}

// entryLine renders one ledger entry with its visible per-file 1-based
// identifier. The identifier uses the entity's base name; the sentence
// text uses an alias (never "the " + alias).
func entryLine(rng *rand.Rand, entity, alias, cat string, qty, k int) string {
	w := wordFor(qty)
	id := fmt.Sprintf("entry %d of %s", k, entity)
	switch rng.Intn(6) {
	case 0:
		return fmt.Sprintf("%s: %s logged %s units, category %s.", id, alias, w, cat)
	case 1:
		return fmt.Sprintf("%s: the record from %s stands at %s, category %s.", id, alias, w, cat)
	case 2:
		return fmt.Sprintf("%s: %s entered %s, category %s.", id, alias, w, cat)
	case 3:
		return fmt.Sprintf("%s: per %s, %s in total, category %s.", id, alias, w, cat)
	case 4:
		return fmt.Sprintf("%s, %s — the kept quantity is %s, category %s.", id, alias, w, cat)
	default:
		return fmt.Sprintf("%s: counted for %s as %s, category %s.", id, alias, w, cat)
	}
}

// withdrawalLine renders the single withdrawal that amends entry k. It
// names the same identifier, states its replacement figure and category,
// and follows the entry it amends.
func withdrawalLine(entity, alias, cat string, qty, k int) string {
	w := wordFor(qty)
	id := fmt.Sprintf("entry %d of %s", k, entity)
	return fmt.Sprintf("Withdrawal for %s: the figure of %s is withdrawn and replaced by %s, category %s, per %s.", id, id, w, cat, alias)
}

// hasDoubledWord reports whether s contains the same word twice in a row
// (case-insensitive, adjacent tokens). Go's regexp is RE2 and has no
// backreferences, so the check tokenizes by hand (plan §12.9).
func hasDoubledWord(s string) bool {
	fields := strings.Fields(s)
	for i := 1; i < len(fields); i++ {
		a := strings.Trim(fields[i-1], ",.;:!?—-\"'")
		b := strings.Trim(fields[i], ",.;:!?—-\"'")
		if a != "" && strings.EqualFold(a, b) {
			return true
		}
	}
	return false
}

// lineIdentifier extracts the "entry N of <entity>" identifier from a
// ledger line.
func lineIdentifier(l string) (string, error) {
	i := strings.Index(l, "entry ")
	if i < 0 {
		return "", fmt.Errorf("no identifier")
	}
	rest := l[i:]
	end := len(rest)
	for _, sep := range []string{":", ",", " — "} {
		if j := strings.Index(rest, sep); j > 0 && j < end {
			end = j
		}
	}
	id := strings.TrimSpace(rest[:end])
	if id == "" {
		return "", fmt.Errorf("empty identifier")
	}
	return id, nil
}

// runSelfCheck re-reads the generated ledger lines of one file and fails
// the generation on: a ledger line appearing twice, a doubled word, two
// withdrawals naming one entry, or an identifier that does not resolve
// within its own file. lines carries only the ledger block lines of the
// file, in emission order.
func runSelfCheck(file string, lines []string) error {
	seen := map[string]bool{}
	identifiers := map[string]bool{}
	withdrawn := map[string]bool{}
	for _, l := range lines {
		if seen[l] {
			return fmt.Errorf("%s: duplicated ledger line: %q", file, l)
		}
		seen[l] = true
		if hasDoubledWord(l) {
			return fmt.Errorf("%s: doubled word in ledger line: %q", file, l)
		}
		id, err := lineIdentifier(l)
		if err != nil {
			return fmt.Errorf("%s: %v (line %q)", file, err, l)
		}
		if strings.HasPrefix(l, "Withdrawal for ") {
			if withdrawn[id] {
				return fmt.Errorf("%s: two withdrawals name %s", file, id)
			}
			withdrawn[id] = true
			if !identifiers[id] {
				return fmt.Errorf("%s: withdrawal names unknown %s", file, id)
			}
		} else {
			if identifiers[id] {
				return fmt.Errorf("%s: identifier %s used by two entries", file, id)
			}
			identifiers[id] = true
		}
	}
	return nil
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: gen <output-dir> <answer-key-path>")
		os.Exit(2)
	}
	rng := rand.New(rand.NewSource(31337))
	out := os.Args[1]
	keyPath := os.Args[2]
	if err := os.MkdirAll(out, 0o755); err != nil {
		panic(err)
	}
	sums := map[string]int{}

	for i := 0; i < 40; i++ {
		var b strings.Builder
		b.WriteString(fmt.Sprintf("Chapter %02d.\n\n", i+1))
		// ~40 KB of filler before the ledger block: whole-file reads hit
		// the 8 KB ToolResultCap cut long before the ledger.
		for b.Len() < 40000 {
			b.WriteString(fillers[rng.Intn(len(fillers))] + "\n")
		}
		b.WriteString("\nLedger.\n\n")
		perm := rng.Perm(len(entities))
		// 12 entries: entities perm[0..9] then perm[0..1] again with new
		// aliases; identifiers are 1-based per file (k+1).
		recs := make([]entry, 0, 12)
		for k := 0; k < 12; k++ {
			y := entities[perm[k%len(perm)]]
			recs = append(recs, entry{
				entity:   y,
				aliasIdx: rng.Intn(3),
				cat:      categories[rng.Intn(len(categories))],
				qty:      1 + rng.Intn(30),
			})
		}
		finalQty := make([]int, len(recs))
		for k, r := range recs {
			finalQty[k] = r.qty
		}
		var lines []string
		for k, r := range recs {
			alias := aliases[r.entity][r.aliasIdx]
			lines = append(lines, entryLine(rng, r.entity, alias, r.cat, r.qty, k+1))
			// At most one withdrawal per entry: k%3 == 2 selects disjoint
			// entries (2, 5, 8, 11) — never the same k twice.
			if k%3 == 2 {
				corr := 1 + rng.Intn(30)
				finalQty[k] = corr
				lines = append(lines, withdrawalLine(r.entity, alias, r.cat, corr, k+1))
			}
			sums[r.cat] += finalQty[k]
		}
		if err := runSelfCheck(fmt.Sprintf("note-%02d.txt", i+1), lines); err != nil {
			fmt.Fprintln(os.Stderr, "gen self-check failed:", err)
			os.Exit(1)
		}
		for _, l := range lines {
			b.WriteString(l + "\n")
		}
		for j := 0; j < 20; j++ {
			b.WriteString(fillers[rng.Intn(len(fillers))] + "\n")
		}
		name := filepath.Join(out, fmt.Sprintf("note-%02d.txt", i+1))
		if err := os.WriteFile(name, []byte(b.String()), 0o644); err != nil {
			panic(err)
		}
	}
	// Deterministic key order in the answer file.
	keys := make([]string, 0, len(sums))
	for k := range sums {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ordered := make(map[string]int, len(sums))
	for _, k := range keys {
		ordered[k] = sums[k]
	}
	keyBytes, err := json.Marshal(ordered)
	if err != nil {
		panic(err)
	}
	// The key is written ONLY to the explicit path (setup.sh passes a path
	// in the task dir, outside the workdir; the bench's leak_check asserts
	// the work folder never contains answer-key.json).
	if err := os.WriteFile(keyPath, keyBytes, 0o644); err != nil {
		panic(err)
	}
	fmt.Println("generated 40 files under", out, "; key", string(keyBytes))
}
