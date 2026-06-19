package proxy

import (
	"reflect"
	"testing"
)

func TestParseGroundingHeader(t *testing.T) {
	cases := []struct {
		in       string
		entries  []GroundingEntry
		maxScore float64
	}{
		{"", nil, 0.0},
		{"   ", nil, 0.0},
		// Back-compat: no "|" prefix → Type "corpus"
		{"arrange/OPTIONS:0.81", []GroundingEntry{{Type: "corpus", Label: "arrange/OPTIONS", Score: 0.81}}, 0.81},
		{"arrange/OPTIONS:0.30, arrange/EXAMPLES:0.72", []GroundingEntry{
			{Type: "corpus", Label: "arrange/OPTIONS", Score: 0.30},
			{Type: "corpus", Label: "arrange/EXAMPLES", Score: 0.72},
		}, 0.72},
		// Mixed: some entries carry no score → ignored for max, kept as labels.
		{"arrange/OPTIONS:0.41, ilm/find, arrange/X:0.12", []GroundingEntry{
			{Type: "corpus", Label: "arrange/OPTIONS", Score: 0.41},
			{Type: "corpus", Label: "ilm/find"},
			{Type: "corpus", Label: "arrange/X", Score: 0.12},
		}, 0.41},
		// No parseable score anywhere → max 0.0.
		{"ilm/find, ilm/grep", []GroundingEntry{
			{Type: "corpus", Label: "ilm/find"},
			{Type: "corpus", Label: "ilm/grep"},
		}, 0.0},
		// New typed format: explicit type prefix before "|"
		{"zdb|arrange/OPTIONS:0.81", []GroundingEntry{
			{Type: "zdb", Label: "arrange/OPTIONS", Score: 0.81},
		}, 0.81},
		{"corpus|doc/find:0.50,zdb|query/select:0.70", []GroundingEntry{
			{Type: "corpus", Label: "doc/find", Score: 0.50},
			{Type: "zdb", Label: "query/select", Score: 0.70},
		}, 0.70},
		// memory and learned types, no score
		{"memory|recent fact,learned|user pref", []GroundingEntry{
			{Type: "memory", Label: "recent fact"},
			{Type: "learned", Label: "user pref"},
		}, 0.0},
		// Mixed old and new in same header
		{"zdb|find/OPTIONS:0.81,ilm/grep", []GroundingEntry{
			{Type: "zdb", Label: "find/OPTIONS", Score: 0.81},
			{Type: "corpus", Label: "ilm/grep"},
		}, 0.81},
	}
	for _, c := range cases {
		entries, maxScore := parseGroundingHeader(c.in)
		if !reflect.DeepEqual(entries, c.entries) {
			t.Errorf("parseGroundingHeader(%q) entries = %v, want %v", c.in, entries, c.entries)
		}
		if maxScore != c.maxScore {
			t.Errorf("parseGroundingHeader(%q) maxScore = %v, want %v", c.in, maxScore, c.maxScore)
		}
	}
}
