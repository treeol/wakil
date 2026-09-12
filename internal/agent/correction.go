package agent

// correction.go implements the Correction-Capture Learning Loop (Card #192).
//
// Users repeat the same corrections across sessions: "Don't use var, use const."
// "Run tests with make test not go test." The agent forgets between sessions
// because no one captures the pattern. This module closes the loop:
//
//  1. Detect — identify a correction signal (user revert via /rewind, or an
//     explicit negation pattern in the user's next message).
//  2. Propose — construct a memory entry with the correction, provenance, and
//     a suggested key.
//  3. Confirm — ask the user (via a.Confirm) whether to store it. Corrections
//     are NEVER stored without explicit user confirmation. The
//     "correction_capture" tool name is carved out in SuspendAuto so /auto
//     mode and policy "allow" rules cannot bypass the interactive prompt.
//  4. Store — write a durable proposed memory entry. The user must promote it
//     to active via memory_promote before it influences future turns. This
//     respects the "never claim to have learned anything" rule: the entry
//     starts as proposed, not active.
//  5. Auto-apply — once promoted, retrieveMemoryContext (retrieval.go) already
//     surfaces it to the model at the start of relevant turns via FTS5 search.
//
// Detection signals:
//   - SignalRevert: /rewind was used in the previous turn, and the user's new
//     message re-asks for something related (the rewind undid work the agent
//     will now redo differently — the user's new message is the correction).
//   - SignalExplicit: the user's message contains a negation/correction
//     pattern ("no, use X not Y", "stop doing Z", "I wanted W, not V").
//
// False positive control:
//   - Only the PARENT agent (not subagents) runs detection.
//   - Detection patterns are conservative: they match at word boundaries and
//     require directive structure (not just substring presence). Questions
//     like "can I use X instead of Y?" do NOT trigger — the pattern requires
//     an imperative or declarative correction form.
//   - For SignalRevert, the message must reference a restored file path or
//     contain an explicit correction pattern — not just be any substantive
//     message after a /rewind.
//   - The Confirm gate (SuspendAuto carve-out) means the user reviews every
//     proposal — false positives are rejected by the user. Session-level
//     counters (correctionProposals/Accepted/Rejected) track the rejection
//     rate within the current session.
//   - The memory store requires promotion (memory_promote) before the entry
//     becomes active — a two-step gate (confirm + promote) prevents accidental
//     pollution of the model's context.
//   - Secret screening: user text is scanned for common secret patterns before
//     being stored. If a secret is detected, the proposal is refused.

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// correctionSignal identifies what kind of correction was detected.
type correctionSignal int

const (
	// SignalNone means no correction detected.
	SignalNone correctionSignal = iota
	// SignalRevert means /rewind was used and the user's new message is a
	// correction of the reverted work.
	SignalRevert
	// SignalExplicit means the user's message contains an explicit negation
	// or correction pattern.
	SignalExplicit
)

// correctionState holds the in-session state for the correction-capture loop.
// Embedded in App. Not persisted — corrections that weren't confirmed are
// intentionally lost (no dangling proposals across sessions).
type correctionState struct {
	// lastRewind stores the result of the most recent /rewind call that
	// actually undid work (TurnsRewound > 0 and no errors). When non-nil, the
	// next user message is a candidate for correction capture.
	lastRewind *rewindResult

	// lastRewindAt is the timestamp of the last /rewind. Used to expire the
	// signal: if the user does a /rewind and then waits a long time before
	// sending a message, the rewind context is stale.
	lastRewindAt time.Time

	// correctionProposals/Accepted/Rejected count session-level metrics.
	correctionProposals int
	correctionAccepted  int
	correctionRejected  int
}

// correctionDetectWindow is how long after a /rewind the detection signal
// stays valid. If the user rewinds and then waits more than this duration
// before sending a message, the rewind context is considered stale.
const correctionDetectWindow = 10 * time.Minute

