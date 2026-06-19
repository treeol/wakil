# Wakil TUI Refurbishment Plan — v3.1 (Final)

## What I verified by reading the code

### Current architecture

| File | Lines | Role |
|---|---|---|
| `tui.go` | 866 | Model struct, Update loop, key routing, streaming cache, ANSI wrapping |
| `tui_view.go` | 818 | View(), sidebar, tab bar, status line, context block, markdown, all styles |
| `tui_msgs.go` | 136 | Message type definitions |
| `tui_select.go` | ~200 | Mouse text selection + clipboard copy |
| `complete.go` | 240 | `@`-mention autocomplete picker |
| `agent_async.go:706-808` | ~100 | `handleTUICommand` — 15+ slash commands in a switch |

### Styling — current state

Styles scattered across three tiers:
1. **Package-level vars** (5): `styleSidebarBorder`, `styleConvBorder`, `styleInputBorder`, `styleTitle`, `styleState`
2. **Helper functions** (5): `styleUser()`, `styleAsst()`, `styleOK()`, `styleErr()`, `dim2()`
3. **Inline constructions** (~40+): `lipgloss.NewStyle().Foreground(lipgloss.Color("240"))` throughout render functions

**Color palette**: 33 (blue/user), 252 (white/asst), 214 (amber/state), 2 (green/ok), 1 (red/error), 196 (bright-red/AUTO), 240 (dim-gray), 237 (dark-gray), 235/241/247/252 (dot pulse shades).

**Borders**: All `HiddenBorder` — zero visible borders, separation by color contrast only. Deliberate (tui_view.go:64-67).

### Command system

`handleTUICommand` is a ~100-line switch with 15+ cases. `helpTextTUI` is a hardcoded string manually synced.

**Verified**: `handleTUICommand` only mutates `*App`, never `tuiModel` (grep confirmed zero model writes). The `func(args, *App)` handler signature is sufficient for registry extraction.

### Dependency versions (read: go.mod, go.sum)
- `bubbletea v1.3.10`, `bubbles v1.0.0`, `lipgloss v1.1.1-0.20250404203927-76690c660834`, `glamour v1.0.0`, `charmbracelet/x/ansi v0.11.6`

### Locally verified API claims

| Claim | Verification | Result |
|---|---|---|
| `bubbles/spinner` re-arms unconditionally | read: `bubbles@v1.0.0/spinner/spinner.go:136-162` | **CONFIRMED** — always returns `m.tick()`, no idle flag, default 100ms |
| `bubbles/list` binds `/` as filter key | read: `bubbles@v1.0.0/list/keys.go:61-64` | **CONFIRMED** — also binds q/esc/g/G/h/j/k/l/f/b/u/d |
| `lipgloss.SetColorProfile` exists | read: `lipgloss@.../renderer.go:126` | **CONFIRMED** — `func SetColorProfile(p termenv.Profile)`, thread-safe, mutates global renderer state |
| HiddenBorder == NormalBorder == RoundedBorder frame size | ran test program in wakil module context | **CONFIRMED** — all three: HFrame=2, VFrame=2, rendered width=42 for Width(40) |
| `ansi.Truncate` preserves background SGR | ran test program | **CONFIRMED** — `\x1b[48;5;1mHELLO\x1b[0m` (truncated to 5), resets properly, visual width correct. Also preserves combined FG+BG: `\x1b[38;5;2;48;5;1mCOMBINED\x1b[0m` |

---

## Refined Plan

### Guiding principles

1. **Preserve all invariants**: idle-quiet ticking, O(chunk) streaming, ANSI-correct wrapping (including background SGR), plainLines selection accuracy, value-copy protection, deliberate key routing.
2. **Prefer evolution over replacement**: extend what exists rather than swapping in components that don't know about wakil's state machine.
3. **Visual refresh, not structural rewrite**.
4. **Every change needs a test**.
5. **`agentState` is for agent lifecycle only** — never overload it for UI focus modes.
6. **glamour ≠ lipgloss** — glamour uses `ansi.StyleConfig`, not `lipgloss.Style`. The theme struct organizes lipgloss styles; a parallel glamour StyleConfig is derived from the theme's color values.
7. **Theme is build-time-static** — no runtime switching. The struct's value is organization and consistency. No `themeID` cache field is needed because the theme never changes at runtime. If runtime switching is added in the future, cache invalidation becomes mandatory at that point (not deferred-and-hardcoded, just not done).

---

### Phase 0: Test Net (precondition, no visual change)

