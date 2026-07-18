package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	agent "github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/tools"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

type agentState int

const (
	stateIdle agentState = iota
	stateStreaming
	stateConfirm
	stateCompacting
)

const sidebarWidth = 28

// maxSubTabs bounds how many subagent tabs are retained; once exceeded, the
// oldest finished tabs (never the running or currently-viewed one) are pruned.
const maxSubTabs = 12

// subTabAutoCloseDelay is how long after a subagent tab becomes done before it
// is auto-closed (if the user is not currently viewing it). One-shot timer
// armed in the SubagentDoneMsg handler.
const subTabAutoCloseDelay = 30 * time.Second

// pasteSuppressWindow is how long key events are swallowed after a binary
// paste is detected mid-stream. Fragments of one paste arrive within
// microseconds of each other; 150ms is far beyond any inter-fragment gap
// while short enough that real typing resumes almost immediately. Each
// swallowed event extends the window, so a long paste tail stays covered.
const pasteSuppressWindow = 150 * time.Millisecond

// itemKind tags each committed conversation entry for visual rendering.
type itemKind int8

const (
	iUser itemKind = iota // user turn
	iAsst                 // assistant response (may include tool result lines)
	iSys                  // system notes, confirm prompts, approve/decline
)

// convItem is one committed (non-streaming) conversation entry.
// text is pre-styled and may contain ANSI escape codes; it is word-wrapped
// at render time so it reflows correctly on terminal resize.
type convItem struct {
	kind itemKind
	text string

	// Render cache: committed items are immutable, so the (formatted, wrapped)
	// output is reused across refreshViewport calls and recomputed only when the
	// width changes. Matters because iAsst markdown rendering isn't cheap.
	cache  string
	cacheW int
}

type tuiModel struct {
	app        *agent.App
	cancel     context.CancelFunc
	cancelling bool // true after first Ctrl+C, until agent.AgentDoneMsg

	vp       viewport.Model
	ta       textarea.Model
	state    agentState
	pendConf *agent.ConfirmReqMsg

	// When the in-flight turn started; used to chime only on turns long enough
	// to be worth notifying about. Zero between turns.
	turnStart time.Time

	// tps is the live token/sec decode estimate shown in the status line while
	// streaming. Set from agent.TokRateMsg, cleared when the turn ends.
	tps float64

	// Pointer to slice so Bubble Tea's value-copy model contract doesn't
	// cause diverging copies (same reason convBuf was *strings.Builder).
	items *[]convItem

	// In-flight SSE content; committed to items on turn end or confirm gate.
	streaming *strings.Builder

	// reasoning accumulation state (reasoning, reasoningDone, reasoningExpanded).
	// Extracted to reasoning_model.go (WP-6.6); embedded so selector access is
	// unchanged. reasoning stays *strings.Builder — see file for the copy invariant.
	reasoningModel

	// Mouse text selection over the conversation pane (see tui_select.go).
	sel        selection
	plainLines []string // ANSI-stripped view content, kept in sync by refreshViewport
	flash      string   // transient status shown in the input border (e.g. "copied ✓")

	// imageChips tracks the placeholder strings (e.g. "[image: clipboard:png
	// · 1.8 MB]") inserted into the text input for clipboard-attached images.
	// At send time, chips still present in the input are stripped from the
	// outgoing text; chips the user deleted detach the corresponding pending
	// image. Cleared on send and on /new. Pointer to slice for the same
	// Bubble Tea value-copy reason as items.
	imageChips *[]string

	// pasteSuppressUntil, when in the future, swallows ALL key events (except
	// ctrl+c) — set right after a binary paste is detected mid-stream. A
	// fragmented binary paste keeps delivering KeyMsg events after detection:
	// printable runs arrive as KeyRunes bursts and stray control bytes are
	// decoded as control keys (0x0D becomes "enter", which would SEND the
	// garbage). Each swallowed event extends the deadline, so the window
	// covers the whole tail of the paste regardless of its length.
	pasteSuppressUntil time.Time

	// pasteCutStash holds the text that was cut from the input when a binary
	// paste was detected. If the clipboard read then FAILS (false positive:
	// the "garbage" was real text, e.g. a pasted hexdump analysis), the cut
	// text is restored to the input instead of being lost. Cleared when the
	// clipboard read succeeds.
	pasteCutStash string

	// Prefix cache: the rendered + stripped committed items (everything except the
	// live streaming tail). Rebuilt only when items change or the viewport resizes,
	// not on every streaming chunk. prefixDirty marks that a full rebuild is needed.
	prefixStyled string
	prefixPlain  []string
	prefixDirty  bool
	prefixW      int // width at which prefixStyled was built

	// "@" file-mention autocomplete picker (see complete.go).
	comp completionState

	// Interactive session browser opened by bare "/resume" (see resume_picker.go).
	resumePicker resumePickerState

	// Subagent tab state (subTabs, subCur, subSeq). Extracted to subagent_model.go
	// (WP-6.6); embedded so selector access is unchanged.
	subAgentModel

	width, height int
	ready         bool

	// dotPhase cycles 0-3 while the agent is busy, driving the pulsing dot.
	// Reset to 0 when idle; the tick self-terminates (no re-arm when idle).
	dotPhase int
	// hadTurn is set after the first agent.AgentDoneMsg so the status line can show
	// "awaiting input" instead of silent idle.
	hadTurn bool

	// Input history for UP/DOWN navigation (most-recent entry first).
	// Extracted to history_model.go (WP-6.6); embedded so selector access is unchanged.
	historyModel

	// Reverse-incremental search (Ctrl+R) state. Extracted to search_model.go
	// (WP-6.6); embedded so selector access is unchanged.
	searchModel
}

// subTab holds the state of one dispatched subagent, used to render its
// tab in the main pane and its info in the sidebar.
type subTab struct {
	n            int
	task         string
	chatID       string
	backend      string           // resolved backend (from SubagentStartMsg.Backend)
	usedBackend  string           // actual backend from last response (SubagentDoneMsg.UsedBackend)
	costUSD      float64          // child's priced cost, folded into the parent tracker (SubagentDoneMsg.CostUSD)
	buf          *strings.Builder // tool-call lines + final JSON output
	grounding    []proxy.GroundingEntry
	ctxSize      int
	hardMaxBytes int
	filesChanged []string  // mechanical record of canonical paths touched (edit-tier only)
	capability   string    // "discovery", "edit", or "tools" — drives the sidebar tool list (from Start)
	model        string    // child's resolved model (from Start)
	toolNames    []string  // tool names for the sidebar (tools-tier only; nil for discovery/edit — hardcoded)
	active       bool      // worker acquired a parallelism slot (queued → running)
	done         bool      // authoritative done (SubagentDoneMsg received)
	finished     bool      // display-only early done (SubagentFinishedMsg received)
	finishedAt   time.Time // when SubagentFinishedMsg arrived (for timestamped display)
	finStatus    string    // status from SubagentFinishedMsg: "ok"/"incomplete"/"failed"/"declined"
	finCostUSD   float64   // child's own total from SubagentFinishedMsg (display-only)
	finFilesN    int       // count of files changed from SubagentFinishedMsg
	finPreview   string    // summary preview from SubagentFinishedMsg

	// Render cache for renderSubTabContent. Invalidated when buf grows or vpW changes.
	cachedLines []string
	cacheVpW    int
	cacheBufLen int
}

