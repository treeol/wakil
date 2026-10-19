package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/treeol/wakil/internal/memory"
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
	MemoryKey      string    `json:"memory_key,omitempty"`       // mid-tier memory index entry for this archive (auto)
	StashKey       string    `json:"stash_key,omitempty"`        // model-named memory stash for this collapse (optional)
	StashDesc      string    `json:"stash_desc,omitempty"`       // one-line description of the model-named stash
	// IndexRefreshFailed: the last re-put of MemoryKey failed; the retained
	// entry may be stale (older archive reference, no latest summary).
	IndexRefreshFailed bool `json:"index_refresh_failed,omitempty"`
	// Stash outcome flags for the LAST stash attempt on this record:
	// StashRefused — requested memory_key refused by namespacing.
	// StashWriteFailed — requested memory_key failed to write (store
	// unavailable or PutActive error). Mutually exclusive with success.
	StashRefused     bool `json:"stash_refused,omitempty"`
	StashWriteFailed bool `json:"stash_write_failed,omitempty"`
	// StashKeptFromPrior: the last attempt refused/failed, so StashKey
	// points at an EARLIER stash on this turn (not the requested one).
	StashKeptFromPrior bool `json:"stash_kept_from_prior,omitempty"`
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
	if rec.StashKey != "" {
		fmt.Fprintf(&b, "memory: %s — %s\n", rec.StashKey, rec.StashDesc)
	}
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

// indexCollapsedInMemory writes a mid-tier memory entry for a collapse archive
// (the auto-index). Memory is the recall/discovery layer; the on-disk archive
// is the durable record. Best-effort: failure never blocks the collapse and is
// only surfaced once per session. Key: ctx/<chatID>/<archive basename> — the
// basename is a CreateTemp unique name, stable across rewinds (turn numbers
// are reused after rewind, so they are never part of the key).
func (a *App) indexCollapsedInMemory(rec CollapseRecord, value string) string {
	if a.MemoryStore == nil {
		return ""
	}
	var key string
	if rec.MemoryKey != "" {
		key = rec.MemoryKey // refresh the same entry (cumulative re-put)
	} else {
		base := archiveBaseName(rec.Archive)
		if base == "" {
			return ""
		}
		key = "ctx/" + a.chatID() + "/" + base
	}
	expires := time.Now().Add(7 * 24 * time.Hour).UnixMilli()
	_, err := a.MemoryStore.PutActive(context.Background(), key, value, "collapse",
		memory.TierMid, a.AgentPrefix, a.chatID(), a.computeTainted(), &expires, nil,
		"auto-index of collapsed request working set (archive is the durable copy)")
	if err != nil {
		// Separate flag from collapseArchiveFailed: a memory-index failure
		// must not swallow the one-time warning for a later archive failure
		// (and vice versa) — different failure modes, different consequences.
		if !a.collapseMemoryFailed {
			a.collapseMemoryFailed = true
			fmt.Fprintln(a.Out, Yellow("⚠ collapse: memory index write failed — archive on disk is still the recovery path"))
		}
		return ""
	}
	return key
}

