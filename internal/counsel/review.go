package counsel

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"regexp"
	"strings"
	"time"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/diag"
)

// Whole-or-fail reviewer dispatch.
//
// A document sent for review either arrives WHOLE, arrives as clearly labelled
// parts that together cover every byte of it (with a small overlap), or the
// call fails loudly. Nothing is ever cut silently: a review of a prefix is
// worse than no review.
//
// The dispatcher also refuses to trust a reply it cannot vouch for: a reply
// that hit max_tokens, came back empty, or cites the wrong "last section read"
// is VOID. A VOID reply is never merged, never counted as a review round and
// never reported as "no findings".

const (
	// defaultMinAnswerTokens is the answer budget floor for a review.
	defaultMinAnswerTokens = 4096
	// answerReserveTokens is held back from the context window in addition to
	// the answer budget (framing/tokenizer slack on the answer side).
	answerReserveTokens = 512
	// overlapTokens is the approximate overlap carried from one part into the
	// next so a finding that straddles a boundary can still be seen.
	overlapTokens = 200
	// reviewCharsPerToken is the CONSERVATIVE bytes-per-token used to size
	// requests (code and non-English text tokenise worse than 4 chars/token).
	reviewCharsPerToken = 3
	// contextHeadroom is the share of the context window the request may use.
	contextHeadroom = 0.90
	// evidencePrefix introduces the reviewer's last-section citation line.
	evidencePrefix = "LAST SECTION READ:"
	// partSlackChars covers the per-part header, continuation labels and the
	// overlap marker.
	partSlackChars = 400
	// maxPartsPerMember bounds the number of sequential calls one member makes
	// for one document.
	maxPartsPerMember = 24
)

// ModelLimits are per-model overrides of the context window and the review
// answer budget (config: mashura_models). Zero means "use the default".
type ModelLimits struct {
	ContextTokens   int // model context window in tokens; 0 = provider table / registry
	ReviewMaxTokens int // max answer tokens for a review; 0 = the call's configured MaxTokens
	// Reasoning is the per-model reasoning/thinking spec. When non-nil, the
	// provider layer maps it to the native reasoning parameter and makes
	// max_tokens additive (reasoning budget + answer budget).
	Reasoning *config.ReasoningSpec
}

// ReviewLog records what one reviewer was actually sent (logged per reviewer
// and shown to the user).
type ReviewLog struct {
	Model         string
	ContextTokens int
	TokensSent    int // estimated input tokens summed over all parts
	Parts         int
	MaxTokens     int // answer budget of the (last) call
	ReadThrough   string
	// FinishReasons is the provider finish/stop reason of each call of the
	// final attempt, in part order ("?" when the provider reported none).
	FinishReasons []string
	Retried       bool   // a length truncation forced a retry
	Why           string // how the retry differed when it was not the chunked path
	// ReasoningTokens is the count of reasoning/thinking tokens the provider
	// reported for this reviewer (OpenRouter completion_tokens_details.
	// reasoning_tokens). 0 when the provider does not report a separate count
	// (Anthropic-native output_tokens includes thinking).
	ReasoningTokens int64
	// AnswerTokens is the non-reasoning output tokens (completion_tokens minus
	// reasoning_tokens) when the provider reports them separately. 0 otherwise.
	AnswerTokens int64
	// ProviderRetried is true when a 429/5xx provider error was retried once.
	ProviderRetried bool
	// ProviderError is the provider's non-200 refusal (e.g. "HTTP 400: …")
	// when the call ended with one. It is a hard error — not VOID and not a
	// review outcome — and is stated in the footer.
	ProviderError string
}

// ProviderHTTPError is a non-200 answer from the provider API. It is a HARD
// error, never VOID: the provider refused the request, so there is no reply to
// judge and it is not a review outcome. Error() keeps the historical
// "<label>: HTTP <status>[: <msg>]" text.
type ProviderHTTPError struct {
	Label  string
	Status int
	Msg    string
}

