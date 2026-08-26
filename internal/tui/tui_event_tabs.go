package tui

// tui_event_tabs.go: subagent/async-job tab mutations driven by domain events
// (m4b stage 3). Extracted from handleEventMsg so the tab lifecycle is one
// place. The tab's identity keys are the DOMAIN IDs (sub_<uuid>, op_<id>) —
// on the wiring path the projection mints them from the agent's proxy IDs,
// and the TUI never sees the raw chat IDs.

import (
	"strings"
	"time"

	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/proxy"

	tea "github.com/charmbracelet/bubbletea"
)

// spawnSubTab creates the tab for a SubagentSpawned event (the old
// SubagentStartMsg case): never steals focus, prunes at maxSubTabs, reflows
// when the tab bar first appears.
func (m tuiModel) spawnSubTab(p event.SubagentSpawned, cmds []tea.Cmd) (tuiModel, []tea.Cmd) {
	focusN := 0
	if m.subCur >= 0 && m.subCur < len(m.subTabs) {
		focusN = m.subTabs[m.subCur].n
	}
	m.subSeq++
	tab := &subTab{
		n:          m.subSeq,
		task:       p.Task,
		chatID:     string(p.SubagentID), // domain ID is the tab identity
		backend:    p.Backend,
		capability: p.Capability,
		model:      p.Model,
		toolNames:  p.ToolNames,
		buf:        new(strings.Builder),
	}
	m.subTabs = append(m.subTabs, tab)
	m.subTabs = pruneSubTabs(m.subTabs, focusN, maxSubTabs)
	m.subCur = tabIndexByN(m.subTabs, focusN)
	if len(m.subTabs) == 1 {
		m = m.reflow()
	}
	return m, cmds
}

// completeSubTab applies the authoritative SubagentCompleted (the old
// SubagentDoneMsg case): enrichment without visual regression, done-flag
// transition arms the auto-close timer (card #133), error text is kept if
// already set (defense-in-depth).
func (m tuiModel) completeSubTab(p event.SubagentCompleted, cmds []tea.Cmd) (tuiModel, []tea.Cmd) {
	becameDone := false
	for _, t := range m.subTabs {
		if t.chatID != string(p.SubagentID) {
			continue
		}
		if !t.done {
			becameDone = true
			t.active = false
		}
		t.done = true
		t.finished = true
		if p.Err != "" || t.finErr == "" {
			t.finErr = p.Err
		}
		// Grounding arrives as display labels (Type is lost in projection —
		// the raw list renders as-is).
		if len(p.Grounding) > 0 {
			t.grounding = nil
			for _, g := range p.Grounding {
				t.grounding = append(t.grounding, proxy.GroundingEntry{Label: g})
			}
		}
		t.ctxSize = p.CtxSize
		t.hardMaxBytes = p.HardMaxBytes
		t.usedBackend = p.UsedBackend
		t.costUSD = p.CostUSD
		t.filesChanged = p.FilesChanged
		break
	}
	if becameDone {
		chatID := string(p.SubagentID)
		cmds = append(cmds, tea.Tick(subTabAutoCloseDelay, func(time.Time) tea.Msg {
			return subTabCloseMsg{ChatID: chatID}
		}))
	}
	return m, cmds
}

// startJobTab creates/upserts an async-job tab for AsyncJobStarted (the old
// AsyncJobStartMsg case minus the origin guard — the session guard in
// handleEventMsg replaces it: a stale-session Start never reaches here).
func (m tuiModel) startJobTab(p event.AsyncJobStarted, cmds []tea.Cmd) (tuiModel, []tea.Cmd) {
	focusN := 0
	if m.subCur >= 0 && m.subCur < len(m.subTabs) {
		focusN = m.subTabs[m.subCur].n
	}
	for _, t := range m.subTabs {
		if t.kind == subTabAsyncJob && t.opID == string(p.OpID) {
			return m, cmds // idempotent by opID
		}
	}
	m.subSeq++
	tab := &subTab{
		kind:   subTabAsyncJob,
		n:      m.subSeq,
		task:   p.Label,
		opID:   string(p.OpID),
		active: true,
		buf:    new(strings.Builder),
	}
	m.subTabs = append(m.subTabs, tab)
	m.subTabs = pruneSubTabs(m.subTabs, focusN, maxSubTabs)
	m.subCur = tabIndexByN(m.subTabs, focusN)
	if len(m.subTabs) == 1 {
		m = m.reflow()
	}
	if m.hasActiveJobTab() && m.state == stateIdle {
		var dotCmd tea.Cmd
		m, dotCmd = m.startDotTickIfUnarmed()
		if dotCmd != nil {
			cmds = append(cmds, dotCmd)
		}
	}
	return m, cmds
}

// completeJobTab terminalizes an async-job tab for AsyncJobCompleted (the
// old AsyncJobDoneMsg case, including the done-before-Start fallback).
func (m tuiModel) completeJobTab(p event.AsyncJobCompleted, cmds []tea.Cmd) (tuiModel, []tea.Cmd) {
	found := false
	becameDone := false
	for _, t := range m.subTabs {
		if t.kind != subTabAsyncJob || t.opID != string(p.OpID) {
			continue
		}
		if !t.done {
			t.done = true
			t.finished = true
			t.active = false
			becameDone = true
			if p.Err != "" {
				t.finErr = p.Err
			}
			if p.SummaryPreview != "" {
				if t.buf.Len() > 0 && t.statusLines > 0 {
					t.buf.WriteString("\n\n")
				}
				t.buf.WriteString(p.SummaryPreview)
			}
		}
		found = true
		break
	}
	if !found {
		// Done-before-Start: surface the result in a fresh terminal tab.
		m.subSeq++
		tab := &subTab{
			kind:     subTabAsyncJob,
			n:        m.subSeq,
			task:     "job",
			opID:     string(p.OpID),
			active:   false,
			done:     true,
			finished: true,
			buf:      new(strings.Builder),
		}
		if p.Err != "" {
			tab.finErr = p.Err
		}
		if p.SummaryPreview != "" {
			tab.buf.WriteString(p.SummaryPreview)
		}
		focusN := 0
		if m.subCur >= 0 && m.subCur < len(m.subTabs) {
			focusN = m.subTabs[m.subCur].n
		}
		m.subTabs = append(m.subTabs, tab)
		becameDone = true
		m.subTabs = pruneSubTabs(m.subTabs, focusN, maxSubTabs)
		m.subCur = tabIndexByN(m.subTabs, focusN)
		if len(m.subTabs) == 1 {
			m = m.reflow()
		}
	}
	if becameDone {
		opID := string(p.OpID)
		cmds = append(cmds, tea.Tick(subTabAutoCloseDelay, func(time.Time) tea.Msg {
			return subTabCloseMsg{OpID: opID}
		}))
	}
	return m, cmds
}
