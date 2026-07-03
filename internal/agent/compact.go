package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/tools"
)

// charsPerToken is the chars-to-token ratio used to convert a token budget
// (from ContextLimit.Usable) into char units that compaction operates in.
// Mirrors the ~4 chars/token estimate already used by proxy.ApproxTokens.
const charsPerToken = 4

// transcriptSize is a cheap proxy for context size (chars of content + args).
func TranscriptSize(conv []proxy.Message) int {
	n := 0
	for _, m := range conv {
		n += len(DerefStr(m.Content))
		for _, tc := range m.ToolCalls {
			n += len(tc.Function.Arguments)
		}
	}
	return n
}

// turnBoundary returns the index of the start of the keep-th user turn counted
// from the end. Still used by tool-result eviction (TTL is measured in turns).
func turnBoundary(conv []proxy.Message, keep int) int {
	count := 0
	for i := len(conv) - 1; i >= 0; i-- {
		if conv[i].Role == "user" {
			count++
			if count == keep {
				return i
			}
		}
	}
	return 0
}

// keepBoundary finds the start index of the verbatim tail: the most recent
// consecutive turns whose total byte size fits within keepBytes. The split
// always lands on a user-message boundary so turn groupings stay intact.
//
// Returns 0 when everything fits (nothing to summarize). Returns the index of
// the last user message when that single turn already exceeds keepBytes (keep
// at least the most recent turn verbatim no matter what).
func keepBoundary(conv []proxy.Message, keepBytes int) int {
	if keepBytes <= 0 || len(conv) == 0 {
		return 0
	}
	size := 0
	for i := len(conv) - 1; i >= 0; i-- {
		size += len(DerefStr(conv[i].Content))
		for _, tc := range conv[i].ToolCalls {
			size += len(tc.Function.Arguments)
		}
		if size > keepBytes && conv[i].Role == "user" {
			// conv[i:] is too large. The verbatim tail starts at the next user
			// message after i (first complete turn that still fits).
			for j := i + 1; j < len(conv); j++ {
				if conv[j].Role == "user" {
					return j
				}
			}
			// i is the last user turn and it alone exceeds keepBytes.
			// Keep it verbatim anyway — never summarize the most recent turn.
			return i
		}
	}
	return 0 // everything fits within keepBytes
}

// renderTranscript turns messages into plain text for embedding in a summary
// prompt. We embed rather than rely on resent history because the proxy's
// memory path ignores client-sent history — only the latest message is read.
func renderTranscript(conv []proxy.Message) string {
	var b strings.Builder
	for _, m := range conv {
		switch m.Role {
		case "user":
			fmt.Fprintf(&b, "USER: %s\n", DerefStr(m.Content))
		case "assistant":
			if DerefStr(m.Content) != "" {
				fmt.Fprintf(&b, "ASSISTANT: %s\n", DerefStr(m.Content))
			}
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&b, "ASSISTANT called %s(%s)\n", tc.Function.Name, tc.Function.Arguments)
			}
		case "tool":
			fmt.Fprintf(&b, "TOOL[%s] -> %s\n", m.Name, Truncate(DerefStr(m.Content), 600))
		case "system":
			fmt.Fprintf(&b, "%s\n", DerefStr(m.Content))
		}
	}
	return b.String()
}

func Truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// activeThresholds returns the compaction thresholds in chars for the current
// session. When a live context limit is known from the backend the thresholds
// are computed as fractions of the effective window so they scale automatically
// when the backend, model, or their n_ctx changes.
//
// effective_ctx = Usable() × cfg.ContextCapacityFrac × charsPerToken
// Then CompactAtFrac, KeepBytesFrac, HardMaxFrac are applied on top.
//
// When the limit is unknown (startup before the probe, subagents, tests that
// leave CtxLimit zero) the absolute config values are the fallback — always
// kept valid by validateContextLimits.
func (a *App) activeThresholds() (compactAt, keepBytes, hardMax int) {
	// Use the raw NCtx field, not a.ContextLimit(), which synthesises a fallback
	// with a non-zero NCtx even when the backend has never been probed. We want
	// the fraction path only when an authoritative n_ctx is known from the backend.
	if a.CtxLimit.NCtx > 0 && a.Cfg.CompactAtFrac > 0 {
		// effective_ctx = usable tokens × capacity_frac × charsPerToken
		usable := a.CtxLimit.Usable()
		capacityFrac := a.Cfg.ContextCapacityFrac
		if capacityFrac <= 0 {
			capacityFrac = 0.80 // zero value → use default
		}
		effectiveChars := int(float64(usable) * capacityFrac * float64(charsPerToken))

		ca := int(a.Cfg.CompactAtFrac * float64(effectiveChars))
		kb := int(a.Cfg.KeepBytesFrac * float64(effectiveChars))
		hm := int(a.Cfg.HardMaxFrac * float64(effectiveChars))
		// Only use fraction results when the hierarchy is satisfied. For tiny
		// backends (< ~8k chars usable) the fixed SummaryBytes may violate it;
		// fall through to absolute values in that case rather than corrupt the
		// compaction invariant.
		if kb > 0 && kb+a.Cfg.SummaryBytes < ca && ca < hm {
			return ca, kb, hm
		}
	}
	// Absolute fallback — apply MaxChars cap on compactAt for compatibility
	// with configs that relied on MaxChars as an upper bound for compaction.
	ca := a.Cfg.CompactAt
	if a.Cfg.MaxChars > 0 && ca > a.Cfg.MaxChars {
		ca = a.Cfg.MaxChars
	}
	return ca, a.Cfg.KeepBytes, a.Cfg.HardMaxBytes
}