func (e *ProviderHTTPError) Error() string {
	if e.Msg == "" {
		return fmt.Sprintf("%s: HTTP %d", e.Label, e.Status)
	}
	return fmt.Sprintf("%s: HTTP %d: %s", e.Label, e.Status, e.Msg)
}

// Retryable reports whether one retry may help: rate limiting (429) and
// server-side failures (5xx). 4xx request errors (bad parameters, auth) are
// final — retrying the same request cannot succeed.
func (e *ProviderHTTPError) Retryable() bool {
	return e.Status == 429 || e.Status >= 500
}

// retryBackoff is the pause before the single retry of a 429/5xx reviewer
// call: a random 5–15 s. A var so tests can shorten it.
var retryBackoff = func() time.Duration {
	return 5*time.Second + time.Duration(rand.Int64N(int64(10*time.Second)))
}

// withProviderRetry wraps send: a 429/5xx provider error is retried exactly
// once after retryBackoff; any other outcome is returned as is. Usage of the
// failed attempt is folded into the result (failed calls can still bill).
// retried reports whether the retry happened.
func withProviderRetry(send sendFunc, retried *bool) sendFunc {
	return func(ctx context.Context, q, b string, maxTok int) (string, OracleUsage, error) {
		ans, usage, err := send(ctx, q, b, maxTok)
		var pe *ProviderHTTPError
		if err == nil || !errors.As(err, &pe) || !pe.Retryable() {
			return ans, usage, err
		}
		t := time.NewTimer(retryBackoff())
		select {
		case <-ctx.Done():
			t.Stop()
			return ans, usage, err
		case <-t.C:
		}
		*retried = true
		diag.Printf("mashura: provider %s — retrying once\n", pe.Error())
		ans2, usage2, err2 := send(ctx, q, b, maxTok)
		usage2.InputTokens += usage.InputTokens
		usage2.OutputTokens += usage.OutputTokens
		usage2.ReasoningTokens += usage.ReasoningTokens
		usage2.WebSearchRequests += usage.WebSearchRequests
		return ans2, usage2, err2
	}
}

// VoidError marks a reviewer reply that must not be trusted: truncated,
// empty, or failing the last-section check.
type VoidError struct{ Reason string }

func (e *VoidError) Error() string { return "VOID: " + e.Reason }

// IncompleteReplyError is returned by the provider layer when a reply is cut
// off (Length=true: finish_reason=length / stop_reason=max_tokens) or empty.
// Error() keeps the original message text.
type IncompleteReplyError struct {
	Msg    string
	Length bool
}

func (e *IncompleteReplyError) Error() string { return e.Msg }

// AnyVoid reports whether any panel member's reply was VOID.
func AnyVoid(results []PanelMemberResult) bool {
	for _, r := range results {
		if r.Void {
			return true
		}
	}
	return false
}

// sendFunc performs ONE provider call with an explicit answer budget.
type sendFunc func(ctx context.Context, question, briefing string, maxTokens int) (string, OracleUsage, error)

// reviewOutcome is dispatchReview's result for one member.
type reviewOutcome struct {
	Answer string
	Usage  OracleUsage
	Err    error // *VoidError when the reply is VOID
	Log    *ReviewLog
}

// critiqueMarker begins the appendix buildCritiqueAppendix adds in debate
// round 2 (quoted round-1 answers of the other members). The appendix is
// carried EXPLICITLY (dispatchReview's appendix argument) and never inferred
// from the briefing text: a document that merely quotes or fences the marker
// must not move the expected "last section read", and the appendix must be
// identical in every part instead of being re-discovered per chunk.
const critiqueMarker = "── Round 1 responses from other panel members ──"

func estTokens(chars int) int { return (chars + reviewCharsPerToken - 1) / reviewCharsPerToken }

