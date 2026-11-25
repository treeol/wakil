// Command gen generates the collapse-long-v2 fixture: 40 prose files of
// about 60 KB each, each with a dense ledger block past the ToolResultCap cut
// plus number-laden filler decoys around it. The seed is fixed, so the
// fixture is reproducible. It lives outside the task's repo/ so neither the
// generator nor the seed ever reaches the work folder or its git history;
// setup.sh runs it with the output directory as argument. The generator also
// writes the answer key (expected per-category sums) as answer-key.json next
// to the generated docs/ (outside the workdir; the self-test asserts it
// equals the check.sh constants).
//
// Determinism discipline: no map iteration in output paths (all selections
// come from slices in fixed order via the seeded RNG); two generations with
// the same seed are byte-identical (verified by the author before commit).
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

// Aliases per entity: each entity (year) has a real name and two aliases, so
// no single name match finds every instance.
var aliases = map[string][]string{
	"harbour": {"the harbourmaster", "the port-keeper", "the quay ledger"},
	"ferry":   {"the ferry operator", "the ferryman", "the crossing house"},
	"kiln":    {"the kiln-house keeper", "the burner", "the firing shed"},
	"mill":    {"the miller", "the grinding shop", "the wheel-house"},
	"bakery":  {"the baker", "the bakehouse", "the oven room"},
	"market":  {"the stall-holder", "the market office", "the toll clerk"},
	"chapel":  {"the chapelwarden", "the vestry", "the small chapel"},
	"smithy":  {"the smith", "the forge", "the anvil bench"},
	"orchard": {"the ortsman", "the cider press", "the orchard house"},
	"granary": {"the granary guard", "the grain store", "the barn loft"},
}

var years = []string{"harbour", "ferry", "kiln", "mill", "bakery", "market", "chapel", "smithy", "orchard", "granary"}

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
	year     string
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

func ledgerLine(rng *rand.Rand, year, alias string, qty int, corrIdx int, isCorrection bool) string {
	w := wordFor(qty)
	if isCorrection {
		return fmt.Sprintf("Withdrawal for the entry %d of %s: the %s figure for the %s ledger is withdrawn and the corrected figure is %s.", corrIdx, year, alias, year, w)
	}
	switch rng.Intn(6) {
	case 0:
		return fmt.Sprintf("Entry for the %s of %s: %s units.", year, alias, w)
	case 1:
		return fmt.Sprintf("In %s, the record for %s stands at %s.", year, alias, w)
	case 2:
		return fmt.Sprintf("For %s, the %s was entered as %s.", year, alias, w)
	case 3:
		return fmt.Sprintf("The %s reports %s units for the %s ledger.", alias, w, year)
	case 4:
		return fmt.Sprintf("Counted in %s under %s: %s in total.", year, alias, w)
	default:
		return fmt.Sprintf("Ledger line, %s / %s — the kept quantity is %s.", year, alias, w)
	}
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: gen <output-dir>")
		os.Exit(2)
	}
	rng := rand.New(rand.NewSource(1337))
	out := os.Args[1]
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
		perm := rng.Perm(len(years))
		// 12 entries: years perm[0..9] then perm[0..1] again with new aliases.
		recs := make([]entry, 0, 12)
		for k := 0; k < 12; k++ {
			y := years[perm[k%len(perm)]]
			recs = append(recs, entry{
				year:     y,
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
			alias := aliases[r.year][r.aliasIdx]
			lines = append(lines, fmt.Sprintf("%s (category %s)", ledgerLine(rng, r.year, alias, r.qty, k, false), r.cat))
			if k%3 == 1 && k > 0 {
				corr := 1 + rng.Intn(30)
				finalQty[k] = corr
				lines = append(lines, fmt.Sprintf("%s (category %s)", ledgerLine(rng, r.year, alias, corr, k, true), r.cat))
			}
			sums[r.cat] += finalQty[k]
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
	// Write the key outside the docs/ dir (the bench asserts it equals the
	// check.sh constants; the agent never sees it — gen/ is outside repo/).
	keyPath := filepath.Join(filepath.Dir(out), "answer-key.json")
	if err := os.WriteFile(keyPath, keyBytes, 0o644); err != nil {
		panic(err)
	}
	fmt.Println("generated 40 files under", out, "; key", string(keyBytes))
}