// pruneSubTabs caps the retained subagent tabs at max, dropping the oldest
// finished tabs first. Running tabs (done == false) and the currently-viewed
// tab (focusN) are always kept, so a long-lived session can't accumulate tabs
// without bound nor lose a live stream or the one the user is watching. With
// parallel dispatch several tabs may be running at once — all are protected.
// A tab that is finished (display-only early event) but not yet done
// (authoritative SubagentDoneMsg pending) is protected in the FIRST pass (only
// done tabs are dropped). But if there aren't enough done tabs to reach the cap,
// a second pass may drop finished-but-not-done tabs as a last resort (still
// never running or focused tabs) — see the fallback loop below.
func pruneSubTabs(tabs []*subTab, focusN, max int) []*subTab {
	if len(tabs) <= max {
		return tabs
	}
	drop := len(tabs) - max
	kept := make([]*subTab, 0, len(tabs))
	for _, t := range tabs {
		if drop > 0 && t.done && t.n != focusN {
			drop--
			continue
		}
		kept = append(kept, t)
	}
	// If not enough done tabs were droppable, drop finished (but not done) tabs
	// as the next priority — still protecting running and focused tabs.
	if drop > 0 {
		kept = kept[:0]
		for _, t := range tabs {
			if drop > 0 && t.finished && t.n != focusN {
				drop--
				continue
			}
			kept = append(kept, t)
		}
	}
	return kept
}

// tabIndexByN maps a subtab sequence number to its slice index, or -1 (main
// tab) when n is 0 or no longer present.
func tabIndexByN(tabs []*subTab, n int) int {
	if n == 0 {
		return -1
	}
	for i, t := range tabs {
		if t.n == n {
			return i
		}
	}
	return -1
}

func NewTUIModel(app *agent.App) tuiModel {
	ta := textarea.New()
	ta.Placeholder = "type a task… (Enter=send, Shift+Enter=newline, /help)"
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.SetWidth(80)
	ta.SetHeight(3)
	ta.Focus()

	vp := viewport.New(80, 20)
	// Restrict scroll keys to dedicated non-typing keys only. The default
	// keymap binds space, f, b, u, d, j, k, h, l and arrows — these all
	// conflict with normal textarea input when the user types while scrolled.
	vp.KeyMap = viewport.KeyMap{
		PageDown: key.NewBinding(key.WithKeys("pgdown")),
		PageUp:   key.NewBinding(key.WithKeys("pgup")),
		Up:       key.NewBinding(key.WithKeys("up")),
		Down:     key.NewBinding(key.WithKeys("down")),
	}
	items := make([]convItem, 0, 64)
	// Show agent prompt source so the user can confirm which file was loaded.
	if app != nil {
		items = append(items, convItem{kind: iSys, text: dim2(app.AgentPromptNote())})
	}
	// Resumed session: rebuild the conversation view from the loaded transcript.
	if len(app.Conv) > 0 {
		items = convItemsFrom(app.Conv)
		resumeNote := sprint("· resumed session %s — %d messages", agent.ShortID(app.Client.ChatID), len(app.Conv))
		if app.Workflow != nil {
			resumeNote += " · workflow restored: " + app.Workflow.PhaseName()
		}
		items = append(items, convItem{kind: iSys, text: dim2(resumeNote)})
	}
	return tuiModel{
		app:        app,
		vp:         vp,
		ta:         ta,
		state:      stateIdle,
		items:      &items,
		streaming:  &strings.Builder{},
		imageChips: &[]string{},
		subAgentModel: subAgentModel{
			subCur: -1,
		},
		reasoningModel: reasoningModel{
			reasoning: &strings.Builder{},
		},
		historyModel: historyModel{
			histIdx:      -1,
			inputHistory: loadHistory(),
		},
	}
}