// limitsFor resolves the effective limits for a model (prefixed key first).
func limitsFor(ctx context.Context, ccfg PanelCallConfig, prefixed, model string) (ctxTokens, configuredMax int) {
	lim := ccfg.ModelLimits[prefixed]
	if lim.ContextTokens == 0 && lim.ReviewMaxTokens == 0 {
		lim = ccfg.ModelLimits[model]
	}
	ctxTokens = lim.ContextTokens
	if ctxTokens <= 0 {
		ctxTokens = ResolveContextLength(ctx, model)
	}
	configuredMax = lim.ReviewMaxTokens
	if configuredMax <= 0 {
		configuredMax = ccfg.MaxTokens
	}
	return ctxTokens, configuredMax
}

// ── briefing structure ──────────────────────────────────────────────────────

// blockHeaderRe matches the "── label ──" lines that delimit briefing blocks
// (source files, model answers, omitted-marker lines).
var blockHeaderRe = regexp.MustCompile(`^── (.+) ──\s*$`)

// sourceHeaderRe matches the "(N lines)" suffix of a source-file block label.
var sourceHeaderRe = regexp.MustCompile(`\(\d+ lines\)\s*$`)

// fileExtRe matches a plausible file extension (".go", ".tsx"); version-like
// suffixes of model ids (".1", ".7-sonnet") do not match.
var fileExtRe = regexp.MustCompile(`^\.[A-Za-z][A-Za-z0-9]{0,7}$`)

// headingRe matches an ATX markdown heading.
var headingRe = regexp.MustCompile(`^(#{1,6})[ \t]+(\S.*?)[ \t#]*$`)

// unit is a contiguous byte range of the briefing: one labelled block, or the
// unlabelled text before the first block. Concatenating all units reproduces
// the briefing exactly.
type unit struct {
	label string // "" for the unlabelled preamble
	text  string
}

func parseUnits(briefing string) []unit {
	var units []unit
	var cur strings.Builder
	label := ""
	flush := func() {
		if cur.Len() > 0 {
			units = append(units, unit{label: label, text: cur.String()})
			cur.Reset()
		}
	}
	inFence := false
	fence := ""
	for _, line := range strings.SplitAfter(briefing, "\n") {
		t := strings.TrimRight(line, "\r\n")
		if trim := strings.TrimLeft(t, " "); strings.HasPrefix(trim, "```") || strings.HasPrefix(trim, "~~~") {
			if !inFence {
				inFence, fence = true, trim[:3]
			} else if trim[:3] == fence {
				inFence = false
			}
		}
		if m := blockHeaderRe.FindStringSubmatch(t); m != nil {
			// A "── x ──" line quoted inside a code fence is text, not a
			// block boundary — except a source-file header ("(N lines)"), so
			// a file with an unbalanced fence cannot swallow the next block.
			if !inFence || sourceHeaderRe.MatchString(m[1]) {
				flush()
				label = strings.TrimSpace(m[1])
				inFence = false
			}
		}
		cur.WriteString(line)
	}
	flush()
	return units
}

// unitAtomic reports whether a unit must never be split: source files (any
// block whose label names a non-document extension) are sent whole or not at
// all — a mid-file cut is a silent truncation.
func unitAtomic(u unit) bool {
	if u.label == "" {
		return false
	}
	name := u.label
	if i := strings.Index(name, " ("); i >= 0 {
		name = name[:i]
	}
	if i := strings.LastIndexByte(name, '.'); i >= 0 && i > strings.LastIndexByte(name, '/') {
		switch strings.ToLower(name[i:]) {
		case ".md", ".markdown", ".txt", ".rst", ".adoc":
			return false
		}
		// An extension only counts when it looks like one (no spaces, short):
		// model ids such as "claude-3.7-sonnet" are labels, not files.
		return fileExtRe.MatchString(name[i:])
	}
	return false
}

type headingPos struct {
	off   int
	level int
	text  string
}

// headingsOf lists the ATX headings of text outside code fences.
func headingsOf(text string) []headingPos {
	var out []headingPos
	off := 0
	inFence := false
	var fence string
	for _, line := range strings.SplitAfter(text, "\n") {
		t := strings.TrimRight(line, "\r\n")
		trim := strings.TrimLeft(t, " ")
		switch {
		case strings.HasPrefix(trim, "```") || strings.HasPrefix(trim, "~~~"):
			mark := trim[:3]
			if !inFence {
				inFence, fence = true, mark
			} else if mark == fence {
				inFence = false
			}
		case !inFence:
			if m := headingRe.FindStringSubmatch(t); m != nil {
				out = append(out, headingPos{off: off, level: len(m[1]), text: strings.TrimSpace(m[2])})
			}
		}
		off += len(line)
	}
	return out
}

