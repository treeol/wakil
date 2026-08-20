package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/treeol/wakil/internal/memory"
	"github.com/treeol/wakil/internal/proxy"
)

// handoffSummaryPrompt instructs the summarizer to produce a structured handoff
// summary suitable for seeding a continuation turn in a new session. The
// summary is delimited as prior-session context, not as instructions, to
// mitigate prompt-injection from adversarial transcript content.
const handoffSummaryPrompt = `Summarize this conversation for session handoff. Produce a structured summary with:
- Original task / user goal
- Completed work, partially completed work, known blockers
- Important decisions made (architecture choices, rejected alternatives, assumptions)
- Files changed (paths, purpose of changes)
- Commands run (tests, linters, builds — pass/fail results)
- Open questions / next actions (exact next command or file to inspect)

Be concise but complete — this summary is the only bridge to the next session.

Transcript:
`

// handoffOlderSummaryPrompt instructs the summarizer to summarize ONLY the older
// block of a session, tersely. It explicitly avoids reconstructing the recent
// tail, which is preserved separately at higher fidelity for the next session.
const handoffOlderSummaryPrompt = `Summarize the EARLIER portion of this session for a continuation in a new session. The most recent portion is preserved separately and verbatim, so do NOT try to reconstruct it — cover only the older material here. Produce a structured summary with:
- Original task / user goal
- Completed work, partially completed work, known blockers
- Important decisions made (architecture choices, rejected alternatives, assumptions)
- Files changed (paths, purpose of changes)
- Commands run (tests, linters, builds — pass/fail results)
- Open questions / next actions

Be concise but complete — the newer portion is NOT summarized here, so keep forward-references short and concrete.

Older transcript:
`

// HandoffPayload is the structured result of generating a handoff: a coarse
// summary of the older block plus a high-fidelity (rendered, prose-only) recent
// tail. Only CoarseSummary should feed the session index's summary field; the
// recent tail is continuation context (and is stripped from the index by the
// handoff envelope at re-index time).
type HandoffPayload struct {
	CoarseSummary string // summarized older block (bounded); may be ""
	RecentTail    string // high-fidelity recent prose (bounded); may be ""
}

// Handoff payload byte budgets. These are deliberately INDEPENDENT of
// rememberFoldByteCap (4000), which budgets a small recall fold injected into an
// ongoing session — a different constraint than seeding a near-empty new session.
//
// handoffTotalByteCap bounds the payload FIELDS (coarse summary + recent tail);
// it does not include the envelope framing (header/markers/headings/instruction)
// added by BuildContinuationPrompt/BuildHandoffContext. The two sub-budgets are
// set so they sum exactly to the total; if either is later changed, keep the
// sum in line with handoffTotalByteCap.
const (
	handoffTotalByteCap   = 16000 // total handoff payload (coarse + tail)
	handoffSummaryByteCap = 8000  // coarse-summary sub-budget
	handoffTailByteCap    = 8000  // recent-tail sub-budget
)

// handoffRecord is the sidecar JSON written next to the session file as a
// fallback when the durable memory store is unavailable, and as an audit
// artifact regardless.
type handoffRecord struct {
	OldChatID   string    `json:"old_chat_id"`
	NewChatID   string    `json:"new_chat_id"`
	Workspace   string    `json:"workspace"`
	Timestamp   time.Time `json:"timestamp"`
	Summary     string    `json:"summary"`
	RecentTail  string    `json:"recent_tail,omitempty"`
	Prompt      string    `json:"continuation_prompt"`
	Model       string    `json:"model,omitempty"`
	TranscriptN int       `json:"transcript_msgs"`
}