// correctionMaxUserTextLen caps the user message length used for proposal
// construction. Corrections longer than this are truncated to keep the memory
// entry concise.
const correctionMaxUserTextLen = 500

// detectCorrection checks whether the user's message constitutes a correction
// of prior agent behavior, and if so, returns the signal type and a proposed
// memory entry. Does NOT store anything — the caller must call
// proposeCorrection to surface it to the user.
//
// Returns (SignalNone, nil) when no correction is detected.
func (a *App) detectCorrection(userText string) (correctionSignal, *correctionProposal) {
	if a.IsSubagent {
		return SignalNone, nil
	}

	// Signal 1: /rewind was used recently — the user's new message is the
	// correction for the reverted work. Only trigger if the user's message
	// references the reverted work (mentions a restored file path, or
	// contains an explicit correction pattern). A bare substantive message
	// like "no, I don't think that's a problem" after a rewind should NOT
	// trigger a correction prompt.
	if a.lastRewind != nil && time.Since(a.lastRewindAt) < correctionDetectWindow {
		if isSubstantiveCorrection(userText) && referencesRewindOrCorrection(userText, a.lastRewind) {
			proposal := a.buildRewindProposal(userText)
			if proposal != nil {
				return SignalRevert, proposal
			}
		}
	}

	// Signal 2: explicit negation/correction pattern in the message.
	if matches := detectExplicitCorrection(userText); matches != nil {
		proposal := a.buildExplicitProposal(userText, matches)
		if proposal != nil {
			return SignalExplicit, proposal
		}
	}

	return SignalNone, nil
}

// correctionProposal holds the proposed memory entry before it's stored.
type correctionProposal struct {
	Key     string
	Value   string
	Kind    string
	Anchors []string
	Signal  correctionSignal
}

// buildRewindProposal constructs a memory proposal from a /rewind + new
// message sequence. The key captures the file paths that were reverted
// (if any), and the value captures the user's correction text.
func (a *App) buildRewindProposal(userText string) *correctionProposal {
	rr := a.lastRewind
	if rr == nil {
		return nil
	}

	// Build a descriptive key from the reverted file paths. When the rewind
	// restored specific files, the key names them so future retrieval by
	// similar file context can find the correction.
	keyParts := []string{"correction/revert"}
	for i, p := range rr.RestoredPaths {
		if i >= 3 {
			break
		}
		keyParts = append(keyParts, p)
	}

	// Build the value: a structured correction note with provenance.
	var b strings.Builder
	b.WriteString("User correction after /rewind")
	if len(rr.RestoredPaths) > 0 {
		b.WriteString(" (restored files: ")
		b.WriteString(strings.Join(rr.RestoredPaths[:min(len(rr.RestoredPaths), 5)], ", "))
		b.WriteString(")")
	}
	b.WriteString(":\n")
	b.WriteString(truncateForMemory(userText))

	// Anchors: the reverted file paths, so staleness tracking can detect
	// when those files change.
	var anchors []string
	for _, p := range rr.RestoredPaths {
		anchors = append(anchors, p)
		if len(anchors) >= 5 {
			break
		}
	}

	return &correctionProposal{
		Key:     strings.Join(keyParts, "/"),
		Value:   b.String(),
		Kind:    "correction",
		Anchors: anchors,
		Signal:  SignalRevert,
	}
}

// buildExplicitProposal constructs a memory proposal from an explicit
// correction pattern in the user's message (e.g., "no, use const not var").
func (a *App) buildExplicitProposal(userText string, matches []correctionMatch) *correctionProposal {
	// Build a key from the most significant correction pattern.
	keyPart := "correction/explicit"
	if len(matches) > 0 {
		// Use a snippet of the corrected term as the key suffix.
		term := matches[0].corrected
		if len(term) > 40 {
			term = term[:40]
		}
		// Clean for use as a key path component.
		term = strings.Map(func(r rune) rune {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
				return r
			}
			return '-'
		}, term)
		if term != "" {
			keyPart = "correction/" + term
		}
	}

	var b strings.Builder
	b.WriteString("User correction (explicit):\n")
	b.WriteString(truncateForMemory(userText))

	return &correctionProposal{
		Key:    keyPart,
		Value:  b.String(),
		Kind:   "correction",
		Signal: SignalExplicit,
	}
}

