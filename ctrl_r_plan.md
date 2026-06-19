# Implementation Plan: Ctrl+R Reverse Incremental History Search

## Goal

Add bash-style reverse incremental search (Ctrl+R) through command history in
the Wakil TUI, reusing the existing `inputHistory` slice and `input_history`
file infrastructure.

## Current state (verified)

- **History storage**: `inputHistory []string` on `tuiModel` (most-recent-first),
  persisted to `~/.local/share/wakil/input_history` via `appendHistory()`
  (`internal/tui/history.go`).
- **Existing navigation**: UP/DOWN linear browse (`tui.go:608-630`). UP saves
  the current draft to `histSaved`, increments `histIdx`, sets textarea value.
  DOWN decrements, restores `histSaved` when past newest.
- **Key handling**: `handleKey(msg tea.KeyMsg)` at `tui.go:551-678` uses
  `msg.String()` string matching. Returns `(model, cmds, consumed bool)`.
  Unconsumed keys fall through to `ta.Update()` for normal text input.
- **Confirm gate**: When `state == stateConfirm`, ALL keys are consumed by the
  gate at `tui.go:552-578` before reaching the main switch.
- **Status line**: `statusLine()` at `tui_view.go:359` delegates to
  `buildStatusLine(statusLineInput{...})` (pure function at `tui_view.go:292`).
  The status line row is already reserved in layout; content is conditional.
- **No reverse search exists** anywhere in the codebase.

## Mashura review — corrections incorporated

The plan was reviewed by mashura (external AI opinion). The following issues
were identified and are incorporated below. Items marked ✓ were verified
against the actual source; items marked ⚠ are from mashura's reasoning.

### Verified against source

1. ✓ **Bubbletea v1** (`go.mod:7` — `bubbletea v1.3.10`). `tea.KeyRunes`,
   `msg.Type`, `msg.Runes` are available. However, the codebase uses
   `msg.String()` exclusively. **Decision**: use `msg.String()` for control
   keys (matching existing code); use `msg.Type == tea.KeyRunes` + `msg.Runes`
   for content accumulation (to avoid the space/alt/paste bugs below).

2. ✓ **`handleCompletionKey` runs BEFORE `handleKey`** (`tui.go:268`). If the
   `@`/`/` picker is active, it can consume keys (up/down/tab/esc/enter) before
   `handleKey` sees them. **Decision**: when `searchActive`, Ctrl+R must still
   work even if the picker is open — but simpler: entering search mode closes
   the picker first (`m.comp = completionState{}`). Then subsequent Ctrl+R
   presses won't have the picker active.

3. ✓ **Consumed keys forward to viewport** (`tui.go:300-303`): when
   `consumed=true`, the key is sent to `m.vp.Update(msg)` (except up/down).
   This is fine for search — Ctrl+R and printable chars don't scroll the
   viewport meaningfully.

4. ✓ **`consumed=false` path recomputes picker** (`tui.go:310-312`): if search
   intercept returns `consumed=false`, `ta.Update(msg)` runs and then
   `computeCompletion` fires against the history-match text. **Decision**: all
   search-mode key handling MUST return `consumed=true`.

### From mashura reasoning (not yet verified, to confirm during implementation)

5. ⚠ **Space handling**: In bubbletea v1, space may be `tea.KeySpace`, not
   `tea.KeyRunes`. Must handle `msg.Type == tea.KeySpace` explicitly in the
   content-accumulation path, or spaces in multi-word queries will be dropped.
   **Action**: add explicit `tea.KeySpace` case during implementation.

6. ⚠ **Alt/paste guard**: Don't accumulate from `msg.String()` (it prefixes
   `alt+`). Accumulate from `msg.Runes` and guard `msg.Alt == false`. Decide
   paste behavior (bash appends paste content to query).

7. ⚠ **Past-the-end repeat**: `searchHistory(..., searchIdx+1)` returns -1 at
   the oldest match. Must guard against `inputHistory[-1]` panic. **Decision**:
   when no further match found, keep current match + index, set `searchFailed =
   true`, show `(failed reverse-i-search)` prompt. Never wrap.

8. ⚠ **Empty query + repeat Ctrl+R**: `strings.Contains(x, "")` is always true,
   so empty-query Ctrl+R steps through every entry. **Decision**: explicit
   behavior — empty query Ctrl+R steps to the previous entry (index+1), like
   bash. Document as intentional, not accidental.

9. ⚠ **Esc ordering**: The existing `case "esc"` only cancels when
   `!stateIdle` and otherwise falls through to textarea. Since search is
   idle-only, must intercept Esc while `searchActive` BEFORE the existing esc
   case in the switch. **Action**: place `searchActive` Esc check at the top of
   `handleKey`, before the main switch, or as the first case.