func (m tuiModel) Init() tea.Cmd {
	if m.app != nil && m.app.StartupNote != "" {
		note := m.app.StartupNote
		m.app.StartupNote = "" // consume once — Init() may run more than once in theory
		return tea.Batch(textarea.Blink, func() tea.Msg { return agent.SysNoteMsg{Text: note} })
	}
	return textarea.Blink
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.sel = selection{} // re-wrap invalidates selection coordinates
		m = m.reflow()
		m.ready = true

	case tea.MouseMsg:
		var handled bool
		var mCmd tea.Cmd
		m, handled, mCmd = m.handleMouse(msg)
		if mCmd != nil {
			cmds = append(cmds, mCmd)
		}
		if handled {
			// Consumed by text selection; don't also forward to the viewport
			// (which would scroll) or the textarea.
			return m, tea.Batch(cmds...)
		}

	case tea.KeyMsg:
		// Binary-paste suppression window: after a mid-stream binary paste is
		// detected (below), the remaining fragments of the same paste keep
		// arriving as key events — printable runs as KeyRunes bursts, stray
		// control bytes as control keys (a 0x0D decodes as "enter" and would
		// SEND the garbage). Swallow everything except ctrl+c while the
		// window is open; each swallowed event extends it, so the window
		// tracks the paste tail regardless of length.
		if !m.pasteSuppressUntil.IsZero() && msg.String() != "ctrl+c" {
			if time.Now().Before(m.pasteSuppressUntil) {
				m.pasteSuppressUntil = time.Now().Add(pasteSuppressWindow)
				return m, tea.Batch(cmds...)
			}
			m.pasteSuppressUntil = time.Time{}
		}

		// Any keystroke dismisses an active selection and its highlight.
		m.flash = ""
		if m.sel.active {
			m.sel = selection{}
			m.refreshViewport()
		}

		// The resume picker owns input while open — checked before the "@"/"/"
		// completion picker since /resume closes that picker on open
		// (openResumePicker), so the two are never simultaneously active.
		// A handful of keys (ctrl+c/ctrl+d) are deliberately NOT consumed so
		// quitting the program still works with the picker open.
		if m.resumePicker.active {
			prevActive := m.resumePicker.active
			var rCmd tea.Cmd
			var rConsumed bool
			m, rCmd, rConsumed = m.handleResumePickerKey(msg)
			if rConsumed {
				if m.resumePicker.active != prevActive {
					m = m.reflow()
				}
				if rCmd != nil {
					cmds = append(cmds, rCmd)
				}
				var taCmd tea.Cmd
				m.ta, taCmd = m.ta.Update(tea.Msg(nil))
				return m, tea.Batch(append(cmds, taCmd)...)
			}
			// Not consumed (e.g. ctrl+c/ctrl+d) — fall through to normal handling.
		}

		// The "@" picker swallows navigation/accept keys while it's open.
		prevActive := m.comp.active
		if newM, ok := m.handleCompletionKey(msg); ok {
			m = newM
			// Esc or accept-file closes the picker; reflow so the viewport reclaims
			// the rows the picker was occupying (same fix as the subagent tab bar).
			if m.comp.active != prevActive {
				m = m.reflow()
			}
			var taCmd tea.Cmd
			m.ta, taCmd = m.ta.Update(tea.Msg(nil)) // keep blink; don't feed the key
			return m, tea.Batch(append(cmds, taCmd)...)
		}

		// Bracketed-paste interception: a bracketed paste arrives COMPLETE in
		// one KeyRunes event with Paste=true, so the whole-paste anchored
		// check applies. On match, stash the runes (restored if the clipboard
		// has no image) and read the real image.
		//
		// Deliberately Paste=true ONLY: non-bracketed pastes arrive as many
		// fragments, and intercepting a single matching fragment here would
		// swallow the signature-carrying piece while the rest of the garbage
		// flows into the textarea with nothing left for the accumulated scan
		// to recognize — the exact "garbage stays until Enter" failure mode.
		// Fragmented pastes are handled by the post-insert scan below.
		if msg.Paste && msg.Type == tea.KeyRunes && containsBinary(msg.Runes) {
			m.pasteCutStash = string(msg.Runes)
			m.addItem(iSys, dim2("· binary paste detected: reading image from clipboard…"))
			m.refreshViewport()
			return m, tea.Batch(append(cmds, readClipboardCmd())...)
		}

		// Track before handleKey: Enter with the /command picker open falls
		// through here (not consumed by handleCompletionKey) and handleKey closes
		// the picker via m.comp = completionState{} — the reflow guard below
		// catches that transition.
		prevKeyComp := m.comp.active
		var keyCmds []tea.Cmd
		var consumed bool
		m, keyCmds, consumed = m.handleKey(msg)
		cmds = append(cmds, keyCmds...)
		if consumed {
			// Key was handled (confirm gate, send, or a control key); don't let it
			// leak into the textarea — e.g. a sent Enter must not insert a newline.
			var taCmd tea.Cmd
			m.ta, taCmd = m.ta.Update(tea.Msg(nil))
			cmds = append(cmds, taCmd)
			if m.comp.active != prevKeyComp {
				m = m.reflow()
			}
			// Don't forward UP/DOWN to the viewport when consumed for history
			// navigation — otherwise the conversation would scroll in sync.
			k := msg.String()
			if k != "up" && k != "down" {
				m.vp, _ = m.vp.Update(msg)
			}
			return m, tea.Batch(cmds...)
		}

		// Normal typing flows to the textarea; recompute the picker against the
		// new input so it tracks what's being typed after "@" or "/".
		var taCmd, vpCmd tea.Cmd
		m.ta, taCmd = m.ta.Update(msg)

		// Post-insert scan: catch binary pastes however they arrive — one
		// bracketed event, multi-rune bursts, or rapid single-rune events.
		// This runs after EVERY key event that reached the textarea, not
		// just KeyRunes: paste fragments also land as KeySpace and other
		// event types, and gating on event type is exactly what previously
		// left garbage sitting in the input until Enter re-ran the same
		// detector. Content-based confirmation (NUL, PNG chunk train, or a
		// ≥96-rune tail that is symbol-dense OR space-starved) keeps hand-
		// typed prose safe — typed text never satisfies those conditions.
		// On detection, collapse immediately: keep the user's text before
		// the garbage, stash the cut (restored if the clipboard read finds
		// no image), open the suppression window for the paste tail, and
		// read the real image — the chip lands via clipboardImageMsg.
		if idx := binaryPasteStart(m.ta.Value()); idx >= 0 {
			all := []rune(m.ta.Value())
			keep := strings.TrimRight(string(all[:idx]), " ")
			m.pasteCutStash = string(all[idx:]) // restored if clipboard read fails
			m.ta.SetValue(keep)
			m.ta.CursorEnd()
			m.pasteSuppressUntil = time.Now().Add(pasteSuppressWindow)
			m.comp = computeCompletion(m.ta, compSrcFromApp(m.app))
			return m, tea.Batch(append(cmds, taCmd, readClipboardCmd())...)
		}

		prevComp := m.comp.active
		m.comp = computeCompletion(m.ta, compSrcFromApp(m.app))
		// Picker opened or closed: reflow so the viewport height tracks the change.
		// Without this the stale viewport overflows View() and Bubble Tea's cursor
		// tracker drifts (same issue as the subagent tab bar, fixed at line ~402).
		if m.comp.active != prevComp {
			m = m.reflow()
		}
		m.vp, vpCmd = m.vp.Update(msg)
		cmds = append(cmds, taCmd, vpCmd)
		return m, tea.Batch(cmds...)

	case agent.ReasoningChunkMsg:
		m.reasoning.WriteString(msg.Text)
		m.refreshViewport()

	case agent.StreamChunkMsg:
		// First content delta after reasoning: collapse the thinking block to a
		// single committed iSys line and clear the live reasoning buffer.
		if m.reasoning.Len() > 0 && !m.reasoningDone {
			toks := m.reasoning.Len() / 4
			m.reasoning.Reset()
			m.reasoningDone = true
			m.reasoningExpanded = false
			m.addItem(iSys, dim2(sprint("· thought (~%d tokens)", toks)))
		}
		m.streaming.WriteString(msg.Text)
		m.refreshViewport()

	case agent.TokRateMsg:
		m.tps = msg.Tps

	case agent.ConfirmReqMsg:
		// If the user has reverse-search open when a confirm arrives, abort it
		// so the search prompt doesn't persist into the non-idle state.
		if m.searchActive {
			m.searchExit(false)
		}
		m.state = stateConfirm
		m.pendConf = &msg
		m.flushStreaming()
		m.addItem(iSys, fmtConfirmBlock(msg.Headline, msg.Detail, msg.ReadAction))

	case agent.ToolResultMsg:
		m.addItem(iSys, dim2("· "+msg.Name+"\n"+agent.Indent(agent.Truncate(msg.Result, 800))))

	case agent.AgentDoneMsg:
		m.flushStreaming()
		// Edge case: turn ended during/after reasoning but before any content
		// (e.g. tool call only, cancellation). Show the collapsed thought if any.
		if m.reasoning.Len() > 0 {
			toks := m.reasoning.Len() / 4
			m.addItem(iSys, dim2(sprint("· thought (~%d tokens)", toks)))
			m.reasoning.Reset()
		}
		m.reasoningDone = false
		m.reasoningExpanded = false
		if msg.Warn != "" {
			// Tidy agent.Yellow warning (e.g. backend stream error) in place of a trace.
			m.addItem(iSys, agent.Yellow(msg.Warn))
		}
		if msg.Err != nil && !errors.Is(msg.Err, context.Canceled) {
			if errors.Is(msg.Err, proxy.ErrBackendStream) {
				m.addItem(iSys, agent.Yellow("⚠ backend stream error"))
			} else {
				m.addItem(iSys, styleErr("error: "+msg.Err.Error()))
			}
		}
		if errors.Is(msg.Err, context.Canceled) {
			m.addItem(iSys, dim2("[turn cancelled]"))
		}
		if msg.LearnNudge != "" {
			m.addItem(iSys, dim2(msg.LearnNudge))
		}
		// Chime on a successful finish, but only for turns long enough to have
		// drawn the user's attention away (skip quick replies and errors/cancels).
		if msg.Err == nil && !m.turnStart.IsZero() && time.Since(m.turnStart) > 3*time.Second {
			cmds = append(cmds, playFinishSound())
		}
		m.turnStart = time.Time{}
		m.tps = 0
		m.state = stateIdle
		m.dotPhase = 0 // return dot to static dim; tick self-terminates (no re-arm at idle)
		m.hadTurn = true
		m.cancel = nil
		m.cancelling = false

	case agent.CompactedMsg:
		m.addItem(iSys, dim2("· compacted earlier turns"))

	case agent.SubagentStartMsg:
		// Which tab is the user currently viewing? 0 = main. Tracked by n so it
		// survives the prune/index shuffle below.
		focusN := 0
		if m.subCur >= 0 && m.subCur < len(m.subTabs) {
			focusN = m.subTabs[m.subCur].n
		}
		m.subSeq++
		tab := &subTab{
			n:          m.subSeq,
			task:       msg.Task,
			chatID:     msg.ChatID,
			backend:    msg.Backend,
			capability: msg.Capability,
			model:      msg.Model,
			toolNames:  msg.ToolNames,
			buf:        new(strings.Builder),
		}
		m.subTabs = append(m.subTabs, tab)
		// Never steal focus: the user stays on whatever view they are reading
		// (usually main). The new tab is reachable via the tab bar; with
		// parallel dispatch, auto-following would bounce the view N times.
		m.subTabs = pruneSubTabs(m.subTabs, focusN, maxSubTabs)
		m.subCur = tabIndexByN(m.subTabs, focusN)
		// When the first tab appears the tab bar steals one row from vpH.
		// Reflow so m.vp.Height is updated; otherwise the stale (too-tall)
		// viewport overflows View() by one row and Bubble Tea's cursor tracker
		// drifts, causing the bottom tab bar to render at the wrong screen row.
		if len(m.subTabs) == 1 {
			m = m.reflow()
		}

	case agent.SubagentActiveMsg:
		// Queued → running: the worker acquired a parallelism slot.
		for _, t := range m.subTabs {
			if t.chatID == msg.ChatID {
				t.active = true
				break
			}
		}

	case agent.SubagentChunkMsg:
		// Route by ChatID: with parallel dispatch, several tabs may stream at
		// once, so the chunk carries the identity of its producer.
		for _, t := range m.subTabs {
			if t.chatID == msg.ChatID {
				t.buf.WriteString(msg.Text)
				break
			}
		}

	case agent.SubagentFinishedMsg:
		// Display-only early completion: the worker just returned. Flip the tab
		// to a visually-done state immediately — spinner stops, status glyph,
		// cost, files count, summary preview — without waiting for the Phase C
		// barrier. SubagentDoneMsg below is the authoritative finalization; it
		// fills any remaining fields and must not regress a finished tab.
		for _, t := range m.subTabs {
			if t.chatID == msg.ChatID {
				t.finished = true
				t.finishedAt = msg.FinishedAt
				t.finStatus = msg.Status
				t.finCostUSD = msg.CostUSD
				t.finFilesN = len(msg.FilesChanged)
				t.finPreview = msg.SummaryPreview
				break
			}
		}

	case agent.SubagentDoneMsg:
		// Authoritative finalization (Phase C). Fill fields the early event
		// didn't carry (grounding, ctx size, hardMax, usedBackend) and mark the
		// tab fully done. No visual regression: if the tab was already finished
		// via SubagentFinishedMsg, it stays done — we only enrich it.
		found := false
		for _, t := range m.subTabs {
			if t.chatID == msg.ChatID {
				found = true
				t.done = true
				t.finished = true // done implies finished for rendering
				t.grounding = msg.Grounding
				t.ctxSize = msg.CtxSize
				t.hardMaxBytes = msg.HardMaxBytes
				t.usedBackend = msg.UsedBackend
				t.costUSD = msg.CostUSD
				t.filesChanged = msg.FilesChanged
				break
			}
		}
		// Arm a 30s auto-close timer for the done tab. Fire-and-validate: if
		// the tab was already pruned, closed, or is focused when the timer
		// fires, the handler is a no-op. One-shot — not re-armed.
		if found {
			chatID := msg.ChatID
			cmds = append(cmds, tea.Tick(subTabAutoCloseDelay, func(time.Time) tea.Msg {
				return subTabCloseMsg{ChatID: chatID}
			}))
		}

	case subTabCloseMsg:
		// Auto-close: remove the tab if it is done and not currently focused.
		// If focused, skip (one-shot, no re-arm — the tab will be cleaned up
		// by pruneSubTabs when new tabs arrive, or by manual close).
		focusN := 0
		if m.subCur >= 0 && m.subCur < len(m.subTabs) {
			focusN = m.subTabs[m.subCur].n
		}
		oldLen := len(m.subTabs)
		removed := false
		for i, t := range m.subTabs {
			if t.chatID == msg.ChatID && t.done && t.n != focusN {
				m.subTabs = append(m.subTabs[:i], m.subTabs[i+1:]...)
				removed = true
				break
			}
		}
		if removed {
			m.subCur = tabIndexByN(m.subTabs, focusN)
			// Symmetric with SubagentStartMsg's 0→1 reflow: when the last tab
			// disappears the tab bar row is reclaimed by the viewport.
			if oldLen > 0 && len(m.subTabs) == 0 {
				m = m.reflow()
			}
		}

	case agent.LearnTurnMsg:
		if m.state == stateIdle {
			if m.searchActive {
				m.searchExit(false)
			}
			const learnText = "learn this for next time"
			m.addItem(iUser, learnText)
			m.vp.GotoBottom()
			ctx, cancel := context.WithCancel(context.Background())
			m.cancel = cancel
			m.state = stateStreaming
			m.turnStart = time.Now()
			m.tps = 0
			cmds = append(cmds, AdaptCmd(agent.RunTurn(m.app, ctx, learnText)), startDotTick())
		}

	case dotTickMsg:
		// Re-arm only while busy; the tick self-terminates when the model is idle.
		if m.state != stateIdle {
			m.dotPhase = (m.dotPhase + 1) % len(dotPulseShades)
			cmds = append(cmds, startDotTick())
		}

	case agent.SysNoteMsg:
		m.addItem(iSys, dim2(msg.Text))

	case agent.BackendCtxLimitMsg:
		// Apply the newly-resolved context limit in the event loop — safe because
		// /backend is only available in stateIdle (no concurrent RunTurn goroutine).
		// Reset the pressure warning so it re-evaluates against the new window.
		m.app.CtxLimit = msg.Limit
		m.app.CtxPressureWarned = false
		if msg.Note != "" {
			m.addItem(iSys, dim2(msg.Note))
		}

	case agent.ModelListUpdatedMsg:
		// Refresh the model list after an endpoint switch so /model and
		// /submodel autocomplete reflects the new endpoint's models. Applied
		// in the event loop — same safety as BackendCtxLimitMsg above.
		m.app.ModelList = msg.Models

	case copiedMsg:
		m.flash = sprint("copied %d chars ✓", msg.n)

	case clipboardImageMsg:
		// A clipboard read completed (triggered by paste-detection or
		// /image clipboard). On success, queue the image for the next
		// message AND insert its placeholder chip into the text input at
		// the cursor — the compact "[image: clipboard:png · 1.8 MB]" stands
		// in for the pasted image, keeping the input clean. At send time
		// the chip is stripped from the outgoing text (the image travels
		// via PendingImages); deleting the chip un-attaches the image.
		//
		// On failure with a cut stash pending, the detection was likely a
		// false positive (the "garbage" was real text with no image on the
		// clipboard) — restore the cut text so nothing is lost.
		if msg.Err != "" {
			if m.pasteCutStash != "" {
				m.ta.InsertString(m.pasteCutStash)
				m.pasteCutStash = ""
				m.addItem(iSys, dim2("· no image on clipboard — restored the pasted text"))
			} else {
				m.addItem(iSys, styleErr("clipboard: "+msg.Err))
			}
		} else {
			m.pasteCutStash = "" // real image confirmed; the cut garbage stays gone
			m.app.PendingImages = append(m.app.PendingImages, msg.Img)
			chip := msg.Img.Placeholder()
			*m.imageChips = append(*m.imageChips, chip)
			m.ta.InsertString(chip + " ")
		}
		m.refreshViewport()

	case agent.NewConvMsg:
		// Clear viewport items so the new conversation starts fresh.
		*m.items = (*m.items)[:0]
		m.streaming.Reset()
		m.reasoning.Reset()
		m.reasoningDone = false
		m.reasoningExpanded = false
		// Chip tracking belongs to the old conversation's draft; the pending
		// images themselves are owned by the App and survive /new on purpose
		// (same as /image <path> queuing before a fresh chat).
		*m.imageChips = (*m.imageChips)[:0]
		if msg.RebuildConv && len(m.app.Conv) > 0 {
			*m.items = convItemsFrom(m.app.Conv)
		}
		m.prefixDirty = true
		m.refreshViewport()
		if msg.Note != "" {
			m.addItem(iSys, dim2(msg.Note))
		}

	case agent.OpenResumePickerMsg:
		prevH := m.resumePickerHeight()
		m = m.openResumePicker(msg)
		if m.resumePickerHeight() != prevH {
			m = m.reflow()
		}

	case agent.MCPReconnectedMsg:
		// Apply the rebuilt tool list from the Update loop — not from the Cmd
		// goroutine — so there is no race with the agent goroutine reading app.Tools.
		m.app.Tools = msg.Tools
		m.addItem(iSys, dim2(sprint("· reconnected %q (%d tools)", msg.Name, len(msg.Tools))))

	case agent.WFFinalReviewMsg:
		// Closing oracle check, triggered by /plan approve after an every-step
		// critical pause on the last step.
		if m.state != stateIdle || m.app.Workflow == nil {
			break
		}
		if m.searchActive {
			m.searchExit(false)
		}
		m.addItem(iSys, dim2("· running final oracle review"))
		m.vp.GotoBottom()
		{
			ctx, cancel := context.WithCancel(context.Background())
			m.cancel = cancel
			m.state = stateStreaming
			m.turnStart = time.Now()
			m.tps = 0
			cmds = append(cmds, AdaptCmd(agent.RunFinalReview(m.app, ctx)), startDotTick())
		}

	case agent.WFStartTurnMsg:
		// Workflow auto-turn: show a system note and kick off the next agent turn.
		// This message arrives after agent.AgentDoneMsg (same goroutine, same channel),
		// so state is already idle. Guard against the abort-mid-chain race: if the
		// user typed /plan abort between agent.AgentDoneMsg being consumed and this message
		// arriving, Workflow is nil and we must not start a stray turn.
		if m.state != stateIdle || m.app.Workflow == nil {
			break
		}
		if m.searchActive {
			m.searchExit(false)
		}
		m.addItem(iSys, dim2("· "+msg.Note))
		m.vp.GotoBottom()
		ctx, cancel := context.WithCancel(context.Background())
		m.cancel = cancel
		m.state = stateStreaming
		m.turnStart = time.Now()
		m.tps = 0
		cmds = append(cmds, AdaptCmd(agent.RunTurn(m.app, ctx, msg.UserText)), startDotTick())
	}

	var taCmd, vpCmd tea.Cmd
	m.ta, taCmd = m.ta.Update(msg)
	m.vp, vpCmd = m.vp.Update(msg)
	cmds = append(cmds, taCmd, vpCmd)
	return m, tea.Batch(cmds...)
}

