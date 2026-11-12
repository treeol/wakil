package counsel

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"regexp"
	"strings"
	"sync"
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
	// VoidRetried is true when the member's reply was VOID and was retried once
	// automatically before being reported.
	VoidRetried bool
	// VoidNotObtained is true when even the automatic retry stayed VOID: the
	// review was NOT obtained. It is reported as such (never merged, never a
	// round, never "no findings").
	VoidNotObtained bool
	// Synthesized is true when an oversize source was split into line-range
	// chunks and a final synthesis call merged the per-chunk reviews.
	Synthesized bool
	// SynthesisTokensSent is the estimated input tokens of the synthesis call
	// (0 when no synthesis ran).
	SynthesisTokensSent int
	// ProviderError is the provider's non-200 refusal (e.g. "HTTP 400: …")
	// when the call ended with one. It is a hard error — not VOID and not a
	// review outcome — and is stated in the footer.
	ProviderError string
	// CoverageNotConfirmed is true when a reply with non-empty review content
	// was accepted without a matching last-section citation (missing or wrong).
	// The reply is obtained (not VOID) but coverage is not confirmed.
	CoverageNotConfirmed bool

	// VOID diagnostics for the (last) VOID attempt: the classified cause
	// (limit/empty/citation/other), the provider finish reason, the attempt
	// count, and the reasoning/answer token split. AttemptBudget and
	// AttemptReasoning record the last attempt's answer budget and reasoning
	// spec string. BudgetStop carries the budget-guard reason when the
	// budget stopped further calls. Populated by runMember.
	VoidCause        string // limit | empty | citation | other
	VoidFinish       string
	VoidAttempts     int
	VoidReasoning    int64
	VoidAnswer       int64
	AttemptBudget    int
	AttemptReasoning string
	BudgetStop       string
	// CallCount is the total number of sends attempted for this member
	// across every dispatch (including any internal truncated retry and
	// any runMember retry). Each run-loop iteration counts once.
	CallCount int
	// R1Kept / Round2KeptR1 / Round2Failed mark a debate round-2 failure
	// where the round-1 answer was kept as the member's usable result.
	// All three are set together (aliases for the same outcome).
	R1Kept       bool
	Round2KeptR1 bool
	Round2Failed bool
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
	return func(ctx context.Context, q, b, ap string, maxTok int) (string, OracleUsage, error) {
		ans, usage, err := send(ctx, q, b, ap, maxTok)
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
		ans2, usage2, err2 := send(ctx, q, b, ap, maxTok)
		usage2.InputTokens += usage.InputTokens
		usage2.OutputTokens += usage.OutputTokens
		usage2.ReasoningTokens += usage.ReasoningTokens
		usage2.WebSearchRequests += usage.WebSearchRequests
		usage2 = addProviderCost(usage2, usage)
		return ans2, usage2, err2
	}
}

// VoidError marks a reviewer reply that must not be trusted: truncated,
// empty, or a bare citation with no review content. Cause and Finish carry
// the classified retry cause (limit/empty/citation/other) and the provider
// finish reason when known (set by runMember, which classifies the outcome).
type VoidError struct {
	Reason string
	Cause  string
	Finish string
}

func (e *VoidError) Error() string { return "VOID: " + e.Reason }

// BudgetError marks a round stopped by the review/session budget guard. It
// is NOT a VOID: no reply was judged, and Err is a BudgetError (never a
// *VoidError) on BudgetStopped members.
type BudgetError struct{ Reason string }

func (e *BudgetError) Error() string {
	if e.Reason == "" {
		return "NOT COMPLETED: review budget reached"
	}
	return "NOT COMPLETED: " + e.Reason
}

// budgetReachedReason is the sole budget-stop reason string.
const budgetReachedReason = "review budget reached"