// proposeCorrection surfaces a correction proposal to the user via the Confirm
// gate. If the user approves, the correction is stored as a durable proposed
// memory entry (requires memory_promote to become active). If the user
// declines, the proposal is silently dropped.
//
// Returns true if the correction was stored (user approved + store succeeded).
func (a *App) proposeCorrection(ctx context.Context, signal correctionSignal, p *correctionProposal) bool {
	if p == nil || a.MemoryStore == nil {
		return false
	}
	if a.Confirm == nil {
		return false // no interactive confirmer — never auto-store
	}

	// Secret screening: refuse to store user text that looks like it contains
	// credentials. This prevents accidental persistence of secrets pasted by
	// the user during a correction.
	if containsSecretPattern(p.Value) {
		if a.Out != nil {
			fmt.Fprintln(a.Out, Dim("· correction proposal skipped — text may contain secrets"))
		}
		return false
	}

	// Build the confirmation prompt.
	signalLabel := "explicit correction"
	if signal == SignalRevert {
		signalLabel = "correction after /rewind"
	}

	headline := "💾 Store correction as a memory entry?"
	detail := fmt.Sprintf(
		"Detected: %s\n\n"+
			"Proposed memory key: %s\n"+
			"Content:\n%s\n\n"+
			"This will be stored as a PROPOSED entry (needs memory_promote to become active).\n"+
			"Future sessions can retrieve it via memory_search when working on similar tasks.",
		signalLabel, p.Key, p.Value)

	a.correctionProposals++

	// The "correction_capture" tool name is carved out in SuspendAuto so /auto
	// mode and policy "allow" rules cannot bypass this prompt. The readAction
	// is false — this is a write (durable memory storage), not a read.
	if !a.Confirm("correction_capture", headline, detail, false) {
		a.correctionRejected++
		if a.Out != nil {
			fmt.Fprintln(a.Out, Dim("· correction proposal declined — not stored"))
		}
		return false
	}

	// Store as a durable proposed entry. Compute taint directly (we call the
	// store rather than going through handleMemoryPut).
	tainted := a.computeTainted()

	entry, err := a.MemoryStore.PutProposed(ctx, p.Key, p.Value, p.Kind,
		a.AgentPrefix, a.chatID(), tainted, p.Anchors,
		fmt.Sprintf("correction-capture: %s", signalLabel))
	if err != nil {
		if a.Out != nil {
			fmt.Fprintf(a.Out, "⚠ correction store failed: %v\n", err)
		}
		return false
	}

	a.correctionAccepted++
	if a.Out != nil {
		fmt.Fprintf(a.Out, Dim("· correction stored as proposed memory entry: %s [id: %d] — use memory_promote to activate\n"),
			p.Key, entry.ID)
	}
	return true
}

// SetLastRewind records that a /rewind was performed, so the next user
// message can be evaluated as a correction. Only records rewinds that
// actually undid work (TurnsRewound > 0 and no errors) — a no-op rewind
// (invalid N, active turn) is not a correction signal.
// Called from the /rewind command handler in commands.go.
func (a *App) SetLastRewind(result *rewindResult) {
	if a.IsSubagent {
		return
	}
	// Don't record no-op or failed rewinds.
	if result == nil || result.TurnsRewound == 0 || len(result.Errors) > 0 {
		return
	}
	a.lastRewind = result
	a.lastRewindAt = time.Now()
}

// ClearLastRewind invalidates the rewind signal. Called internally after
// the correction is consumed or expired.
func (a *App) ClearLastRewind() {
	a.lastRewind = nil
}

// ── Detection helpers ──────────────────────────────────────────────────────

