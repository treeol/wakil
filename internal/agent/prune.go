package agent

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/treeol/wakil/internal/proxy"
	wtools "github.com/treeol/wakil/internal/tools"
)

// prune.go: dynamic context pruning by per-round value.
//
// A "round" is one assistant message carrying tool_calls plus the tool-result
// messages answering them. Each round gets a stable id r<N> from a per-chat
// counter. The model rates the PREVIOUS round with exactly one tag in its
// next assistant message: [[keep]], [[drop]], or [[stub: <one line>]].
// Tags are parsed once (last one wins), stripped from the stored content,
// and kept on the round record. Missing or invalid ratings mean keep.
//
// Pruning is projection-only (like collapse): a.Conv keeps the originals;
// the projection replaces the CONTENT of each tool-result message of a
// pruned round with a one-line stub. Assistant messages, tool_calls and ids
// are never touched, so call/result pairing is always valid. Round records
// persist with the session.
//
// When context_prune is off (the default) nothing here parses, strips, or
// prunes anything — Conv and wire bytes are identical to master.
//
// Round ids are assigned ONLY when prune is enabled. With the feature off,
// plan/current records and the projection carry no prune state. With it on,
// rounds are identified by their assistant message's position in Conv
// (order = appearance), so /keep turns keyed by Turn stay orthogonal: the
// /keep check reads the Turn stamped on the round's messages.

// RoundRating is how the model rated a round.
type RoundRating string

const (
	// RatingNone means no valid tag was seen for this round (keep semantics).
	RatingNone RoundRating = ""
	RatingKeep RoundRating = "keep"
	RatingDrop RoundRating = "drop"
	RatingStub RoundRating = "stub"
)

// RoundRecord is one pruned-eligible tool round in the session.
type RoundRecord struct {
	ID        int         `json:"id"`                   // r<N>; per-chat counter, never reused in-session
	CallIDs   []string    `json:"call_ids,omitempty"`   // tool_call ids of the round
	FirstCall string      `json:"first_call,omitempty"` // first tool_call id (positional anchor into Conv)
	ConvIdx   int         `json:"conv_idx,omitempty"`   // index in Conv of the assistant message that carried the calls
	Turn      int         `json:"turn,omitempty"`       // Conv turn of the round's messages (for /keep exemption)
	Rating    RoundRating `json:"rating,omitempty"`     // rating given by the NEXT round's assistant message
	StubText  string      `json:"stub_text,omitempty"`  // model-supplied stub line (≤ stubTextMaxChars)
	Pruned    bool        `json:"pruned,omitempty"`     // true once projected with stubs
	Archive   string      `json:"archive,omitempty"`    // spill path of the archived round
	Write     bool        `json:"write,omitempty"`      // round had write-class tool calls
	PrunedAt  time.Time   `json:"pruned_at,omitempty"`
	BytesLost int         `json:"bytes_lost,omitempty"` // original tool-result bytes removed by this prune
}

// stubTextMaxChars caps the model-supplied stub line.
const stubTextMaxChars = 200

// pruneTagRe matches one rating tag anywhere in assistant text.
// Groups: 1 = kind (keep|drop|stub), 3 = stub text (may be empty, up to
// stubTextMaxChars). The stub capture excludes "]"/"[" pairs so a nested
// tag inside a stub line is consumed as text up to the real close; simpler:
// the capture is any non-"]" chars, so the first "]]" closes the tag (a stub
// line never contains "]]" in practice).
var pruneTagRe = regexp.MustCompile(`\[\[(keep|drop|stub)(:([^\]]{0,200}))?\]\]`)

