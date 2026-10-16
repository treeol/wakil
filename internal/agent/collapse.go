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

// CollapseRecord records that a finished request's working set (or a mid-turn
// prefix of the active request) was archived to disk and replaced in the
// projection by a note pointing to the archive.
type CollapseRecord struct {
	Turn           int       `json:"turn"`
	Archive        string    `json:"archive"`                    // spill path of the full W render
	WBytes         int       `json:"w_bytes"`                    // size of the working set (W)
	Note           string    `json:"note"`                       // deterministic note appended to A (or U when Partial)
	At             time.Time `json:"at"`                         // when the collapse happened
	Partial        bool      `json:"partial,omitempty"`          // true = mid-turn prefix collapse; false = full finished turn
	BoundaryCallID string    `json:"boundary_call_id,omitempty"` // tool_call ID of the collapse_context call; elides s.first+1 .. curAsstIdx-1
	Summary        string    `json:"summary,omitempty"`          // model-supplied checkpoint summary
	NextSteps      string    `json:"next_steps,omitempty"`       // model-supplied continuation plan
}

// span is the first and last index in a.Conv for messages of a given turn.
type span struct {
	first, last int
}

// turnSpansFromSlice computes the first/last index in conv for each turn > 0.
// Messages with Turn==0 (preamble, summary, handoff seed, legacy) are not in
// the map. Callers must hold convMu (RLock or Lock) or pass an owned snapshot.
func turnSpansFromSlice(conv []proxy.Message) map[int]span {
	spans := map[int]span{}
	for i, m := range conv {
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
	// Cap the note at 600 bytes, but never truncate the archive path line
	// (it's the recovery reference). If the note exceeds the cap, trim the
	// "touched" section and keep the header + archive path.
	if len(note) > 600 {
		// Rebuild a minimal note: header + archive path only.
		var mb strings.Builder
		fmt.Fprintf(&mb, "[request %d working set collapsed — %d tool calls · %d chars]\n",
			turn, sumCounts(toolCounts), renderWorkingSetBytes(w))
		fmt.Fprintf(&mb, "full record: %s (read_file with offset/limit)\n", archive)
		note = mb.String()
		// If even the minimal note exceeds 600 (very long archive path),
		// leave it as-is — the path is essential and must not be truncated.
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

// findBoundaryAssistantIdx returns the index in conv (within [s.first+1, s.last])
// of the assistant message whose ToolCalls contains callID. Returns -1 if not
// found or if callID is empty.
func findBoundaryAssistantIdx(conv []proxy.Message, s span, callID string) int {
	if callID == "" {
		return -1
	}
	for i := s.first + 1; i <= s.last && i < len(conv); i++ {
		m := conv[i]
		if m.Role != "assistant" {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.ID == callID {
				return i
			}
		}
	}
	return -1
}

// buildPartialCheckpointBlock formats the checkpoint block appended to a
// shallow copy of U in contextProjection when a turn has a mid-turn partial
// collapse record. Explicitly labeled as an assistant checkpoint so the model
// does not confuse its own summary with user instructions.
func buildPartialCheckpointBlock(rec CollapseRecord) string {
	var b strings.Builder
	b.WriteString("[assistant checkpoint — earlier working set in this request collapsed]\n")
	if rec.Summary != "" {
		b.WriteString("Summary: ")
		b.WriteString(rec.Summary)
		b.WriteString("\n")
	}
	if rec.NextSteps != "" {
		b.WriteString("Next steps: ")
		b.WriteString(rec.NextSteps)
		b.WriteString("\n")
	}
	b.WriteString(rec.Note)
	return b.String()
}

// contextProjection returns the messages that should be sent to the model.
// In standard mode, or when no collapse records exist, this is a.Conv itself.
// In long-session mode with collapse records, finished collapsed turns are
// replaced by their U + A + note, and mid-turn partially-collapsed turns emit
// U + checkpoint block, elide the archived prefix (s.first+1 .. curAsstIdx-1),
// and pass through curAsstIdx .. s.last verbatim. The returned slice is a
// fresh copy when records exist; otherwise it is a.Conv (zero cost,
// byte-identical).
func (a *App) contextProjection() []proxy.Message {
	if !a.IsLongSession() {
		// Standard mode: return a.Conv under convMu.RLock for a consistent
		// snapshot. In practice the turn goroutine is the sole reader at
		// this point, but the lock prevents races with SaveSession/TUI.
		a.convMu.RLock()
		conv := a.Conv
		a.convMu.RUnlock()
		return conv
	}
	// Copy collapse records under stateMu so we can iterate them without
	// holding the lock during the projection build.
	a.stateMu.RLock()
	collapsedCopy := make(map[int]CollapseRecord, len(a.collapsed))
	for t, rec := range a.collapsed {
		collapsedCopy[t] = rec
	}
	a.stateMu.RUnlock()
	if len(collapsedCopy) == 0 {
		a.convMu.RLock()
		conv := a.Conv
		a.convMu.RUnlock()
		return conv
	}

	a.convMu.RLock()
	defer a.convMu.RUnlock()

	spans := turnSpansFromSlice(a.Conv)
	// Build a set of collapsed turn numbers for fast lookup.
	collapsedSet := collapsedCopy

	// Precompute resolved boundary indices for partial records. A partial
	// record is only active if its BoundaryCallID still resolves to an
	// assistant message after s.first+1 (so at least one message is elided).
	partialBoundary := map[int]int{}
	hasCollapsed := false
	for _, m := range a.Conv {
		rec, ok := collapsedSet[m.Turn]
		if !ok || m.Turn == 0 {
			continue
		}
		if rec.Partial {
			if _, checked := partialBoundary[m.Turn]; !checked {
				s, sok := spans[m.Turn]
				if sok {
					bIdx := findBoundaryAssistantIdx(a.Conv, s, rec.BoundaryCallID)
					if bIdx > s.first+1 {
						partialBoundary[m.Turn] = bIdx
						hasCollapsed = true
					} else {
						partialBoundary[m.Turn] = -1
					}
				}
			}
		} else {
			hasCollapsed = true
		}
	}
	if !hasCollapsed {
		return a.Conv
	}

	var out []proxy.Message
	emittedTurn := map[int]bool{}
	for idx, m := range a.Conv {
		rec, isCollapsed := collapsedSet[m.Turn]
		if !isCollapsed || m.Turn == 0 {
			// Not collapsed, or legacy — pass through unchanged.
			out = append(out, m)
			continue
		}
		s, ok := spans[m.Turn]
		if !ok {
			// No span found (shouldn't happen) — pass through.
			out = append(out, m)
			continue
		}
		if rec.Partial {
			bIdx := partialBoundary[m.Turn]
			if bIdx <= s.first+1 {
				// Boundary not found or nothing before boundary — pass through.
				out = append(out, m)
				continue
			}
			if idx == s.first {
				// Emit U with the partial checkpoint block appended to a shallow copy.
				uMsg := m
				cpBlock := buildPartialCheckpointBlock(rec)
				if uMsg.Content != nil && *uMsg.Content != "" {
					newContent := *uMsg.Content + "\n\n" + cpBlock
					uMsg.Content = &newContent
				} else {
					uMsg.Content = &cpBlock
				}
				out = append(out, uMsg)
				continue
			}
			if idx > s.first && idx < bIdx {
				// Elide the archived mid-turn prefix.
				continue
			}
			// idx >= bIdx: pass through the boundary assistant batch and any
			// subsequent messages in this turn verbatim.
			out = append(out, m)
			continue
		}
		// Full collapsed turn: emit only U (first message) and A (last message + note).
		if emittedTurn[m.Turn] {
			continue // already emitted U and A for this turn
		}
		// Emit U (first message of the turn).
		out = append(out, a.Conv[s.first])
		// Emit A (last message) with the note appended to its content.
		lastMsg := a.Conv[s.last]
		noteText := rec.Note
		if rec.Summary != "" {
			noteText = "[checkpoint summary: " + rec.Summary + "]\n" + noteText
		}
		if lastMsg.Content != nil && *lastMsg.Content != "" {
			newContent := *lastMsg.Content + "\n\n" + noteText
			lastMsg.Content = &newContent
		} else {
			// A has nil or empty content — use the note as the content.
			lastMsg.Content = &noteText
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
	a.convMu.RLock()
	spans := turnSpansFromSlice(a.Conv)
	convSnap := a.Conv
	a.convMu.RUnlock()
	if len(spans) == 0 {
		return
	}

	a.stateMu.Lock()
	if a.collapsed == nil {
		a.collapsed = map[int]CollapseRecord{}
	}
	suspended := a.lastOutcomeSuspended
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

	for _, t := range turnNums {
		// Already collapsed? If it's a partial (mid-turn) collapse, allow
		// upgrading to a full collapse once the turn finishes with a clean answer.
		a.stateMu.RLock()
		existingRec, alreadyCollapsed := a.collapsed[t]
		_, kept := a.keep[t]
		a.stateMu.RUnlock()
		if alreadyCollapsed && !existingRec.Partial {
			continue
		}
		if kept {
			continue
		}

		s := spans[t]

		// Rule 1: Turn > 0 (already guaranteed by turnSpans).
		// Rule 2: finished — last message is an assistant with non-empty text and no tool calls.
		lastMsg := convSnap[s.last]
		if !isCleanAnswer(lastMsg) {
			continue
		}

		// Rule 3: |W| >= context_collapse_min_bytes (W = all messages except U and A).
		// If this turn already has a mid-turn partial collapse record, upgrade it
		// unconditionally so the turn ends in a clean U+A full collapse rather
		// than staying stuck in partial state.
		wSize := 0
		for i := s.first + 1; i < s.last; i++ {
			wSize += messageSize(convSnap[i])
		}
		if !existingRec.Partial && wSize < a.Cfg.ContextCollapseMinBytes {
			continue
		}

		// Rule 5: not a subagent (IsLongSession already checks this).
		// Rule 6: for the most recent turn only, check async/inbox/suspended.
		if t == mostRecent {
			if asyncPending || inboxHas || suspended {
				continue
			}
		}

		// Rule 7: archive to disk. On failure, skip this turn (one warning per session);
		// if upgrading from a partial collapse, the existing partial record is retained.
		var w []proxy.Message
		for i := s.first + 1; i < s.last; i++ {
			w = append(w, convSnap[i])
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
			Summary: existingRec.Summary,
		}
		a.stateMu.Unlock()

		fmt.Fprintln(a.Out, Dim(fmt.Sprintf("· collapsed request %d — %dk → 0.4k sent (archived)", t, wSize/1024)))
	}
}

// handleCollapseContext implements the collapse_context tool: sweeps any
// newly-eligible finished prior turns via collapseFinishedTurns(), then
// archives and collapses the completed prefix of the current turn (s.first+1
// .. curAsstIdx-1) into a partial CollapseRecord anchored at tc.ID.
func (a *App) handleCollapseContext(tc proxy.ToolCall) string {
	if !a.IsLongSession() {
		return "collapse_context: no-op in standard mode (use /mode long-session to enable context collapse)"
	}

	var args struct {
		Summary   string `json:"summary"`
		NextSteps string `json:"next_steps"`
	}
	if tc.Function.Arguments != "" {
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return "ERROR: invalid arguments for collapse_context: " + err.Error()
		}
	}
	args.Summary = strings.TrimSpace(args.Summary)
	args.NextSteps = strings.TrimSpace(args.NextSteps)
	if args.Summary == "" {
		return "ERROR: collapse_context requires a non-empty 'summary' of the completed work"
	}

	// Step 1: sweep any finished prior turns that became eligible since the
	// start of the turn (e.g. deferred by Rule 6 while async work was active).
	a.stateMu.RLock()
	beforeCount := 0
	for _, r := range a.collapsed {
		if !r.Partial {
			beforeCount++
		}
	}
	a.stateMu.RUnlock()
	a.collapseFinishedTurns()
	a.stateMu.RLock()
	afterPriorCount := 0
	for _, r := range a.collapsed {
		if !r.Partial {
			afterPriorCount++
		}
	}
	_, curKept := a.keep[a.curTurn]
	existingCur, hasExistingCur := a.collapsed[a.curTurn]
	a.stateMu.RUnlock()
	priorCollapsed := afterPriorCount - beforeCount

	if a.curTurn <= 0 {
		if priorCollapsed > 0 {
			return fmt.Sprintf("collapsed %d prior request(s); no active request prefix to collapse", priorCollapsed)
		}
		return "collapse_context: no active request prefix to collapse"
	}
	if curKept {
		if priorCollapsed > 0 {
			return fmt.Sprintf("collapsed %d prior request(s); current request %d is exempted by /keep", priorCollapsed, a.curTurn)
		}
		return fmt.Sprintf("collapse_context: request %d is exempted from collapse by /keep", a.curTurn)
	}

	// Guard: if async work is actively running or sitting in the inbox,
	// defer collapsing the current turn's prefix so async op handles and
	// correlation context stay visible until delivery.
	if a.countActiveAsyncOps() > 0 || a.hasInboxContent() {
		if priorCollapsed > 0 {
			return fmt.Sprintf("collapsed %d prior request(s); current request prefix deferred while async operations are pending", priorCollapsed)
		}
		return "collapse_context: deferred for current request while async operations or inbox deliveries are pending"
	}

	a.convMu.RLock()
	spans := turnSpansFromSlice(a.Conv)
	s, ok := spans[a.curTurn]
	if !ok {
		a.convMu.RUnlock()
		if priorCollapsed > 0 {
			return fmt.Sprintf("collapsed %d prior request(s)", priorCollapsed)
		}
		return "collapse_context: nothing in current request to collapse yet"
	}

	curAsstIdx := findBoundaryAssistantIdx(a.Conv, s, tc.ID)
	if curAsstIdx <= s.first+1 {
		a.convMu.RUnlock()
		if priorCollapsed > 0 {
			res := fmt.Sprintf("collapsed %d prior request(s); no completed tool rounds in current request %d to collapse yet", priorCollapsed, a.curTurn)
			if args.NextSteps != "" {
				res += "\nNext steps: " + args.NextSteps
			}
			return res
		}
		return "collapse_context: no completed tool rounds in the current request to collapse yet (call after completing at least one tool round)"
	}

	// If collapse_context was already called in the SAME assistant tool batch,
	// curAsstIdx matches the existing partial record's boundary assistant index.
	if hasExistingCur && existingCur.Partial {
		prevIdx := findBoundaryAssistantIdx(a.Conv, s, existingCur.BoundaryCallID)
		if prevIdx == curAsstIdx {
			a.convMu.RUnlock()
			return fmt.Sprintf("collapse_context: current prefix up to this tool batch is already collapsed (archive: %s)", existingCur.Archive)
		}
	}

	// Extract the completed prefix of the current turn: s.first+1 .. curAsstIdx-1.
	w := make([]proxy.Message, 0, curAsstIdx-(s.first+1))
	wSize := 0
	for i := s.first + 1; i < curAsstIdx; i++ {
		w = append(w, a.Conv[i])
		wSize += messageSize(a.Conv[i])
	}
	a.convMu.RUnlock()

	// Minimum threshold for explicit mid-turn collapse: min(ContextCollapseMinBytes, 512)
	// when ContextCollapseMinBytes > 0.
	minBytes := a.Cfg.ContextCollapseMinBytes
	if minBytes > 512 {
		minBytes = 512
	}
	if wSize < minBytes {
		if priorCollapsed > 0 {
			return fmt.Sprintf("collapsed %d prior request(s); current request prefix (%d chars) is below minimum collapse threshold (%d chars)", priorCollapsed, wSize, minBytes)
		}
		return fmt.Sprintf("collapse_context: current request working set (%d chars) is below minimum collapse threshold (%d chars)", wSize, minBytes)
	}

	archive := wtools.SpillToCache(a.chatID(), fmt.Sprintf("turn-%d-part", a.curTurn), renderWorkingSet(w))
	if archive == "" {
		if !a.collapseArchiveFailed {
			a.collapseArchiveFailed = true
			fmt.Fprintln(a.Out, Yellow("⚠ collapse: archive write failed for turn "+fmt.Sprint(a.curTurn)+" — working set kept in context"))
		}
		return "collapse_context: archive write failed — working set kept in context"
	}

	note := buildCollapseNote(a.curTurn, w, archive)
	rec := CollapseRecord{
		Turn:           a.curTurn,
		Archive:        archive,
		WBytes:         wSize,
		Note:           note,
		At:             time.Now(),
		Partial:        true,
		BoundaryCallID: tc.ID,
		Summary:        args.Summary,
		NextSteps:      args.NextSteps,
	}
	cpLen := len(buildPartialCheckpointBlock(rec))

	a.stateMu.Lock()
	if a.collapsed == nil {
		a.collapsed = map[int]CollapseRecord{}
	}
	a.collapsed[a.curTurn] = rec
	a.stateMu.Unlock()

	fmt.Fprintln(a.Out, Dim(fmt.Sprintf("· collapsed request %d working set (mid-turn) — %dk → %.1fk sent (archived)",
		a.curTurn, wSize/1024, float64(cpLen)/1024)))

	var b strings.Builder
	fmt.Fprintf(&b, "collapsed request %d working set (%d bytes → %d bytes checkpoint; full record at %s)",
		a.curTurn, wSize, cpLen, archive)
	if priorCollapsed > 0 {
		fmt.Fprintf(&b, " + %d prior request(s)", priorCollapsed)
	}
	return b.String()
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
	// Clear checkpoints before restructuring a.Conv so early returns in
	// Compact() never leave stale ConvLen indices pointing into a shortened Conv.
	// Lock order: clearCheckpoints acquires cpMu (before convMu).
	a.clearCheckpoints()
	a.convMu.Lock()
	a.Conv = proj
	a.convMu.Unlock()

	a.stateMu.Lock()
	a.collapsed = nil
	a.stateMu.Unlock()
}

// pruneCollapseOnRewind removes collapse/keep entries for turns that no longer
// exist in a.Conv after a rewind truncation, or that exist but no longer end
// with a clean answer (for full records) or whose BoundaryCallID no longer
// resolves (for partial records). Resets turnSeq to the max remaining Turn.
func (a *App) pruneCollapseOnRewind() {
	a.convMu.RLock()
	spans := turnSpansFromSlice(a.Conv)
	maxTurn := 0
	// For each remaining turn, check if it still ends with a clean answer.
	turnLastMsg := map[int]proxy.Message{}
	for _, m := range a.Conv {
		if m.Turn > 0 {
			turnLastMsg[m.Turn] = m
			if m.Turn > maxTurn {
				maxTurn = m.Turn
			}
		}
	}
	convSnap := a.Conv
	a.convMu.RUnlock()

	a.stateMu.Lock()
	if a.collapsed != nil {
		for t, rec := range a.collapsed {
			lastMsg, exists := turnLastMsg[t]
			if !exists {
				delete(a.collapsed, t)
				continue
			}
			if rec.Partial {
				s, sok := spans[t]
				if !sok || findBoundaryAssistantIdx(convSnap, s, rec.BoundaryCallID) <= s.first+1 {
					delete(a.collapsed, t)
				}
				continue
			}
			if !isCleanAnswer(lastMsg) {
				delete(a.collapsed, t)
			}
		}
	}
	if a.keep != nil {
		for t := range a.keep {
			if _, exists := turnLastMsg[t]; !exists {
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

// AddKeep exempts turn n from collapse. If the turn is already collapsed,
// the collapse record is deleted so the turn is sent in full again.
func (a *App) AddKeep(n int) error {
	if n <= 0 {
		return fmt.Errorf("invalid request number %d (must be > 0)", n)
	}
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	if a.keep == nil {
		a.keep = map[int]bool{}
	}
	a.keep[n] = true
	// If already collapsed, remove the record so it's sent in full again.
	if a.collapsed != nil {
		delete(a.collapsed, n)
	}
	return nil
}

// RemoveKeep removes the keep exemption for turn n, allowing it to be
// collapsed again at the next request.
func (a *App) RemoveKeep(n int) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	if a.keep != nil {
		delete(a.keep, n)
	}
}

// RestoreTurnState rebuilds the collapsed/keep maps from a saved Session and
// resets turnSeq to the max Turn in a.Conv. Called from LoadSession,
// ResumeConversation, and SetConv (daemon resume paths).
func (a *App) RestoreTurnState(s *Session) {
	a.stateMu.Lock()
	// Always reset: an empty/legacy session should clear any prior state.
	a.collapsed = map[int]CollapseRecord{}
	for _, rec := range s.Collapsed {
		a.collapsed[rec.Turn] = rec
	}
	a.keep = map[int]bool{}
	for _, t := range s.Keep {
		a.keep[t] = true
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