// RunHandoffPipeline executes the read-only-plus-persistence half of a handoff
// (7b3 m4): validation, old-session save, summary generation, session-history
// indexing, and the durable handoff record. It does NOT create or mutate the
// new conversation — the caller (wiring's ConversationManager) builds the new
// facade and seeds it from the returned result.
//
// This is the exported seam over performHandoff's steps 1–4 so the wiring
// package (which cannot call the unexported performHandoff internals) runs the
// exact same pipeline the old TUI's /handoff Cmd ran. Proceed-continuation is
// the caller's job: ContinuationPrompt is returned for the caller to enqueue.
func RunHandoffPipeline(ctx context.Context, app *App) (HandoffResult, error) {
	if len(app.Conv) == 0 {
		return HandoffResult{}, fmt.Errorf("nothing to hand off (empty conversation)")
	}
	hasUser := false
	for _, m := range app.Conv {
		if m.Role == "user" {
			hasUser = true
			break
		}
	}
	if !hasUser {
		return HandoffResult{}, fmt.Errorf("nothing to hand off (no user messages in conversation)")
	}

	oldChatID := app.Client.ChatID
	newChatID := NewChatID()
	workspace := app.SessionWorkspace()

	// 1. Save the old session with full transcript + workflow.
	app.Session.Conv = app.Conv
	app.Session.Updated = time.Now()
	if app.Session.Workspace == "" {
		app.Session.Workspace = workspace
	}
	app.Session.SavedWorkflow = app.Workflow
	if err := WriteSession(app.Session); err != nil {
		return HandoffResult{}, fmt.Errorf("could not save old session: %w", err)
	}

	// 2. Generate the recency-split payload (slow step — summarizer call).
	payload, err := generateHandoffPayload(ctx, app)
	if err != nil {
		return HandoffResult{}, fmt.Errorf("summary generation failed: %w", err)
	}

	// 3. Index the finalized session with its generated summary (best-effort,
	// with performHandoff's fallback chain: on a summary-store failure, still
	// index the session preserving any existing summary).
	if app.SessionHistory != nil {
		in := sessionToIndexInput(*app.Session)
		if cs := strings.TrimSpace(payload.CoarseSummary); cs != "" {
			in.Summary = cs
			in.SummaryGenerated = true
		}
		in.SourceHash = sourceHash(*app.Session)
		if err := app.SessionHistory.Index(ctx, in); err != nil {
			fmt.Fprintf(app.Out, "session history: store handoff summary: %v\n", err)
			_ = app.indexSession(ctx, *app.Session, true) // fallback: preserve existing summary
		}
	}

	// 4. Store the handoff record (durable memory + sidecar; best-effort).
	continuationPrompt := BuildContinuationPrompt(payload, oldChatID, workspace)
	warnings := storeHandoffRecord(ctx, app, payload, continuationPrompt, oldChatID, newChatID, workspace)

	note := fmt.Sprintf("handoff: %s → context stored", ShortID(oldChatID))
	if len(warnings) > 0 {
		note += " | " + strings.Join(warnings, "; ")
	}

	return HandoffResult{
		ContinuationPrompt: continuationPrompt,
		Summary:            payload.CoarseSummary,
		Payload:            payload,
		Note:               note,
		OldChatID:          oldChatID,
		NewChatID:          newChatID,
	}, nil
}

// HandoffResult is the structured outcome of RunHandoffPipeline — the fields
// of HandoffMsg that a handoff-driving caller (wiring's ConversationManager)
// needs to seed the new conversation and display the rotation.
type HandoffResult struct {
	ContinuationPrompt string
	Summary            string
	Payload            HandoffPayload
	Note               string
	OldChatID          string
	NewChatID          string
}