// handleKey processes keys wakil acts on itself. The bool return reports
// whether the key was consumed; consumed keys must not also be forwarded to
// the textarea (otherwise a sent Enter would insert a newline after Reset).
func (m tuiModel) handleKey(msg tea.KeyMsg) (tuiModel, []tea.Cmd, bool) {
	if m.state == stateConfirm && m.pendConf != nil {
		readAction := m.pendConf.ReadAction
		// answer resolves the gate: post a note, hand the choice to the agent
		// goroutine (buffered channel, never blocks), and resume streaming.
		answer := func(c agent.ConfirmChoice, note string) {
			ch := m.pendConf.RespCh
			m.pendConf = nil
			m.state = stateStreaming
			m.addItem(iSys, note)
			ch <- c
		}
		switch msg.String() {
		case "y", "Y":
			answer(agent.ChoiceApprove, styleOK("  [approved]"))
		case "a", "A":
			if readAction {
				answer(agent.ChoiceAllowReads, styleOK("  [reads allowed for this session]"))
			}
			// 'a' is meaningless for non-read actions — swallow it (consumed below).
		case "n", "N", "esc":
			answer(agent.ChoiceDecline, dim2("  [declined]"))
		case "ctrl+c":
			answer(agent.ChoiceDecline, dim2("  [declined + cancelled]"))
			m.cancelTurn()
		}
		// Every key is consumed by the confirm gate.
		return m, nil, true
	}

	// While reverse-search is active, intercept abort keys before the main
	// switch so they don't trigger cancel-turn or quit.
	if m.searchActive {
		switch msg.String() {
		case "ctrl+r":
			// Repeat: find the next older match past the current one.
			if m.searchIdx >= 0 {
				m.searchRun(m.searchIdx + 1)
			} else {
				m.searchRun(0)
			}
			return m, nil, true
		case "ctrl+g", "esc", "ctrl+c":
			// Abort: restore the original draft, exit search mode.
			// (Ctrl+C does NOT quit — it only aborts the search.)
			m.searchExit(false)
			return m, nil, true
		}
		// All other keys fall through to the main switch / intercept below.
	}

	switch msg.String() {
	case "ctrl+c":
		if m.state == stateIdle {
			return m, []tea.Cmd{tea.Quit}, true
		}
		// First Ctrl+C: request cancellation. Second Ctrl+C (cancel already
		// sent but goroutine hasn't acknowledged yet): force-quit immediately.
		if m.cancelling {
			return m, []tea.Cmd{tea.Quit}, true
		}
		m.cancelling = true
		m.cancelTurn()
		return m, nil, true

	case "esc":
		// Stop the in-flight turn. When idle there's nothing to stop, so let it
		// fall through to the textarea. (In the confirm gate above, esc declines.)
		if m.state != stateIdle {
			m.cancelTurn()
			return m, nil, true
		}

	case "ctrl+d":
		if m.state == stateIdle {
			return m, []tea.Cmd{tea.Quit}, true
		}

	case "ctrl+r":
		// Enter reverse-incremental search. Only when idle — never during
		// streaming or confirm (the gate above already consumes in confirm).
		if m.state != stateIdle {
			return m, nil, true
		}
		// If somehow already active (shouldn't reach here — handled above),
		// treat as repeat.
		if m.searchActive {
			m.searchRun(m.searchIdx + 1)
			return m, nil, true
		}
		if len(m.inputHistory) == 0 {
			// No history: enter search anyway so the prompt shows, but there
			// will never be a match.
			m.searchActive = true
			m.searchQuery = ""
			m.searchIdx = -1
			m.searchSaved = m.ta.Value()
			m.searchFailed = true
			m.comp = completionState{} // close picker
			m.ta.Reset()
			return m, nil, true
		}
		m.searchActive = true
		m.searchQuery = ""
		m.searchIdx = -1
		m.searchSaved = m.ta.Value()
		m.searchFailed = false
		m.comp = completionState{} // close picker
		m.ta.Reset()
		// Show the most recent entry immediately (empty query matches all).
		m.searchRun(0)
		return m, nil, true

	case "ctrl+e":
		// Toggle expand/collapse of live reasoning text. Only meaningful while
		// reasoning is actively streaming (before reasoningDone collapses it).
		if m.reasoning != nil && m.reasoning.Len() > 0 && !m.reasoningDone {
			m.reasoningExpanded = !m.reasoningExpanded
			m.refreshViewport()
		}
		return m, nil, true

	case "up":
		if m.searchActive {
			// Exit search keeping the match, then navigate history normally.
			// Seed histSaved with the pre-search draft so DOWN-to-bottom
			// restores the user's original text, not a stale value.
			m.histSaved = m.searchSaved
			m.histIdx = m.searchIdx // reconcile: continue from the matched entry
			m.searchExit(true)
			// Fall through to the normal UP handler below.
		}
		if m.state == stateIdle && len(m.inputHistory) > 0 {
			if m.histIdx == -1 {
				m.histSaved = m.ta.Value()
			}
			if m.histIdx < len(m.inputHistory)-1 {
				m.histIdx++
				m.ta.SetValue(m.inputHistory[m.histIdx])
			}
			return m, nil, true
		}

	case "down":
		if m.searchActive {
			m.histSaved = m.searchSaved
			m.histIdx = m.searchIdx
			m.searchExit(true)
		}
		if m.state == stateIdle && m.histIdx >= 0 {
			if m.histIdx > 0 {
				m.histIdx--
				m.ta.SetValue(m.inputHistory[m.histIdx])
			} else {
				m.histIdx = -1
				m.ta.SetValue(m.histSaved)
			}
			return m, nil, true
		}

	case "enter":
		// Enter is ours (send); never let it reach the textarea as a newline.
		// Shift+Enter, handled by the textarea below, inserts newlines instead.
		if m.state != stateIdle {
			return m, nil, true
		}
		// If reverse-search is active, exit it keeping the matched entry, then
		// fall through to the normal send logic (which reads ta.Value()).
		if m.searchActive {
			m.searchExit(true)
		}
		input := strings.TrimSpace(m.ta.Value())
		if input == "" {
			return m, nil, true
		}
		// Send-time safety net: if mangled binary-image content still made it
		// into the textarea (a paste path the live interception didn't see),
		// refuse to send it to the model and read the clipboard instead. The
		// scan is position-independent, so "look at this: <garbage>" is
		// caught too — the typed prefix is kept, only the garbage goes.
		if idx := binaryPasteStart(input); idx >= 0 {
			all := []rune(input)
			keep := strings.TrimRight(string(all[:idx]), " ")
			m.pasteCutStash = string(all[idx:]) // restored if clipboard read fails
			m.ta.SetValue(keep)
			m.ta.CursorEnd()
			m.comp = completionState{}
			m.addItem(iSys, dim2("· input contained a pasted image, not text — reading image from clipboard…"))
			return m, []tea.Cmd{readClipboardCmd()}, true
		}
		// Add to history (skip duplicate of most-recent entry).
		if len(m.inputHistory) == 0 || m.inputHistory[0] != input {
			m.inputHistory = append([]string{input}, m.inputHistory...)
			appendHistory(input) // persist to shared file
		}
		m.histIdx = -1
		m.histSaved = ""
		m.ta.Reset()
		m.comp = completionState{} // input cleared; close the picker

		if handled, quit, cmd := agent.HandleTUICommand(input, m.app); handled {
			if quit {
				return m, []tea.Cmd{tea.Quit}, true
			}
			// A slash command consumed the input, but image chips (and their
			// pending images) belong to the next real message — re-insert the
			// chips into the now-empty textarea so they survive the command.
			// Without this, running e.g. "/image" to check the queue would
			// wipe the chips and the next send would detach the images.
			for _, chip := range *m.imageChips {
				if strings.Contains(input, chip) {
					m.ta.InsertString(chip + " ")
				}
			}
			if cmd != nil {
				return m, []tea.Cmd{AdaptCmd(cmd)}, true
			}
			return m, nil, true
		}

		// Reconcile image chips: strip surviving chips from the outgoing text
		// (the image travels via PendingImages, not as text); detach the
		// pending image for any chip the user deleted from the input.
		var msgText string
		msgText, m.app.PendingImages = reconcileImageChips(input, *m.imageChips, m.app.PendingImages)
		*m.imageChips = (*m.imageChips)[:0]
		// A chip-only input yields empty text but a queued image — that is a
		// legitimate image-only message. Empty text AND no images = nothing.
		if msgText == "" && len(m.app.PendingImages) == 0 {
			return m, nil, true
		}

		// Resolve "@" mentions: the user sees their typed text plus chips; the
		// proxy receives the text with file/folder content injected.
		outgoing, refs := tools.ResolveMentions(msgText, m.app.Cfg.MentionBase)
		m.addItem(iUser, input)
		if len(refs) > 0 {
			m.addItem(iSys, tools.ChipsLine(refs))
		}
		// Show image placeholders when images are attached to this turn.
		if len(m.app.PendingImages) > 0 {
			for _, img := range m.app.PendingImages {
				m.addItem(iSys, img.Placeholder())
			}
		}
		m.vp.GotoBottom() // re-pin: a sent turn always scrolls into view

		ctx, cancel := context.WithCancel(context.Background())
		m.cancel = cancel
		m.state = stateStreaming
		m.turnStart = time.Now()
		m.tps = 0
		return m, []tea.Cmd{AdaptCmd(agent.RunTurn(m.app, ctx, outgoing)), startDotTick()}, true
	}

	// --- Reverse-search content intercept ---
	// When search is active, printable/space/backspace keys build the query.
	// All other keys exit search (keeping the match) and fall through to the
	// textarea (return consumed=false).
	if m.searchActive {
		switch msg.Type {
		case tea.KeyRunes:
			if msg.Alt {
				// Alt+rune: exit search, let the key go to the textarea.
				m.searchExit(true)
				return m, nil, false
			}
			m.searchQuery += string(msg.Runes)
			m.searchRun(0)
			return m, nil, true
		case tea.KeySpace:
			m.searchQuery += " "
			m.searchRun(0)
			return m, nil, true
		case tea.KeyBackspace, tea.KeyCtrlH:
			if len(m.searchQuery) > 0 {
				// Remove last rune (handles multi-byte correctly).
				r := []rune(m.searchQuery)
				m.searchQuery = string(r[:len(r)-1])
			}
			m.searchRun(0)
			return m, nil, true
		default:
			// Any other key (arrows, Ctrl+A, etc.): exit search keeping the
			// match, then let the key flow to the textarea naturally.
			m.searchExit(true)
			return m, nil, false
		}
	}

	return m, nil, false
}