// splitByHeadings splits text into pieces of at most max bytes at markdown
// headings, preferring the shallowest heading level that works. The pieces
// are contiguous: their concatenation is exactly text. An error is returned
// when some section is larger than max and has no deeper heading to split at.
func splitByHeadings(text string, max int) ([]string, error) {
	if len(text) <= max {
		return []string{text}, nil
	}
	heads := headingsOf(text)
	if len(heads) == 0 {
		return nil, fmt.Errorf("a %d-byte section has no markdown headings to split at (limit %d bytes)", len(text), max)
	}
	minLevel := 6
	for _, h := range heads {
		minLevel = min(minLevel, h.level)
	}
	var lastErr error
	for level := minLevel; level <= 6; level++ {
		var cuts []int
		for _, h := range heads {
			if h.level <= level && h.off > 0 {
				cuts = append(cuts, h.off)
			}
		}
		var sections []string
		prev := 0
		for _, c := range cuts {
			sections = append(sections, text[prev:c])
			prev = c
		}
		sections = append(sections, text[prev:])
		biggest, bigIdx := 0, 0
		for i, s := range sections {
			if len(s) > biggest {
				biggest, bigIdx = len(s), i
			}
		}
		if biggest > max {
			lastErr = fmt.Errorf("a %d-byte section (starting %q) exceeds the %d-byte part limit and has no deeper heading to split at",
				biggest, firstLine(sections[bigIdx]), max)
			continue
		}
		var out []string
		var cur strings.Builder
		for _, s := range sections {
			if cur.Len() > 0 && cur.Len()+len(s) > max {
				out = append(out, cur.String())
				cur.Reset()
			}
			cur.WriteString(s)
		}
		if cur.Len() > 0 {
			out = append(out, cur.String())
		}
		return out, nil
	}
	return nil, lastErr
}

func firstLine(s string) string {
	s = strings.TrimLeft(s, "\n")
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 60 {
		s = s[:60] + "…"
	}
	return s
}

// piece is one unsplittable-or-already-split slice of the briefing.
type piece struct {
	text  string
	label string // owning block label ("" for preamble)
	cont  bool   // continuation of a split block (gets a "(continued)" header)
}

// part is one request's worth of briefing.
type part struct {
	text        string // text as sent (overlap marker + body)
	body        string // new (non-overlap) material only
	lastHeading string // last markdown heading in body ("" if none)
	lastLabel   string // last block label in body ("" if none)
}

func pieceText(p piece) string {
	if p.cont {
		return "── " + p.label + " (continued) ──\n" + p.text
	}
	return p.text
}

