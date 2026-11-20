// Command gen generates the long-read fixture: about 60 prose files of
// about 8 KB each, each stating one number in words and one category. The
// seed is fixed, so the fixture is reproducible. It lives outside the task's
// repo/ so neither the generator nor the seed ever reaches the work folder or
// its git history; setup.sh runs it with the output directory as argument.
package main

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
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

var fillers = []string{
	"The harbour notice warned that the tide tables for the next fortnight would be posted near the customs house, beside the scale that sailors used in former years.",
	"Paragraph after paragraph the clerkess summarised the parish registers, noting every baptism and burial in a hand that had grown steady with practice.",
	"Nobody in the room could remember a winter quite so severe, when the river froze solid enough for carts to cross with their loads of firewood.",
	"A small brass key, green with age, opened a drawer in which the town seal had been kept since the charter was granted.",
	"The expedition's first camp was pitched above the tree line, where the wind sang through the guy ropes all night long.",
	"When the market bell rang, merchants from the inland villages brought their wool, salt, and iron to trade against grain and fish.",
	"The astronomer recorded each transit with the same patience, filling a cabinet of notebooks with dates, measures, and small marginal doubts.",
	"Children learned their letters from a battered primer that had passed through many generations of the same household.",
	"The bridge toll was collected in copper coin, and the keeper, who had held the post for a long span, remembered every regular traveller by name.",
	"Chart, compass, and chronometer lay side by side on the chart table, ready for the evening's observations.",
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: gen <output-dir>")
		os.Exit(2)
	}
	rng := rand.New(rand.NewSource(42))
	out := os.Args[1]
	if err := os.MkdirAll(out, 0o755); err != nil {
		panic(err)
	}
	for i := 0; i < 60; i++ {
		cat := categories[rng.Intn(len(categories))]
		num := numberWords[rng.Intn(len(numberWords))].w

		var b strings.Builder
		b.WriteString(fmt.Sprintf("Chapter %02d.\n\nThis record concerns the %s category.\n\n", i+1, cat))
		// The number is stated once, in words, in varying phrasing.
		phrasings := []string{
			"The count for this entry is " + num + ".",
			"Our tally, arrived at after the long recount, stands at " + num + ".",
			num + " units were set aside for the purpose.",
			"The final figure, after every correction, comes to " + num + ".",
			"Whoever kept this ledger entered the total as " + num + ".",
		}
		b.WriteString(phrasings[rng.Intn(len(phrasings))] + "\n\n")
		for b.Len() < 8000 {
			b.WriteString(fillers[rng.Intn(len(fillers))] + "\n")
		}
		name := filepath.Join(out, fmt.Sprintf("note-%02d.txt", i+1))
		if err := os.WriteFile(name, []byte(b.String()), 0o644); err != nil {
			panic(err)
		}
	}
	fmt.Println("generated 60 files under", out)
}
