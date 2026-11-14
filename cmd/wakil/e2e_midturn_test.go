package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/core/event"
)

// midTurnGroup is one whole paired tool batch in a decoded request: an
// assistant message with tool calls plus the tool result for every call.
type midTurnGroup struct {
	callIDs []string
	args    map[string]string
	results map[string]string
	// size is the production measure (messageSize: content + arguments,
	// summed over the assistant message and its tool results).
	size int
}

// midTurnGroups returns the whole paired groups of a decoded request, in
// order. A group counts only when every call has a matching tool result.
func midTurnGroups(msgs []verifyMessage) []midTurnGroup {
	results := make(map[string]string)
	for _, m := range msgs {
		if m.Role == "tool" && m.ToolCallID != "" {
			results[m.ToolCallID] = m.Content
		}
	}
	var groups []midTurnGroup
	for _, m := range msgs {
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		g := midTurnGroup{args: make(map[string]string), results: make(map[string]string)}
		paired := true
		size := len(m.Content)
		for _, tc := range m.ToolCalls {
			g.callIDs = append(g.callIDs, tc.ID)
			g.args[tc.ID] = tc.Arguments
			size += len(tc.Arguments)
			res, ok := results[tc.ID]
			if !ok {
				paired = false
				continue
			}
			g.results[tc.ID] = res
			size += len(res)
		}
		if !paired {
			continue
		}
		g.size = size
		groups = append(groups, g)
	}
	return groups
}

// midTurnSystemContents returns every system message content in order.
func midTurnSystemContents(msgs []verifyMessage) []string {
	var out []string
	for _, m := range msgs {
		if m.Role == "system" {
			out = append(out, m.Content)
		}
	}
	return out
}