// stashCollapse writes the model-named memory stash for an explicit
// collapse_context call with memory_key: the distilled result of the
// just-completed work, stored under the model's chosen key with its
// description. This is the "distill and stash" loop — the raw chunk leaves
// context, one pointer stays, memory_get/memory_search recall it on demand.
// Keys are namespaced under ctx/<chatID>/ so parallel sessions don't collide.
// Mid-tier with a 7d TTL by default — durable writes are a promotion-policy
// decision and are never bypassed here. Best-effort: on failure the collapse
// itself stays intact (the on-disk archive is the durable copy).
func (a *App) stashCollapse(rec CollapseRecord, key, description string) (keyOut, descOut string, refused bool) {
	if a.MemoryStore == nil {
		fmt.Fprintln(a.Out, Yellow("⚠ stash: memory store unavailable — archive is still written"))
		return "", "", false
	}
	// Namespace enforcement: caller-supplied keys may only live under
	// ctx/<this-chatID>/. A bare key is auto-qualified; a qualified key must
	// be exactly this session's namespace — anything else (another ctx/
	// prefix, a foreign top-level key) could overwrite unrelated memory
	// (project notes, handoff records, promoted corrections) via a TierMid
	// re-put. Refusal is non-destructive: the caller keeps its prior key.
	ns := "ctx/" + a.chatID() + "/"
	suffix := ""
	if i := strings.Index(key, "/"); i >= 0 {
		if !strings.HasPrefix(key, ns) {
			fmt.Fprintln(a.Out, Yellow("⚠ stash: memory_key must live under "+ns+" (namespacing refused) — archive is still written"))
			return "", "", true
		}
		suffix = strings.TrimPrefix(key, ns)
	} else {
		suffix = key
		key = ns + key
	}
	// The key suffix must be a non-empty single path segment: no "..", no
	// ".", no slashes or backslashes, no path-like content — memory keys are
	// opaque to the store, so the caller must guarantee lexical sanity here.
	if suffix == "" || suffix == "." || suffix == ".." ||
		strings.ContainsAny(suffix, "/\\") {
		fmt.Fprintln(a.Out, Yellow("⚠ stash: memory_key suffix invalid (empty or path-like) — archive is still written"))
		return "", "", true
	}
	if description == "" {
		description = "model stash of collapsed working set (request " + fmt.Sprint(rec.Turn) + ")"
	}
	value := buildPartialCheckpointBlock(rec)
	if rec.StashDesc != "" && rec.StashDesc != description {
		value = rec.StashDesc + "\n\n" + value
	}
	expires := time.Now().Add(7 * 24 * time.Hour).UnixMilli()
	if _, err := a.MemoryStore.PutActive(context.Background(), key, value, "stash",
		memory.TierMid, a.AgentPrefix, a.chatID(), a.computeTainted(), &expires, nil,
		"model-named stash via collapse_context (archive is the durable copy)"); err != nil {
		fmt.Fprintln(a.Out, Yellow("⚠ stash: memory write failed — archive is still written at "+rec.Archive))
		return "", "", false
	}
	return key, description, false
}

// indexValueForTurn builds the auto-index memory value for a finished turn:
// truncated user text + final answer excerpt + preserved summary + note.
// Tool counts alone are a weak search target; including U, the answer, and
// Summary makes "what did we decide in request N" discoverable via
// memory_search even when the query is phrased from the answer (recall
// fixture finding — the answer is often the only distinctive text).
func (a *App) indexValueForTurn(t int, rec CollapseRecord) string {
	uText, aText := "", ""
	a.convMu.RLock()
	spans := turnSpansFromSlice(a.Conv)
	if s, ok := spans[t]; ok && s.first < len(a.Conv) {
		uText = strings.TrimSpace(DerefStr(a.Conv[s.first].Content))
		// Final answer = the last clean assistant message of the turn.
		for i := s.last; i > s.first; i-- {
			if isCleanAnswer(a.Conv[i]) {
				aText = strings.TrimSpace(DerefStr(a.Conv[i].Content))
				break
			}
		}
	}
	a.convMu.RUnlock()
	var b strings.Builder
	if uText != "" {
		fmt.Fprintf(&b, "request %d: %s\n", t, Truncate(uText, 200))
	}
	if aText != "" {
		fmt.Fprintf(&b, "answer: %s\n", Truncate(aText, 200))
	}
	if rec.Summary != "" {
		fmt.Fprintf(&b, "summary: %s\n", rec.Summary)
	}
	if rec.StashKey != "" {
		fmt.Fprintf(&b, "stash: %s — %s\n", rec.StashKey, rec.StashDesc)
	}
	b.WriteString(rec.Note)
	return b.String()
}

// archiveBaseName returns the final path element of a spill path.
func archiveBaseName(path string) string {
	if path == "" {
		return ""
	}
	if i := strings.LastIndexAny(path, "/\\"); i >= 0 {
		return path[i+1:]
	}
	return path
}