// planParts splits briefing into parts whose BODY is at most budgetChars
// (overlap marker and continuation headers excluded; they are covered by
// partSlackChars inside the caller's budget). A single part is returned
// verbatim when the briefing fits.
func planParts(briefing string, budgetChars int) ([]part, error) {
	if budgetChars <= 0 {
		return nil, errors.New("no room left in the context window for the document")
	}
	if len(briefing) <= budgetChars {
		return []part{makePart(briefing, nil, "")}, nil
	}
	// Reserve slack for overlap + headers inside every part.
	overlapChars := overlapTokens * reviewCharsPerToken
	bodyMax := budgetChars - overlapChars - partSlackChars
	if bodyMax < 1000 {
		return nil, fmt.Errorf("the context window leaves only %d bytes per part after overlap — too small to split the document", bodyMax)
	}
	var pieces []piece
	for _, u := range parseUnits(briefing) {
		if len(u.text) <= bodyMax {
			pieces = append(pieces, piece{text: u.text, label: u.label})
			continue
		}
		if unitAtomic(u) {
			return nil, fmt.Errorf("source %q is %d bytes (~%d tokens) and cannot be split (limit %d bytes per part); source files are never cut mid-file",
				u.label, len(u.text), estTokens(len(u.text)), bodyMax)
		}
		// Splittable (markdown/text): headings. Reserve room for a
		// continuation header on every non-first piece.
		sub, err := splitByHeadings(u.text, bodyMax-len(u.label)-32)
		if err != nil {
			where := u.label
			if where == "" {
				where = "the briefing text"
			}
			return nil, fmt.Errorf("%s: %w", where, err)
		}
		for i, s := range sub {
			pieces = append(pieces, piece{text: s, label: u.label, cont: i > 0 && u.label != ""})
		}
	}
	// Pack pieces greedily into parts.
	var parts []part
	var cur strings.Builder
	var prevBody string
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		body := cur.String()
		overlap := ""
		if len(parts) > 0 {
			overlap = tailOverlap(prevBody, overlapChars)
		}
		p := makePart(body, nil, overlap)
		parts = append(parts, p)
		prevBody = body
		cur.Reset()
	}
	for _, pc := range pieces {
		t := pieceText(pc)
		if cur.Len() > 0 && cur.Len()+len(t) > bodyMax {
			flush()
		}
		cur.WriteString(t)
	}
	flush()
	if len(parts) > maxPartsPerMember {
		return nil, fmt.Errorf("the document needs %d parts (limit %d) for this model's context window", len(parts), maxPartsPerMember)
	}
	return parts, nil
}

// tailOverlap returns up to maxChars from the end of prev, starting at a line
// boundary.
func tailOverlap(prev string, maxChars int) string {
	if len(prev) <= maxChars {
		return prev
	}
	tail := prev[len(prev)-maxChars:]
	if i := strings.IndexByte(tail, '\n'); i >= 0 && i+1 < len(tail) {
		tail = tail[i+1:]
	}
	return tail
}

func makePart(body string, _ []string, overlap string) part {
	p := part{body: body, text: body}
	if overlap != "" {
		p.text = "[overlap — the end of the previous part, repeated for context; do not review it twice]\n" +
			overlap + "\n[end of overlap — this part's own material starts here]\n" + body
	}
	us := parseUnits(body)
	if len(us) > 0 && strings.TrimSpace(body) != "" {
		last := us[len(us)-1]
		p.lastLabel = last.label
		if hs := headingsOf(last.text); len(hs) > 0 {
			p.lastHeading = hs[len(hs)-1].text
		}
	}
	if p.lastHeading == "" && p.lastLabel == "" {
		if hs := headingsOf(body); len(hs) > 0 {
			p.lastHeading = hs[len(hs)-1].text
		}
	}
	return p
}

// ── evidence of reading ────────────────────────────────────────────────────

var evidenceRe = regexp.MustCompile(`(?im)^\W*` + regexp.QuoteMeta(evidencePrefix) + `[ \t]*(.*?)[ \t]*$`)

// extractEvidence strips a trailing "LAST SECTION READ: X" line from answer and
// returns the cleaned answer and X ("" when absent).
func extractEvidence(answer string) (string, string) {
	lines := strings.Split(strings.TrimRight(answer, " \t\r\n"), "\n")
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-4; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		m := evidenceRe.FindStringSubmatch(lines[i])
		if m == nil {
			return answer, ""
		}
		rest := strings.TrimRight(strings.Join(lines[:i], "\n"), " \t\r\n")
		return rest, strings.TrimSpace(strings.Trim(m[1], "*`_ "))
	}
	return answer, ""
}

var (
	normDropRe  = regexp.MustCompile("[#*`\\[\\]()_\"']")
	normLeadRe  = regexp.MustCompile(`^[§\d.\s:)\-–—]+`)
	normSpaceRe = regexp.MustCompile(`\s+`)
)

func normHeading(s string) string {
	s = strings.ToLower(normDropRe.ReplaceAllString(s, " "))
	s = normSpaceRe.ReplaceAllString(strings.TrimSpace(s), " ")
	if t := strings.TrimSpace(normLeadRe.ReplaceAllString(s, "")); t != "" {
		s = t
	}
	return strings.TrimRight(s, " .:")
}