// BudgetState is the shared budget guard for one panel operation. spent is
// the sum of provider-reported costs of calls made under this operation;
// the session budget additionally counts spend outside the panel via
// sessionCost (relative to the base captured at construction).
//
// Accounting is post-call: a call's cost is folded in only after it returns,
// so parallel members can each spend one in-flight call before the guard
// trips. Worst-case overshoot is roughly one in-flight call per concurrent
// member. Calls without a provider-reported cost are estimated at the last
// observed per-token rate (lastRate); unpriced counts the calls that had to
// be estimated (or had no rate to estimate from).
type BudgetState struct {
	mu            sync.Mutex
	spent         float64
	reviewBudget  float64
	sessionBudget float64
	sessionCost   func() float64
	sessionBase   float64
	stopped       bool
	reason        string
	lastRate      float64
	unpriced      int
}

// newBudgetState builds a BudgetState from the panel call config,
// capturing the session-cost base once when a session cost func is set.
func newBudgetState(ccfg PanelCallConfig) *BudgetState {
	bs := &BudgetState{
		reviewBudget:  ccfg.ReviewBudgetUSD,
		sessionBudget: ccfg.SessionBudgetUSD,
		sessionCost:   ccfg.SessionCostFunc,
	}
	if bs.sessionCost != nil {
		bs.sessionBase = bs.sessionCost()
	}
	return bs
}

// add folds one call's cost into spent. Priced calls add their
// provider-reported cost and update the per-token rate estimate; unpriced
// calls are estimated at that rate when one is known.
func (b *BudgetState) add(u OracleUsage) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	tokens := u.InputTokens + u.OutputTokens
	if u.HasProviderCost {
		b.spent += u.ProviderCostUSD
		if tokens > 0 {
			b.lastRate = u.ProviderCostUSD / float64(tokens)
		}
	} else if tokens > 0 && b.lastRate > 0 {
		b.spent += float64(tokens) * b.lastRate
		b.unpriced++
	} else {
		b.unpriced++
	}
}

// spentUSD returns the accumulated spend estimate (priced + estimated).
func (b *BudgetState) spentUSD() float64 {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spent
}

// shouldStop reports whether the budget guard has stopped further calls,
// marking the state stopped (sticky) when a budget is reached.
func (b *BudgetState) shouldStop() (string, bool) {
	if b == nil {
		return "", false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped {
		return b.reason, true
	}
	if b.reviewBudget > 0 && b.spent >= b.reviewBudget {
		b.stopped, b.reason = true, budgetReachedReason
		return b.reason, true
	}
	if b.sessionBudget > 0 {
		if b.sessionCost != nil {
			if (b.sessionCost()-b.sessionBase)+b.spent >= b.sessionBudget {
				b.stopped, b.reason = true, budgetReachedReason
				return b.reason, true
			}
		} else if b.spent >= b.sessionBudget {
			b.stopped, b.reason = true, budgetReachedReason
			return b.reason, true
		}
	}
	return "", false
}

// stopReason returns the recorded budget-stop reason ("" when not stopped).
func (b *BudgetState) stopReason() string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reason
}

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

// AnyBudgetStopped reports whether any panel member was stopped by the
// budget guard (a BudgetError, never VOID).
func AnyBudgetStopped(results []PanelMemberResult) bool {
	for _, r := range results {
		if r.BudgetStopped {
			return true
		}
	}
	return false
}

// isVoid reports whether err is a *VoidError (a reply that must not be trusted).
func isVoid(err error) bool {
	var v *VoidError
	return errors.As(err, &v)
}

