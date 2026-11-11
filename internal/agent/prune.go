package agent

import (
	"encoding/json"
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
// counter. The model rates the PREVIOUS round with exactly one value in its
// next assistant message. Primary channel: the optional string property
// "prev" in tool call arguments — "keep | drop | stub: <one line to remember>"
// — which lets a pure tool-call turn (no text) rate the round it closes. The
// "prev" property is advertised in every tool schema when pruning is on,
// stripped from the arguments immediately after Stream returns (before the
// assistant message is stored) and again at handleToolCall entry, so it never
// reaches tool execution or the wire. Secondary channel: the [[...]] text
// tag in assistant content, parsed and stripped before storing. A missing
// or invalid rating means keep; an unrated round of at least 2 KB becomes
// eligible for the deterministic auto-stub fallback (Layer 2).
//
// Pruning is projection-only (like collapse): a.Conv keeps the originals;
// the projection replaces the CONTENT of each tool-result message of a
// pruned round with a one-line stub. Assistant messages, tool_calls and ids
// are never touched, so call/result pairing is always valid. Round records
// persist with the session.
//
// When context_prune is off (the default) nothing here parses, strips, or
// prunes anything — schemas, Conv and wire bytes are identical to master.
// /prune on|off flips this page per chat without writing the config.
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
	// RatingAuto marks an auto-stub (deterministic fallback for unrated
	// rounds ≥ autoStubMinBytes). Never model-supplied; it only signals
	// why this round was pruned.
	RatingAuto RoundRating = "auto"
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
	AutoNote  string      `json:"auto_note,omitempty"`  // deterministic stub text for auto-stubbed rounds
}

// stubTextMaxChars caps the model-supplied stub line.
const stubTextMaxChars = 200

