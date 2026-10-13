package agent

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/treeol/wakil/internal/proxy"
	wtools "github.com/treeol/wakil/internal/tools"
)

// appendConv appends msgs to a.Conv under convMu, stamping Turn=a.curTurn on
// any message that has Turn==0. All in-turn appends go through here so every
// message of a request carries the same turn number.
func (a *App) appendConv(msgs ...proxy.Message) {
	for i := range msgs {
		if msgs[i].Turn == 0 {
			msgs[i].Turn = a.curTurn
		}
	}
	a.convMu.Lock()
	a.Conv = append(a.Conv, msgs...)
	a.convMu.Unlock()
}

// CollapseRecord records that a finished request's working set was archived
// to disk and replaced in the projection by a note pointing to the archive.
type CollapseRecord struct {
	Turn    int       `json:"turn"`
	Archive string    `json:"archive"` // spill path of the full W render
	WBytes  int       `json:"w_bytes"` // size of the working set (W)
	Note    string    `json:"note"`    // deterministic note appended to A
	At      time.Time `json:"at"`      // when the collapse happened
}

// span is the first and last index in a.Conv for messages of a given turn.
type span struct {
	first, last int
}

// turnSpans returns a map from turn number to the first/last index in a.Conv
// for that turn's messages. Messages with Turn==0 (preamble, summary, handoff
// seed, legacy) are not in the map. One pass over a.Conv under convMu.RLock.
func (a *App) turnSpans() map[int]span {
	a.convMu.RLock()
	defer a.convMu.RUnlock()
	spans := map[int]span{}
	for i, m := range a.Conv {
		if m.Turn > 0 {
			s, ok := spans[m.Turn]
			if !ok {
				spans[m.Turn] = span{first: i, last: i}
			} else {
				s.last = i
				spans[m.Turn] = s
			}
		}
	}
	return spans
}