// searchExit leaves reverse-search mode. When keepMatch is true the current
// textarea content (the matched entry) is kept; when false the original draft
// saved on entry (searchSaved) is restored. All search fields are reset.
func (m *tuiModel) searchExit(keepMatch bool) {
	if !keepMatch {
		m.ta.SetValue(m.searchSaved)
	}
	m.searchActive = false
	m.searchQuery = ""
	m.searchIdx = -1
	m.searchSaved = ""
	m.searchFailed = false
}

// searchRun scans inputHistory from startIdx for searchQuery and updates the
// textarea + search state. On match: shows the entry, clears searchFailed.
// On no match: sets searchFailed, keeps the previous match and index.
func (m *tuiModel) searchRun(startIdx int) {
	idx := searchHistory(m.inputHistory, m.searchQuery, startIdx)
	if idx >= 0 {
		m.searchIdx = idx
		m.searchFailed = false
		m.ta.SetValue(m.inputHistory[idx])
	} else {
		m.searchFailed = true
	}
}

func (m *tuiModel) cancelTurn() {
	if m.cancel != nil {
		m.cancel()
		// Do NOT nil m.cancel here — keep it so agent.AgentDoneMsg can clean up
		// and so we can detect a cancel is in-flight (m.cancelling).
	}
}

func (m *tuiModel) addItem(k itemKind, text string) {
	*m.items = append(*m.items, convItem{kind: k, text: text})
	m.prefixDirty = true
	m.refreshViewport()
}

