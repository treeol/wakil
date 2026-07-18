package tui

// subAgentModel groups the subagent-tab state, extracted from the tuiModel god
// struct (WP-6.6). Embedded in tuiModel, so selector access is unchanged
// (m.subTabs, m.subCur, m.subSeq).
//
// Sharing model note: subTabs is a []*subTab slice HEADER (not pointer-to-slice
// like items *[]convItem). Bubble Tea copies the header on Update; the backing
// array is shared unless an append reallocates. This is pre-existing behavior —
// pruneSubTabs and tabIndexByN already take the slice as a parameter, so
// embedding doesn't change their call sites.
type subAgentModel struct {
	// Subagent tabs. subCur=-1 means the main sidebar tab is active.
	// A tab is "running" iff !tab.done — multiple tabs may run concurrently
	// (parallel subagent dispatch), so there is no single "running tab" field;
	// chunk/done messages are routed by ChatID.
	subTabs []*subTab
	subCur  int // -1 = main, 0..n-1 = subTabs index
	subSeq  int // auto-increment for label generation (s1, s2, …)
}