// fitConvToWindow is the downshift guard: if /backend switched to a backend
// with a smaller context window and Conv already exceeds the new hard ceiling,
// compact+drop before the next request rather than deliver an over-window
// transcript (the P25 connection-reset class of failures).
//
// A loud ⚠ note is written to a.Out when compaction is needed so the user
// knows their context is being trimmed to fit the new backend.
func (a *App) fitConvToWindow(ctx context.Context) {
	_, _, hm := a.activeThresholds()
	if hm <= 0 || TranscriptSize(a.Conv) <= hm {
		return
	}
	usable := a.ContextLimit().Usable()
	fmt.Fprintf(a.Out, Yellow("⚠ switched to smaller-context backend — compacting to fit %dk token window\n"), usable/1000)
	a.enforceHardMax(ctx, hm)
}

// summarizer produces a running summary of older turns. Injectable for tests.
type summarizer func(ctx context.Context, text string) (string, error)

// proxySummarizer asks the proxy to summarize, embedding the transcript in the
// prompt content (no tools, so it won't try to act on it).
func (a *App) proxySummarizer(ctx context.Context, text string) (string, error) {
	// The summary call would overwrite the turn's grounding; keep the display
	// showing what the user's actual query was grounded on.
	prevAtt, prevScore, prevG := a.Client.GroundingState()
	defer a.Client.SetGrounding(prevAtt, prevScore, prevG)

	prompt := "Summarize the following conversation transcript concisely. Preserve key facts, " +
		"decisions, file paths, commands run, and any open tasks. Output only the summary.\n\n" + text
	msg, err := a.Client.Stream(ctx, []proxy.Message{{Role: "user", Content: StrPtr(prompt)}}, nil, nil, nil)
	if err != nil {
		return "", err
	}
	a.RecordInferenceCost() // aux inference: summarization/compaction
	return strings.TrimSpace(DerefStr(msg.Content)), nil
}