// autoStubMinBytes is the round-size threshold for the auto-stub fallback:
// unrated rounds whose tool results total at least this many bytes become
// eligible for deterministic stubbing when nothing else applies.
const autoStubMinBytes = 2048

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
// Cfg is immutable after startup. The /prune per-chat override takes
// precedence over Cfg.ContextPrune; its stateMu read keeps this safe for
// RPC goroutines (session transitions) as well as the turn goroutine.
func (a *App) pruneEnabled() bool {
	if a.IsSubagent {
		return false
	}
	// Direct check, no RLock inside: some callers already hold stateMu
	// (e.g. buildPreamble via ensurePreamble's caller chains), and
	// Go's RWMutex is not reentrant — a nested RLock deadlocks.
	if a.pruneOverride != "" {
		return a.pruneOverride == "on"
	}
	return a.Cfg.ContextPrune == "on"
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
	case RatingAuto:
		return autoStubLine(rec, toolName, "", archive)
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
	var drops, stubs, autos []int
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
		// Never-inflate (Layer 4): a round whose results total less
		// than autoStubMinBytes is ineligible for auto-stubbing anyway,
		// but drop/stub with a stub ≥ original is also skipped later.
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
		case RatingNone:
			// Auto-stub fallback: unrated rounds with at least
			// autoStubMinBytes of tool results become eligible.
			// Exclusions already applied above: protected (last N),
			// /keep turns, collapsed turns, Pinned rounds. Write
			// rounds are NEVER auto-stubbed (write-class is never
			// pruned without an explicit model rating).
			if rec.Write || rec.BytesLost < autoStubMinBytes {
				continue
			}
			autos = append(autos, id)
		}
	}
	out := append([]int{}, drops...)
	out = append(out, stubs...)
	out = append(out, autos...)
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
	at, target := a.pruneThresholds()
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
		// Never-inflate: a one-line stub is ~100+ bytes with the archive
		// path. If the stub is not shorter than the original results, the
		// prune would grow the request — skip it (the archive stays on
		// disk, unreferenced; harmless).
		if stubLen := len(stubLine(rec, archiveToolName(convSnap, rec), archive)); stubLen >= rec.BytesLost {
			continue
		}
		// Record lives in the store, not the local snapshot. Merge the
		// archive outcome onto the live record under the lock, and update
		// both maps so the next pruneCheck sees it.
		isAuto := rec.Rating == RatingNone
		a.stateMu.Lock()
		if a.prunedRounds == nil {
			a.prunedRounds = map[int]bool{}
		}
		a.prunedRounds[id] = true
		if cur, ok := a.pruneRounds[id]; ok {
			if isAuto {
				cur.Rating = RatingAuto
			}
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
		switch {
		case rec.Rating == RatingDrop && !rec.Write:
			kind = "drop"
		case rec.Rating == RatingAuto:
			kind = "auto"
			a.stateMu.Lock()
			a.pruneAutoCount++
			a.stateMu.Unlock()
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

// archiveToolName returns the tool name of the round's first call (for the
// never-inflate length estimate and auto-stub line).
func archiveToolName(conv []proxy.Message, rec RoundRecord) string {
	if len(rec.CallIDs) == 0 {
		return ""
	}
	for _, m := range conv {
		for _, tc := range m.ToolCalls {
			if tc.ID == rec.CallIDs[0] {
				return tc.Function.Name
			}
		}
	}
	return ""
}

// autoStubLine renders the deterministic one-line stub for auto-stubbed
// rounds: "[rN auto-pruned: <tool> <target> (<size>) — full result: <archive>]".
// toolName and target come from the round's first call; size is the round's
// original tool-result bytes.
func autoStubLine(rec RoundRecord, toolName string, target string, archive string) string {
	return fmt.Sprintf("[r%d auto-pruned: %s%s (%s) — full result: %s]",
		rec.ID, toolName, target, formatKB(rec.BytesLost), archive)
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
	var keep, drop, stub, auto, unrated, prunedCount int
	for id, rec := range a.pruneRounds {
		switch rec.Rating {
		case RatingKeep:
			keep++
		case RatingDrop:
			drop++
		case RatingStub:
			stub++
		case RatingAuto:
			auto++
		default:
			unrated++
		}
		if rec.Pruned || a.prunedRounds[id] {
			prunedCount++
		}
	}
	saved := a.pruneBytesSaved
	a.stateMu.RUnlock()
	trigger, target := a.pruneThresholds()
	return fmt.Sprintf("prune: %s · keep %d · drop %d · stub %d · auto %d · unrated %d · pruned %d · saved ~%d KB · trigger %dk · target %dk",
		a.pruneStateWord(), keep, drop, stub, auto, unrated, prunedCount, saved/1024, trigger/1000, target/1000)
}

// pruneStateWord renders "on"/"off" from the effective state (override over
// config), so status output never contradicts the toggle that set it.
func (a *App) pruneStateWord() string {
	if a.pruneEnabled() {
		return "on"
	}
	return "off"
}

// pruneThresholds returns the effective (at, target) trigger sizes in
// CHARS (the hard-max unit) for this App. The absolute token keys
// (context_prune_at_tokens / context_prune_target_tokens) are converted
// with charsPerToken (4 chars/token — the codebase token estimate), then
// effective = min(tokens_chars, fraction × hardMax). Returns (at, target)
// in chars; both are 0 when no hard max is known.
func (a *App) pruneThresholds() (int, int) {
	_, _, hardMax := a.activeThresholds()
	if hardMax <= 0 {
		return 0, 0
	}
	atFrac := int(a.Cfg.ContextPruneAt * float64(hardMax))
	targetFrac := int(a.Cfg.ContextPruneTarget * float64(hardMax))
	atTok := a.Cfg.ContextPruneAtTokens * charsPerToken
	targetTok := a.Cfg.ContextPruneTargetTokens * charsPerToken
	at := atFrac
	if atTok > 0 && atTok < at {
		at = atTok
	}
	target := targetFrac
	if targetTok > 0 && targetTok < target {
		target = targetTok
	}
	if target > at {
		target = at
	}
	return at, target
}

// pruneToggleStatus renders the /prune status reply.
func (a *App) pruneToggleStatus() string {
	if a.pruneEnabled() {
		return "prune: on (schema property + prompt active from next request; existing stubs stay)"
	}
	return "prune: off (no new pruning; existing stubs stay; prompt paragraph off from next request)"
}

// ---------------------------------------------------------------------------
// prev via tool arguments (primary rating channel)
// ---------------------------------------------------------------------------

// PrevProp is the name of the optional tool-argument property carrying the
// previous-round rating.
const PrevProp = "prev"

// PrevPropSchema is the JSON-schema fragment for "prev".
var PrevPropSchema = map[string]interface{}{
	"type":        "string",
	"description": "rate the PREVIOUS tool round: keep | drop | stub: <one line to remember>",
}

// withPrevProperty returns tools with the "prev" property injected into every
// tool's JSON schema when pruning is enabled. The slice is cloned and each
// Parameters field is re-marshalled; shared package-level definitions are
// never mutated. Call only when prune is enabled; when off the tools are
// returned byte-identical to master (no injection).
func withPrevProperty(tools []proxy.Tool) []proxy.Tool {
	out := make([]proxy.Tool, len(tools))
	copy(out, tools)
	for i := range out {
		params := out[i].Function.Parameters
		if len(params) == 0 {
			continue
		}
		if !json.Valid(params) {
			continue
		}
		var obj map[string]interface{}
		if json.Unmarshal(params, &obj) != nil {
			continue
		}
		props, _ := obj["properties"].(map[string]interface{})
		if props == nil {
			props = map[string]interface{}{}
			obj["properties"] = props
		}
		if _, ok := props[PrevProp]; ok {
			continue // model tool already defines prev — leave it alone
		}
		props[PrevProp] = PrevPropSchema
		if b, err := json.Marshal(obj); err == nil {
			out[i].Function.Parameters = b
		}
	}
	return out
}

// stripPrevFromArgs removes the "prev" property from a tool-call arguments
// JSON string and returns (cleanedArgs, prevValue, hadPrev). When args do
// not contain "prev", the original string is returned verbatim (no
// re-marshal), so off-mode args stay byte-identical.
func stripPrevFromArgs(args string) (cleaned string, prevValue string, hadPrev bool) {
	if !strings.Contains(args, PrevProp) {
		return args, "", false
	}
	var obj map[string]interface{}
	if json.Unmarshal([]byte(args), &obj) != nil {
		return args, "", false
	}
	v, ok := obj[PrevProp]
	if !ok {
		return args, "", false
	}
	delete(obj, PrevProp)
	if s, ok := v.(string); ok {
		prevValue = s
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return args, prevValue, true
	}
	return string(b), prevValue, true
}

// stripPrevFromToolCall removes "prev" from the call's arguments in place
// and returns the prev value. prev MUST NOT reach tool execution.
func stripPrevFromToolCall(tc *proxy.ToolCall) string {
	cleaned, prev, _ := stripPrevFromArgs(tc.Function.Arguments)
	tc.Function.Arguments = cleaned
	return prev
}

// stripPrevFromMessages strips "prev" from every tool call in msgs' args.
// The prev values are returned in call order (last one wins, mirroring
// parsePruneTag's last-wins rule).
func stripPrevFromMessages(msgs []proxy.Message) []string {
	var prevs []string
	for i := range msgs {
		for j := range msgs[i].ToolCalls {
			if prev := stripPrevFromToolCall(&msgs[i].ToolCalls[j]); prev != "" {
				prevs = append(prevs, prev)
			}
		}
	}
	return prevs
}

// parsePrevValue parses a "prev" value ("keep | drop | stub: ...") with the
// same semantics as parsePruneTag: last-wins is unnecessary here (one value),
// invalid -> unrated, stub text capped at stubTextMaxChars.
func parsePrevValue(v string) (rating RoundRating, stubText string, found bool) {
	v = strings.TrimSpace(v)
	switch {
	case v == "keep":
		return RatingKeep, "", true
	case v == "drop":
		return RatingDrop, "", true
	case strings.HasPrefix(v, "stub:"):
		t := strings.TrimSpace(strings.TrimPrefix(v, "stub:"))
		if t == "" {
			return RatingNone, "", false
		}
		return RatingStub, truncateChars(t, stubTextMaxChars), true
	}
	return RatingNone, "", false
}

// recordPrevRating stores the last prev value onto the previous round
// (priorMax — the max round id created before the current append), with the
// same never-fall-through rule as the text channel. prevValues arrive in
// call order; the last one wins.
func (a *App) recordPrevRating(prevValues []string, priorMax int) {
	if !a.pruneEnabled() || len(prevValues) == 0 || priorMax <= 0 {
		return
	}
	rating, stubText, found := parsePrevValue(prevValues[len(prevValues)-1])
	if !found {
		return
	}
	a.stateMu.Lock()
	if rec, ok := a.pruneRounds[priorMax]; ok && rec.Rating == RatingNone {
		rec.Rating = rating
		rec.StubText = stubText
		a.pruneRounds[priorMax] = rec
	}
	a.stateMu.Unlock()
}

// pruneEnabledOverride is the /prune per-chat state. pruneOverride is the
// per-chat value ("on"/"off"); when empty the Cfg value governs.
func (a *App) pruneEnabledOverride() (string, bool) {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.pruneOverride, a.pruneOverride != ""
}

// effectivePrune reports whether pruning is active for this chat, honoring
// the /prune per-chat override before the config default.
func (a *App) effectivePrune() bool {
	override, set := a.pruneEnabledOverride()
	if set {
		return override == "on"
	}
	return a.Cfg.ContextPrune == "on"
}

// ---------------------------------------------------------------------------
// strip helpers used by turn_phases.go / app.go
// ---------------------------------------------------------------------------

// stripPrevFromMessage strips "prev" from every tool call in the message's
// arguments and binds the ratings to the previous round (last prev wins).
// Must be called immediately after Stream returns, BEFORE appendConv stores
// the message.
func (a *App) stripPrevFromMessage(msg *proxy.Message) {
	if !a.pruneEnabled() {
		return
	}
	prevs := stripPrevFromMessages([]proxy.Message{*msg})
	if len(prevs) == 0 {
		return
	}
	a.stateMu.RLock()
	priorMax := a.roundSeq
	a.stateMu.RUnlock()
	a.recordPrevRating(prevs, priorMax)
}

// stripPrevFromToolCallsBatch strips "prev" from a batch of tool calls and
// returns the prev values in call order.
func stripPrevFromToolCallsBatch(tcs []proxy.ToolCall) []string {
	var prevs []string
	for i := range tcs {
		if prev := stripPrevFromToolCall(&tcs[i]); prev != "" {
			prevs = append(prevs, prev)
		}
	}
	return prevs
}

// stripFinalTags removes all [[...]] rating tags from the final-answer text
// so the returned/stored/displayed final carries no tags. Layer 3.
func (a *App) stripFinalTags(s string) string {
	if !a.pruneEnabled() {
		return s
	}
	return stripPruneTags(s)
}

// ---------------------------------------------------------------------------
// stream tag buffer (Layer 3)
// ---------------------------------------------------------------------------

// tagBuffer accumulates assistant-text chunks when pruning is on so a
// [[...]] rating tag split across SSE deltas never reaches the user. Text
// from "[[" is buffered until the matching "]]" arrives (then dropped —
// it is a rating tag) or 200 chars accumulate (then flushed verbatim —
// the model typed "[[" as prose, not a tag). The tail is flushed via
// drain() at turn end.
type tagBuffer struct {
	pending string // bytes from the most recent "[[" not yet closed
}

func (b *tagBuffer) push(s string) string {
	if b.pending == "" {
		// Fast path: no open tag. Only buffer if this chunk contains "[[".
		if !strings.Contains(s, "[[") {
			return s
		}
	}
	b.pending += s
	for {
		// Find the close.
		if i := strings.Index(b.pending, "]]"); i >= 0 {
			// Drop everything up to and including the tag.
			left := b.pending[:i]
			rest := b.pending[i+2:]
			// Any "[[" in 'left' is prose before the tag — keep it.
			// The tag itself (from its "[[" to "]]") is dropped.
			if j := strings.LastIndex(left, "[["); j >= 0 {
				// Prose before the tag: keep bytes before the tag.
				// The tag spanned from j to i — drop those bytes.
				left = left[:j]
			}
			b.pending = rest
			// Emit left, then continue with rest (which may contain
			// further tags).
			// Note: if rest itself contains "[[" without "]]" yet,
			// it will be re-buffered on the next push; but if rest
			// contains MORE text we must emit now.
			merged := left + rest
			// Re-check: does merged contain another "[[...]]"?
			if !strings.Contains(merged, "[") {
				return merged
			}
			// Simplest correct path: emit left and continue buffering
			// rest on the next iteration via the accumulated pending.
			// Return left and keep rest pending — BUT we already
			// concatenated; fix by only emitting when no tag remains.
			// Fall through to the result below.
		}
		break
	}
	// No complete tag: only emit up to the last incomplete "[[".
	if i := strings.LastIndex(b.pending, "[["); i >= 0 {
		if len(b.pending)-i > 200 {
			// Over 200 chars without a close: not a tag, flush verbatim.
			flush := b.pending
			b.pending = ""
			return flush
		}
		// Keep the tag fragment buffered; emit only what precedes it.
		emit := b.pending[:i]
		b.pending = b.pending[i:]
		return emit
	}
	flush := b.pending
	b.pending = ""
	return flush
}

// drain returns and clears any buffered tail.
func (b *tagBuffer) drain() string {
	s := b.pending
	b.pending = ""
	return s
}

// drainTagBuffer is a no-op in this build: the sink's buffer is per-call,
// so there is nothing session-scoped to drain. Kept for symmetry with the
// sink design; retained so turn_phases can flush a future shared buffer.
func (a *App) drainTagBuffer() {}

// hasTag checks whether s contains a rating tag (used by tests/diagnostics).
func hasTag(s string) bool {
	return pruneTagRe.MatchString(s)
}