func (m *tuiModel) flushStreaming() {
	if m.streaming.Len() == 0 {
		return
	}
	text := m.streaming.String()
	m.streaming.Reset()
	m.addItem(iAsst, text)
}

// refreshViewport re-renders the viewport. It separates committed items from the
// live streaming tail so that per-chunk updates only re-render the tail —
// O(chunk) rather than O(full transcript). The committed prefix is rebuilt only
// when items change (addItem, reflow, agent.NewConvMsg) or the viewport width changes.
func (m *tuiModel) refreshViewport() {
	w := m.vp.Width
	if w <= 0 {
		return
	}

	// Only auto-follow new content if the reader is already pinned to the bottom.
	stick := m.vp.AtBottom()

	// --- committed prefix ---
	if m.prefixDirty || m.prefixW != w {
		var sb strings.Builder
		for i := range *m.items {
			item := &(*m.items)[i]
			if i > 0 && item.kind == iUser {
				sb.WriteString(dim2(strings.Repeat("─", w)) + "\n")
			}
			if item.cache == "" || item.cacheW != w {
				item.cache = renderItem(*item, w)
				item.cacheW = w
			}
			sb.WriteString(item.cache)
			sb.WriteByte('\n')
		}
		m.prefixStyled = sb.String()
		m.prefixPlain = strings.Split(ansi.Strip(m.prefixStyled), "\n")
		m.prefixDirty = false
		m.prefixW = w
	}

	// --- streaming tail ---
	// During extended thinking: render live reasoning (dim/italic) above any
	// in-flight content. Once reasoningDone is set the reasoning has already been
	// committed as an iSys item; only content remains in m.streaming.
	var tailStyled string
	var tailPlain []string
	liveReasoning := m.reasoning != nil && m.reasoning.Len() > 0 && !m.reasoningDone
	if liveReasoning || m.streaming.Len() > 0 {
		var sb strings.Builder
		if liveReasoning {
			sb.WriteString(renderReasoning(m.reasoning.String(), w, m.reasoningExpanded))
		}
		if m.streaming.Len() > 0 {
			sb.WriteString(renderStreaming(m.streaming.String(), w))
		}
		tailStyled = sb.String()
		tailPlain = strings.Split(ansi.Strip(tailStyled), "\n")
	}

	// Merge plain lines for selection hit-testing. Avoid a trailing empty line
	// from the prefix when there is also a tail (the prefix always ends in "\n").
	if len(tailPlain) > 0 {
		prefix := m.prefixPlain
		if len(prefix) > 0 && prefix[len(prefix)-1] == "" {
			prefix = prefix[:len(prefix)-1]
		}
		m.plainLines = append(prefix, tailPlain...)
	} else {
		m.plainLines = m.prefixPlain
	}

	styled := m.prefixStyled + tailStyled
	if m.sel.active {
		m.vp.SetContent(m.highlightedContent())
	} else {
		m.vp.SetContent(styled)
	}
	if stick {
		m.vp.GotoBottom()
	}
}