// degradePointerLine renders the compact projection of an old collapsed
// turn after it ages out of the tail: a truncated echo of the user request
// plus the preserved summary and the archive/memory recovery pointers.
// NOT byte-capped: the archive path and keys are the recovery references and
// are never truncated (spill paths are bounded by the OS path limit in
// practice); the truncated parts (U echo, summary, stash desc) are the only
// variable-size components.
func degradePointerLine(rec CollapseRecord, u []proxy.Message) string {
	var uText string
	for _, m := range u {
		if m.Role == "user" {
			uText = strings.TrimSpace(DerefStr(m.Content))
			break
		}
	}
	var b strings.Builder
	b.WriteString("[older request ")
	fmt.Fprintf(&b, "%d", rec.Turn)
	b.WriteString(" — details archived. ")
	b.WriteString(Truncate(uText, 80))
	b.WriteString("]\n")
	if rec.Summary != "" {
		b.WriteString("summary: ")
		b.WriteString(Truncate(rec.Summary, 200))
		b.WriteString("\n")
	}
	b.WriteString("full record: ")
	b.WriteString(rec.Archive)
	b.WriteString(" (read_file with offset/limit)\n")
	if rec.MemoryKey != "" {
		b.WriteString("memory index: ")
		b.WriteString(rec.MemoryKey)
		b.WriteString("\n")
	}
	if rec.StashKey != "" {
		b.WriteString("stash: ")
		b.WriteString(rec.StashKey)
		b.WriteString(" — ")
		b.WriteString(Truncate(rec.StashDesc, 120))
		b.WriteString("\n")
	}
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
	// Tier-2 degrade: in long-session mode, the ContextTailTurns most recent
	// collapsed turns stay at full size (U + A + note); older collapsed turns
	// degrade to a pointer line. Ranking is among collapsed turns only —
	// /keep, unfinished, and never-collapsed turns don't consume tail slots
	// (they're sent at full size regardless, by design: a kept or uncollapsed
	// turn is content the model should see verbatim). 0 disables (v1).
	tailTurns := a.Cfg.ContextTailTurns
	degradedTurn := map[int]bool{}
	if tailTurns > 0 {
		for t := range collapsedCopy {
			newer := 0
			for t2 := range collapsedCopy {
				if t2 > t && !collapsedCopy[t2].Partial {
					newer++
				}
			}
			if newer >= tailTurns {
				degradedTurn[t] = true
			}
		}
	}
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
		if degradedTurn[m.Turn] {
			// Tier-2 degrade: one pointer line as a user-role message keeps
			// the projection schema-valid (no leading orphan assistant turn).
			pointer := degradePointerLine(rec, a.Conv[s.first:s.first+1])
			content := pointer
			out = append(out, proxy.Message{Role: "user", Turn: m.Turn, Content: &content})
			emittedTurn[m.Turn] = true
			continue
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

// reconcileArchiveIndexes is the bounded, idempotent lifecycle reconciliation
// (plan phase 2): auto-index entries whose owned archive is CONFIRMED absent
// are tombstoned (Store.Forget) so memory_search no longer surfaces dead
// pointers. Runs once per session, triggered from the first collapse sweep.
//
// Safety rules:
//   - Auto-index entries only, identified by record ownership (a.MemoryKey of
//     a collapsed turn in THIS session), never by prefix prose-parsing.
//   - CONFIRMED absence only: stat says not-exist. I/O errors, permission
//     failures, or an unusable path → keep the entry (never delete on doubt).
//   - Stash entries are NEVER pruned here: they hold distilled value beyond
//     the archive (panel direction). Their archive references may go stale —
//     acceptable; the stash content itself is the recall target.
//   - Concurrent refresh safety: entries re-put since the stat are unaffected
//     (we only Forget keys whose archive is confirmed gone; a refresh targets
//     the same key, and a live archive is never confirmed gone).
//   - Legacy/foreign entries (no owning record in this session) are left
//     untouched — this pass has no provenance authority over them.
func (a *App) reconcileArchiveIndexes() {
	if a.MemoryStore == nil || a.indexReconciled {
		return
	}
	a.indexReconciled = true // once per session, even on partial failure

	a.stateMu.RLock()
	keys := make([][2]string, 0, len(a.collapsed)) // [memoryKey, archive]
	for _, rec := range a.collapsed {
		if rec.MemoryKey != "" && rec.Archive != "" {
			keys = append(keys, [2]string{rec.MemoryKey, rec.Archive})
		}
	}
	a.stateMu.RUnlock()

	for _, pair := range keys {
		key, archive := pair[0], pair[1]
		// Confirmed absence = stat fails with fs.ErrNotExist. Any other
		// error (or success) → keep the entry. Paths outside the toolcache
		// root return ErrHostCacheEscape — an error, not confirmed absence →
		// keep (conservative).
		if _, err := wtools.StatHostCacheFile(archive); err == nil {
			continue // archive alive
		} else if !errors.Is(err, fs.ErrNotExist) {
			continue // unknown → never prune on doubt
		}
		// Re-check ownership: the record may have been refreshed (rewound,
		// re-collapsed) between the snapshot and now. Only Forget the key
		// if some record still claims it AND that record's archive is the
		// confirmed-absent one. A refreshed entry pointing at a live archive
		// must survive.
		owned := false
		a.stateMu.RLock()
		for _, rec := range a.collapsed {
			if rec.MemoryKey == key && rec.Archive == archive {
				owned = true
				break
			}
		}
		a.stateMu.RUnlock()
		if !owned {
			continue
		}
		if err := a.MemoryStore.Forget(context.Background(), key); err != nil {
			// Best-effort: a failed tombstone is a stale entry, not data
			// loss. Warn once via the collapse-memory flag.
			if !a.collapseMemoryFailed {
				a.collapseMemoryFailed = true
				fmt.Fprintln(a.Out, Yellow("⚠ reconcile: failed to tombstone index entry for missing archive "+archive+" — entry may surface a dead pointer"))
			}
			continue
		}
		fmt.Fprintln(a.Out, Dim("· pruned index entry for missing archive "+archiveBaseName(archive)))
	}
}

// collapseFinishedTurns runs the collapse rules (§4) on all not-yet-collapsed
// turns. Called at the start of finalizeTurn (before eviction and Compact) and
// at the start of SendOutcome (before fitConvToWindow). Idempotent — already
// collapsed turns are skipped.
func (a *App) collapseFinishedTurns() {
	if !a.IsLongSession() {
		return
	}
	// Bounded lifecycle trigger (plan phase 2): once per session, before the
	// sweep, prune index entries whose archives are confirmed gone.
	a.reconcileArchiveIndexes()
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
		// The archive carries the FULL turn (U + W + A): the tier-2 degrade later
		// replaces even U and A with a pointer line, so the archive is then the
		// only durable copy of the question and answer. The note still describes
		// W only — that's what left the context.
		var w []proxy.Message
		for i := s.first; i <= s.last; i++ {
			w = append(w, convSnap[i])
		}
		var wOnly []proxy.Message
		for i := s.first + 1; i < s.last; i++ {
			wOnly = append(wOnly, convSnap[i])
		}
		archive := wtools.SpillToCache(a.chatID(), fmt.Sprintf("turn-%d", t), renderWorkingSet(w))
		if archive == "" {
			if !a.collapseArchiveFailed {
				a.collapseArchiveFailed = true
				fmt.Fprintln(a.Out, Yellow("⚠ collapse: archive write failed for turn "+fmt.Sprint(t)+" — working set kept in context"))
			}
			continue
		}

		note := buildCollapseNote(t, wOnly, archive)
		rec := CollapseRecord{
			Turn:    t,
			Archive: archive,
			WBytes:  wSize,
			Note:    note,
			At:      time.Now(),
			Summary: existingRec.Summary,
		}
		// Upgrading from a mid-turn partial collapse: preserve the model's
		// checkpoint metadata (stash pointer, next steps) and the existing
		// auto-index key — a finished turn must not silently lose the
		// "one pointer stays" promise made by collapse_context memory_key.
		// (A fresh record on a non-partial turn has none of these set.)
		// Stash-attempt flags carry over too: the upgrade performs no new
		// stash attempt, so the record must keep reporting the LAST attempt's
		// outcome, not silently claim a clean state.
		rec.NextSteps = existingRec.NextSteps
		rec.StashKey = existingRec.StashKey
		rec.StashDesc = existingRec.StashDesc
		rec.StashRefused = existingRec.StashRefused
		rec.StashWriteFailed = existingRec.StashWriteFailed
		rec.StashKeptFromPrior = existingRec.StashKeptFromPrior
		rec.MemoryKey = existingRec.MemoryKey
		// Auto-index into memory (best-effort): the value carries the user
		// request, the preserved summary, and the note so memory_search can
		// find the turn by its content, with the archive path as the
		// verbatim recovery reference. Always attempt a re-put on upgrade
		// from partial: the entry must gain indexValueForTurn content and
		// point at the FINISHED archive, not the stale partial one. Keep the
		// prior key on failure (entry may be stale — flagged, not dropped).
		newKey := a.indexCollapsedInMemory(rec, a.indexValueForTurn(t, rec))
		if newKey == "" && rec.MemoryKey != "" {
			rec.IndexRefreshFailed = true
		} else {
			rec.MemoryKey = newKey
			rec.IndexRefreshFailed = false
		}

		a.stateMu.Lock()
		a.collapsed[t] = rec
		a.stateMu.Unlock()

		// Report the true projected size: what the model actually sees for
		// this turn after collapse is U + A + note (not a fixed 0.4k — the
		// old hardcoded notice systematically overstated the shrink, since
		// A is often several KB; observed live as "notice says 0.4k but the
		// ctx gauge only dropped 13k").
		projected := messageSize(convSnap[s.first]) + messageSize(convSnap[s.last]) + len(rec.Note)
		fmt.Fprintln(a.Out, Dim(fmt.Sprintf(
			"· collapsed request %d — %dk → %dk sent (archived)", t, wSize/1024, projected/1024)))
	}
}

// stashFailureReason describes why the requested stash was not written, for
// the result caveat. Caller must not call it on a successful stash.
func stashFailureReason(rec CollapseRecord) string {
	switch {
	case rec.StashRefused:
		return "refused by namespacing"
	case rec.StashWriteFailed:
		return "write failed"
	default:
		return "unavailable"
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
		Summary     string `json:"summary"`
		NextSteps   string `json:"next_steps"`
		MemoryKey   string `json:"memory_key"`
		Description string `json:"description"`
	}
	if tc.Function.Arguments != "" {
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return "ERROR: invalid arguments for collapse_context: " + err.Error()
		}
	}
	args.Summary = strings.TrimSpace(args.Summary)
	args.NextSteps = strings.TrimSpace(args.NextSteps)
	args.MemoryKey = strings.TrimSpace(args.MemoryKey)
	args.Description = strings.TrimSpace(args.Description)
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
			res := fmt.Sprintf("collapse_context: current prefix up to this tool batch is already collapsed (archive: %s)", existingCur.Archive)
			if args.MemoryKey != "" {
				res += "\nNOTE: a new memory_key is ignored for an already-collapsed prefix — the earlier stash pointer is unchanged."
			}
			return res
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
	if args.MemoryKey != "" {
		key, desc, refused := a.stashCollapse(rec, args.MemoryKey, args.Description)
		switch {
		case key != "":
			rec.StashKey, rec.StashDesc = key, desc
		case refused:
			rec.StashRefused = true
		default:
			rec.StashWriteFailed = true
		}
		// Non-destructive on EVERY failure mode: a refused, failed, or
		// unavailable replacement must not silently drop the "one pointer
		// stays" promise of an earlier stash on a partial record.
		if rec.StashKey == "" && existingCur.Partial && existingCur.StashKey != "" {
			rec.StashKey, rec.StashDesc = existingCur.StashKey, existingCur.StashDesc
			rec.StashKeptFromPrior = true
		}
	} else if existingCur.Partial {
		// Repeated collapse without a new memory_key: keep the earlier
		// stash pointer — a later collapse must not silently drop the
		// "one pointer stays" promise of the first one.
		rec.StashKey, rec.StashDesc = existingCur.StashKey, existingCur.StashDesc
	}
	// Auto-index (mid-tier, 7d) — discovery layer on top of the archive.
	// Carry forward the existing key and refresh the same entry (the
	// checkpoint block is cumulative across repeated collapses). On refresh
	// failure the prior key is RETAINED — the entry may be stale, but the
	// pointer must not silently vanish from the collapse record.
	prevKey := existingCur.MemoryKey
	rec.MemoryKey = prevKey
	if newKey := a.indexCollapsedInMemory(rec, buildPartialCheckpointBlock(rec)); newKey != "" {
		rec.MemoryKey = newKey
	} else if prevKey != "" {
		rec.IndexRefreshFailed = true
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
	// Operation-specific memory caveats (1.4): the model must know which
	// recall paths are usable, not just "something failed".
	switch {
	case a.MemoryStore == nil:
		b.WriteString("\nNOTE: no memory store — recall is archive-read only (read_file the path above).")
	case rec.StashKeptFromPrior:
		fmt.Fprintf(&b, "\nNOTE: requested memory_key was not written (%s) — retained earlier stash %s; archive is the durable copy.",
			stashFailureReason(rec), rec.StashKey)
	case rec.StashRefused:
		fmt.Fprintf(&b, "\nNOTE: memory_key refused (must live under ctx/%s/); archive is the durable copy — recall via read_file %s.", a.chatID(), archive)
	case rec.StashWriteFailed:
		fmt.Fprintf(&b, "\nNOTE: memory stash write failed — archive is the durable copy; recall via read_file %s.", archive)
	case rec.IndexRefreshFailed:
		fmt.Fprintf(&b, "\nNOTE: memory index refresh failed — entry %s may be stale (older archive reference); archive is current at %s.", rec.MemoryKey, archive)
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