// isSubstantiveCorrection checks if the user's message is substantive enough
// to be a correction (not a greeting, ack, or slash command).
func isSubstantiveCorrection(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" || strings.HasPrefix(text, "/") {
		return false
	}
	// Require at least a few words — "yes" or "ok" after a rewind is not a
	// correction, it's an acknowledgment.
	return len(strings.Fields(text)) >= 3
}

// referencesRewindOrCorrection checks whether the user's message references the
// reverted work (mentions a restored file path) or contains an explicit
// correction pattern. This prevents every substantive message after a /rewind
// from triggering a blocking correction prompt.
func referencesRewindOrCorrection(text string, rr *rewindResult) bool {
	if rr == nil {
		return false
	}
	// Check if the message mentions any of the restored file paths (by basename
	// or full path). This is the strongest signal that the user is correcting
	// the reverted work.
	lower := strings.ToLower(text)
	for _, p := range rr.RestoredPaths {
		// Match both the full path and the basename.
		if strings.Contains(lower, strings.ToLower(p)) {
			return true
		}
		// Check basename (last path component).
		base := p
		if idx := strings.LastIndex(p, "/"); idx >= 0 {
			base = p[idx+1:]
		}
		if base != p && strings.Contains(lower, strings.ToLower(base)) {
			return true
		}
	}

	// Check if the message contains an explicit correction pattern. If the
	// user is using correction language after a rewind, it's likely a correction
	// of the reverted work.
	return detectExplicitCorrection(text) != nil
}

// correctionMatch represents a detected correction pattern in user text.
type correctionMatch struct {
	// pattern is the matched pattern type.
	pattern string
	// corrected is a snippet of the text following the correction marker.
	corrected string
}