// voidCauseFrom classifies a VOID outcome for the cause-based retry:
// limit (answer budget), empty, citation (citation-only / citation
// failure), or other. The provider finish reason and the dispatch log are
// consulted so a length stop is classified as limit even when the error
// text does not say so.
func voidCauseFrom(err error, usage OracleUsage, log *ReviewLog) string {
	var inc *IncompleteReplyError
	if errors.As(err, &inc) && inc.Length {
		return "limit"
	}
	if usage.FinishReason == "length" || usage.FinishReason == "max_tokens" {
		return "limit"
	}
	if log != nil {
		for _, fr := range log.FinishReasons {
			if fr == "length" || fr == "max_tokens" {
				return "limit"
			}
		}
	}
	if errors.As(err, &inc) {
		low := strings.ToLower(inc.Msg)
		if strings.TrimSpace(inc.Msg) == "" || strings.HasPrefix(low, "empty") {
			return "empty"
		}
	}
	if err != nil {
		msg := err.Error()
		msg = strings.TrimPrefix(msg, "VOID: ")
		low := strings.ToLower(msg)
		// Exact prefix checks only: a wrapped error whose text merely
		// mentions max_tokens/empty elsewhere must not be classified as a
		// limit/empty VOID (that would wrongly trigger a retry).
		if strings.HasPrefix(low, "reply hit max_tokens") || strings.HasPrefix(low, "the first reply hit max_tokens") || strings.HasPrefix(low, "reply hit max_tokens and") {
			return "limit"
		}
		if strings.HasPrefix(low, "empty reply") || strings.HasPrefix(low, "empty or interrupted") {
			return "empty"
		}
		if strings.Contains(low, "last section") || strings.Contains(low, "citation") || strings.Contains(low, "cite") {
			return "citation"
		}
	}
	return "other"
}

// voidFinishFrom returns the last reported provider finish reason (the
// log's per-part record, else the final usage), skipping "?"/empty.
func voidFinishFrom(usage OracleUsage, log *ReviewLog) string {
	if log != nil {
		for i := len(log.FinishReasons) - 1; i >= 0; i-- {
			if fr := log.FinishReasons[i]; fr != "" && fr != "?" {
				return fr
			}
		}
	}
	return usage.FinishReason
}

// retryVoidDecision reports whether a VOID member may be retried once with
// a CHANGED request: limit requires a larger answer budget (canDouble;
// lowering reasoning is optional), empty requires a lower reasoning spec
// (canLower), and citation/other are never retried — the same request
// cannot fix them.
func retryVoidDecision(cause string, canDouble, canLower bool) bool {
	switch cause {
	case "limit":
		return canDouble
	case "empty":
		return canLower
	default:
		return false
	}
}

// reasoningEffortLadder is the effort ladder lowerReasoningSpec steps down.
var reasoningEffortLadder = []string{"minimal", "low", "medium", "high", "xhigh", "max"}

// lowerReasoningSpec returns a copy of spec with one step less reasoning:
// the effort ladder moves one step down, or an explicit MaxTokens is halved
// (floor 1024, effort kept). none/off (or an unset spec) is already lowest:
// it only changes when MaxTokens > 0. The bool reports whether the spec
// changed; the original is never mutated.
func lowerReasoningSpec(spec *config.ReasoningSpec) (*config.ReasoningSpec, bool) {
	if spec == nil {
		return nil, false
	}
	out := *spec
	if out.MaxTokens > 0 {
		halved := max(out.MaxTokens/2, 1024)
		if halved == out.MaxTokens {
			return spec, false
		}
		out.MaxTokens = halved
		return &out, true
	}
	for i, e := range reasoningEffortLadder {
		if out.Effort == e {
			if i == 0 {
				return spec, false
			}
			out.Effort = reasoningEffortLadder[i-1]
			return &out, true
		}
	}
	return spec, false
}

// doubleBudget returns the doubled answer budget for a limit retry: twice
// the wanted budget, capped by what the window leaves after the input and
// the answer reserve, with the 4096 floor.
func doubleBudget(answerWant, usableForAnswer, inTok int) int {
	return max(min(answerWant*2, usableForAnswer-inTok-answerReserveTokens), defaultMinAnswerTokens)
}

// reasoningSpecString renders a reasoning spec for the attempt log
// ("effort/max_tokens", just one of them, or "none").
func reasoningSpecString(spec *config.ReasoningSpec) string {
	if spec == nil || spec.IsZero() {
		return "none"
	}
	switch {
	case spec.Effort != "" && spec.MaxTokens > 0:
		return fmt.Sprintf("%s/%d", spec.Effort, spec.MaxTokens)
	case spec.Effort != "":
		return spec.Effort
	default:
		return fmt.Sprintf("%d", spec.MaxTokens)
	}
}