// renderItem renders one committed item, word-wrapped to width.
func renderItem(item convItem, w int) string {
	switch item.kind {
	case iUser:
		// Bold amber marker on the first line; continuation lines indented.
		lines := strings.Split(wrapAnsi(item.text, w-2), "\n")
		marker := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("33")).Render("▶")
		first := " " + marker + " " + lipgloss.NewStyle().Foreground(lipgloss.Color("33")).Render(lines[0])
		if len(lines) == 1 {
			return first
		}
		rest := make([]string, len(lines)-1)
		for i, l := range lines[1:] {
			rest[i] = "   " + lipgloss.NewStyle().Foreground(lipgloss.Color("33")).Render(l)
		}
		return first + "\n" + strings.Join(rest, "\n")

	case iAsst:
		// Assistant responses are markdown — format them (headings, bold, lists,
		// code blocks). glamour handles wrapping to w itself.
		return renderMarkdown(item.text, w)

	default: // iSys
		return wrapAnsi(item.text, w)
	}
}

// renderStreaming renders live in-flight SSE content.
func renderStreaming(text string, w int) string {
	wrapped := wrapAnsi(text, w)
	return lipgloss.NewStyle().Foreground(lipgloss.Color("252")).Render(wrapped)
}

// maxReasoningCollapsedLines bounds how many lines of live reasoning are shown
// when collapsed (the default). The last N lines are shown plus an indicator.
const maxReasoningCollapsedLines = 5

