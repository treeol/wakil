package tui

// searchModel groups the reverse-incremental-search (Ctrl+R) state, extracted
// from the tuiModel god struct (WP-6.6). Embedded in tuiModel, so selector access
// is unchanged (m.searchActive, m.searchQuery, ...).
//
// Note: searchIdx indexes into inputHistory, which lives in the sibling embedded
// historyModel. The coupling is fine because the search/history methods stay on
// tuiModel — promotion resolves both groups' fields on the same receiver. Do not
// move search methods onto searchModel (they need inputHistory).
type searchModel struct {
	// Reverse-incremental search (Ctrl+R). searchActive=false = normal input.
	searchActive bool
	searchQuery  string // the query string typed so far
	searchIdx    int    // index into inputHistory of current match (-1 = no match)
	searchSaved  string // original textarea content saved on entering search mode
	searchFailed bool   // true when the last search found no match
}