// renderWorkingSet renders the full working set (W) of a turn as a string
// for archiving to disk. The rendering includes tool-call arguments and tool
// results verbatim — it is the complete record the model can read_file back.
func renderWorkingSet(w []proxy.Message) string {
	var b strings.Builder
	for _, m := range w {
		b.WriteString("--- ")
		b.WriteString(m.Role)
		if m.Name != "" {
			b.WriteString(" (")
			b.WriteString(m.Name)
			b.WriteString(")")
		}
		b.WriteString(" ---\n")
		if m.Content != nil && *m.Content != "" {
			b.WriteString(*m.Content)
			b.WriteString("\n")
		}
		if len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				b.WriteString("tool_call: ")
				b.WriteString(tc.Function.Name)
				b.WriteString("\n")
				b.WriteString("arguments: ")
				b.WriteString(tc.Function.Arguments)
				b.WriteString("\n")
			}
		}
		if m.ToolCallID != "" {
			b.WriteString("tool_call_id: ")
			b.WriteString(m.ToolCallID)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// buildCollapseNote builds the deterministic note appended to A in the
// projection. It summarizes tool counts and touched paths, capped at 600 bytes.
func buildCollapseNote(turn int, w []proxy.Message, archive string) string {
	toolCounts := map[string]int{}
	var order []string
	var touchedPaths []string
	seenPath := map[string]bool{}

	for _, m := range w {
		for _, tc := range m.ToolCalls {
			name := tc.Function.Name
			if _, ok := toolCounts[name]; !ok {
				order = append(order, name)
			}
			toolCounts[name]++
			// Extract paths from tool arguments.
			for _, p := range extractPaths(tc.Function.Arguments) {
				if !seenPath[p] {
					seenPath[p] = true
					touchedPaths = append(touchedPaths, p)
				}
			}
		}
	}

	// Build tool summary: "read_file×6, search_files×3, …"
	var toolParts []string
	for _, name := range order {
		toolParts = append(toolParts, fmt.Sprintf("%s×%d", name, toolCounts[name]))
	}

	// Build the note.
	var b strings.Builder
	fmt.Fprintf(&b, "[request %d working set collapsed — %d tool calls: %s · %d chars]\n",
		turn, sumCounts(toolCounts), strings.Join(toolParts, ", "), renderWorkingSetBytes(w))
	if len(touchedPaths) > 0 {
		limit := len(touchedPaths)
		if limit > 10 {
			limit = 10
		}
		b.WriteString("touched: ")
		b.WriteString(strings.Join(touchedPaths[:limit], ", "))
		if len(touchedPaths) > 10 {
			fmt.Fprintf(&b, " (+%d more)", len(touchedPaths)-10)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "full record: %s (read_file with offset/limit)\n", archive)

	note := b.String()
	if len(note) > 600 {
		note = note[:597] + "…\n"
	}
	return note
}

// extractPaths parses path-like values from a tool-call JSON arguments string.
// Looks for "path", "paths", "file" keys. Errors are silently ignored.
func extractPaths(args string) []string {
	var obj map[string]any
	if err := json.Unmarshal([]byte(args), &obj); err != nil {
		return nil
	}
	var paths []string
	for _, key := range []string{"path", "file"} {
		if v, ok := obj[key]; ok {
			if s, ok := v.(string); ok {
				paths = append(paths, s)
			}
		}
	}
	if v, ok := obj["paths"]; ok {
		if arr, ok := v.([]any); ok {
			for _, item := range arr {
				if s, ok := item.(string); ok {
					paths = append(paths, s)
				}
			}
		}
	}
	return paths
}

func sumCounts(m map[string]int) int {
	total := 0
	for _, c := range m {
		total += c
	}
	return total
}

func renderWorkingSetBytes(w []proxy.Message) int {
	return len(renderWorkingSet(w))
}

// contextProjection returns the messages that should be sent to the model.
// In standard mode, or when no collapse records exist, this is a.Conv itself.
// In long-session mode with collapse records, collapsed turns are replaced by
// their U + A + note. The returned slice is a fresh copy when records exist;
// otherwise it is a.Conv (zero cost, byte-identical).
func (a *App) contextProjection() []proxy.Message {
	if !a.IsLongSession() {
		return a.Conv
	}
	a.stateMu.RLock()
	collapsed := a.collapsed
	a.stateMu.RUnlock()
	if len(collapsed) == 0 {
		return a.Conv
	}

	spans := a.turnSpans()
	// Build a set of collapsed turn numbers for fast lookup.
	collapsedSet := map[int]CollapseRecord{}
	for t, rec := range collapsed {
		collapsedSet[t] = rec
	}

	a.convMu.RLock()
	defer a.convMu.RUnlock()

	// If no messages with collapsed turns exist in a.Conv, return a.Conv.
	hasCollapsed := false
	for _, m := range a.Conv {
		if _, ok := collapsedSet[m.Turn]; ok {
			hasCollapsed = true
			break
		}
	}
	if !hasCollapsed {
		return a.Conv
	}

	var out []proxy.Message
	emittedTurn := map[int]bool{}
	for _, m := range a.Conv {
		rec, isCollapsed := collapsedSet[m.Turn]
		if !isCollapsed || m.Turn == 0 {
			// Not collapsed, or legacy — pass through unchanged.
			out = append(out, m)
			continue
		}
		// Collapsed turn: emit only U (first message) and A (last message + note).
		s, ok := spans[m.Turn]
		if !ok {
			// No span found (shouldn't happen) — pass through.
			out = append(out, m)
			continue
		}
		if emittedTurn[m.Turn] {
			continue // already emitted U and A for this turn
		}
		// Emit U (first message of the turn).
		out = append(out, a.Conv[s.first])
		// Emit A (last message) with the note appended to its content.
		lastMsg := a.Conv[s.last]
		if lastMsg.Content != nil && *lastMsg.Content != "" {
			newContent := *lastMsg.Content + "\n\n" + rec.Note
			lastMsg.Content = &newContent
		} else {
			// A has nil or empty content — use the note as the content.
			note := rec.Note
			lastMsg.Content = &note
		}
		out = append(out, lastMsg)
		emittedTurn[m.Turn] = true
	}
	return out
}

// collapseFinishedTurns runs the collapse rules (§4) on all not-yet-collapsed
// turns. Called at the start of finalizeTurn (before eviction and Compact) and
// at the start of SendOutcome (before fitConvToWindow). Idempotent — already
// collapsed turns are skipped.
func (a *App) collapseFinishedTurns() {
	if !a.IsLongSession() {
		return
	}
	spans := a.turnSpans()
	if len(spans) == 0 {
		return
	}

	a.stateMu.Lock()
	if a.collapsed == nil {
		a.collapsed = map[int]CollapseRecord{}
	}
	a.stateMu.Unlock()

	// Sort turn numbers for deterministic processing.
	turnNums := make([]int, 0, len(spans))
	for t := range spans {
		turnNums = append(turnNums, t)
	}
	sort.Ints(turnNums)

	// The most recent turn is the last in sorted order.
	mostRecent := turnNums[len(turnNums)-1]

	asyncPending := a.countActiveAsyncOps() > 0
	inboxHas := a.hasInboxContent()
	suspended := a.lastOutcomeSuspended

	for _, t := range turnNums {
		// Already collapsed?
		a.stateMu.RLock()
		_, alreadyCollapsed := a.collapsed[t]
		_, kept := a.keep[t]
		a.stateMu.RUnlock()
		if alreadyCollapsed {
			continue
		}
		if kept {
			continue
		}

		s := spans[t]

		// Rule 1: Turn > 0 (already guaranteed by turnSpans).
		// Rule 2: finished — last message is an assistant with non-empty text and no tool calls.
		lastMsg := a.Conv[s.last]
		if !isCleanAnswer(lastMsg) {
			continue
		}

		// Rule 3: |W| >= context_collapse_min_bytes (W = all messages except U and A).
		wSize := 0
		for i := s.first + 1; i < s.last; i++ {
			wSize += messageSize(a.Conv[i])
		}
		if wSize < a.Cfg.ContextCollapseMinBytes {
			continue
		}

		// Rule 5: not a subagent (IsLongSession already checks this).
		// Rule 6: for the most recent turn only, check async/inbox/suspended.
		if t == mostRecent {
			if asyncPending || inboxHas || suspended {
				continue
			}
		}

		// Rule 7: archive to disk. On failure, skip this turn (one warning per session).
		var w []proxy.Message
		for i := s.first + 1; i < s.last; i++ {
			w = append(w, a.Conv[i])
		}
		archive := wtools.SpillToCache(a.chatID(), fmt.Sprintf("turn-%d", t), renderWorkingSet(w))
		if archive == "" {
			if !a.collapseArchiveFailed {
				a.collapseArchiveFailed = true
				fmt.Fprintln(a.Out, Yellow("⚠ collapse: archive write failed for turn "+fmt.Sprint(t)+" — working set kept in context"))
			}
			continue
		}

		note := buildCollapseNote(t, w, archive)

		a.stateMu.Lock()
		a.collapsed[t] = CollapseRecord{
			Turn:    t,
			Archive: archive,
			WBytes:  wSize,
			Note:    note,
			At:      time.Now(),
		}
		a.stateMu.Unlock()

		fmt.Fprintln(a.Out, Dim(fmt.Sprintf("· collapsed request %d — %dk → 0.4k sent (archived)", t, wSize/1024)))
	}
}

// isCleanAnswer reports whether m is a finished assistant message: role
// "assistant", non-empty text content, and no tool calls.
func isCleanAnswer(m proxy.Message) bool {
	return m.Role == "assistant" &&
		m.Content != nil && *m.Content != "" &&
		len(m.ToolCalls) == 0
}

// messageSize returns the approximate size (bytes) of a message's content +
// tool-call arguments.
func messageSize(m proxy.Message) int {
	n := 0
	if m.Content != nil {
		n += len(*m.Content)
	}
	for _, tc := range m.ToolCalls {
		n += len(tc.Function.Arguments)
	}
	return n
}

// collapseArchiveFailed is declared on App (app.go) — this comment is the
// field doc. Turn goroutine only — no lock needed.

// materializeCollapsed replaces collapsed turns in a.Conv with their
// U + A + note (the projection content), then clears all collapse records.
// This is the fallback path when compaction or hard-max must operate on
// the real slice the model sees. After materialisation, a.Conv IS the
// projection — no more filtering needed until new turns are collapsed.
func (a *App) materializeCollapsed() {
	a.stateMu.RLock()
	if len(a.collapsed) == 0 {
		a.stateMu.RUnlock()
		return
	}
	a.stateMu.RUnlock()

	proj := a.contextProjection()
	a.convMu.Lock()
	a.Conv = proj
	a.convMu.Unlock()

	a.stateMu.Lock()
	a.collapsed = nil
	a.stateMu.Unlock()
}

// pruneCollapseOnRewind removes collapse/keep entries for turns that no longer
// exist in a.Conv after a rewind truncation, and resets turnSeq to the max
// remaining Turn so subsequent turns continue numbering correctly.
func (a *App) pruneCollapseOnRewind() {
	a.convMu.RLock()
	maxTurn := 0
	remainingTurns := map[int]bool{}
	for _, m := range a.Conv {
		if m.Turn > 0 {
			remainingTurns[m.Turn] = true
			if m.Turn > maxTurn {
				maxTurn = m.Turn
			}
		}
	}
	a.convMu.RUnlock()

	a.stateMu.Lock()
	if a.collapsed != nil {
		for t := range a.collapsed {
			if !remainingTurns[t] {
				delete(a.collapsed, t)
			}
		}
	}
	if a.keep != nil {
		for t := range a.keep {
			if !remainingTurns[t] {
				delete(a.keep, t)
			}
		}
	}
	a.turnSeq = maxTurn
	a.stateMu.Unlock()
}

// collapsedRecords returns the collapse records as a slice for session
// persistence. Under stateMu.RLock.
func (a *App) collapsedRecords() []CollapseRecord {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	if len(a.collapsed) == 0 {
		return nil
	}
	out := make([]CollapseRecord, 0, len(a.collapsed))
	for _, rec := range a.collapsed {
		out = append(out, rec)
	}
	return out
}

// keepSlice returns the keep set as a slice for session persistence.
func (a *App) keepSlice() []int {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	if len(a.keep) == 0 {
		return nil
	}
	out := make([]int, 0, len(a.keep))
	for t := range a.keep {
		out = append(out, t)
	}
	return out
}

// RestoreTurnState rebuilds the collapsed/keep maps from a saved Session and
// resets turnSeq to the max Turn in a.Conv. Called from LoadSession,
// ResumeConversation, and SetConv (daemon resume paths).
func (a *App) RestoreTurnState(s *Session) {
	a.stateMu.Lock()
	if len(s.Collapsed) > 0 {
		a.collapsed = map[int]CollapseRecord{}
		for _, rec := range s.Collapsed {
			a.collapsed[rec.Turn] = rec
		}
	}
	if len(s.Keep) > 0 {
		a.keep = map[int]bool{}
		for _, t := range s.Keep {
			a.keep[t] = true
		}
	}
	a.lastOutcomeSuspended = false
	a.stateMu.Unlock()

	// Reset turnSeq to the max Turn in a.Conv so new turns continue
	// numbering after the restored history.
	a.convMu.RLock()
	maxTurn := 0
	for _, m := range a.Conv {
		if m.Turn > maxTurn {
			maxTurn = m.Turn
		}
	}
	a.convMu.RUnlock()
	a.stateMu.Lock()
	a.turnSeq = maxTurn
	a.stateMu.Unlock()
}
