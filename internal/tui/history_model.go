package tui

// historyModel groups the input-history navigation state, extracted from the
// tuiModel god struct (WP-6.6). Embedded in tuiModel, so selector access is
// unchanged (m.inputHistory, m.histIdx, m.histSaved). Composite literals that set
// these fields must use the nested form, e.g. tuiModel{historyModel: historyModel{...}}.
//
// Input history for UP/DOWN navigation (most-recent entry first).
type historyModel struct {
	inputHistory []string
	histIdx      int    // -1 = editing current input (not browsing history)
	histSaved    string // current input saved when the user starts navigating
}