**Goal**: Lock down current behavior so every subsequent change can be verified against it.

| # | Test | What it asserts | File |
|---|---|---|---|
| 0.1 | `TestWrapAnsi_WideRunes` | wrapAnsi handles emoji and CJK without breaking mid-rune | `tui_wrap_test.go` |
| 0.2 | `TestWrapAnsi_UnterminatedCodes` | leadingAnsiCodes handles truncated SGR sequences | `tui_wrap_test.go` |
| 0.3 | `TestWrapAnsi_BackgroundSGR` | Background SGR (`\x1b[48;5;Nm`) is captured by leadingAnsiCodes and re-emitted on continuation lines. **Concrete assertions**: each wrapped line begins with the bg SGR, ends with `\x1b[0m`, and `lipgloss.Width(line) == target width`. Force color profile via `lipgloss.SetColorProfile(termenv.ANSI256)` with `t.Cleanup` to restore. **Must not run `t.Parallel()`** — `SetColorProfile` mutates global state. | `tui_wrap_test.go` |
| 0.4 | `TestPlainLinesInvariant` | `len(plainLines) == visual line count` of rendered output, during streaming + scrolled. Must cover `renderMarkdown` output (headings + fenced code block) at fixed width. | `tui_select_test.go` (extend) |
| 0.5 | `TestIdleTickNotRearmed` | dotTickMsg handler returns nil cmd when `state == stateIdle` | `tui_update_test.go` (extend) |
| 0.6 | `TestCacheInvalidationOnWidth` | `prefixDirty == true` after WindowSizeMsg with new width; per-item `cacheW` invalidated | `tui_layout_test.go` (extend) |
| 0.7 | `TestStatusRowReserved` | `statusLine() != ""` in all four states; `lipgloss.Height(statusLine()) == 1` (not just non-empty — exactly one row); `sizes()` reserves exactly one status row in each state. **Idle-quiet defined**: no re-render churn or animation at idle — a stable reserved row with a static dim dot is intentional, not a violation. | `tui_layout_test.go` (extend) |
| 0.8 | `TestPrefixNotRebuiltOnChunk` | Assert directly (not via timing) that `prefixDirty` stays false and `prefixW` unchanged after a streaming chunk. This is the real O(chunk) invariant. | `tui_update_test.go` (extend) |
| 0.9 | `BenchmarkRefreshViewport` | Measures streaming chunk render cost; document baseline, don't assert threshold. | `tui_bench_test.go` |
| 0.10 | `resetMdCache()` helper | Add helper to reset package-level `mdCache`. **Serialize markdown tests** (don't run `t.Parallel()` for any test that calls `renderMarkdown`); use `t.Cleanup(resetMdCache)` in each. Alternatively, move `mdCache` onto the model — but that's a bigger change, defer. | `tui_view.go` (add helper) |

**Risk**: None — tests only, no production code touched (except 0.10 helper).

---

### Phase 1: Theme Extraction (low risk, high leverage)

**Goal**: Centralize all scattered `lipgloss.NewStyle()` calls into a single `theme` struct.

**Design**:
- Theme is a **passive struct of `lipgloss.Style` values + `lipgloss.Color` constants** — no behavioral logic, no layout, no wrapping.
- Theme is **build-time-static**. No `themeID` cache field — the theme never changes at runtime, so cache invalidation on theme change is a non-issue. If runtime switching is added in the future, it will require: `themeVersion` in `convItem` cache key, `prefixDirty = true` on change, and `mdCache` key extension. That work is not deferred — it simply doesn't exist until runtime switching is a goal.
- **Layout stays out of the theme**: no `.Width()`, `.MaxWidth()`, or padding in stored styles. Those are render-time concerns.
- Extract into `tui_theme.go` (new file, ~80 lines).

**Structure**:
```go
type theme struct {
    // Colors
    user        lipgloss.Color // 33
    asst        lipgloss.Color // 252
    accent      lipgloss.Color // 214
    ok          lipgloss.Color // 2
    err         lipgloss.Color // 1
    danger      lipgloss.Color // 196
    dim         lipgloss.Color // 240
    dimDark     lipgloss.Color // 237
    pulseShades []lipgloss.Color // 235,241,247,252

    // Pre-built styles (colors + bold/italic only — no width/padding)
    title        lipgloss.Style
    state        lipgloss.Style
    sidebarBorder lipgloss.Style
    convBorder   lipgloss.Style
    inputBorder  lipgloss.Style
}

var currentTheme = defaultTheme()
```

**Migration**: Replace all inline `lipgloss.NewStyle().Foreground(lipgloss.Color("NNN"))` calls with `currentTheme.*` references.

**Verification**:
- All existing tests pass unchanged.
- `TestThemeConsistency` — **AST-based check** (not grep): use `go/parser` + `go/types` to resolve `lipgloss.Color` selector calls, allowlisting `tui_theme.go`. This catches hex colors, `AdaptiveColor`, import aliases, and avoids false positives on comments/tests. A grep is too fragile.
- `TestThemeNoLayout` — **AST check** that `tui_theme.go` contains no `Padding`/`Margin`/`Width`/`Height` method calls on styles.
- `TestRenderedWidthUnchanged` — `lipgloss.Width()` of full `View()` output is byte-identical at widths 40/80/120 before and after theme extraction.

**Risk**: Low. Pure mechanical refactoring.

---

### Phase 2: Command Registry (low risk, enables Phase 4)

**Goal**: Extract `handleTUICommand`'s switch into a declarative registry.

**Design**:
```go
type command struct {
    name      string
    aliases   []string
    args      string   // for help text
    help      string   // one-line description
    handler   func(args []string, app *App) (handled, quit bool, cmd tea.Cmd)
    enabled   func(state agentState) bool // nil = always; for palette filtering
}
```

- Registry is a `[]command` slice built once at init.
- `handleTUICommand` becomes lookup + dispatch (~5 lines).
- `helpTextTUI` is generated from the registry.
- `/plan` subcommands stay in `handlePlanCommand`, registered as a single delegating entry.
- `enabled` predicate designed in now — the palette needs it. Default `nil` = always enabled.

**Verified**: All current handlers only mutate `*App` (grep confirmed). The `func(args, *App)` signature is sufficient.

**New file**: `tui_commands.go` (~120 lines).

**Verification**:
- All `command_test.go` tests pass unchanged.
- `TestHelpTextFromRegistry` — generated help contains every registered command name.
- `TestRegistryExhaustive` — table test enumerating every command, asserting `(handled, quit, cmd-nil?)`.

**Risk**: Low. Behavioral equivalence maintained.

---

### Phase 3: Visual Polish (medium risk, the actual "refresh")

Each item independently shippable. All consume `currentTheme.*` from Phase 1.

#### 3a: Visible borders with subtle color

**Change**: Replace `HiddenBorder` with `NormalBorder` or `RoundedBorder` using theme dim color.

**Verified**: HiddenBorder, NormalBorder, and RoundedBorder all have HFrame=2, VFrame=2. Rendered width is identical (42 for Width(40)) for all three. **No off-by-2 risk** — confirmed by running a test program in the wakil module context.

**New tests**:
- `TestBorderWidthParity` — `lipgloss.Width(pane.Render(content))` identical for Hidden vs Normal vs Rounded at widths 40/80/120. (Can be a simple assertion since we already verified the numbers.)
- `TestMouseHitTestAfterBorder` — clicking one cell inside the border maps to content x=0, y=0. Clicking on the border itself does not select content. This is a **real regression vector** — the border shifts click coordinates by the border offset.

**Risk**: Low — frame sizes verified identical. Mouse hit-testing is the only real risk.

#### 3b: Enhanced tab bar styling

**Change**: Background color for active tab, underline for active tab.

**Implementation note**: `lipgloss.Background()` only paints occupied cells. The existing `Width(m.width).Render(bar)` creates a full-width fill — bg must be applied inside that width wrap, or the colored region will be ragged.

**Hit-testing**: Uses fixed visual-width constants — unaffected by color.

**New test**: `TestTabBarBgFillsSlot` — active tab's bg extends across full `tabSubW` visual chars.

**Risk**: Low — pure rendering change.

#### 3c: Status line refinement

**Change**: Group into left/center/right zones using lipgloss.

**Implementation**: `JoinHorizontal` alone doesn't right-justify or truncate. Need explicit width budgeting:
```go
leftW := lipgloss.Width(left)
rightW := lipgloss.Width(right)
midAvail := max(0, m.width - leftW - rightW)
middle = ansi.Truncate(middle, midAvail, "…")
```

**Verified**: `ansi.Truncate` (x/ansi v0.11.6) handles background SGR correctly — preserves `\x1b[48;5;Nm` and closes with `\x1b[0m`. Visual width is exact. **No missing primitive** — `ansi.Truncate` is sufficient. (This is truncation, which is a different code path from `wrapAnsi` — don't conflate the two.)

**Height invariant**: `TestStatusRowReserved` (0.7) must hold — zone layout must not produce multi-line output. Assert `lipgloss.Height(statusLine) == 1`.

**Selection exclusion**: Confirm status line is excluded from `plainLines` — if zone padding shifts column math, selection coordinates break.

**New test**: `TestStatusLineZones` — rendered visual width `<= m.width` at 40/80/120; `lipgloss.Height == 1`; status line not in `plainLines`.

**Risk**: Low — `ansi.Truncate` verified sufficient.

#### 3d: Markdown rendering refinement

**Change**: Customize glamour `StyleConfig`:
- Code blocks: subtle background (e.g., `234`)
- Headings: color 33 (blue, matching theme accent)
- Links: color 33 + underline
- Block quotes: left border in color 240

**Critical**: Glamour uses `ansi.StyleConfig`, not `lipgloss.Style`. The lipgloss theme cannot directly drive glamour. This requires:
1. A parallel `glamour.StyleConfig` derived from the theme's color values (a `glamourStyleConfig()` function that reads from `currentTheme`).
2. **No `mdCache` key change needed** — theme is build-time-static, so the cache key stays `{w int}`. The `mdCache` is rebuilt only on width change, which is correct.
3. **glamour color-mapping test** — golden tests that assert the glamour StyleConfig colors correspond to the theme palette (headings use color 33, code block bg uses 234, etc.). This prevents silent drift between the lipgloss theme and the glamour StyleConfig.

**`mdCache` test isolation**: Use `resetMdCache()` from Phase 0.10 in every markdown test. Don't run markdown tests in parallel.

**Version-dependent**: glamour v1.0.0 code-block bg fill-to-width — add a golden test that renders a fenced block at fixed width and asserts bg doesn't bleed past the width boundary.

**New tests**:
- `TestRenderMarkdown_CodeBlockBg` — golden at fixed width + forced color profile (non-parallel, `t.Cleanup` restore).
- `TestRenderMarkdown_Headings` — headings use color 33.
- `TestRenderMarkdown_AtWidths` — no line exceeds width at 40/80/120.
- `TestGlamourStyleConfigMatchesTheme` — assert glamour StyleConfig colors correspond to `currentTheme` palette.

**Risk**: Medium — glamour StyleConfig customization. Isolated to `markdownRenderer()`.

#### 3e: Keep dotPhase — refine phase label only

**Change**: Keep `dotPhase` exactly as-is. Refine the state→text mapping in `buildStatusLine` (wording/grouping only).

**Constraint**: Label is **static per state** (set on state transition, not animated). Must NOT require the dot tick to re-arm at idle.

**Risk**: None — no mechanism change.

---

### Phase 4: Command Palette (high risk, deferred until Phase 2)

**Goal**: `Ctrl+K` triggered fuzzy-search palette over the command registry.

**Design**:
- **Focus model**: `paletteOpen bool` on `tuiModel` — **orthogonal** to `agentState`. The dot tick predicate (`state != stateIdle`) is unaffected.
- **Trigger**: `Ctrl+K` only when `state == stateIdle` AND `!m.comp.active`. Exclusions: not streaming, not confirm, not compacting, not while completion active.
- **Key routing**: When `paletteOpen`, `handleKey` routes all keys to palette. Esc closes. Same consume/forward pattern as confirm gate and `@`-picker.
- **Completion picker interaction**: Block palette opening while completion is active (simpler than pre-empting `handleCompletionKey`).
- **Async state change**: With orthogonal focus, `stateStreaming` doesn't conflict — palette stays as UI overlay, `enabled(state)` re-checked on Enter.
- **Component**: `bubbles/textinput` + custom filtered list (NOT `bubbles/list`).
- **Selection exclusion**: Palette overlay excluded from `plainLines`.

**New tests**:
- `TestPaletteInteractionMatrix` — `paletteOpen` × each `agentState` × `comp.active`: (a) Ctrl+K ignored while completion active, (b) palette-open during streaming doesn't corrupt `plainLines`, (c) toggling `paletteOpen` while idle produces no render side-effects (idle-quiet proof).
- `TestPaletteCtrlKOnlyIdle` — Ctrl+K ignored in streaming/confirm/compacting states.
- `TestPaletteExcludedFromPlainLines` — palette overlay not in `plainLines`.
- `TestPaletteEnabledRechecked` — `enabled(state)` re-evaluated on Enter after async state change.

**Prerequisite**: Phase 2 (registry).

---

### Phase 5: Typed Notice Queue (low risk, deferred)

**Goal**: Evolve `flash` into a typed queue.

```go
type noticeLevel int8
const (noticeInfo noticeLevel = iota; noticeSuccess; noticeWarning; noticeError)
type notice struct { level noticeLevel; text string; expires time.Time }
```

- `flash` → `notices []notice` (max 3).
- Self-terminating tick for auto-dismiss (same pattern as `dotTickMsg`).
- Stays in status line region — no overlay, `plainLines` unaffected.

**New test**: `TestNoticeQueueIdleQuiet` — tick only re-armed when queue non-empty; idle-quiet preserved.

**Prerequisite**: Phase 3c (status line zones).

---

## What we are NOT doing (and why)

| Rejected idea | Reason | Verified by |
|---|---|---|
| `bubbles/spinner` for dotPhase | Re-arms unconditionally at 100ms, no idle awareness | read: spinner.go:136-162 |
| `bubbles/progress` for agent turns | No known denominator — UX lie | mashura review |
| `bubbles/list` for conversation pane | Owns item slice, bypasses prefix cache, `/` collides | read: list/keys.go:61-64 |
| `bubbles/list` for sidebar | Default keymap collides with textarea/bindings | read: list/keys.go:34-96 |
| Overlay toast notifications | No native overlay; collides with plainLines/selection | mashura review |
| Runtime theme switching | Requires cache invalidation work — not a current goal | mashura review |
| `statePalette` as agentState enum | Would make dot tick re-arm while palette open | mashura v2 review |
| `themeID` cache field (constant) | Self-contradictory with build-time-static theme — drop it | mashura v3 review |

---

## Execution order & dependencies

```
Phase 0 (tests) ──→ Phase 1 (theme) ──┐
                                       ├─→ Phase 3a (borders)
                                       ├─→ Phase 3b (tab bar)
                                       ├─→ Phase 3c (status line) ──→ Phase 5 (notices)
                                       ├─→ Phase 3d (markdown)
                                       └─→ Phase 3e (phase label)
Phase 0 (tests) ──→ Phase 2 (registry) ──→ Phase 4 (palette)
```

- Phase 0 is the hard prerequisite for everything.
- Phases 1 and 2 can proceed in parallel after Phase 0.
- Phase 3 items are independent but require Phase 1.
- Phase 4 requires Phase 2.
- Phase 5 requires Phase 3c.

---

## Acceptance criteria (per phase)

| Phase | Done when |
|---|---|
| 0 | All 10 new tests pass; `go test ./cmd/wakil/` green; `resetMdCache()` helper exists; bg-SGR test assertions are concrete (line begins with bg SGR, ends with reset, width == target); color-profile tests are non-parallel with `t.Cleanup` restore |
| 1 | AST-based check confirms zero `lipgloss.Color` calls outside `tui_theme.go`; AST check confirms no layout methods in theme; `lipgloss.Width(View())` byte-identical at 40/80/120; all existing tests green |
| 2 | `handleTUICommand` <10 lines; help text generated from registry; `TestRegistryExhaustive` + `TestHelpTextFromRegistry` pass; all `command_test.go` green |
| 3a | `TestBorderWidthParity` passes; `TestMouseHitTestAfterBorder` passes (click inside border → content x=0; click on border → no selection); `TestLayoutFillsHeightNoGap` passes |
| 3b | `TestTabBarBgFillsSlot` passes (bg extends full `tabSubW`); `tui_tabs_test.go` passes; hit-testing unchanged |
| 3c | `TestStatusLineZones` passes (width `<= m.width` at 40/80/120; `lipgloss.Height == 1`); `TestStatusRowReserved` passes; status line excluded from `plainLines` |
| 3d | `TestRenderMarkdown_CodeBlockBg` + `TestRenderMarkdown_Headings` golden pass; `TestRenderMarkdown_AtWidths` — no line exceeds width; `TestGlamourStyleConfigMatchesTheme` — glamour colors correspond to theme palette; code-block bg doesn't bleed past width |
| 3e | Phase label shown next to dot; label is static per state; existing status tests pass; idle-quiet preserved |
| 4 | `TestPaletteInteractionMatrix` passes (all 3 assertions); `TestPaletteCtrlKOnlyIdle` passes; `TestPaletteExcludedFromPlainLines` passes; `TestPaletteEnabledRechecked` passes; Ctrl+K opens/closes/executes correctly |
| 5 | `TestNoticeQueueIdleQuiet` passes (tick only re-armed when non-empty); notices auto-dismiss after 3s; max 3 visible; rendered in status line zone |