// anyOversizeFile reports whether any part carries a line-range chunk of an
// oversize source file, warranting a synthesis pass.
func anyOversizeFile(parts []part) bool {
	for _, p := range parts {
		if p.oversizeFile {
			return true
		}
	}
	return false
}

// synthesizeInstruction is appended to the question for the synthesis call so
// the model produces ONE review of the document from the per-chunk reviews,
// not a repeat of them.
const synthesizeInstruction = "The document was too large to send in one call, so it was split into labelled line-range chunks and each chunk was reviewed separately. The reviews of those chunks were merged above. Produce a SINGLE consolidated review of the whole document: deduplicate, resolve contradictions, and keep every distinct finding with its location. Do not re-review the document; consolidate the chunk reviews."

// synthesizeReview merges the per-part answers of a chunked oversize-file review
// with one more call to the same model. The input is the merged chunk answers
// (small), so it fits; the reply is checked for emptiness and returned.
func synthesizeReview(ctx context.Context, question, merged string, usable, floor, reserve int, send sendFunc, log *ReviewLog) (reviewOutcome, error) {
	if strings.TrimSpace(merged) == "" {
		return reviewOutcome{}, errors.New("no chunk answers to synthesize")
	}
	q := question
	// The wrapper framing tells the model this is a synthesis of prior reviews.
	briefing := merged
	inChars := len(oracleSystemPrompt) + len(q) + len(synthesizeInstruction) + len("\n\nContext:\n") + len(briefing)
	inTok := estTokens(inChars)
	if inTok+floor+reserve > usable {
		return reviewOutcome{}, fmt.Errorf("the merged chunk reviews are ~%d tokens, too large to synthesize in a %d-token window", inTok, usable)
	}
	want := max(min(floor*4, usable-inTok-reserve), floor)
	log.SynthesisTokensSent = inTok
	q += "\n\n" + synthesizeInstruction
	ans, usage, err := send(ctx, q, briefing, "", want)
	if err != nil {
		return reviewOutcome{}, err
	}
	if strings.TrimSpace(ans) == "" {
		return reviewOutcome{}, errors.New("the synthesis reply was empty")
	}
	return reviewOutcome{Answer: ans, Usage: usage}, nil
}

// sendFunc performs ONE provider call with an explicit answer budget.
// question is the raw question (+ part framing), briefing the part text
// (no appendix), appendix the debate round-2 material ("" otherwise).
// The provider layer builds the skeleton message from these parts.
type sendFunc func(ctx context.Context, question, briefing, appendix string, maxTokens int) (string, OracleUsage, error)

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
	// fileSplit is true when this piece is one line-range chunk of an oversize
	// SOURCE FILE (not a markdown section). Such a document gets a synthesis
	// pass so the per-chunk findings are merged into one review.
	fileSplit bool
}

// part is one request's worth of briefing.
type part struct {
	text        string // text as sent (overlap marker + body)
	body        string // new (non-overlap) material only
	lastHeading string // last markdown heading in body ("" if none)
	lastLabel   string // last block label in body ("" if none)
	// oversizeFile is true when the body contains a line-range chunk of an
	// oversize source file. The dispatcher then runs a synthesis pass.
	oversizeFile bool
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
			// An oversize SOURCE FILE is split into line-range chunks rather
			// than refused: the reviewer sees every line, each chunk labelled
			// with the file and its range, and a synthesis pass merges the
			// per-chunk findings. This is a labelled partition of the file, not
			// a silent truncation.
			chunks, err := splitByLines(u.text, bodyMax-len(u.label)-48)
			if err != nil {
				return nil, fmt.Errorf("source %q is %d bytes (~%d tokens) and cannot be split (limit %d bytes per part): %w",
					u.label, len(u.text), estTokens(len(u.text)), bodyMax, err)
			}
			for i, c := range chunks {
				pieces = append(pieces, piece{text: c.text, label: u.label, cont: i > 0, fileSplit: true})
			}
			continue
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
	curOversize := false
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
		p.oversizeFile = curOversize
		parts = append(parts, p)
		prevBody = body
		cur.Reset()
		curOversize = false
	}
	for _, pc := range pieces {
		t := pieceText(pc)
		if cur.Len() > 0 && cur.Len()+len(t) > bodyMax {
			flush()
		}
		cur.WriteString(t)
		if pc.fileSplit {
			curOversize = true
		}
	}
	flush()
	if len(parts) > maxPartsPerMember {
		return nil, fmt.Errorf("the document needs %d parts (limit %d) for this model's context window", len(parts), maxPartsPerMember)
	}
	return parts, nil
}