// detectExplicitCorrection scans user text for explicit negation/correction
// patterns. Returns nil if no pattern is found.
//
// Patterns matched (case-insensitive, at word/sentence boundaries):
//   - "no, " at the start of the message — direct negation
//   - "don't use X" / "don't do X" — prohibition
//   - "use X not Y" / "use X instead of Y" — preference correction
//   - "stop doing" / "stop using" — behavior correction
//   - "I wanted X" / "I said X" / "I meant X" — intent correction
//   - "instead of X" / "rather than X" — alternative correction (only when
//     preceded by an imperative verb: use, try, do, run, make)
//
// The patterns are deliberately conservative: they require the pattern to
// appear at a sentence boundary (start of message or after punctuation) or
// as an imperative directive, NOT as a substring inside a larger word or
// as part of a question. This prevents false positives like:
//   - "The cause is not known" — "use " does not appear as a word
//   - "Can I use X instead of Y?" — a question, not a correction
//   - "I wanted to ask about deployment" — "I wanted to" is not a correction
func detectExplicitCorrection(text string) []correctionMatch {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" || strings.HasPrefix(lower, "/") {
		return nil
	}

	var matches []correctionMatch

	// Helper: extract a snippet of the text after a matched prefix.
	// Uses the lowercased string for searching AND slicing to avoid byte-index
	// misalignment when ToLower changes byte lengths (e.g. 'İ' → 2→3 bytes).
	snippet := func(full, prefix string, n int) string {
		lowered := strings.ToLower(full)
		idx := strings.Index(lowered, prefix)
		if idx < 0 {
			return ""
		}
		rest := lowered[idx+len(prefix):]
		rest = strings.TrimSpace(rest)
		if len(rest) > n {
			rest = rest[:n]
		}
		return rest
	}

	// Pattern 1: message starts with "no, " — direct negation/correction.
	// "no, use const not var" → correction
	// "no, that's not what I wanted" → correction
	// "no, I said to use interfaces" → correction
	if strings.HasPrefix(lower, "no, ") {
		s := snippet(text, "no, ", 100)
		if s != "" {
			matches = append(matches, correctionMatch{pattern: "no", corrected: s})
		}
	}

	// Pattern 2: "don't use X" / "don't do X" / "don't " + verb
	// Must appear at a sentence boundary (start, or after ". " / "! ").
	if atSentenceBoundary(lower, "don't ") {
		for _, pat := range []string{"don't use ", "don't do ", "don't "} {
			if atSentenceBoundary(lower, pat) {
				s := snippet(text, pat, 100)
				if s != "" {
					matches = append(matches, correctionMatch{pattern: "dont", corrected: s})
				}
				break
			}
		}
	}

	// Pattern 3: "use X not Y" / "use X instead of Y"
	// Must have "use " at a sentence boundary (not as a substring of "cause").
	if atSentenceBoundary(lower, "use ") {
		if strings.Contains(lower, " not ") || strings.Contains(lower, " instead of ") {
			s := snippet(text, "use ", 100)
			if s != "" {
				matches = append(matches, correctionMatch{pattern: "use-not", corrected: s})
			}
		}
	}

	// Pattern 4: "stop doing" / "stop using" — behavior correction
	if atSentenceBoundary(lower, "stop ") {
		for _, pat := range []string{"stop doing ", "stop using "} {
			if strings.Contains(lower, pat) {
				s := snippet(text, pat, 100)
				if s != "" {
					matches = append(matches, correctionMatch{pattern: "stop", corrected: s})
				}
				break
			}
		}
	}

	// Pattern 5: "I wanted X" / "I said X" / "I meant X" — intent correction
	// Must appear at a sentence boundary. "I wanted to ask about X" is NOT a
	// correction — it's a question/request. Only trigger when the phrase is
	// followed by a correction context (not, not Y, different, instead).
	// We check for "I wanted/said/meant" followed by a negation or alternative
	// within the same sentence.
	for _, pat := range []string{"i wanted ", "i said ", "i meant "} {
		if atSentenceBoundary(lower, pat) {
			// Only match if the sentence contains a correction marker
			// (not, instead, different, wrong, should).
			rest := lower[strings.Index(lower, pat)+len(pat):]
			// Check if this sentence (up to next sentence boundary) contains
			// a correction context.
			sentenceEnd := sentenceEndPos(rest)
			sentence := rest[:sentenceEnd]
			if containsCorrectionContext(sentence) {
				s := snippet(text, pat, 100)
				if s != "" {
					matches = append(matches, correctionMatch{pattern: "intent", corrected: s})
				}
				break
			}
		}
	}

	// Pattern 6: "instead of X" / "rather than X" — alternative correction
	// Only match when preceded by an imperative verb (use, try, do, run, make)
	// to distinguish a correction from a neutral question.
	for _, pat := range []string{"instead of ", "rather than "} {
		if idx := strings.Index(lower, pat); idx > 0 {
			// Check if preceded by an imperative verb.
			before := lower[:idx]
			if hasImperativeVerb(before) {
				s := snippet(text, pat, 100)
				if s != "" {
					matches = append(matches, correctionMatch{pattern: "alternative", corrected: s})
				}
				break
			}
		}
	}

	if len(matches) == 0 {
		return nil
	}
	return matches
}

// atSentenceBoundary checks whether the pattern appears at the start of the
// text or after a sentence boundary (". ", "! ", "? ", newline). This prevents
// matching patterns as substrings inside larger words (e.g., "use " inside
// "cause " — though "cause" doesn't contain "use" as a word, the substring
// check would match).
func atSentenceBoundary(lower, pat string) bool {
	if strings.HasPrefix(lower, pat) {
		return true
	}
	// Check after sentence-ending punctuation + space.
	for _, sep := range []string{". ", "! ", "? ", "\n"} {
		idx := strings.Index(lower, sep+pat)
		if idx >= 0 {
			return true
		}
	}
	return false
}

// sentenceEndPos returns the position of the first sentence boundary in s
// (., !, ?, or end of string).
func sentenceEndPos(s string) int {
	for i, r := range s {
		if r == '.' || r == '!' || r == '?' {
			return i
		}
	}
	return len(s)
}