// compact folds older turns into a single leading system summary when the
// transcript grows past CompactAt, keeping recent turns that fit within
// KeepBytes verbatim. When force is true the size check is skipped —
// compaction runs regardless of current transcript size (used by /compact and
// enforceHardMax).
func (a *App) Compact(ctx context.Context, sum summarizer, force bool) (bool, error) {
	compactAt, keepBytes, _ := a.activeThresholds()
	if !force {
		if TranscriptSize(a.Conv) <= compactAt {
			return false, nil
		}
	}
	boundary := keepBoundary(a.Conv, keepBytes)
	if boundary <= 0 {
		return false, nil
	}
	// Evict stale tool results immediately before compaction so the summariser
	// never sees verbose content that would have been pruned anyway. This is
	// the only unconditional eviction site; Send() only evicts under pressure.
	a.evictStaleToolResults()

	// Pinned messages in the "older" block are exempt from summarization.
	// They are extracted from older, held aside, and re-inserted verbatim
	// after the summary so they survive compaction intact. This is what stops
	// a subagent's task instruction from being dissolved into lossy prose.
	//
	// When a pinned tool-role message is found, its parent assistant message
	// (the one carrying the matching tool_calls[].id) is also kept verbatim —
	// otherwise the tool message would be orphaned from its call, violating
	// the chat-completions schema (tool result without preceding tool_calls).
	older := a.Conv[:boundary]
	pinnedSet := make(map[int]bool) // indices in older that are pinned (or parents/children of pinned)
	for i, m := range older {
		if m.Pinned {
			pinnedSet[i] = true
			// If this is a pinned tool message, find its parent assistant
			// tool_calls message and co-preserve it. This prevents an orphaned
			// tool message (tool result without preceding tool_calls).
			if m.Role == "tool" && m.ToolCallID != "" {
				// Search backward for the nearest assistant with a matching
				// tool_call ID — this is the structural parent, not just the
				// first assistant that happens to reuse the ID.
				for j := i - 1; j >= 0; j-- {
					if older[j].Role == "assistant" && len(older[j].ToolCalls) > 0 {
						matched := false
						for _, tc := range older[j].ToolCalls {
							if tc.ID == m.ToolCallID {
								matched = true
								break
							}
						}
						if matched {
							pinnedSet[j] = true
							// Also co-preserve ALL sibling tool results from
							// this assistant message, bounded to the same turn
							// (stop at the next user or assistant message).
							// This prevents the inverse orphan: tool_calls
							// with no matching tool result.
							for _, tc2 := range older[j].ToolCalls {
								for k := j + 1; k < len(older); k++ {
									// Stop at the next assistant or user message —
									// siblings belong to the same turn.
									if older[k].Role == "assistant" || older[k].Role == "user" {
										break
									}
									if older[k].Role == "tool" && older[k].ToolCallID == tc2.ID {
										pinnedSet[k] = true
									}
								}
							}
							break
						}
					}
				}
			}
		}
	}
	var pinnedPrefix []proxy.Message
	var summarizable []proxy.Message
	for i, m := range older {
		if pinnedSet[i] {
			pinnedPrefix = append(pinnedPrefix, m)
		} else {
			summarizable = append(summarizable, m)
		}
	}

	var summary string
	if len(summarizable) > 0 {
		var err error
		summary, err = sum(ctx, renderTranscript(summarizable))
		if err != nil {
			return false, err
		}
		// If the generated summary itself exceeds SummaryBytes, condense it further
		// so the running summary never balloons across repeated compaction cycles.
		if a.Cfg.SummaryBytes > 0 && len(summary) > a.Cfg.SummaryBytes {
			if condensed, err2 := sum(ctx, "Condense the following summary to its essential points only:\n\n"+summary); err2 == nil {
				summary = condensed
			}
		}
	}

	newConv := make([]proxy.Message, 0, len(a.Conv)-boundary+1+len(pinnedPrefix)+1)
	// Pinned messages from older stay at the front (system prompt, task).
	newConv = append(newConv, pinnedPrefix...)
	if summary != "" {
		newConv = append(newConv, proxy.Message{Role: "system", Content: StrPtr("[Summary of earlier conversation]\n" + summary)})
	}
	newConv = append(newConv, a.Conv[boundary:]...)
	a.Conv = newConv
	return true, nil
}

// oldestTurnRange returns [first, next) — the inclusive-exclusive range of
// messages that form the oldest complete user turn in conv. first is the index
// of the oldest user message; next is the index of the following user message
// (or len(conv) when this is the last turn). Returns (-1, -1) when no user
// message exists. Skips a leading system summary message.
//
// Turns containing any pinned message are skipped entirely — neither the
// anchoring user message nor any message within the turn range is eligible
// for dropping. This is the enforcement point for the compaction-exemption:
// enforceHardMax's drop loop calls this function to find the next turn to
// shed, and it must never return a turn that contains protected content
// (subagent task instruction, subagent summary breadcrumb).
func oldestTurnRange(conv []proxy.Message) (first, next int) {
	start := 0
	if len(conv) > 0 && conv[0].Role == "system" {
		start = 1
	}
	for i := start; i < len(conv); i++ {
		if conv[i].Role != "user" || conv[i].Pinned {
			continue
		}
		// Found a candidate user message at i. Compute the turn range [i, next).
		n := len(conv)
		for j := i + 1; j < len(conv); j++ {
			if conv[j].Role == "user" {
				n = j
				break
			}
		}
		// Skip this turn if any message within [i, n) is pinned.
		hasPinned := false
		for _, m := range conv[i:n] {
			if m.Pinned {
				hasPinned = true
				break
			}
		}
		if hasPinned {
			continue
		}
		return i, n
	}
	return -1, -1
}

// turnContainsSubagent reports whether the range [first, next) of conv contains
// a dispatch_subagent tool call or its result. Used by enforceHardMax to avoid
// silently corrupting a subagent-bearing turn.
func turnContainsSubagent(conv []proxy.Message, first, next int) bool {
	for _, m := range conv[first:next] {
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				if tc.Function.Name == "dispatch_subagent" {
					return true
				}
			}
		}
		if m.Role == "tool" && m.Name == "dispatch_subagent" {
			return true
		}
	}
	return false
}