// performHandoff is the Cmd-closure body for /handoff. It does read-only work
// plus persistence: generates the summary, stores it in durable memory (with
// sidecar fallback), and returns a HandoffMsg for the TUI to act on.
//
// It does NOT call NewConversation or mutate app.Conv — that happens in the
// TUI's HandoffMsg handler on the event loop, avoiding races with concurrent
// user input during the (multi-second) summarization window.
//
// The old session is saved (with the full transcript) before the HandoffMsg is
// processed, so the TUI handler can safely rotate the conversation knowing the
// old session is on disk.
//
// The proceed parameter controls the TUI handler behavior: true = auto-start
// a continuation turn (the original /handoff behavior); false = display the
// summary and wait for input (the new default).
func performHandoff(ctx context.Context, app *App, proceed bool) Msg {
	if len(app.Conv) == 0 {
		return HandoffMsg{Err: fmt.Errorf("nothing to hand off (empty conversation)")}
	}

	// Require at least one non-system message (a user turn).
	hasUser := false
	for _, m := range app.Conv {
		if m.Role == "user" {
			hasUser = true
			break
		}
	}
	if !hasUser {
		return HandoffMsg{Err: fmt.Errorf("nothing to hand off (no user messages in conversation)")}
	}

	oldChatID := app.Client.ChatID
	newChatID := NewChatID()
	workspace := app.SessionWorkspace()

	// Emit progress feedback so the user sees something is happening during
	// the multi-second summarization (same channel as streaming chunks).
	app.sendEvent(SysNoteMsg{Text: "· handoff: saving session…"})

	// 1. Save the old session with full transcript before anything else.
	//    SaveSession is best-effort (swallows errors); we also WriteSession
	//    directly to surface failures.
	app.Session.Conv = app.Conv
	app.Session.Updated = time.Now()
	if app.Session.Workspace == "" {
		app.Session.Workspace = workspace
	}
	app.Session.SavedWorkflow = app.Workflow
	if err := WriteSession(app.Session); err != nil {
		return HandoffMsg{
			OldChatID: oldChatID,
			Err:       fmt.Errorf("could not save old session: %w", err),
		}
	}

	// 1b. (No ingest here — the finalized session with its generated summary is
	// stored in step 2b below, a single index write. Ingesting now would be a
	// redundant whole-session replace that then gets overwritten, and would
	// clobber any prior generated summary if step 2b failed.)

	// 2. Generate the recency-split handoff payload via the summarizer (calls
	//    the proxy, same as /compact). This is the slow step — it runs off the
	//    event loop. Add a timeout so a hanging backend can't block the handoff
	//    forever.
	app.sendEvent(SysNoteMsg{Text: "· handoff: generating summary…"})

	sumCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	payload, err := generateHandoffPayload(sumCtx, app)
	if err != nil {
		return HandoffMsg{
			OldChatID: oldChatID,
			Err:       fmt.Errorf("summary generation failed: %w", err),
		}
	}

	// 2b. Store the finalized session AND its generated handoff summary in the
	// session-history index (a single index write, no extra inference — the
	// summary was just produced above). Only the CoarseSummary feeds the index
	// summary field; the RecentTail is continuation context (stripped from the
	// index by the handoff envelope). When the CoarseSummary is empty (e.g. the
	// whole conversation fit the tail), do NOT clobber a summarizer/harvested
	// summary already present in sessionToIndexInput. On failure, still index
	// the session (preserving any existing generated summary) so its turns are
	// searchable.
	if app.SessionHistory != nil {
		in := sessionToIndexInput(*app.Session)
		if cs := strings.TrimSpace(payload.CoarseSummary); cs != "" {
			in.Summary = cs
			in.SummaryGenerated = true
		}
		in.SourceHash = sourceHash(*app.Session)
		if err := app.SessionHistory.Index(ctx, in); err != nil {
			fmt.Fprintf(app.Out, "session history: store handoff summary: %v\n", err)
			_ = app.indexSession(ctx, *app.Session, true) // fallback: preserve existing summary
		}
	}

	// 3. Build the continuation prompt for the new session.
	continuationPrompt := BuildContinuationPrompt(payload, oldChatID, workspace)

	// 4. Store the handoff record in durable memory (best-effort) + sidecar
	//    JSON (always, as audit artifact).
	app.sendEvent(SysNoteMsg{Text: "· handoff: storing record…"})
	warnings := storeHandoffRecord(ctx, app, payload, continuationPrompt, oldChatID, newChatID, workspace)

	note := fmt.Sprintf("handoff: %s → context stored", ShortID(oldChatID))
	if len(warnings) > 0 {
		note += " | " + strings.Join(warnings, "; ")
	}

	return HandoffMsg{
		ContinuationPrompt: continuationPrompt,
		Summary:            payload.CoarseSummary,
		Payload:            payload,
		Proceed:            proceed,
		Note:               note,
		OldChatID:          oldChatID,
		NewChatID:          newChatID,
	}
}