// trailingParenRe strips a trailing "(…)" from a block label such as
// "main.go (120 lines)" — the reviewer cites the file, not the line count.
var trailingParenRe = regexp.MustCompile(`\s*\([^)]*\)\s*$`)

// citationMatches reports whether the cited section IS one of the expected last
// sections: equal after formatting normalisation (markdown marks, numbering,
// case, a trailing "(N lines)"). There is deliberately no substring rule: an
// earlier heading that happens to be a prefix of the last one ("Safety" vs
// "Safety risks") must not pass as proof of having read to the end.
func citationMatches(cited string, expected ...string) bool {
	stripAllParens := func(x string) string {
		// Apply trailingParenRe repeatedly to strip all trailing "(…)"
		// groups — a range label like "foo.go (lines 10–20) (15 lines)"
		// needs both groups removed so the citation "foo.go" matches.
		s := strings.Trim(strings.TrimSpace(x), "*`_# ")
		for {
			stripped := trailingParenRe.ReplaceAllString(s, "")
			if stripped == s {
				break
			}
			s = stripped
		}
		return normHeading(s)
	}
	c := stripAllParens(cited)
	if c == "" {
		return false
	}
	for _, e := range expected {
		if n := stripAllParens(e); n != "" && c == n {
			return true
		}
	}
	return false
}

func (p part) expectedEvidence() []string {
	var e []string
	if p.lastHeading != "" {
		e = append(e, p.lastHeading)
	}
	if p.lastLabel != "" {
		e = append(e, p.lastLabel)
	}
	return e
}

// ── dispatch ────────────────────────────────────────────────────────────────

const evidenceInstruction = "Finish your reply with one final line, exactly: `" + evidencePrefix + " <the heading (or `── label ──` block label — for a source file, cite its path) of the LAST section of the document above that you actually read (ignore quoted answers of other models)>`. This is checked mechanically; a wrong or missing citation voids your reply."