// splitByLines partitions text into contiguous line-range chunks of at most max
// bytes each. Used only for oversize SOURCE FILES, where a heading split is not
// available: every line still reaches the reviewer, in a labelled chunk, and a
// synthesis pass merges the chunks' findings. Returns an error only when a
// single line exceeds max (impossible for real source; a 4 MB log line, say),
// so a genuinely unsplittable blob still fails loudly.
func splitByLines(text string, max int) ([]piece, error) {
	if max < 200 {
		return nil, fmt.Errorf("the context window leaves only %d bytes per chunk — too small to split a file", max)
	}
	var out []piece
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, piece{text: cur.String()})
			cur.Reset()
		}
	}
	for _, line := range strings.SplitAfter(text, "\n") {
		if len(line) > max {
			return nil, fmt.Errorf("a %d-byte line exceeds the %d-byte chunk limit", len(line), max)
		}
		if cur.Len() > 0 && cur.Len()+len(line) > max {
			flush()
		}
		cur.WriteString(line)
	}
	flush()
	return out, nil
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
	// normDecorRe matches a run of header-decoration characters: box-drawing
	// (U+2500–U+257F), em/en dashes and ASCII '-'/'='. Added so a reviewer
	// citing the full "── label ──" header line matches the stored label.
	normDecorRe = regexp.MustCompile(`^[\s─-╿—–=\-]+|[\s─-╿—–=\-]+$`)
)

func normHeading(s string) string {
	s = strings.ToLower(normDropRe.ReplaceAllString(s, " "))
	s = normDecorRe.ReplaceAllString(s, "")
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
		// Strip block-header decoration before the paren loop, so a cited
		// "── label (N lines) ──" still has its line count stripped.
		s = normDecorRe.ReplaceAllString(s, "")
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

// Prompt skeleton (items 3+6). The user message is these seven lines in this
// order; between lines 2 and 4 is exactly the briefing text (parts/appendix),
// between 5 the Question is the question (+ part framing), and line 7 is the
// format instruction. Nothing else is sent. Order is load-bearing for the
// model: instructions first, then the material, then the question, then the
// format instruction last.
const (
	skeletonPrefix      = "Instructions follow the material below; this is the first and only instruction block."
	skeletonMaterial    = "Material under review (the document):"
	skeletonQuestion    = "Question:"
	skeletonFormat      = "Format instructions:"
	skeletonLastSection = "LAST SECTION READ: <<the heading or block label of the LAST section of the material above>>"
)

const evidenceInstruction = "Finish your reply with one final line, exactly: `" + evidencePrefix + " <the heading (or `── label ──` block label — for a source file, cite its path) of the LAST section of the document above that you actually read (ignore quoted answers of other models)>`. This is checked mechanically; a missing citation is flagged as coverage not confirmed."

// buildUserMessage assembles the seven-line skeleton: prefix, material
// header, part text (document + appendix), Question header, question (+ part
// framing inside the Question section only), Format header, and the evidence
// instruction as the last line.
func buildUserMessage(partText, question string) string {
	return skeletonPrefix + "\n" +
		skeletonMaterial + "\n" +
		partText + "\n" +
		skeletonQuestion + "\n" +
		question + "\n" +
		skeletonFormat + "\n" +
		evidenceInstruction
}

// cleanedOrWhole returns the reply with its trailing citation line stripped
// when present, otherwise the reply itself. Used by the citation-accept branch
// to keep the review content without the citation line.
func cleanedOrWhole(ans string) string {
	clean, cited := extractEvidence(ans)
	if cited != "" {
		return clean
	}
	return ans
}

// hasReviewContent reports whether a reply carries any review content once a
// trailing citation line (if any) is stripped. An empty reply, or a bare
// citation with nothing else, has no review content.
func hasReviewContent(ans string) bool {
	clean, _ := extractEvidence(ans)
	return strings.TrimSpace(clean) != ""
}

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
	return dispatchReviewWithReasoning(ctx, label, ctxTokens, configuredMax, floor, 0, question, briefing, appendix, send, nil)
}