// generateHandoffPayload produces the recency-split handoff payload: a coarse
// summary of the older block plus a high-fidelity recent tail. The tail is
// rendered with renderIndexableTranscript rules (user + assistant text only,
// retrieval blocks stripped) so it honors the index's secret-hygiene boundary,
// and capped at handoffTailByteCap. Only the older block is summarized (the
// recent tail is preserved at higher fidelity, not re-inferred).
func generateHandoffPayload(ctx context.Context, app *App) (HandoffPayload, error) {
	// Exclude the preamble (Conv[0] if it's a system message — the new session
	// will regenerate its own preamble).
	conv := app.Conv
	if len(conv) > 0 && conv[0].Role == "system" {
		conv = conv[1:]
	}

	older, tail := splitHandoffConv(conv)

	payload := HandoffPayload{}
	// Render the recent tail (high-fidelity prose) — never summarized. Truncate
	// from the FRONT (keep the newest bytes) so a recency tail never evicts the
	// most recent content, which is exactly what it exists to preserve.
	payload.RecentTail = truncateTailUTF8(renderIndexableTranscript(tail), handoffTailByteCap)

	// If the older block is empty, carry the whole conversation via the tail and
	// skip summarization (nothing to summarize).
	olderRender := renderIndexableTranscript(older)
	if strings.TrimSpace(olderRender) == "" {
		return payload, nil
	}
	// Prepend any harvested compaction summary (a "[Summary of earlier
	// conversation]" system message) to the older-block render, so pre-compaction
	// history — which renderIndexableTranscript otherwise drops — is not lost
	// from the handoff.
	if cs := harvestCompactionSummary(conv); cs != "" {
		olderRender = "EARLIER SESSION SUMMARY: " + cs + "\n\n" + olderRender
	}

	sum := app.summarizeFn()
	if sum == nil {
		// No summarizer available — fall back to a capped render of the older
		// block (bound, never an unlimited dump).
		payload.CoarseSummary = truncateUTF8(olderRender, handoffSummaryByteCap)
		return payload, nil
	}

	text := handoffOlderSummaryPrompt + olderRender
	summary, err := sum(ctx, text)
	if err != nil {
		return HandoffPayload{}, err
	}
	payload.CoarseSummary = truncateUTF8(strings.TrimSpace(summary), handoffSummaryByteCap)
	return payload, nil
}

// harvestCompactionSummary extracts the compaction summary from a "[Summary of
// earlier conversation]" system message, mirroring sessionToIndexInput's harvest
// so a compacted session's pre-compaction history is not lost from the handoff.
// Returns "" when absent.
func harvestCompactionSummary(conv []proxy.Message) string {
	for _, m := range conv {
		if m.Role != "system" {
			continue
		}
		c := DerefStr(m.Content)
		if strings.Contains(c, "[Summary of earlier conversation]") {
			if idx := strings.Index(c, "]"); idx >= 0 && len(c) > idx+1 {
				return strings.TrimSpace(c[idx+1:])
			}
		}
	}
	return ""
}

// truncateTailUTF8 truncates s to at most n bytes keeping the NEWEST (trailing)
// bytes, without splitting a UTF-8 rune. Recency tails must preserve the most
// recent content, so overflow drops the oldest portion rather than the newest.
func truncateTailUTF8(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	// Keep the last n bytes, then back off to the nearest rune boundary.
	cut := s[len(s)-n:]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		_, size := utf8.DecodeRuneInString(cut)
		if size == 0 {
			break
		}
		cut = cut[size:]
	}
	return cut
}