// TestE2EMidTurnCollapseKeepsWork checks a mid-turn collapse keeps the task
// and the recent tail. The earlier scripts (5 tiny writes, ceiling 200
// tokens) could never collapse: the retained tail is the largest whole-group
// suffix within 30% of the ceiling, so five tiny groups all fit and nothing
// is elided, and a 200-token ceiling is smaller than the system message
// alone. This harness instead uses ceiling 30000 tokens (120000 chars at
// charsPerToken=4; the system message alone is ~90k chars) and 30 write_file
// calls of ~5000 chars each (unique paths and sentinels), so the projected
// request crosses the ceiling mid-turn and the elided prefix exceeds the
// 36000-char (30%) retained tail. The count must be high: wire JSON schema
// bytes are not conversation size (sentSize counts message content plus
// arguments), so fewer calls never cross the threshold. Pruning is disabled
// so only the mid-turn
// collapse can drop groups. The test then finds the first outgoing request
// with fewer paired batches than its predecessor and checks the collapse
// contract on decoded messages (never a raw filename substring search).
func TestE2EMidTurnCollapseKeepsWork(t *testing.T) {
	const (
		ceilingTokens = 30000
		ceilingChars  = ceilingTokens * 4 // charsPerToken in production
		tailCapChars  = ceilingChars * 30 / 100
		writeCount    = 30
		contentChars  = 5000
	)
	script := make([][]string, 0, writeCount+2)
	scriptIDs := make([]string, 0, writeCount)
	scriptArgs := make(map[string]string, writeCount)
	for i := 1; i <= writeCount; i++ {
		id := fmt.Sprintf("tw%02d", i)
		path := fmt.Sprintf("dir/sub/tw%02d.txt", i)
		sentinel := fmt.Sprintf("SENTINEL-%02d-", i)
		content := strings.Repeat(sentinel, contentChars/len(sentinel)+1)[:contentChars]
		argsBytes, err := json.Marshal(map[string]string{"path": path, "content": content})
		if err != nil {
			t.Fatalf("marshal write_file args %d: %v", i, err)
		}
		scriptIDs = append(scriptIDs, id)
		scriptArgs[id] = string(argsBytes)
		script = append(script, e2eToolCallFrames(id, "write_file", string(argsBytes)))
	}
	script = append(script, []string{e2eContentChunk("wrapped up")}, []string{e2eContentChunk("ok")})
	e := startE2E(t, e2eOpts{
		script: script,
		extra: map[string]any{
			"context_prune":                   "off",
			"context_collapse_ceiling_tokens": ceilingTokens,
			"context_collapse_min_bytes":      1,
		},
	})
	e2eReady(t, e)
	e.submit(fmt.Sprintf("write %d numbered files with sentinel content", writeCount))
	// Answers every write_file approval and waits for turn_completed: the
	// ceiling crossing must happen before this returns and the turn must
	// still complete afterwards.
	e2eAnswerApprovals(t, e, 1)
	if got := e.countKind(event.KindTurnCompleted); got < 1 {
		t.Errorf("turn_completed for turn 1 not observed (count=%d)", got)
	}

	reqs := e.requests()
	if len(reqs) < 3 {
		t.Fatalf("too few requests recorded: %d\n%s", len(reqs), e.tail(3000))
	}
	decoded := make([][]verifyMessage, len(reqs))
	groupsPerReq := make([][]midTurnGroup, len(reqs))
	for i, body := range reqs {
		msgs := verifyDecodeMessages(t, body)
		decoded[i] = msgs
		groupsPerReq[i] = midTurnGroups(msgs)
		t.Logf("request %d: bytes=%d pairedGroups=%d", i, len(body), len(groupsPerReq[i]))
	}
	// First request with fewer paired batches than its predecessor: the
	// collapse can only fire when the projected request crossed the ceiling
	// (or hard max) before that request was built.
	collapsedIdx := -1
	for i := 1; i < len(reqs); i++ {
		if len(groupsPerReq[i]) < len(groupsPerReq[i-1]) {
			collapsedIdx = i
			break
		}
	}
	if collapsedIdx < 0 {
		counts := make([]int, len(reqs))
		for i := range reqs {
			counts[i] = len(groupsPerReq[i])
		}
		t.Fatalf("no mid-turn collapse found: paired-group counts=%v (ceiling=%d chars, tail cap=%d)\n%s", counts, ceilingChars, tailCapChars, e.tail(3000))
	}
	prevIdx := collapsedIdx - 1
	prevGroups := groupsPerReq[prevIdx]
	collapsedGroups := groupsPerReq[collapsedIdx]
	prevTotal, retainedBytes := 0, 0
	for _, g := range prevGroups {
		prevTotal += g.size
	}
	for _, g := range collapsedGroups {
		retainedBytes += g.size
	}
	t.Logf("ceiling crossing: request %d carried %d paired groups totalling %d chars (tail cap %d, ceiling %d chars); collapse fired before request %d", prevIdx, len(prevGroups), prevTotal, tailCapChars, ceilingChars, collapsedIdx)
	t.Logf("retained tail bytes=%d (tail cap %d, %.0f%% of ceiling), request bytes %d -> %d, paired groups %d -> %d, elided groups %d, total requests=%d", retainedBytes, tailCapChars, float64(retainedBytes)/float64(ceilingChars)*100, len(reqs[prevIdx]), len(reqs[collapsedIdx]), len(prevGroups), len(collapsedGroups), len(prevGroups)-len(collapsedGroups), len(reqs))
	if len(prevGroups) < 2 {
		t.Errorf("previous request paired groups = %d, want at least 2 to define a retained suffix", len(prevGroups))
	}
	if prevTotal <= tailCapChars {
		t.Errorf("previous paired groups total %d <= tail cap %d: no elision pressure, ceiling crossing not demonstrated", prevTotal, tailCapChars)
	}
	if retainedBytes > tailCapChars {
		t.Errorf("retained tail bytes=%d exceeds tail cap %d", retainedBytes, tailCapChars)
	}
	if len(reqs[collapsedIdx]) >= len(reqs[prevIdx]) {
		t.Errorf("collapsed request bytes=%d not smaller than previous request bytes=%d", len(reqs[collapsedIdx]), len(reqs[prevIdx]))
	}

	// System unchanged: every system message identical to the first request.
	firstSystem := midTurnSystemContents(decoded[0])
	if !slices.Equal(midTurnSystemContents(decoded[prevIdx]), firstSystem) {
		t.Errorf("previous request system messages differ from first request")
	}
	if !slices.Equal(midTurnSystemContents(decoded[collapsedIdx]), firstSystem) {
		t.Errorf("collapsed request system messages differ from first request")
	}
	// Task Content exactly unchanged: first user message identical to the
	// first request, and present exactly once in the collapsed request.
	firstTask, _ := verifyUserContent(decoded[0])
	prevTask, _ := verifyUserContent(decoded[prevIdx])
	collapsedTask, _ := verifyUserContent(decoded[collapsedIdx])
	if collapsedTask != firstTask || prevTask != firstTask {
		t.Errorf("task message differs from first request task message")
	}
	taskCount := 0
	for _, m := range decoded[collapsedIdx] {
		if m.Content == firstTask && firstTask != "" {
			taskCount++
		}
	}
	if taskCount != 1 {
		t.Errorf("task message occurrences in collapsed request = %d, want exactly 1", taskCount)
	}

	// Expected rounds: previous request groups plus the newest group in
	// the collapsed request (the batch just executed, absent from the
	// previous request). Its args are checked against the scripted
	// originals; its result is checked only for call/result pairing.
	if len(collapsedGroups) == 0 {
		t.Fatalf("collapsed request has no paired groups")
	}
	newest := collapsedGroups[len(collapsedGroups)-1]
	if len(prevGroups) == 0 {
		t.Fatalf("previous request has no paired groups")
	}
	if len(newest.callIDs) != 1 {
		t.Fatalf("newest group in collapsed request has %d calls, want exactly 1 (script ID %v)", len(newest.callIDs), scriptIDs)
	}
	newestID := newest.callIDs[0]
	// Newest ID must increment script: it is the next scripted id after
	// the last id already present in the previous request.
	prevLastID := prevGroups[len(prevGroups)-1].callIDs[0]
	prevPos := slices.Index(scriptIDs, prevLastID)
	if prevPos < 0 {
		t.Fatalf("previous request last call ID %q not in scripted IDs %v", prevLastID, scriptIDs)
	}
	if prevPos+1 >= len(scriptIDs) || newestID != scriptIDs[prevPos+1] {
		t.Errorf("newest call ID=%q, want next scripted ID after %q (%v)", newestID, prevLastID, scriptIDs)
	}
	if got, want := newest.args[newestID], scriptArgs[newestID]; got != want {
		t.Errorf("newest call %s arguments differ from scripted original", newestID)
	}
	if _, ok := newest.results[newestID]; !ok {
		t.Errorf("newest call %s missing paired tool result in collapsed request", newestID)
	}
	complete := make([]midTurnGroup, 0, len(prevGroups)+1)
	complete = append(complete, prevGroups...)
	complete = append(complete, newest)

	// Expected retained tail: largest recent whole-group suffix of the
	// complete sequence within the tail cap (most recent group always kept).
	expStart := len(complete) - 1
	acc := complete[expStart].size
	for i := len(complete) - 2; i >= 0; i-- {
		if acc+complete[i].size > tailCapChars {
			break
		}
		acc += complete[i].size
		expStart = i
	}
	expected := complete[expStart:]
	if len(expected) == len(complete) {
		t.Errorf("expected suffix retains all %d complete groups; no elision expected", len(complete))
	}
	if expStart > 0 && acc+complete[expStart-1].size <= tailCapChars {
		t.Errorf("retained suffix not largest: adding next older group (%d chars) would still fit tail cap %d (retained %d)", complete[expStart-1].size, tailCapChars, acc)
	}
	if len(collapsedGroups) != len(expected) {
		t.Errorf("collapsed paired groups = %d, want %d (largest suffix within %d chars)", len(collapsedGroups), len(expected), tailCapChars)
	}
	// All but the last kept group verbatim versus the previous request;
	// newest args versus scripted original, result only paired (above).
	for k := range expected {
		if k >= len(collapsedGroups) {
			break
		}
		if !slices.Equal(collapsedGroups[k].callIDs, expected[k].callIDs) {
			t.Errorf("kept group %d call IDs=%v, want %v", k, collapsedGroups[k].callIDs, expected[k].callIDs)
			continue
		}
		isNewest := k == len(expected)-1
		for _, id := range expected[k].callIDs {
			if isNewest {
				if got, want := collapsedGroups[k].args[id], scriptArgs[id]; got != want {
					t.Errorf("kept newest call %s arguments differ from scripted original", id)
				}
				continue
			}
			if got, want := collapsedGroups[k].args[id], expected[k].args[id]; got != want {
				t.Errorf("kept call %s arguments differ from previous request", id)
			}
			if got, want := collapsedGroups[k].results[id], expected[k].results[id]; got != want {
				t.Errorf("kept tool result %s content differs from previous request", id)
			}
		}
	}
	// Latest pair present: newest batch tw14 is the last group of the
	// collapsed request (not the previous request's last group tw13).
	if len(collapsedGroups) == 0 {
		t.Errorf("missing latest pair: collapsed groups=0")
	} else {
		latestCollapsed := collapsedGroups[len(collapsedGroups)-1]
		if !slices.Equal(latestCollapsed.callIDs, []string{newestID}) {
			t.Errorf("latest pair call IDs=%v, want [%s] (newest scripted batch)", latestCollapsed.callIDs, newestID)
		}
		if prevPos+1 < len(scriptIDs) && newestID != scriptIDs[prevPos+1] {
			t.Errorf("latest pair is %q, want newest scripted ID %q, not previous last %q", newestID, scriptIDs[prevPos+1], prevLastID)
		}
		if newestID != "tw14" {
			t.Errorf("latest pair is %q, want tw14 retained (not tw13)", newestID)
		}
		if newestID == prevLastID {
			t.Errorf("latest pair %q duplicates previous request last group; newest batch not retained", newestID)
		}
	}
}