func partFraming(i, n int, hasOverlap bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[This is part %d of %d of the document. Answer the question above for THIS part; the other parts are reviewed in separate calls and merged afterwards.", i, n)
	if hasOverlap {
		b.WriteString(" The first lines of the context are an overlap repeated from the previous part.")
	}
	b.WriteString("]")
	return b.String()
}

// voidAfterTruncation keeps a truncated review VOID when its one retry fails
// for any reason (timeout, provider error): the first reply WAS cut off, so
// an ordinary error from the retry must not make the member look merely
// failed — it would skip the NOT COMPLETED banner and the auto-counsel
// refund. nil and VOID errors pass through unchanged.
func voidAfterTruncation(err error, what string) error {
	if err == nil {
		return nil
	}
	var v *VoidError
	if errors.As(err, &v) {
		return err
	}
	return &VoidError{Reason: fmt.Sprintf("the first reply hit max_tokens and the %s failed (%v)", what, err)}
}

// dispatchReview sends question+briefing to one model, whole when it fits and
// as labelled parts otherwise (appendix, when non-empty, is quoted reference
// material added unchanged to EVERY part and never part of the evidence
// target), enforcing the answer budget, the retry-once
// chunked path on truncation, and the last-section evidence check.
func dispatchReview(ctx context.Context, label string, ctxTokens, configuredMax int, floor int, question, briefing, appendix string, send sendFunc) reviewOutcome {
	if floor <= 0 {
		floor = defaultMinAnswerTokens
	}
	if configuredMax <= 0 {
		configuredMax = floor
	}
	answerWant := max(configuredMax, floor)
	usable := int(float64(ctxTokens) * contextHeadroom)
	// Largest input (tokens) that still leaves room for the floor answer.
	inputBudget := usable - floor - answerReserveTokens
	log := &ReviewLog{Model: label, ContextTokens: ctxTokens}
	send = withProviderRetry(send, &log.ProviderRetried)

	doc := briefing
	hasStructure := len(headingsOf(doc)) > 0 || strings.Contains(doc, "\n── ") || strings.HasPrefix(doc, "── ")
	instr := ""
	if hasStructure {
		instr = "\n\n" + evidenceInstruction
	}
	// Fixed cost of one request besides the document body.
	overheadChars := len(oracleSystemPrompt) + len(question) + len(instr) + len("\n\nContext:\n") + len(appendix) + 220 // part framing
	docBudgetChars := inputBudget*reviewCharsPerToken - overheadChars

	var parts []part
	var planErr error
	if inputBudget <= 0 || docBudgetChars <= 0 {
		planErr = fmt.Errorf("model context window (%d tokens) leaves no room for the question and a %d-token answer", ctxTokens, floor)
	} else {
		parts, planErr = planParts(briefing, docBudgetChars)
	}
	if planErr != nil {
		return reviewOutcome{Log: log, Err: fmt.Errorf("document does not fit %s (context %d tokens, ~%d tokens needed): %w — nothing was sent",
			label, ctxTokens, estTokens(len(briefing)+overheadChars), planErr)}
	}

	run := func(parts []part, want int) (reviewOutcome, bool) {
		var out reviewOutcome
		var answers []string
		n := len(parts)
		log.Parts, log.TokensSent = n, 0
		log.FinishReasons = nil
		for i, p := range parts {
			q := question
			if n > 1 {
				q += "\n\n" + partFraming(i+1, n, p.text != p.body)
			}
			q += instr
			text := p.text + appendix
			inChars := len(oracleSystemPrompt) + len(q) + len("\n\nContext:\n") + len(text)
			inTok := estTokens(inChars)
			log.TokensSent += inTok
			maxTok := min(want, usable-inTok-answerReserveTokens)
			maxTok = max(maxTok, floor) // floor is guaranteed by the input budget
			log.MaxTokens = maxTok
			diag.Printf("mashura: %s part %d/%d ~%d input tokens, max_tokens %d, context %d\n", label, i+1, n, inTok, maxTok, ctxTokens)
			ans, usage, err := send(ctx, q, text, maxTok)
			out.Usage.InputTokens += usage.InputTokens
			out.Usage.OutputTokens += usage.OutputTokens
			out.Usage.ReasoningTokens += usage.ReasoningTokens
			out.Usage.WebSearchRequests += usage.WebSearchRequests
			log.ReasoningTokens += usage.ReasoningTokens
			fr := usage.FinishReason
			if fr == "" {
				fr = "?"
			}
			log.FinishReasons = append(log.FinishReasons, fr)
			if err != nil {
				var inc *IncompleteReplyError
				var void *VoidError
				if i > 0 && !errors.As(err, &inc) && !errors.As(err, &void) {
					// A later part failed (timeout, provider error) after earlier
					// parts succeeded: the member did NOT review the whole
					// document. Never merge the partial answers and never let
					// the round look complete — VOID (not counted, not "no
					// findings"), exactly like a truncated part.
					out.Err = &VoidError{Reason: fmt.Sprintf("interrupted at part %d of %d (%v) — earlier parts were not merged", i+1, n, err)}
					return out, false
				}
				if errors.As(err, &inc) {
					if inc.Length {
						out.Err = &VoidError{Reason: fmt.Sprintf("reply hit max_tokens (%d) on part %d of %d", maxTok, i+1, n)}
						return out, true // truncated
					}
					out.Err = &VoidError{Reason: fmt.Sprintf("empty or interrupted reply on part %d of %d", i+1, n)}
					return out, false
				}
				var pe *ProviderHTTPError
				if errors.As(err, &pe) {
					log.ProviderError = fmt.Sprintf("HTTP %d", pe.Status)
					if pe.Msg != "" {
						log.ProviderError += ": " + pe.Msg
					}
				}
				out.Err = err
				return out, false
			}
			if strings.TrimSpace(ans) == "" {
				out.Err = &VoidError{Reason: fmt.Sprintf("empty reply on part %d of %d", i+1, n)}
				return out, false
			}
			if exp := p.expectedEvidence(); hasStructure && len(exp) > 0 {
				clean, cited := extractEvidence(ans)
				if cited == "" {
					out.Err = &VoidError{Reason: fmt.Sprintf("part %d of %d: reply did not cite the last section it read (expected %q)", i+1, n, exp[0])}
					return out, false
				}
				if !citationMatches(cited, exp...) {
					out.Err = &VoidError{Reason: fmt.Sprintf("part %d of %d: reply cited %q as the last section read but the part ends at %q — the reviewer did not read it all", i+1, n, cited, exp[0])}
					return out, false
				}
				if strings.TrimSpace(clean) == "" {
					// A bare citation is not a review: with the evidence line
					// removed nothing is left, which must never read as a
					// completed review with "no findings".
					out.Err = &VoidError{Reason: fmt.Sprintf("part %d of %d: the reply contained only the last-section citation and no review", i+1, n)}
					return out, false
				}
				ans = clean
				log.ReadThrough = cited
			}
			if n > 1 {
				answers = append(answers, fmt.Sprintf("── part %d of %d ──\n%s", i+1, n, strings.TrimSpace(ans)))
			} else {
				answers = append(answers, ans)
			}
		}
		out.Answer = strings.Join(answers, "\n\n")
		return out, false
	}

	out, truncated := run(parts, answerWant)
	if truncated {
		// Retry ONCE on a smaller chunked path (also when the first attempt was
		// already chunked: its parts are split further). Half the largest part
		// body, so every part — and therefore every answer — is shorter.
		log.Retried = true
		largest := 0
		for _, p := range parts {
			largest = max(largest, len(p.body))
		}
		target := largest/2 + overlapTokens*reviewCharsPerToken + partSlackChars + 1000
		retryParts, err := planParts(briefing, min(docBudgetChars, target))
		if err != nil || len(retryParts) <= len(parts) {
			why := "the document cannot be split any smaller"
			if err != nil {
				why = err.Error()
			}
			// Chunking cannot shorten the answer here (e.g. one unsplittable
			// source block). A length stop is caused by the answer budget, so
			// the one retry raises it — as far as the context window allows —
			// and sends the SAME document whole. Nothing is cut; the reply is
			// still checked for citation and completeness.
			bigger := min(max(answerWant*4, 16384), usable-answerReserveTokens-estTokens(len(briefing)+overheadChars))
			if len(parts) == 1 && bigger > answerWant {
				log.Why = "finish_reason=length; chunking impossible (" + why + "); retried whole with max_tokens " + fmt.Sprint(bigger)
				usage := out.Usage
				out, _ = run(parts, bigger)
				out.Usage.InputTokens += usage.InputTokens
				out.Usage.OutputTokens += usage.OutputTokens
				out.Usage.ReasoningTokens += usage.ReasoningTokens
				out.Usage.WebSearchRequests += usage.WebSearchRequests
				out.Log = log
				out.Err = voidAfterTruncation(out.Err, "whole-document retry")
				if out.Err != nil {
					out.Answer = ""
				}
				return out
			}
			out.Err = &VoidError{Reason: "reply hit max_tokens and the chunked retry is impossible: " + why}
			out.Log = log
			out.Answer = ""
			return out
		}
		usage := out.Usage
		out, _ = run(retryParts, answerWant)
		out.Usage.InputTokens += usage.InputTokens
		out.Usage.OutputTokens += usage.OutputTokens
		out.Usage.ReasoningTokens += usage.ReasoningTokens
		out.Usage.WebSearchRequests += usage.WebSearchRequests
		out.Err = voidAfterTruncation(out.Err, "chunked retry")
	}
	out.Log = log
	if out.Err != nil {
		out.Answer = "" // never merge or show a partial reply
	}
	diag.Printf("mashura: %s sent ~%d tokens in %d part(s), context %d, finish_reason %v, read through %q, err=%v\n", label, log.TokensSent, log.Parts, ctxTokens, log.FinishReasons, log.ReadThrough, out.Err)
	return out
}