10. ⚠ **Ctrl+C during search**: Define behavior — abort search (don't quit).
    **Decision**: if `searchActive`, Ctrl+C aborts search and returns
    `consumed=true`. Does not quit.

11. ⚠ **UP/DOWN during search**: Two cursors (`searchIdx` and `histIdx`) into
    the same slice. **Decision**: arrows during search exit search mode
    (keeping the match), then the existing UP/DOWN handler runs. Reconcile
    `histIdx` to the accepted match index so subsequent UP continues sensibly.

12. ⚠ **No "forwarding" in bubbletea**: "Any other key forwards to normal
    handling" has no native mechanism. **Decision**: on an exit-and-process
    key, call `searchExit(true)` then return `consumed=false` so the caller's
    `ta.Update(msg)` + `computeCompletion` path runs naturally. Do NOT both
    mutate search state AND forward in the same call.

13. ⚠ **Add `searchFailed bool` field**: The plan implies a failed state but
    never adds the field. **Action**: add it to the struct.

14. ⚠ **Status line width clamping**: `buildStatusLine` isn't width-clamped in
    the code shown. A long matched command will be clipped. **Decision**: during
    `searchActive`, the status line shows ONLY the search prompt (no backend/
    AUTO/plan segments), and the match preview is truncated to fit
    `m.width - len(prompt_prefix)`.

15. ⚠ **Multi-line entries change textarea height**: `ta.SetValue` with a
    multi-line entry makes the textarea taller, feeding back into
    `sizes()`/`reflow()`. **Action**: manual test during implementation; may
    need to clamp the textarea height or truncate the match preview to one
    line.

16. ⚠ **Backspace variant**: Terminals may send `backspace` or `ctrl+h`.
    **Action**: handle both in the search intercept.

### Additional test cases (from mashura)

- Space in query (multi-word search)
- Alt-key during search (should not insert)
- Paste during search
- Picker open + Ctrl+R (should close picker, enter search)
- UP/DOWN during search (should exit search, accept match)
- Past-the-end repeat (no panic, shows failed state)
- Empty query + repeat Ctrl+R (steps through all history)
- Ctrl+C during search (aborts search, does not quit)
- Construct `tea.KeyMsg` structs directly in tests (not via `String()`) to
  catch the space/alt/paste bugs

## Design

### State (new fields on `tuiModel`, next to existing history fields at tui.go:122-125)

```go
// Reverse-incremental search state. searchActive=false = normal input mode.
searchActive bool
searchQuery  string // the query string typed so far
searchIdx    int    // index into inputHistory of current match (-1 = no match)
searchSaved  string // original textarea content saved on entering search mode
searchFailed bool   // true when the last search found no match (bash: "(failed reverse-i-search)")
```

### Search helper (new function in history.go)

```go
// searchHistory finds the first index >= startIdx in history (most-recent-first)
// whose entry contains query (case-insensitive substring). Returns -1 if none.
func searchHistory(history []string, query string, startIdx int) int {
    q := strings.ToLower(query)
    for i := startIdx; i < len(history); i++ {
        if strings.Contains(strings.ToLower(history[i]), q) {
            return i
        }
    }
    return -1
}
```

### Key handling changes in handleKey (tui.go:551-678)

**Entry point** — new `case "ctrl+r":` in the main switch (before existing cases):

1. If `state != stateIdle` → ignore (don't activate during streaming/confirm).
2. If `!searchActive` → enter search mode:
   - `searchSaved = m.ta.Value()`
   - `searchActive = true`, `searchQuery = ""`, `searchIdx = -1`
   - `m.ta.Reset()` (clear textarea so the match shows cleanly)
   - Return consumed=true.
3. If `searchActive` (repeat Ctrl+R) → find next older match:
   - `searchIdx = searchHistory(m.inputHistory, m.searchQuery, searchIdx+1)`
   - If found: `m.ta.SetValue(m.inputHistory[searchIdx])`
   - If not found: keep current match (or show "failed" state).

**Intercept before textarea fall-through** — when `searchActive` is true,
handle keys before they reach `ta.Update()`:

- **Printable chars** (rune keys, `msg.Type == tea.KeyRunes`): append to
  `searchQuery`, re-scan from index 0:
  - `searchIdx = searchHistory(m.inputHistory, searchQuery, 0)`
  - If found: `m.ta.SetValue(inputHistory[searchIdx])`
  - If not found: show `(failed reverse-i-search)` in status, keep last match.
- **Backspace**: remove last char of `searchQuery`, re-scan from 0.
- **Enter**: exit search mode, then fall through to the existing Enter logic
  (which reads `m.ta.Value()` — already set to the matched entry).
- **Esc / Ctrl+G**: abort — restore `searchSaved` to textarea, exit search mode.
- **Ctrl+R**: handled by the case above (repeat search).
- **Any other key** (arrows, Ctrl+A, etc.): exit search mode, keep the current
  match in the textarea, then forward the key to normal handling. This mirrors
  bash behavior where pressing Right accepts the match and moves the cursor.

### Rendering — search prompt via existing status line

Add a `searchPrompt string` field to `statusLineInput` (tui_view.go:245-261).
When `searchActive` is true, `statusLine()` populates it and `buildStatusLine`
renders it instead of (or alongside) the normal status:

```
(reverse-i-search)`<query>': <matched entry preview>
```

On no match:
```
(failed reverse-i-search)`<query>': <last match or empty>
```

This reuses the existing status line row — **zero layout changes**. The status
line is already conditionally rendered (tui_view.go:193: `if status := m.statusLine(); status != ""`).

## Implementation steps

### Step 0: Verify assumptions
- ✓ Bubbletea v1.3.10 confirmed (`go.mod:7`).
- Probe terminal: does space arrive as `tea.KeySpace` or `tea.KeyRunes`?
  Does backspace arrive as `backspace` or `ctrl+h`? (Quick `tea.LogToFile`
  test or check during implementation.)
- ✓ `handleCompletionKey` runs before `handleKey` (`tui.go:268`) — confirmed.

### Step 1: State fields + search helper
- Add 5 fields to `tuiModel` struct (tui.go ~line 125): `searchActive`,
  `searchQuery`, `searchIdx`, `searchSaved`, `searchFailed`.
- Add `searchHistory()` to `internal/tui/history.go`.
- No behavior change yet — fields are zero-valued.

### Step 2: Enter/exit search mode in handleKey
- Add `case "ctrl+r":` to the main switch in `handleKey`.
- Entering search: close picker (`m.comp = completionState{}`), save textarea
  content to `searchSaved`, clear textarea, set `searchActive=true`.
- Repeat Ctrl+R: find next older match via `searchHistory(..., searchIdx+1)`.
  Guard against -1 return (set `searchFailed=true`, keep current match).
- Add `searchExit(keepMatch bool)` helper: resets all 5 search fields. If
  `keepMatch=false`, restores `searchSaved` to textarea.
- Esc/Ctrl+C/Ctrl+G during search: call `searchExit(false)`, return consumed.
  Place this check BEFORE the main switch (or as first case) so it intercepts
  before the existing esc/ctrl+c cases.
- **Test**: Ctrl+R enters/aborts, repeat finds older matches, no panic at end.

### Step 3: Intercept printable keys during search
- In `handleKey`, after the main switch, before `return m, nil, false`:
  if `searchActive`, intercept content keys:
  - `msg.Type == tea.KeyRunes` and `!msg.Alt`: append `string(msg.Runes)` to
    `searchQuery`, re-scan from 0.
  - `msg.Type == tea.KeySpace`: append `" "` to `searchQuery`, re-scan.
  - `backspace` or `ctrl+h`: remove last char of `searchQuery`, re-scan.
  - All return `consumed=true` (prevents picker recompute via tui.go:312).
- On match: `searchFailed=false`, `ta.SetValue(inputHistory[searchIdx])`.
- On no match: `searchFailed=true`, keep last match in textarea.
- **Test**: type query (including spaces), verify incremental matching.

### Step 4: Enter and other keys during search
- `case "enter":` — if `searchActive`, call `searchExit(true)` at the top
  (before TrimSpace guard), then fall through to existing Enter logic which
  reads `m.ta.Value()` (already set to the match).
- UP/DOWN during search: call `searchExit(true)`, set `histIdx` to
  `searchIdx` (reconcile cursors), then let existing UP/DOWN logic run.
  Return `consumed=true` (the existing handler also returns consumed).
- Any other key (Ctrl+A, Left, Right, etc.): call `searchExit(true)`, return
  `consumed=false` so the key flows to `ta.Update` naturally.
- **Test**: Enter executes match, arrows exit search and navigate history.

### Step 5: Status line rendering
- Add `searchPrompt string` field to `statusLineInput` (tui_view.go:245).
- Populate in `statusLine()` when `searchActive` (tui_view.go:372).
- Render in `buildStatusLine()` (tui_view.go:292): when `searchPrompt != ""`,
  show ONLY the search prompt (suppress all other segments):
  - Match: `(reverse-i-search)\`<query>': <truncated match preview>`
  - No match: `(failed reverse-i-search)\`<query>': <last match or empty>`
- Truncate match preview to `m.width - len(prompt_prefix)` to avoid overflow.
- **Test**: verify prompt appears, query and match shown, no overflow.

### Step 6: Unit tests
- `TestSearchHistory` — empty query, no match, case-insensitive, multi-match,
  startIdx past end (returns -1, no panic).
- `TestHandleKeyCtrlR_EntersSearch` — state fields set correctly.
- `TestHandleKeyCtrlR_RepeatFindsOlder` — second Ctrl+R finds next match.
- `TestHandleKeyCtrlR_PastTheEnd` — no panic, `searchFailed=true`, keeps match.
- `TestHandleKeyCtrlR_EscAborts` — restores saved draft, exits search.
- `TestHandleKeyCtrlR_CtrlCAborts` — aborts search, does not quit.
- `TestHandleKeyCtrlR_EnterExecutesMatch` — matched entry is sent.
- `TestHandleKeyCtrlR_TypeSearches` — printable chars update query + match.
- `TestHandleKeyCtrlR_SpaceInQuery` — space appends to query, not dropped.
- `TestHandleKeyCtrlR_AltKeyIgnored` — alt+rune does not insert.
- `TestHandleKeyCtrlR_BackspaceRemovesQuery` — query shrinks, re-scans.
- `TestHandleKeyCtrlR_BackspaceVariant` — both `backspace` and `ctrl+h` work.
- `TestHandleKeyCtrlR_NotDuringStreaming` — Ctrl+R ignored when not idle.
- `TestHandleKeyCtrlR_NotDuringConfirm` — gate consumes before search.
- `TestHandleKeyCtrlR_PickerOpenCloses` — entering search closes picker.
- `TestHandleKeyCtrlR_ArrowsExitSearch` — UP/DOWN exits search, reconciles
  histIdx.
- `TestHandleKeyCtrlR_EmptyQueryRepeat` — steps through all history entries.
- Construct `tea.KeyMsg` structs directly (not via `String()`) to catch
  space/alt/paste bugs.

## Edge cases

| Case | Handling |
|---|---|
| Empty history | Ctrl+R enters search, no match, shows `(failed reverse-i-search)` |
| No match for query | `searchFailed=true`, show `(failed reverse-i-search)`, keep last valid match in textarea |
| Past-the-end repeat | No panic — guard -1 return, keep current match, set `searchFailed=true`, no wrap |
| Empty query + repeat | Steps through all entries (index+1 each press) — intentional, like bash |
| Space in query | Explicit `tea.KeySpace` case (not `KeyRunes`) — spaces preserved |
| Alt key during search | `msg.Alt` guard — alt+rune does not insert into query |
| Backspace variant | Handle both `backspace` and `ctrl+h` |
| Confirm gate active | Gate consumes all keys (tui.go:552-578) before reaching search logic — no change needed |
| Streaming (not idle) | Ctrl+R ignored — same as existing UP/DOWN restriction (tui.go:609) |
| Picker (`@`/`/`) open | Entering search closes picker first; subsequent keys are search-only |
| UP/DOWN during search | Exits search (keeps match), reconciles `histIdx` to `searchIdx` |
| Ctrl+C during search | Aborts search (does not quit) |
| Multi-line history entries | `strings.Contains` matches across newlines; `ta.SetValue` handles display — test height feedback |
| Resume session | `inputHistory` loaded via `loadHistory()` on init (tui.go:225) — search works immediately |

## Complexity estimate (revised after mashura review)

| Part | Lines | Risk |
|---|---|---|
| State fields (5) | ~6 | None |
| searchHistory helper | ~10 | None |
| searchExit helper | ~12 | Low |
| Key handling (enter/exit/repeat/intercept) | ~70 | Medium |
| Enter/arrows/other-key integration | ~15 | Medium |
| Status line rendering (exclusive + truncation) | ~20 | Low |
| Unit tests (18 test cases) | ~120 | Low |
| **Total** | **~253** | |

## Files touched

| File | Changes |
|---|---|
| `internal/tui/tui.go` | 4 new struct fields, `case "ctrl+r"` in handleKey, search-mode intercept before fall-through, searchExit helper |
| `internal/tui/history.go` | `searchHistory()` function |
| `internal/tui/tui_view.go` | `searchPrompt` field on `statusLineInput`, render in `buildStatusLine`, populate in `statusLine()` |
| `internal/tui/tui_search_test.go` | New test file — all unit tests |

No changes to `config.go`, `exec.go`, `main.go`, or any agent/proxy code.
