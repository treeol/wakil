package tui

import "strings"

// reasoningModel groups the extended-thinking (reasoning) accumulation state,
// extracted from the tuiModel god struct (WP-6.6). Embedded in tuiModel, so
// selector access is unchanged (m.reasoning, m.reasoningDone, m.reasoningExpanded).
//
// CRITICAL Bubble Tea value-copy invariant: reasoning MUST remain *strings.Builder,
// never a value strings.Builder. The model is copied on every Update; a pointer
// keeps all copies sharing the one in-flight builder. Do not change the type.
// Locked by TestTuiModelCopy_SharesReasoningBuilder (reasoning_model_test.go).
type reasoningModel struct {
	// reasoning accumulates extended-thinking deltas while the model is thinking.
	// On the first content delta the builder is Reset and a single collapsed
	// "· thought (~N tokens)" summary is committed as a separate iSys item.
	// Never written to Conv history.
	reasoning         *strings.Builder
	reasoningDone     bool // true once reasoning has been collapsed
	reasoningExpanded bool // expand live reasoning beyond the collapsed cap
}
