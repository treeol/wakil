package tui

import (
	"context"
	"math"
	"strings"
	"testing"

	agent "wakil/internal/agent"

	"wakil/internal/config"
	"wakil/internal/counsel"
	"wakil/internal/proxy"

	"github.com/charmbracelet/lipgloss"
)

func rowBySource(rows []proxy.CostRow, source string) (proxy.CostRow, bool) {
	for _, r := range rows {
		if r.Source == source {
			return r, true
		}
	}
	return proxy.CostRow{}, false
}

func approxEq(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// A mashura (oracle) call records under "mashura·<model>" (per-model split),
// with exact token counts and pricing from the per-model config rate.
func TestMashuraRecordsExactCostFromConfig(t *testing.T) {
	const model = "claude-fable-5"
	cfg := config.Config{
		OracleModel: model,
		Costs: config.CostsConfig{
			Mashura: map[string]config.ModelRate{
				model: {InputUSDPer1M: 3.0, OutputUSDPer1M: 15.0},
			},
		},
	}
	app := &agent.App{Cfg: cfg, Costs: proxy.NewCostTracker()}
	app.RecordOracleCost(counsel.OracleUsage{InputTokens: 1_000_000, OutputTokens: 2_000_000})

	total, rows := app.Costs.Snapshot()
	wantKey := proxy.CostSourceMashuraPrefix + model // "mashura·claude-fable-5"
	r, ok := rowBySource(rows, wantKey)
	if !ok {
		t.Fatalf("no %q row in %+v", wantKey, rows)
	}
	if r.Calls != 1 {
		t.Errorf("calls = %d, want 1", r.Calls)
	}
	if r.InputTok != 1_000_000 || r.OutputTok != 2_000_000 {
		t.Errorf("tokens = (%d,%d), want (1000000,2000000)", r.InputTok, r.OutputTok)
	}
	// 1M/1M*3 + 2M/1M*15 = 3 + 30 = 33
	if !approxEq(r.CostUSD, 33.0) {
		t.Errorf("cost = %v, want 33.0", r.CostUSD)
	}
	if r.Confidence != proxy.ConfExact {
		t.Errorf("confidence = %q, want exact", r.Confidence)
	}
	if !r.Priced {
		t.Error("priced = false, want true")
	}
	if !approxEq(total, 33.0) {
		t.Errorf("total = %v, want 33.0", total)
	}
}

// Inference accumulates main + aux calls into one source: tokens and cost add up,
// call count increments, and the modeled rate is applied to input+output tokens.
func TestInferenceAccumulatesMainPlusAux(t *testing.T) {
	cfg := config.Config{Costs: config.CostsConfig{Inference: config.InferenceRate{USDPer1MTokens: 2.0}}}
	client := &proxy.Client{}
	app := &agent.App{Cfg: cfg, Client: client, Costs: proxy.NewCostTracker()}

	client.SetUsage(proxy.UsageStat{InputTok: 100, OutputTok: 50, Exact: true}) // main
	app.RecordInferenceCost()
	client.SetUsage(proxy.UsageStat{InputTok: 30, OutputTok: 20, Exact: true}) // aux (compaction)
	app.RecordInferenceCost()

	_, rows := app.Costs.Snapshot()
	r, ok := rowBySource(rows, proxy.CostSourceInference)
	if !ok {
		t.Fatalf("no inference row in %+v", rows)
	}
	if r.Calls != 2 {
		t.Errorf("calls = %d, want 2", r.Calls)
	}
	if r.InputTok != 130 || r.OutputTok != 70 {
		t.Errorf("tokens = (%d,%d), want (130,70)", r.InputTok, r.OutputTok)
	}
	// (130+70) tokens / 1e6 * 2.0
	if want := 200.0 / 1e6 * 2.0; !approxEq(r.CostUSD, want) {
		t.Errorf("cost = %v, want %v", r.CostUSD, want)
	}
	if r.Confidence != proxy.ConfModeled {
		t.Errorf("confidence = %q, want modeled (exact tokens, proxy rate)", r.Confidence)
	}
}

// Estimated (proxy reported no usage) inference is marked approx, not modeled.
func TestInferenceEstimatedIsApprox(t *testing.T) {
	cfg := config.Config{Costs: config.CostsConfig{Inference: config.InferenceRate{USDPer1MTokens: 2.0}}}
	client := &proxy.Client{}
	app := &agent.App{Cfg: cfg, Client: client, Costs: proxy.NewCostTracker()}

	client.SetUsage(proxy.UsageStat{InputTok: 40, OutputTok: 60, Exact: false})
	app.RecordInferenceCost()

	_, rows := app.Costs.Snapshot()
	r, _ := rowBySource(rows, proxy.CostSourceInference)
	if r.Confidence != proxy.ConfApprox {
		t.Errorf("confidence = %q, want approx", r.Confidence)
	}
}

// An unpriced source renders "—", never "$0.00"; a priced source (even at zero)
// renders dollars.
func TestUnpricedSourceRendersDash(t *testing.T) {
	if got := proxy.CostCell(proxy.CostRow{Source: "inference", Calls: 3, Priced: false}); got != "—" {
		t.Errorf("unpriced cell = %q, want —", got)
	}
	if got := proxy.CostCell(proxy.CostRow{Source: "mashura", CostUSD: 0.30, Priced: true}); got != "$0.30" {
		t.Errorf("priced cell = %q, want $0.30", got)
	}
	// A priced source that genuinely cost nothing is honest about it ($0.00) —
	// only the unpriced (no-rate) case suppresses the number.
	if got := proxy.CostCell(proxy.CostRow{Priced: true, CostUSD: 0}); got != "$0.00" {
		t.Errorf("priced-zero cell = %q, want $0.00", got)
	}
}

// End-to-end: recording inference with no configured rate leaves the source
// unpriced — token counts accrue but the cell shows "—".
func TestUnpricedInferenceEndToEnd(t *testing.T) {
	client := &proxy.Client{}
	app := &agent.App{Cfg: config.Config{}, Client: client, Costs: proxy.NewCostTracker()} // no [costs] rates
	client.SetUsage(proxy.UsageStat{InputTok: 100, OutputTok: 100, Exact: true})
	app.RecordInferenceCost()

	_, rows := app.Costs.Snapshot()
	r, ok := rowBySource(rows, proxy.CostSourceInference)
	if !ok {
		t.Fatal("no inference row")
	}
	if r.Priced {
		t.Error("priced = true, want false (no rate configured)")
	}
	if r.InputTok != 100 || r.OutputTok != 100 {
		t.Errorf("tokens = (%d,%d), want (100,100)", r.InputTok, r.OutputTok)
	}
	if got := proxy.CostCell(r); got != "—" {
		t.Errorf("cell = %q, want —", got)
	}
}

// The total sums priced sources only; an unpriced source contributes calls but no
// dollars. Rows come back sorted by cost descending.
func TestTotalSumsOnlyPricedSources(t *testing.T) {
	tr := proxy.NewCostTracker()
	tr.Record(proxy.CostSourceMashura, 10, 20, 0.30, true, proxy.ConfExact)
	tr.Record(proxy.CostSourceInference, 100, 50, 0, false, proxy.ConfModeled) // unpriced
	tr.Record(proxy.CostSourceSearch, 0, 0, 0.05, true, proxy.ConfModeled)

	total, rows := tr.Snapshot()
	if !approxEq(total, 0.35) {
		t.Errorf("total = %v, want 0.35 (0.30 + 0.05, inference excluded)", total)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	// cost descending: mashura(0.30) > search(0.05) > inference(0)
	if rows[0].Source != proxy.CostSourceMashura || rows[1].Source != proxy.CostSourceSearch || rows[2].Source != proxy.CostSourceInference {
		t.Errorf("sort order = [%s,%s,%s], want [mashura,search,inference]",
			rows[0].Source, rows[1].Source, rows[2].Source)
	}
}

// The confidence glyph reflects the source tier: exact is a solid dot
// (billed-grade); modeled and approx are hollow (never read as billed) and are
// distinguished from each other by colour.
func TestCostGlyphReflectsConfidence(t *testing.T) {
	exactG, exactC := proxy.CostGlyphStyle(proxy.ConfExact)
	modG, modC := proxy.CostGlyphStyle(proxy.ConfModeled)
	apxG, apxC := proxy.CostGlyphStyle(proxy.ConfApprox)

	if exactG != "●" {
		t.Errorf("exact glyph = %q, want ● (solid)", exactG)
	}
	if modG != "○" || apxG != "○" {
		t.Errorf("modeled/approx glyphs = %q/%q, want both ○ (hollow)", modG, apxG)
	}
	if exactC == modC || exactC == apxC {
		t.Error("exact colour must differ from modeled/approx so it cannot be mistaken for them")
	}
	if modC == apxC {
		t.Error("modeled and approx colours must differ")
	}
}

func TestWeakerConfidence(t *testing.T) {
	cases := []struct{ a, b, want string }{
		{"", proxy.ConfExact, proxy.ConfExact},
		{proxy.ConfExact, "", proxy.ConfExact},
		{proxy.ConfExact, proxy.ConfModeled, proxy.ConfModeled},
		{proxy.ConfModeled, proxy.ConfExact, proxy.ConfModeled},
		{proxy.ConfModeled, proxy.ConfApprox, proxy.ConfApprox},
		{proxy.ConfApprox, proxy.ConfModeled, proxy.ConfApprox},
		{proxy.ConfExact, proxy.ConfExact, proxy.ConfExact},
	}
	for _, c := range cases {
		if got := proxy.WeakerConfidence(c.a, c.b); got != c.want {
			t.Errorf("WeakerConfidence(%q,%q) = %q, want %q", c.a, c.b, got, c.want)
		}
	}
}

func TestCostTrackerNilSafe(t *testing.T) {
	var tr *proxy.CostTracker
	tr.Record(proxy.CostSourceSearch, 0, 0, 1.0, true, proxy.ConfModeled) // must not panic
	total, rows := tr.Snapshot()
	if total != 0 || rows != nil {
		t.Errorf("nil snapshot = (%v,%v), want (0,nil)", total, rows)
	}
}

// Stream captures exact usage from a trailing usage chunk and marks it exact.
func TestStreamCapturesExactUsage(t *testing.T) {
	usageFrame := `{"choices":[],"usage":{"prompt_tokens":120,"completion_tokens":45}}`
	srv := sseServer(t, []string{contentChunk("hello world"), usageFrame})
	defer srv.Close()

	c := newTestClient(srv.URL)
	if _, err := c.Stream(context.Background(), nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	u := c.LastUsage()
	if !u.Exact {
		t.Error("Exact = false, want true (proxy reported usage)")
	}
	if u.InputTok != 120 || u.OutputTok != 45 {
		t.Errorf("usage = (%d,%d), want (120,45)", u.InputTok, u.OutputTok)
	}
}

// With no usage chunk, Stream estimates output tokens from streamed length and
// marks the result inexact.
func TestStreamEstimatesUsageWhenAbsent(t *testing.T) {
	srv := sseServer(t, []string{contentChunk("0123456789")}) // 10 chars ≈ 3 tokens
	defer srv.Close()

	c := newTestClient(srv.URL)
	if _, err := c.Stream(context.Background(), nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	u := c.LastUsage()
	if u.Exact {
		t.Error("Exact = true, want false (no usage chunk → estimate)")
	}
	if u.OutputTok == 0 {
		t.Error("estimated OutputTok = 0, want > 0 from streamed content")
	}
}

// The rendered block fits the sidebar column, leads with billed/est subtotals,
// and shows source names (possibly compacted) without overflow.
func TestCostLinesLayout(t *testing.T) {
	app := &agent.App{Costs: proxy.NewCostTracker()}
	app.Costs.Record(proxy.CostSourceMashura, 10, 20, 0.30, true, proxy.ConfExact)
	app.Costs.Record(proxy.CostSourceInference, 100, 50, 0.12, true, proxy.ConfModeled)
	app.Costs.Record(proxy.CostSourceSearch, 0, 0, 0, false, proxy.ConfModeled) // unpriced

	const innerW = sidebarWidth - 4 // 24, matches mainSidebarLines
	m := tuiModel{app: app}
	lines := m.costLines(innerW)
	if len(lines) == 0 {
		t.Fatal("no cost lines rendered")
	}
	joined := strings.Join(lines, "\n")
	// Must have the header and at least one subtotal line.
	if !strings.Contains(joined, "costs") {
		t.Errorf("missing 'costs' header in:\n%s", joined)
	}
	// mashura is ConfExact → "billed" subtotal; inference is ConfModeled → "est" subtotal.
	if !strings.Contains(joined, "billed") {
		t.Errorf("missing 'billed' subtotal (exact sources present) in:\n%s", joined)
	}
	if !strings.Contains(joined, "est") {
		t.Errorf("missing 'est' subtotal (modeled sources present) in:\n%s", joined)
	}
	// "inference" source renders as "inference" (no backend suffix when key is the legacy constant).
	if !strings.Contains(joined, "inference") {
		t.Error("inference source name truncated or missing")
	}
	for i, ln := range lines {
		if w := lipgloss.Width(ln); w > innerW {
			t.Errorf("line %d width %d exceeds innerW %d: %q", i, w, innerW, ln)
		}
	}
}

// When no source is priced the total shows "—" rather than a misleading "$0.00".
func TestCostLinesUnpricedTotal(t *testing.T) {
	app := &agent.App{Costs: proxy.NewCostTracker()}
	app.Costs.Record(proxy.CostSourceInference, 100, 50, 0, false, proxy.ConfModeled)

	m := tuiModel{app: app}
	lines := m.costLines(sidebarWidth - 4)
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "$0.00") {
		t.Errorf("unpriced total should not render $0.00:\n%s", joined)
	}
	if !strings.Contains(joined, "—") {
		t.Errorf("expected — for unpriced figures:\n%s", joined)
	}
}

// --- P30 tests ---

// TestSnapshotSplitBilledVsEstimated verifies SnapshotSplit separates exact
// rows (billed) from modeled/approx rows (estimated).
func TestSnapshotSplitBilledVsEstimated(t *testing.T) {
	tr := proxy.NewCostTracker()
	tr.Record("mashura·opus", 10, 20, 0.30, true, proxy.ConfExact)
	tr.Record("inference·OR/gpt-4o", 100, 50, 0.10, true, proxy.ConfExact)
	tr.Record("inference·local", 200, 100, 0.02, true, proxy.ConfModeled)
	tr.Record("search", 0, 0, 0.05, true, proxy.ConfModeled)

	billedTotal, estimatedTotal, anyBilled, anyEstimated, rows := tr.SnapshotSplit()
	if !approxEq(billedTotal, 0.40) {
		t.Errorf("billedTotal = %v, want 0.40 (mashura+inference·OR)", billedTotal)
	}
	if !approxEq(estimatedTotal, 0.07) {
		t.Errorf("estimatedTotal = %v, want 0.07 (local+search)", estimatedTotal)
	}
	if !anyBilled {
		t.Error("anyBilled should be true")
	}
	if !anyEstimated {
		t.Error("anyEstimated should be true")
	}
	if len(rows) != 4 {
		t.Errorf("rows = %d, want 4", len(rows))
	}
}

// TestSnapshotSplitBilledSubtotalExcludesEstimated verifies that the billed
// subtotal is the sum of exact sources only — the spec invariant.
func TestSnapshotSplitBilledSubtotalExcludesEstimated(t *testing.T) {
	tr := proxy.NewCostTracker()
	// Mix: one exact (external, billed), two modeled (local, estimated).
	tr.Record("inference·openrouter/gpt-4o", 1000, 500, 0.80, true, proxy.ConfExact)
	tr.Record("inference·local", 5000, 2000, 0.03, true, proxy.ConfModeled)
	tr.Record("search", 0, 0, 0.05, true, proxy.ConfApprox)

	billedTotal, estimatedTotal, _, _, _ := tr.SnapshotSplit()
	if !approxEq(billedTotal, 0.80) {
		t.Errorf("billedTotal = %v, want 0.80 (only the exact external row)", billedTotal)
	}
	if !approxEq(estimatedTotal, 0.08) {
		t.Errorf("estimatedTotal = %v, want 0.08 (local 0.03 + search 0.05)", estimatedTotal)
	}
}

// TestSnapshotSplitNilSafe confirms SnapshotSplit handles a nil tracker.
func TestSnapshotSplitNilSafe(t *testing.T) {
	var tr *proxy.CostTracker
	billed, estimated, anyB, anyE, rows := tr.SnapshotSplit()
	if billed != 0 || estimated != 0 || anyB || anyE || rows != nil {
		t.Errorf("nil snapshot should return zero values; got billed=%v est=%v anyB=%v anyE=%v rows=%v",
			billed, estimated, anyB, anyE, rows)
	}
}

// TestMixedSessionDistinctRows verifies that a session using both local and
// external backends produces separate rows (not merged under one "inference" key).
func TestMixedSessionDistinctRows(t *testing.T) {
	app := &agent.App{
		Cfg: config.Config{
			Costs: config.CostsConfig{
				Inference: config.InferenceRate{USDPer1MTokens: 0.5},
				InferenceBackends: map[string]config.ModelRate{
					"openrouter/openai/gpt-4o": {InputUSDPer1M: 2.50, OutputUSDPer1M: 10.0},
				},
			},
		},
		Client: &proxy.Client{Model: "openai/gpt-4o"},
		Costs:  proxy.NewCostTracker(),
		BackendList: []agent.BackendInfo{
			{Name: "openrouter", External: true},
		},
	}

	// Local turn: no backend used.
	app.Client.SetUsage(proxy.UsageStat{InputTok: 200, OutputTok: 100, Exact: true})
	app.Client.SetLastUsedBackend("")
	app.RecordInferenceCost()

	// External turn: openrouter used.
	app.Client.SetUsage(proxy.UsageStat{InputTok: 500, OutputTok: 200, Exact: true})
	app.Client.SetLastUsedBackend("openrouter")
	app.RecordInferenceCost()

	_, rows := app.Costs.Snapshot()
	if len(rows) != 2 {
		t.Fatalf("expected 2 distinct rows (local + external); got %d: %+v", len(rows), rows)
	}

	// Find each row by source key.
	localRow, gotLocal := rowBySource(rows, proxy.CostSourceInference) // "" backend → legacy key
	extKey := proxy.CostSourceInfPrefix + "openrouter/" + app.Client.Model
	extRow, gotExt := rowBySource(rows, extKey)
	if !gotLocal {
		t.Errorf("missing local inference row %q; rows: %+v", proxy.CostSourceInference, rows)
	}
	if !gotExt {
		t.Errorf("missing external inference row %q; rows: %+v", extKey, rows)
	}

	// Local row: ConfModeled (compute cost, not a bill).
	if gotLocal && localRow.Confidence != proxy.ConfModeled {
		t.Errorf("local confidence = %q, want modeled", localRow.Confidence)
	}
	// External row: ConfExact (real provider tokens, can be billed).
	if gotExt && extRow.Confidence != proxy.ConfExact {
		t.Errorf("external confidence = %q, want exact", extRow.Confidence)
	}

	// SnapshotSplit billed total = external priced cost only.
	billedTotal, _, _, _, _ := app.Costs.SnapshotSplit()
	// 500/1M*2.50 + 200/1M*10.0 = 0.00125 + 0.002 = 0.00325
	wantBilled := float64(500)/1e6*2.50 + float64(200)/1e6*10.0
	if !approxEq(billedTotal, wantBilled) {
		t.Errorf("billed total = %v, want %v (OR inference only)", billedTotal, wantBilled)
	}
}

// TestMashuraPerModelRows verifies each panel model gets its own cost row.
func TestMashuraPerModelRows(t *testing.T) {
	const modelA = "claude-opus-4-8"
	const modelB = "openrouter:google/gemini-2.5-pro"
	cfg := config.Config{
		Costs: config.CostsConfig{
			Mashura: map[string]config.ModelRate{
				modelA: {InputUSDPer1M: 15.0, OutputUSDPer1M: 75.0},
				modelB: {InputUSDPer1M: 1.25, OutputUSDPer1M: 5.0},
			},
		},
	}
	app := &agent.App{Cfg: cfg, Costs: proxy.NewCostTracker()}
	app.RecordOracleCostFor(modelA, counsel.OracleUsage{InputTokens: 100_000, OutputTokens: 50_000})
	app.RecordOracleCostFor(modelB, counsel.OracleUsage{InputTokens: 200_000, OutputTokens: 80_000})

	_, rows := app.Costs.Snapshot()
	if len(rows) != 2 {
		t.Fatalf("expected 2 mashura rows, got %d: %+v", len(rows), rows)
	}
	keyA := proxy.CostSourceMashuraPrefix + modelA
	keyB := proxy.CostSourceMashuraPrefix + modelB
	rA, okA := rowBySource(rows, keyA)
	rB, okB := rowBySource(rows, keyB)
	if !okA {
		t.Errorf("missing row for %q", keyA)
	}
	if !okB {
		t.Errorf("missing row for %q", keyB)
	}
	// 100k/1M*15 + 50k/1M*75 = 1.5 + 3.75 = 5.25
	wantA := float64(100_000)/1e6*15.0 + float64(50_000)/1e6*75.0
	if okA && !approxEq(rA.CostUSD, wantA) {
		t.Errorf("modelA cost = %v, want %v", rA.CostUSD, wantA)
	}
	// 200k/1M*1.25 + 80k/1M*5 = 0.25 + 0.4 = 0.65
	wantB := float64(200_000)/1e6*1.25 + float64(80_000)/1e6*5.0
	if okB && !approxEq(rB.CostUSD, wantB) {
		t.Errorf("modelB cost = %v, want %v", rB.CostUSD, wantB)
	}
}

// TestCostLinesBilledAndEstSubtotals verifies both subtotals appear when a
// session has both exact (external) and modeled (local) inference sources.
func TestCostLinesBilledAndEstSubtotals(t *testing.T) {
	app := &agent.App{Costs: proxy.NewCostTracker()}
	app.Costs.Record("inference·openrouter/gpt-4o", 500, 200, 0.40, true, proxy.ConfExact)
	app.Costs.Record("inference·local", 2000, 1000, 0, false, proxy.ConfModeled)

	m := tuiModel{app: app}
	lines := m.costLines(sidebarWidth - 4)
	joined := strings.Join(lines, "\n")

	if !strings.Contains(joined, "billed") {
		t.Errorf("missing 'billed' subtotal; got:\n%s", joined)
	}
	if !strings.Contains(joined, "est") {
		t.Errorf("missing 'est' subtotal; got:\n%s", joined)
	}
	// The external row cost $0.40 should appear in billed, not in est.
	if !strings.Contains(joined, "$0.40") {
		t.Errorf("billed total $0.40 not shown; got:\n%s", joined)
	}
	// Local is unpriced → est shows "—".
	// Check "—" appears (for the unpriced local row or subtotal).
	if !strings.Contains(joined, "—") {
		t.Errorf("unpriced marker — missing; got:\n%s", joined)
	}
}

// TestCostLinesOnlyBilled: when all rows are exact, only "billed" shows.
func TestCostLinesOnlyBilled(t *testing.T) {
	app := &agent.App{Costs: proxy.NewCostTracker()}
	app.Costs.Record("mashura·claude-opus", 10, 20, 0.50, true, proxy.ConfExact)

	m := tuiModel{app: app}
	joined := strings.Join(m.costLines(sidebarWidth-4), "\n")
	if !strings.Contains(joined, "billed") {
		t.Errorf("missing billed subtotal; got:\n%s", joined)
	}
	if strings.Contains(joined, "est") {
		t.Errorf("est subtotal should be absent when no modeled rows; got:\n%s", joined)
	}
}

// TestCostLinesOnlyEstimated: when all rows are modeled, only "est" shows.
func TestCostLinesOnlyEstimated(t *testing.T) {
	app := &agent.App{Costs: proxy.NewCostTracker()}
	app.Costs.Record("inference", 100, 50, 0.01, true, proxy.ConfModeled)

	m := tuiModel{app: app}
	joined := strings.Join(m.costLines(sidebarWidth-4), "\n")
	if strings.Contains(joined, "billed") {
		t.Errorf("billed subtotal should be absent when no exact rows; got:\n%s", joined)
	}
	if !strings.Contains(joined, "est") {
		t.Errorf("missing est subtotal; got:\n%s", joined)
	}
}

// TestShortSourceNameLocal verifies compact label generation for source keys.
// All labels are ≤9 visual chars; truncation uses "…" as the last character.
func TestShortSourceNameLocal(t *testing.T) {
	cases := []struct {
		source, want string
	}{
		// Legacy aggregate keys: passed through ansi.Truncate at 9.
		{"inference", "inference"}, // 9 chars — exact fit
		{"search", "search"},       // 6 chars
		{"mashura", "mashura"},     // 7 chars, no prefix match

		// Per-backend inference: "inf·<backend>" truncated to 9.
		{"inference·local", "inf·local"},      // "inf·local" = 9 chars
		{"inference·openrouter", "inf·open…"}, // "inf·openrouter" = 14, truncated to 9 → "inf·open…"
		// Model is stripped from the display — only backend matters at 9 chars.
		{"inference·openrouter/openai/gpt-4o", "inf·open…"},

		// Per-model mashura: "m·<shortmodel>" truncated to 9.
		// "anthropic:claude-opus-4-8" → strip ":" → "claude-opus-4-8" → strip "claude-" → "opus-4-8"
		// "m·opus-4-8" = 10 chars → truncated to 9 → "m·opus-4…"
		{"mashura·anthropic:claude-opus-4-8", "m·opus-4…"},
		// "openrouter:google/gemini-2.5-pro" → strip ":" → "google/gemini-2.5-pro" → strip "/" → "gemini-2.5-pro"
		// "m·gemini-2.5-pro" = 16 chars → truncated to 9 → "m·gemini…"
		{"mashura·openrouter:google/gemini-2.5-pro", "m·gemini…"},
	}
	for _, c := range cases {
		got := shortSourceName(c.source)
		if got != c.want {
			t.Errorf("shortSourceName(%q) = %q, want %q", c.source, got, c.want)
		}
	}
}

func TestApproxTokens(t *testing.T) {
	cases := []struct {
		chars int
		want  int64
	}{{0, 0}, {-5, 0}, {1, 1}, {4, 1}, {5, 2}, {8, 2}, {10, 3}}
	for _, c := range cases {
		if got := proxy.ApproxTokens(c.chars); got != c.want {
			t.Errorf("ApproxTokens(%d) = %d, want %d", c.chars, got, c.want)
		}
	}
}