// dropOldestTurn removes the oldest complete user turn from the transcript,
// preserving any leading system summary. Dropping complete turns (rather than
// individual messages) keeps the remaining transcript coherent — no orphaned
// tool results or assistant messages without a preceding user message.
func dropOldestTurn(conv []proxy.Message) []proxy.Message {
	first, next := oldestTurnRange(conv)
	if first < 0 {
		// No droppable user turns — nothing safe to drop; remove one non-pinned
		// raw message as last resort. Skip:
		// - pinned messages (system prompt, task, breadcrumbs)
		// - assistant messages with tool_calls (dropping orphans their tool results)
		// - tool messages whose parent assistant is being retained (dropping
		//   orphans the tool_calls — the inverse schema violation)
		start := 0
		if len(conv) > 0 && conv[0].Role == "system" {
			start = 1
		}
		for i := start; i < len(conv); i++ {
			if conv[i].Pinned {
				continue
			}
			if conv[i].Role == "assistant" && len(conv[i].ToolCalls) > 0 {
				continue
			}
			if conv[i].Role == "tool" && conv[i].ToolCallID != "" {
				// Check if the parent assistant is still in conv (being retained).
				// If it is, dropping this tool message would orphan it.
				parentRetained := false
				for j := i - 1; j >= 0; j-- {
					if conv[j].Role == "assistant" && len(conv[j].ToolCalls) > 0 {
						for _, tc := range conv[j].ToolCalls {
							if tc.ID == conv[i].ToolCallID {
								parentRetained = true
								break
							}
						}
						break // nearest assistant is the structural parent
					}
				}
				if parentRetained {
					continue
				}
			}
			return append(conv[:i:i], conv[i+1:]...)
		}
		return conv
	}
	// Drop [first, next): the entire oldest user turn.
	return append(conv[:first:first], conv[next:]...)
}

// spillPathsInTurn collects every spill-cache path referenced by tool messages
// in the first user turn of conv (i.e. the turn dropOldestTurn is about to
// remove). Paths come from capToolResult and stubToolResult annotations.
func spillPathsInTurn(conv []proxy.Message) []string {
	first, next := oldestTurnRange(conv)
	if first < 0 {
		return nil
	}
	var paths []string
	for _, m := range conv[first:next] {
		if m.Role == "tool" {
			if p := tools.ExtractSpillPath(DerefStr(m.Content)); p != "" {
				paths = append(paths, p)
			}
		}
	}
	return paths
}

// enforceHardMax is the unconditional safety net. It receives the hard ceiling
// in chars from the caller (typically activeThresholds().hardMax for the main
// session, or a fixed subagent constant). It forces a compact pass first to
// summarise before losing content, then drops the oldest complete turns until
// under max.
//
// A turn containing a dispatch_subagent call+result is treated as atomic —
// never split — and its loss is surfaced as a hard warning. Content shed by
// eviction is listed by spill-cache path so the user can retrieve it.
// Pass max ≤ 0 to disable (no-op, matching HardMaxBytes=0 behaviour).
func (a *App) enforceHardMax(ctx context.Context, max int) {
	if max <= 0 || TranscriptSize(a.Conv) <= max {
		return
	}
	sizeBefore := TranscriptSize(a.Conv)
	// Try a forced compact pass first — summarise rather than drop when possible.
	a.Compact(ctx, a.summarizeFn(), true) //nolint:errcheck

	var droppedPaths []string
	droppedTurns := 0
	droppedSubagent := false
	for TranscriptSize(a.Conv) > max && len(a.Conv) > 1 {
		first, next := oldestTurnRange(a.Conv)
		if first < 0 {
			break
		}
		if turnContainsSubagent(a.Conv, first, next) {
			droppedSubagent = true
		}
		droppedPaths = append(droppedPaths, spillPathsInTurn(a.Conv)...)
		a.Conv = dropOldestTurn(a.Conv)
		droppedTurns++
	}

	if droppedTurns == 0 {
		return
	}

	// Signal exhaustion to dispatchSubagent: content was shed from the
	// transcript during this turn. The subagent's final response may be based
	// on incomplete context — dispatchSubagent uses this to produce a truthful
	// Status:"incomplete" summary instead of a misleading parse-error.
	a.exhausted = true

	// Build the warning shown in the conversation viewport.
	userTurnsLeft := 0
	for _, m := range a.Conv {
		if m.Role == "user" {
			userTurnsLeft++
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, Yellow("⚠ hard-max shed %d turn(s)"), droppedTurns)
	fmt.Fprintf(&b, Dim(" (ctx was %dk, limit %dk)"), sizeBefore/1000, max/1000)
	if droppedSubagent {
		b.WriteString("\n  ⛔ " + Yellow("one or more dropped turns contained a dispatch_subagent summary — findings lost"))
	}
	if len(droppedPaths) > 0 {
		fmt.Fprintf(&b, "\n  spilled content at:")
		for _, p := range droppedPaths {
			fmt.Fprintf(&b, "\n  · %s", p)
		}
	}
	if userTurnsLeft == 0 {
		if len(a.Conv) > 0 && a.Conv[0].Role == "system" {
			b.WriteString("\n  " + Yellow("transcript reduced to summary only"))
		} else {
			b.WriteString("\n  " + Yellow("transcript is now empty"))
		}
	}
	fmt.Fprintln(a.Out, b.String())
}