// containsCorrectionContext checks whether a sentence fragment contains
// markers indicating it's a correction (not, instead, different, wrong,
// should, rather).
func containsCorrectionContext(s string) bool {
	markers := []string{" not ", " instead", " different", " wrong", " should ", " rather "}
	for _, m := range markers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// hasImperativeVerb checks whether the text ends with an imperative verb
// that would precede "instead of" / "rather than" in a correction context.
// Uses exact word matching (not substring) to avoid false positives like
// "because" matching "use".
func hasImperativeVerb(before string) bool {
	before = strings.TrimSpace(before)
	verbs := []string{"use", "try", "do", "run", "make", "go", "test", "build", "write", "edit"}
	fields := strings.Fields(before)
	if len(fields) == 0 {
		return false
	}
	last := fields[len(fields)-1]
	for _, v := range verbs {
		if last == v {
			return true
		}
	}
	return false
}

// truncateForMemory truncates text to the max length for memory entries,
// preserving word boundaries.
func truncateForMemory(text string) string {
	text = strings.TrimSpace(text)
	if len(text) <= correctionMaxUserTextLen {
		return text
	}
	// Find the last word boundary within the limit.
	cut := text[:correctionMaxUserTextLen]
	if i := strings.LastIndex(cut, " "); i > correctionMaxUserTextLen-100 {
		cut = cut[:i]
	}
	return cut + "…"
}

// containsSecretPattern performs a basic scan for common secret patterns
// in user text before storing it to memory. Returns true if the text may
// contain credentials. This is a conservative, best-effort check — it's
// not a complete secret scanner, but it catches the most common patterns.
func containsSecretPattern(text string) bool {
	lower := strings.ToLower(text)
	// Check for common secret-bearing patterns.
	patterns := []string{
		"api_key", "apikey", "api-key",
		"secret_key", "secretkey",
		"access_token", "accesstoken",
		"private_key", "privatekey",
		"password", "passwd",
		"bearer ", "authorization:",
		"-----begin",
		"aws_secret", "aws_access",
		"client_secret",
		"token=", "token: ",
	}
	for _, p := range patterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// ── Integration point: called from SendOutcome ──────────────────────────────

// detectAndProposeCorrection is the main entry point for the correction-capture
// loop. Called from SendOutcome after egress consent but before the model runs.
// It checks for correction signals and, if found, proposes a memory entry to
// the user. The user's response (approve/decline) determines whether the
// correction is stored.
//
// This is a synchronous, blocking call (it uses a.Confirm). It is a no-op for
// subagents, when no memory store is available, or when no correction is
// detected.
//
// Rewind signal lifecycle: the rewind is consumed by the FIRST user message
// after it, regardless of whether a correction is detected. This means:
//   - If the message triggers a correction proposal, the signal is cleared
//     after the proposal (whether accepted or declined).
//   - If the message does NOT trigger a proposal, the signal is still cleared
//     (the rewind is a one-shot signal — one message, one chance).
//   - If the signal has expired (beyond correctionDetectWindow), it's cleared.
func (a *App) detectAndProposeCorrection(ctx context.Context, userText string) {
	if a.IsSubagent || a.MemoryStore == nil || a.Confirm == nil {
		// Still clear stale rewind even if we can't propose.
		if a.lastRewind != nil {
			a.ClearLastRewind()
		}
		return
	}

	// If the rewind signal exists, it's consumed by this message regardless
	// of outcome. Capture it before detection so we can clear it after.
	hasRewind := a.lastRewind != nil

	signal, proposal := a.detectCorrection(userText)

	// The rewind signal is always consumed by the first user message after it.
	// This is the one-shot lifecycle: one message, one chance to detect.
	if hasRewind {
		defer a.ClearLastRewind()
	}

	if signal == SignalNone || proposal == nil {
		return
	}

	a.proposeCorrection(ctx, signal, proposal)
}