// parsePruneTag extracts the rating carried by assistant content. The LAST
// valid tag wins. Invalid or missing tags yield RatingNone (keep semantics).
// A stub tag must carry non-empty text (after trimming); an empty one is
// dropped entirely and does not count as a valid tag (an empty stub means
// the model said nothing to remember — treated as no rating). Text is
// preserved through tag stripping: stripping removes only the tag tokens,
// so any prose around the tag (or inside a stub line, e.g. a literal
// "[[keep]]" the model typed as prose) is kept verbatim in the stored
// content. Only the OUTERMOST tags are stripped: stripPruneTags removes
// every match of tags at all nesting levels, but a stub line's inner
// "[[keep]]" IS one of those matches — it is removed, which is correct
// because it is indistinguishable from a real control tag. In practice
// the model's stub line is a one-liner and never embeds the tag syntax; if
// it does, the inner text survives but the bracket token does not.
func parsePruneTag(content string) (rating RoundRating, stubText string, found bool) {
	matches := pruneTagRe.FindAllStringSubmatch(content, -1)
	for _, m := range matches {
		switch m[1] {
		case "keep":
			rating, stubText, found = RatingKeep, "", true
		case "drop":
			rating, stubText, found = RatingDrop, "", true
		case "stub":
			if t := strings.TrimSpace(m[3]); t != "" {
				rating, stubText, found = RatingStub, truncateChars(t, stubTextMaxChars), true
			}
		}
	}
	return rating, stubText, found
}

// stripPruneTags removes ALL rating tag tokens from content — including any
// bracket tokens embedded inside a stub line (see parsePruneTag's doc).
func stripPruneTags(content string) string {
	return pruneTagRe.ReplaceAllString(content, "")
}