// renderReasoning renders live extended-thinking text: dim + italic so it is
// visually distinct from the final answer and clearly marked as transient.
// When collapsed and the text exceeds maxReasoningCollapsedLines, only the
// last N lines are shown with a dim "expand" indicator.
func renderReasoning(text string, w int, expanded bool) string {
	wrapped := wrapAnsi(text, w)
	lines := strings.Split(wrapped, "\n")

	if !expanded && len(lines) > maxReasoningCollapsedLines {
		hidden := len(lines) - maxReasoningCollapsedLines
		indicator := fmt.Sprintf("⋯ +%d lines (ctrl+e to expand)", hidden)
		shown := append([]string{indicator}, lines[len(lines)-maxReasoningCollapsedLines:]...)
		wrapped = strings.Join(shown, "\n")
	}

	return lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Italic(true).Render(wrapped)
}

func (m tuiModel) sizes() (vpW, vpH, inputOuterH int) {
	// Input box = border (2) + textarea, plus one row for the status line only
	// when there is one to show (must mirror View()).
	inputOuterH = m.ta.Height() + 2
	if m.statusLine() != "" {
		inputOuterH++
	}
	tabH := 0
	if len(m.subTabs) > 0 {
		tabH = 1
	}
	topOuterH := m.height - inputOuterH - m.completionHeight() - m.resumePickerHeight() - tabH
	if topOuterH < 6 {
		topOuterH = 6
	}
	vpH = topOuterH - 2
	if vpH < 4 {
		vpH = 4
	}
	vpW = m.width - sidebarWidth - 2
	if vpW < 20 {
		vpW = m.width - 2
	}
	return
}

func (m tuiModel) reflow() tuiModel {
	vpW, vpH, _ := m.sizes()
	m.vp.Width = vpW
	m.vp.Height = vpH
	// Narrow the textarea to leave room for the hist/ctx block (same helper as
	// View, so the widths always agree); full width when the block is hidden.
	_, taW, _ := m.inputContextBlock()
	m.ta.SetWidth(taW)
	m.prefixDirty = true // width changed — committed items must be re-wrapped
	m.refreshViewport()
	return m
}

// startDotTick arms a single 200 ms tick for the pulsing activity dot.
// The tick self-terminates: the dotTickMsg handler only re-arms when busy.
func startDotTick() tea.Cmd {
	return tea.Tick(200*time.Millisecond, func(time.Time) tea.Msg { return dotTickMsg{} })
}

// wrapAnsi word-wraps s to at most width visible columns per line,
// preserving existing newlines and ANSI escape sequences.
func wrapAnsi(s string, width int) string {
	if width <= 0 {
		return s
	}
	var out []string
	for _, line := range strings.Split(s, "\n") {
		out = append(out, wrapAnsiLine(line, width))
	}
	return strings.Join(out, "\n")
}

func wrapAnsiLine(s string, width int) string {
	if lipgloss.Width(s) <= width {
		return s
	}
	// Extract leading ANSI codes so they can be re-applied on continuation lines,
	// fixing the bug where dim/color styling is lost after a mid-line wrap.
	prefix := leadingAnsiCodes(s)
	var lines []string
	for lipgloss.Width(s) > width {
		cut := wrapPoint(s, width)
		seg := s[:cut]
		if prefix != "" {
			seg += "\x1b[0m" // self-contained: close any open codes at segment end
		}
		lines = append(lines, seg)
		continuation := strings.TrimLeft(s[cut:], " ")
		if prefix != "" && continuation != "" {
			continuation = prefix + continuation // re-open codes on continuation
		}
		s = continuation
	}
	if s != "" {
		lines = append(lines, s)
	}
	return strings.Join(lines, "\n")
}

// leadingAnsiCodes returns the leading ANSI SGR escape sequences of s (e.g.
// "\x1b[2m" for dim). A reset sequence (\x1b[0m) terminates the prefix with
// no codes to carry. Returns "" if s starts with non-ANSI text.
func leadingAnsiCodes(s string) string {
	i := 0
	for i+2 < len(s) {
		if s[i] != '\x1b' || s[i+1] != '[' {
			break
		}
		j := strings.IndexByte(s[i+2:], 'm')
		if j < 0 {
			break
		}
		code := s[i+2 : i+2+j]
		i += 3 + j
		if code == "0" || code == "" {
			return "" // reset: nothing to propagate
		}
	}
	return s[:i]
}

// wrapPoint returns the byte index at which to break s at visual column width,
// preferring a space boundary. Skips ANSI escape sequences when counting.
// Uses lipgloss.Width for display-width correctness on CJK/emoji.
func wrapPoint(s string, width int) int {
	visual := 0
	inEsc := false
	lastSpace := -1
	for i, r := range s {
		if r == '\x1b' {
			inEsc = true
			continue
		}
		if inEsc {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
			}
			continue
		}
		if r == ' ' {
			lastSpace = i
		}
		visual += lipgloss.Width(string(r))
		if visual >= width {
			if lastSpace > 0 {
				return lastSpace + 1
			}
			return i + len(string(r))
		}
	}
	return len(s)
}