// splitHandoffConv splits a (preamble-stripped) conversation into older and
// recentTail at a user-turn boundary. The tail target is 30% of the RENDERED
// (renderIndexableTranscript) byte weight, capped at handoffTailByteCap. The
// split lands on a user boundary so a user turn and its assistant responses are
// never separated, and complete recent user turns are accumulated until the
// target is reached. If the whole transcript renders within the tail cap, the
// entire conversation becomes the tail and older is empty (no summarization).
func splitHandoffConv(conv []proxy.Message) (older, tail []proxy.Message) {
	total := len(renderIndexableTranscript(conv))
	if total == 0 {
		return conv, nil
	}
	// If the whole conversation renders within the tail budget, carry it all as
	// the recent tail and skip summarization (older empty).
	if total <= handoffTailByteCap {
		return nil, conv
	}
	target := int(float64(total) * 0.30)
	if target > handoffTailByteCap {
		target = handoffTailByteCap
	}

	idx := len(conv)
	acc := 0
	first := true
	for i := len(conv) - 1; i >= 0; i-- {
		if conv[i].Role != "user" {
			continue
		}
		// This user turn spans conv[i:idx] (i is a user start; idx is the next
		// user start or the end). Its rendered byte weight is additive.
		turn := conv[i:idx]
		w := len(renderIndexableTranscript(turn))
		// Always include the most recent user turn (never summarize the latest
		// task/redirect), even if it alone exceeds the target.
		if first || acc+w <= target {
			acc += w
			idx = i
			first = false
			continue
		}
		break
	}
	if idx == 0 {
		return nil, conv // whole conversation is the tail
	}
	return conv[:idx], conv[idx:]
}

// BuildContinuationPrompt constructs the first-turn prompt for the new session.
// The handoff context is delimited as an untrusted, message-LEADING envelope
// (context-then-instruction), so the leading-anchored stripRetrievalBlock can
// strip it at index time — the feedback-loop guard. The instruction ("continue
// with the next action") is placed AFTER the envelope so it survives stripping
// as the new session's first indexed user turn. Distinct markers neutralize
// spoofing of the handoff boundary.
func BuildContinuationPrompt(payload HandoffPayload, oldChatID, workspace string) string {
	var b strings.Builder
	b.WriteString(handoffBlockHeader)
	b.WriteString("Prior session: " + ShortID(oldChatID) + "  workspace: " + formatWorkspace(workspace) + "\n\n")
	if strings.TrimSpace(payload.CoarseSummary) != "" {
		b.WriteString("## Coarse summary of the earlier part of the session\n")
		b.WriteString(neutralizeHandoffMarker(payload.CoarseSummary))
		b.WriteString("\n\n")
	}
	if strings.TrimSpace(payload.RecentTail) != "" {
		b.WriteString("## Recent portion (higher fidelity) — chronologically NEWER than the coarse summary; trust this over it if they disagree\n")
		b.WriteString(neutralizeHandoffMarker(payload.RecentTail))
		b.WriteString("\n")
	}
	b.WriteString(handoffBlockEnd)
	b.WriteString("\n\nContinue where the previous session left off. Start by briefly acknowledging what was done and what remains, then proceed with the next action.")
	return b.String()
}