// truncateChars truncates s to at most n runes (UTF-8 safe).
func truncateChars(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// pruneEnabled reports whether round pruning is active for this App.
// Cfg is immutable after startup, so no lock is needed for the check
// itself; a.Cfg.ContextPrune is only ever set at construction time.
func (a *App) pruneEnabled() bool {
	return !a.IsSubagent && a.Cfg.ContextPrune == "on"
}

/* RECORDING (called from appendConv; all under stateMu). */

// recordAssistantRound registers the just-appended assistant message with
// tool_calls as a new round (id assigned). convIdx is the index of that
// message in Conv (its position when appended).
func (a *App) recordAssistantRound(msgs []proxy.Message, convIdx int) {
	if !a.pruneEnabled() {
		return
	}
	for k, m := range msgs {
		if len(m.ToolCalls) == 0 {
			continue
		}
		callIDs := make([]string, 0, len(m.ToolCalls))
		write := false
		for _, tc := range m.ToolCalls {
			callIDs = append(callIDs, tc.ID)
			if writeTargetTools[tc.Function.Name] {
				write = true
			}
		}
		a.stateMu.Lock()
		if a.pruneRounds == nil {
			a.pruneRounds = map[int]RoundRecord{}
		}
		id := a.roundSeq + 1
		if maxID := maxRoundIDLocked(a.pruneRounds); id <= maxID {
			id = maxID + 1
		}
		a.roundSeq = id
		a.pruneRounds[id] = RoundRecord{
			ID:        id,
			CallIDs:   callIDs,
			FirstCall: callIDs[0],
			ConvIdx:   convIdx + k,
			Turn:      m.Turn,
			Write:     write,
		}
		a.stateMu.Unlock()
	}
}

func maxRoundIDLocked(rounds map[int]RoundRecord) int {
	max := 0
	for id := range rounds {
		if id > max {
			max = id
		}
	}
	return max
}

// recordRoundRating stores the rating carried by just-appended assistant
// text onto the PREVIOUS round: the max round id created BEFORE this
// append. It never falls through to an older round — if the previous round
// is already rated, the tag is ignored (the model emits at most one tag
// per round; a stray prose tag in a later message must not re-rate an
// older round).
func (a *App) recordRoundRating(msgs []proxy.Message, priorMax int) {
	if !a.pruneEnabled() {
		return
	}
	for _, m := range msgs {
		if m.Role != "assistant" || m.Content == nil || *m.Content == "" {
			continue
		}
		rating, stubText, found := parsePruneTag(*m.Content)
		if !found {
			continue
		}
		a.stateMu.Lock()
		if rec, ok := a.pruneRounds[priorMax]; ok && rec.Rating == RatingNone {
			rec.Rating = rating
			rec.StubText = stubText
			a.pruneRounds[priorMax] = rec
		}
		a.stateMu.Unlock()
	}
}

// recordToolResultBytes tallies original result bytes onto their rounds.
func (a *App) recordToolResultBytes(msgs []proxy.Message) {
	if !a.pruneEnabled() {
		return
	}
	a.stateMu.Lock()
	if len(a.pruneRounds) > 0 {
		byCall := map[string]int{}
		for id, rec := range a.pruneRounds {
			for _, cid := range rec.CallIDs {
				byCall[cid] = id
			}
		}
		for _, m := range msgs {
			if m.Role == "tool" && m.ToolCallID != "" {
				if id, ok := byCall[m.ToolCallID]; ok {
					rec := a.pruneRounds[id]
					rec.BytesLost += len(DerefStr(m.Content))
					a.pruneRounds[id] = rec
				}
			}
		}
	}
	a.stateMu.Unlock()
}

/* PROJECTION. */

// pruneProjection wraps the collapse projection: it replaces the CONTENT of
// each tool-result message of a pruned round with a one-line stub. Only
// tool-result contents change; assistant messages, tool_calls and ids are
// untouched, so call/result pairing stays valid. When nothing is pruned it
// returns the input unchanged (slice identity preserved).
func (a *App) pruneProjection(in []proxy.Message) []proxy.Message {
	if !a.pruneEnabled() {
		return in
	}
	a.stateMu.RLock()
	if len(a.prunedRounds) == 0 {
		a.stateMu.RUnlock()
		return in
	}
	callToRound := map[string]int{}
	for id, rec := range a.pruneRounds {
		if !a.prunedRounds[id] && !rec.Pruned {
			continue
		}
		for _, cid := range rec.CallIDs {
			callToRound[cid] = id
		}
	}
	// Copy records for the rounds being projected.
	recs := make(map[int]RoundRecord, len(a.prunedRounds))
	for id, rec := range a.pruneRounds {
		if a.prunedRounds[id] || rec.Pruned {
			recs[id] = rec
		}
	}
	a.stateMu.RUnlock()
	if len(callToRound) == 0 {
		return in
	}
	changed := false
	out := make([]proxy.Message, 0, len(in))
	for _, m := range in {
		id, isTool := callToRound[m.ToolCallID]
		if !isTool || m.ToolCallID == "" || m.Role != "tool" {
			out = append(out, m)
			continue
		}
		rec := recs[id]
		stub := stubLine(rec, m.Name, rec.Archive)
		c := m
		c.Content = &stub
		out = append(out, c)
		changed = true
	}
	if !changed {
		return in
	}
	return out
}

// stubLine renders the projection content for one pruned tool result.
func stubLine(rec RoundRecord, toolName string, archive string) string {
	id := fmt.Sprintf("r%d", rec.ID)
	switch rec.Rating {
	case RatingDrop:
		if rec.Write {
			return fmt.Sprintf("[%s write round pruned: %s — full result: %s]", id, toolName, archive)
		}
		return fmt.Sprintf("[%s pruned (drop): %s — full result: %s]", id, toolName, archive)
	case RatingStub:
		return fmt.Sprintf("[%s pruned: %s — full result: %s]", id, rec.StubText, archive)
	default:
		return fmt.Sprintf("[%s pruned — full result: %s]", id, archive)
	}
}

// pruneCandidates returns candidate round ids in prune order: drop rounds
// first (oldest first), then stub rounds (oldest first). Keep and unrated
// rounds are never candidates. Already-pruned rounds, rounds inside collapsed
// or /keep turns, and the most recent `protect` rounds are excluded.
// Callers pass rounds in Conv order (order slice); ordering is by position in
// that slice.
// roundHasPinned reports whether any tool result in Conv belonging to the
// round is Pinned. Pinned results (e.g. dispatch_subagent breadcrumbs) must
// never be stubbed by pruning, matching eviction's Pinned exemption.
func roundHasPinned(conv []proxy.Message, rec RoundRecord) bool {
	for _, m := range conv {
		if m.Role == "tool" && m.Pinned {
			for _, cid := range rec.CallIDs {
				if cid == m.ToolCallID {
					return true
				}
			}
		}
	}
	return false
}

func pruneCandidates(rounds map[int]RoundRecord, order []int, collapsedTurns, keepTurns map[int]bool, turnOfRound map[int]int, protect int, roundHasPinnedKeep map[int]bool) []int {
	protected := map[int]bool{}
	if len(order) > protect {
		for _, id := range order[len(order)-protect:] {
			protected[id] = true
		}
	} else {
		for _, id := range order {
			protected[id] = true
		}
	}
	var drops, stubs []int
	for _, id := range order {
		if protected[id] {
			continue
		}
		rec, ok := rounds[id]
		if !ok || rec.Pruned {
			continue
		}
		// Pinned rounds are never candidates (checked by the caller too,
		// but candidate lists alone stay safe for any future caller).
		if roundHasPinnedKeep[id] {
			continue
		}
		turn := turnOfRound[id]
		if turn > 0 && (collapsedTurns[turn] || keepTurns[turn]) {
			continue
		}
		if rec.Rating == RatingDrop && rec.Write {
			// Write-class drops are demoted to stubs (never dropped).
			stubs = append(stubs, id)
			continue
		}
		switch rec.Rating {
		case RatingDrop:
			drops = append(drops, id)
		case RatingStub:
			stubs = append(stubs, id)
		}
	}
	out := append([]int{}, drops...)
	out = append(out, stubs...)
	return out
}

// roundOrderFromConv returns round ids in Conv order plus turn-of-round.
// Round position is derived from ConvIdx, so rewind truncation and resume
// both re-derive the same order from the same Conv.
func roundOrderFromConv(conv []proxy.Message, rounds map[int]RoundRecord) (order []int, turnOfRound map[int]int) {
	turnOfRound = map[int]int{}
	var ids []int
	for id := range rounds {
		ids = append(ids, id)
	}
	// Order by scanning Conv: a round's position is where its FirstCall
	// appears in Conv — never the stored ConvIdx, which compaction,
	// materialization, and rewind can shift. Records whose FirstCall is
	// absent (removed rounds) are excluded so dead rounds never take
	// protect slots or get archived. Turn is re-read from the live
	// message, not the stored record.
	byFirst := make(map[string]int, len(rounds))
	for id, rec := range rounds {
		if rec.FirstCall != "" {
			byFirst[rec.FirstCall] = id
		}
	}
	seen := make(map[int]bool, len(rounds))
	for i := range conv {
		m := conv[i]
		for _, tc := range m.ToolCalls {
			if id, ok := byFirst[tc.ID]; ok && !seen[id] {
				seen[id] = true
				order = append(order, id)
				turnOfRound[id] = m.Turn
			}
		}
	}
	return order, turnOfRound
}

/* TRIGGER. */

// pruneCheck runs after tool results are appended and before the next model
// request is built (invoked from the tool loop and, via the projection path,
// at request boundaries). It prunes candidates until the projected size is
// ≤ target, or no candidates remain. It never prunes rounds inside collapsed
// turns or /keep turns, never the protected recent rounds, and only after a
// successful archive write.
func (a *App) pruneCheck() {
	if !a.pruneEnabled() {
		return
	}
	_, _, hardMax := a.activeThresholds()
	if hardMax <= 0 {
		return
	}
	at := int(a.Cfg.ContextPruneAt * float64(hardMax))
	target := int(a.Cfg.ContextPruneTarget * float64(hardMax))
	if at <= 0 || target <= 0 {
		return
	}
	if size := a.sentSize(a.contextProjection()); size <= at {
		return
	}
	a.convMu.RLock()
	convSnap := append([]proxy.Message(nil), a.Conv...)
	a.convMu.RUnlock()
	a.stateMu.RLock()
	if len(a.pruneRounds) == 0 {
		a.stateMu.RUnlock()
		return
	}
	rounds := make(map[int]RoundRecord, len(a.pruneRounds))
	for id, rec := range a.pruneRounds {
		rounds[id] = rec
	}
	collapsedTurns := map[int]bool{}
	for t := range a.collapsed {
		collapsedTurns[t] = true
	}
	keepTurns := map[int]bool{}
	for t := range a.keep {
		keepTurns[t] = true
	}
	a.stateMu.RUnlock()

	order, turnOfRound := roundOrderFromConv(convSnap, rounds)
	// Pinned rounds: any round carrying a Pinned tool result is ineligible
	// (the breadcrumb must survive, per turn_phases.go + eviction). Filter
	// them out before candidate selection.
	pinnedRounds := make(map[int]bool, len(rounds))
	for id, rec := range rounds {
		if roundHasPinned(convSnap, rec) {
			pinnedRounds[id] = true
		}
	}
	candidates := pruneCandidates(rounds, order, collapsedTurns, keepTurns, turnOfRound, a.Cfg.ContextPruneProtect, pinnedRounds)

	for _, id := range candidates {
		if size := a.sentSize(a.contextProjection()); size <= target {
			break
		}
		rec := rounds[id]
		if rec.Pruned {
			continue
		}
		// Archive first; only mark pruned when the write succeeded.
		archive := wtools.SpillToCache(a.chatID(), fmt.Sprintf("round-%d", id), renderRoundArchive(convSnap, rec))
		if archive == "" {
			if !a.pruneArchiveFailed {
				a.pruneArchiveFailed = true
				fmt.Fprintln(a.Out, Yellow("⚠ prune: archive write failed for round "+fmt.Sprint(id)+" — round kept in context"))
			}
			continue
		}
		// Record lives in the store, not the local snapshot. Merge the
		// archive outcome onto the live record under the lock, and update
		// both maps so the next pruneCheck sees it.
		a.stateMu.Lock()
		if a.prunedRounds == nil {
			a.prunedRounds = map[int]bool{}
		}
		a.prunedRounds[id] = true
		if cur, ok := a.pruneRounds[id]; ok {
			cur.Pruned = true
			cur.Archive = archive
			cur.PrunedAt = time.Now()
			a.pruneRounds[id] = cur
			rec = cur
		}
		a.pruneBytesSaved += rec.BytesLost
		a.stateMu.Unlock()
		rounds[id] = rec
		kind := "stub"
		if rec.Rating == RatingDrop && !rec.Write {
			kind = "drop"
		}
		fmt.Fprintln(a.Out, Dim(fmt.Sprintf("· pruned r%d (%s, %s)", id, kind, formatKB(rec.BytesLost))))
	}
}

// renderRoundArchive renders the full round (assistant message incl. tool_call
// ids + all results) for archiving.
func renderRoundArchive(conv []proxy.Message, rec RoundRecord) string {
	want := map[string]bool{}
	for _, cid := range rec.CallIDs {
		want[cid] = true
	}
	var b strings.Builder
	emittedAssistant := make(map[int]bool)
	for i, m := range conv {
		if m.Role == "assistant" && !emittedAssistant[i] {
			for _, tc := range m.ToolCalls {
				if want[tc.ID] {
					b.WriteString(renderWorkingSet([]proxy.Message{m}))
					emittedAssistant[i] = true
					break
				}
			}
		}
		if m.Role == "tool" && want[m.ToolCallID] {
			b.WriteString(renderWorkingSet([]proxy.Message{m}))
		}
	}
	return b.String()
}

func formatKB(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%d KB", n/1024)
}

/* Session persistence. */

// pruneRecordsSlice returns round records for persistence.
func (a *App) pruneRecordsSlice() []RoundRecord {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	if len(a.pruneRounds) == 0 {
		return nil
	}
	out := make([]RoundRecord, 0, len(a.pruneRounds))
	for _, rec := range a.pruneRounds {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// restorePruneState rebuilds prune maps from a saved Session. Caller holds
// stateMu (restoreTurnStateLocked) — this function does not lock.
func (a *App) restorePruneState(s *Session) {
	a.pruneRounds = map[int]RoundRecord{}
	a.prunedRounds = map[int]bool{}
	maxID := 0
	for _, rec := range s.PruneRecords {
		a.pruneRounds[rec.ID] = rec
		if rec.Pruned {
			a.prunedRounds[rec.ID] = true
		}
		if rec.ID > maxID {
			maxID = rec.ID
		}
	}
	a.roundSeq = maxID
	a.pruneBytesSaved = s.PruneBytesSaved
	a.pruneArchiveFailed = s.PruneArchiveFailed
}

// pruneStatusLine renders the /mode status addition.
func (a *App) pruneStatusLine() string {
	a.stateMu.RLock()
	var keep, drop, stub, unrated, prunedCount int
	for id, rec := range a.pruneRounds {
		switch rec.Rating {
		case RatingKeep:
			keep++
		case RatingDrop:
			drop++
		case RatingStub:
			stub++
		default:
			unrated++
		}
		if rec.Pruned || a.prunedRounds[id] {
			prunedCount++
		}
	}
	saved := a.pruneBytesSaved
	a.stateMu.RUnlock()
	return fmt.Sprintf("prune: %s · keep %d · drop %d · stub %d · unrated %d · pruned %d · saved ~%d KB",
		a.Cfg.ContextPrune, keep, drop, stub, unrated, prunedCount, saved/1024)
}