// dispatchReviewWithReasoning is dispatchReview with an explicit reasoning
// reserve: reasoningBudget tokens are subtracted from the usable context
// when sizing the answer budget, so the additive wire total (reasoning +
// answer) fits the window. The answer floor is 4096 for every tool — a
// floor below it (including 0 and the old check floor) is raised to it.
func dispatchReviewWithReasoning(ctx context.Context, label string, ctxTokens, configuredMax int, floor int, reasoningBudget int, question, briefing, appendix string, send sendFunc, budget *BudgetState) reviewOutcome {
	if floor < defaultMinAnswerTokens {
		floor = defaultMinAnswerTokens
	}
	if configuredMax <= 0 {
		configuredMax = floor
	}
	answerWant := max(configuredMax, floor)
	usable := int(float64(ctxTokens) * contextHeadroom)
	// Reserve explicit reasoning tokens from the usable window so the
	// additive wire total (reasoning + answer) fits: usable − reasoning
	// − input − reserve ≥ floor. The answer floor is never reduced for
	// reasoning. (Floorless callers pass 0; runMember passes the effective
	// per-model reasoning budget.)
	usableForAnswer := usable - reasoningBudget
	// Largest input (tokens) that still leaves room for the floor answer.
	inputBudget := usableForAnswer - floor - answerReserveTokens
	log := &ReviewLog{Model: label, ContextTokens: ctxTokens}
	send = withProviderRetry(send, &log.ProviderRetried)

	doc := briefing
	hasStructure := len(headingsOf(doc)) > 0 || strings.Contains(doc, "\n── ") || strings.HasPrefix(doc, "── ")
	// Fixed cost of one request besides the document body: skeleton header
	// lines + question + evidence instruction + appendix + part framing slack.
	overheadChars := len(skeletonPrefix) + len(skeletonMaterial) + len(skeletonQuestion) + len(skeletonFormat) + len(question) + len(evidenceInstruction) + len(appendix) + 220
	docBudgetChars := inputBudget*reviewCharsPerToken - overheadChars

	var parts []part
	var planErr error
	if inputBudget <= 0 || docBudgetChars <= 0 {
		planErr = fmt.Errorf("model context window (%d tokens) leaves no room for the question and a %d-token answer", ctxTokens, floor)
	} else {
		parts, planErr = planParts(briefing, docBudgetChars)
	}
	if planErr != nil {
		return reviewOutcome{Log: log, Err: fmt.Errorf("document does not fit %s (input ~%d tokens needed vs %d-token context window; usable input budget ~%d tokens after the %d-token answer floor and reserve): %w — nothing was sent",
			label, estTokens(len(briefing)+overheadChars), ctxTokens, inputBudget, floor, planErr)}
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
			text := p.text + appendix
			userMsg := buildUserMessage(text, q)
			inChars := len(oracleSystemPrompt) + len(userMsg)
			inTok := estTokens(inChars)
			log.TokensSent += inTok
			maxTok := min(want, usableForAnswer-inTok-answerReserveTokens)
			maxTok = max(maxTok, floor) // floor is guaranteed by the input budget
			log.MaxTokens = maxTok
			diag.Printf("mashura: %s part %d/%d ~%d input tokens, max_tokens %d, context %d\n", label, i+1, n, inTok, maxTok, ctxTokens)
			if budget != nil {
				if reason, stop := budget.shouldStop(); stop {
					// Budget guard: stop BEFORE this part send with a
					// BudgetError (never VOID) so the member is not
					// retried and no further calls are made.
					out.Err = &BudgetError{Reason: reason}
					return out, false
				}
			}
			ans, usage, err := send(ctx, q, p.text, appendix, maxTok)
			out.Usage = addUsage(out.Usage, usage)
			log.CallCount++
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
						reason := fmt.Sprintf("reply hit max_tokens (%d) on part %d of %d", maxTok, i+1, n)
						if usage.ReasoningTokens*5 >= int64(maxTok)*4 {
							reason = fmt.Sprintf("reply hit max_tokens (%d); reasoning used the budget (%d of %d) on part %d of %d", maxTok, usage.ReasoningTokens, maxTok, i+1, n)
						}
						out.Err = &VoidError{Reason: reason}
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
				if cited == "" || !citationMatches(cited, exp...) {
					if hasReviewContent(cleanedOrWhole(ans)) {
						// Missing or wrong citation with real content: accept
						// the review, flag coverage as not confirmed. The
						// reply is obtained (not VOID).
						log.CoverageNotConfirmed = true
						if cited != "" {
							ans = clean
						}
						// else keep ans as the whole non-empty reply text
					} else {
						reason := fmt.Sprintf("part %d of %d: reply did not cite the last section it read (expected %q)", i+1, n, exp[0])
						if cited != "" {
							reason = fmt.Sprintf("part %d of %d: reply cited %q (normalized %q) as the last section read but the part ends at %q (normalized %q) — the reviewer did not read it all", i+1, n, cited, normHeading(cited), exp[0], normHeading(exp[0]))
						}
						if strings.TrimSpace(clean) == "" && cited != "" {
							reason = fmt.Sprintf("part %d of %d: the reply contained only the last-section citation and no review", i+1, n)
						}
						out.Err = &VoidError{Reason: reason}
						return out, false
					}
				} else {
					if strings.TrimSpace(clean) == "" {
						out.Err = &VoidError{Reason: fmt.Sprintf("part %d of %d: the reply contained only the last-section citation and no review", i+1, n)}
						return out, false
					}
					ans = clean
					log.ReadThrough = cited
				}
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
				out.Usage = addUsage(out.Usage, usage)
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
		out.Usage = addUsage(out.Usage, usage)
		out.Err = voidAfterTruncation(out.Err, "chunked retry")
	}
	out.Log = log
	if out.Err != nil {
		out.Answer = "" // never merge or show a partial reply
	}
	// Synthesis pass: when an oversize SOURCE FILE was split into line-range
	// chunks, the per-chunk findings are merged by one more call to the SAME
	// model — the chunks are a labelled partition, and the reader is asked to
	// produce a single review of the file from all of them. The synthesis input
	// is the merged per-part answers (far smaller than the document), so it
	// fits; a synthesis failure keeps the merged parts (they are each a real
	// review of a real range) and says so.
	if out.Err == nil && len(parts) > 1 && anyOversizeFile(parts) {
		synth, serr := synthesizeReview(ctx, question, out.Answer, usable, floor, answerReserveTokens, send, log)
		if serr != nil {
			log.Why = "synthesis pass failed (" + serr.Error() + "); showing the per-part reviews"
		} else {
			out.Answer = synth.Answer
			out.Usage = addUsage(out.Usage, synth.Usage)
			log.Synthesized = true
		}
	}
	diag.Printf("mashura: %s sent ~%d tokens in %d part(s), context %d, finish_reason %v, read through %q, err=%v\n", label, log.TokensSent, log.Parts, ctxTokens, log.FinishReasons, log.ReadThrough, out.Err)
	return out
}