// BuildHandoffContext constructs the pinned context message for stop-mode
// handoff. Unlike BuildContinuationPrompt, it does NOT instruct the model to
// "proceed with the next action" — the user will provide the next instruction.
// The handoff context is delimited as an untrusted envelope with distinct
// markers. Note: stop-mode context is injected as a SYSTEM message (not
// indexed), so the feedback-loop strip is not required for it, but the same
// envelope framing is used for consistency and spoof-mitigation.
func BuildHandoffContext(payload HandoffPayload, oldChatID, workspace string) string {
	var b strings.Builder
	b.WriteString(handoffBlockHeader)
	b.WriteString("Prior session: " + ShortID(oldChatID) + "  workspace: " + formatWorkspace(workspace) + "\n\n")
	if strings.TrimSpace(payload.CoarseSummary) != "" {
		b.WriteString("## Coarse summary of the earlier part of the session\n")
		b.WriteString(neutralizeHandoffMarker(payload.CoarseSummary))
		b.WriteString("\n\n")
	}
	if strings.TrimSpace(payload.RecentTail) != "" {
		b.WriteString("## Recent portion (higher fidelity) — chronologically NEWER than the coarse summary; trust this over it if they disagree\n")
		b.WriteString(neutralizeHandoffMarker(payload.RecentTail))
		b.WriteString("\n")
	}
	b.WriteString(handoffBlockEnd)
	return b.String()
}

// formatWorkspace renders a workspace path for embedding in handoff context,
// flattening it to one line, stripping control characters so a malicious path
// cannot spoof framing/layout, and neutralizing the handoff end marker so a
// malicious path cannot prematurely close the envelope.
func formatWorkspace(ws string) string {
	return neutralizeHandoffMarker(flattenLabel(ws))
}

// storeHandoffRecord persists the handoff payload to durable memory (mid-tier,
// 7-day TTL — coarse summary only, never verbatim tail) and always writes a
// sidecar JSON next to the session file as an audit artifact (coarse summary +
// recent tail, structured). Returns warnings for non-fatal failures.
func storeHandoffRecord(ctx context.Context, app *App, payload HandoffPayload, continuationPrompt, oldChatID, newChatID, workspace string) []string {
	var warnings []string

	// Durable memory (best-effort — nil store is common in test/headless).
	// Only the coarse summary goes to durable memory — the recent tail is
	// untrusted verbatim transcript prose and must not be persisted/resurfaced
	// via retrieveMemoryContext; it lives only in the on-disk sidecar audit file.
	// Skip the durable record entirely when there is no coarse summary (e.g. the
	// whole conversation fit the tail) so we don't write an empty-value entry.
	if strings.TrimSpace(payload.CoarseSummary) != "" {
		if app.MemoryStore != nil {
			key := "handoff/" + oldChatID
			value := fmt.Sprintf("Session handoff summary for %s\n\n%s", oldChatID, payload.CoarseSummary)
			_, err := app.MemoryStore.PutActive(ctx, key, value, "handoff", memory.TierMid,
				"main", oldChatID, memory.TaintUnknown, ptr(time.Now().Add(7*24*time.Hour).UnixMilli()),
				nil, "session handoff")
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("memory store: %v (sidecar written)", err))
			}
		} else {
			warnings = append(warnings, "memory store unavailable (sidecar written)")
		}
	}

	// Sidecar JSON — always written as audit artifact. Mode 0o600: the compacted
	// session file is private transcript material and the sidecar now carries a
	// high-fidelity recent tail (untrusted but still private).
	dir := sessionsDir()
	if dir != "" {
		rec := handoffRecord{
			OldChatID:   oldChatID,
			NewChatID:   newChatID,
			Workspace:   workspace,
			Timestamp:   time.Now(),
			Summary:     payload.CoarseSummary,
			RecentTail:  payload.RecentTail,
			Prompt:      continuationPrompt,
			Model:       app.Client.Model,
			TranscriptN: len(app.Conv),
		}
		b, _ := json.MarshalIndent(rec, "", "  ")
		path := filepath.Join(dir, oldChatID+".handoff.json")
		if err := os.WriteFile(path, b, 0o600); err != nil {
			warnings = append(warnings, fmt.Sprintf("sidecar: %v", err))
		}
	}

	return warnings
}

// ptr is a small helper for *int64.
func ptr(v int64) *int64 { return &v }
