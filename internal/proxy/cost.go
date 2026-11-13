package proxy

import (
	"fmt"
	"sort"
	"sync"

	"github.com/treeol/wakil/internal/config"
)

// Cost-tracking subsystem. Estimates over precision: modeled numbers are tagged
// with a confidence tier so a compute-cost guess never reads as a billed amount.
//
// Scope: capture + display, per-session. Budget enforcement is implemented in
// internal/agent/cost_state.go (checkBudgetExhausted, BudgetUSD) and
// internal/agent/turn_phases.go. Cross-session persistence is still deferred
// (this tracker dies with the App).

// Cost sources. Adding a source is one constant plus one [costs] config entry —
// the tracker map and the sidebar block adapt automatically.
const (
	// CostSourceMashura and CostSourceInference are the legacy aggregate keys
	// used when no per-model or per-backend splitting is available (e.g. tests,
	// subagents). Live sessions use the prefixed per-entity keys below.
	CostSourceMashura   = "mashura"   // oracle aggregate (kept for compat)
	CostSourceInference = "inference" // inference aggregate (used when no backend routing)
	CostSourceSearch    = "search"    // web/search queries

	// CostSourceMashuraPrefix is the prefix for per-model mashura rows:
	// "mashura·<model-id>". Exact confidence; priced from cfg.Costs.Mashura.
	CostSourceMashuraPrefix = "mashura·"

	// CostSourceInfPrefix is the prefix for per-backend inference rows:
	// "inference·<backend>" (local, modeled) or "inference·<backend>/<model>"
	// (external, exact). Priced from cfg.Costs.Inference or InferenceBackends.
	CostSourceInfPrefix = "inference·"
)

// Confidence tiers, ordered most-trustworthy first. The glyph keeps a modeled or
// approximate figure visually distinct from an exact (billed-grade) one.
const (
	ConfExact   = "exact"   // real provider usage × real provider pricing
	ConfModeled = "modeled" // real-ish token counts × a configured proxy rate
	ConfApprox  = "approx"  // counts estimated (e.g. from output length)
)

// Cost categories for the per-source split (B5).
const (
	CostCatMain     = "main"
	CostCatSubagent = "subagent"
	CostCatMashura  = "mashura"
	CostCatAux      = "aux"
)

// CategoryForSource infers a category from a legacy source key.
func CategoryForSource(source string) string {
	switch {
	case source == CostSourceMashura || len(source) >= len(CostSourceMashuraPrefix) && source[:len(CostSourceMashuraPrefix)] == CostSourceMashuraPrefix:
		return CostCatMashura
	default:
		return CostCatMain
	}
}

// costEntry accumulates one source's calls, tokens, and modeled spend.
type costEntry struct {
	Calls          int
	InputTok       int64
	OutputTok      int64
	CachedTok      int64 // subset of InputTok served from the backend's prompt cache; 0 when never reported
	CacheWriteTok  int64 // tokens written to the cache this turn; 0 when never reported
	CostUSD        float64
	ProviderUSD    float64
	ProviderPriced bool
	EstimatedUSD   float64
	// CatUSD is the priced USD per category. A single source key can receive
	// records from several categories (e.g. main and subagent inference share
	// the inference source), so the split must be kept per record, not per key.
	CatUSD     map[string]float64
	Category   string
	Priced     bool   // false → render "—" rather than a fake "$0.00"
	Confidence string // ConfExact | ConfModeled | ConfApprox (weakest seen wins)
	// UnpricedCalls counts calls with neither a provider-reported cost nor a
	// price entry. They add tokens and a call, but no dollars: an unpriced call
	// must never read as a free one.
	UnpricedCalls int
}

// CostTracker accumulates per-source cost estimates for one session. It lives on
// App, written by the agent goroutine and read by the TUI render loop, so every
// method is mutex-guarded. All methods are nil-safe: a nil tracker (subagents,
// headless runs, tests) silently no-ops, so callers need no guard.
type CostTracker struct {
	mu  sync.Mutex
	src map[string]*costEntry
}

func NewCostTracker() *CostTracker {
	return &CostTracker{src: map[string]*costEntry{}}
}

// Record adds one call's tokens and modeled cost to a source. priced=false means
// the source has no configured rate; its displayed cost stays "—". The displayed
// confidence escalates toward the least-certain value seen (exact→modeled→approx)
// so one estimated call taints the whole source — a number never overstates.
//
// detail carries cache-read and cache-write token counts, purely additive
// bookkeeping alongside the cost arithmetic the caller already computed.
func (t *CostTracker) Record(source string, inTok, outTok int64, costUSD float64, priced bool, confidence string, detail ...config.TokenDetail) {
	t.RecordCategorized(source, CategoryForSource(source), inTok, outTok, costUSD, priced, confidence, detail...)
}

// RecordCategorized is Record with an explicit category (main/subagent/mashura/aux).
// Provider-reported (exact+priced) USD is tracked separately from estimated.
func (t *CostTracker) RecordCategorized(source, category string, inTok, outTok int64, costUSD float64, priced bool, confidence string, detail ...config.TokenDetail) {
	if t == nil {
		return
	}
	var d config.TokenDetail
	if len(detail) > 0 {
		d = detail[0]
	}
	if category == "" {
		category = CategoryForSource(source)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.src == nil {
		t.src = map[string]*costEntry{}
	}
	e := t.src[source]
	if e == nil {
		e = &costEntry{Category: category}
		t.src[source] = e
	}
	// If same source seen with different category, keep first non-main? Prefer aux/mashura/subagent over main.
	if e.Category == CostCatMain && category != CostCatMain {
		e.Category = category
	}
	if e.Category == "" {
		e.Category = category
	}
	e.Calls++
	e.InputTok += inTok
	e.OutputTok += outTok
	e.CachedTok += d.CachedTok
	e.CacheWriteTok += d.CacheWriteTok
	e.CostUSD += costUSD
	if !priced {
		e.UnpricedCalls++
	}
	if priced {
		e.Priced = true
		if confidence == ConfExact {
			e.ProviderUSD += costUSD
			e.ProviderPriced = true
		} else {
			e.EstimatedUSD += costUSD
		}
		if e.CatUSD == nil {
			e.CatUSD = map[string]float64{}
		}
		e.CatUSD[category] += costUSD
	}
	e.Confidence = WeakerConfidence(e.Confidence, confidence)
}

// RecordRow merges a full CostRow preserving category/provider split (fold/restore).
func (t *CostTracker) RecordRow(r CostRow, categoryOverride string) {
	if t == nil {
		return
	}
	cat := categoryOverride
	if cat == "" {
		cat = r.Category
	}
	if cat == "" {
		cat = CategoryForSource(r.Source)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.src == nil {
		t.src = map[string]*costEntry{}
	}
	e := t.src[r.Source]
	if e == nil {
		e = &costEntry{Category: cat}
		t.src[r.Source] = e
	}
	if e.Category == CostCatMain && cat != CostCatMain {
		e.Category = cat
	}
	e.Calls += r.Calls
	e.InputTok += r.InputTok
	e.OutputTok += r.OutputTok
	e.CachedTok += r.CachedTok
	e.CacheWriteTok += r.CacheWriteTok
	e.CostUSD += r.CostUSD
	e.ProviderUSD += r.ProviderUSD
	if r.ProviderPriced {
		e.ProviderPriced = true
	}
	e.EstimatedUSD += r.EstimatedUSD
	e.UnpricedCalls += r.UnpricedCalls
	if r.Priced {
		e.Priced = true
	}
	mergeCatUSD(e, r.CatUSD, cat, r.CostUSD)
	e.Confidence = WeakerConfidence(e.Confidence, r.Confidence)
	if e.Confidence == "" && r.Confidence != "" {
		e.Confidence = r.Confidence
	}
}

// CostRow is one source's snapshot for rendering.
type CostRow struct {
	Source         string
	Category       string
	Calls          int
	InputTok       int64
	OutputTok      int64
	CachedTok      int64 // subset of InputTok served from the backend's prompt cache; 0 when never reported
	CacheWriteTok  int64 // tokens written to the cache; 0 when never reported
	CostUSD        float64
	ProviderUSD    float64
	ProviderPriced bool
	EstimatedUSD   float64
	// CatUSD is priced USD per category for this source (see costEntry.CatUSD).
	CatUSD     map[string]float64
	Priced     bool
	Confidence string
	// UnpricedCalls is the number of calls in this row that had no cost.
	UnpricedCalls int
}

// mergeCatUSD adds usd to the category bucket on e. A row restored or folded
// without a per-category map is attributed wholly to fallbackCat.
func mergeCatUSD(e *costEntry, src map[string]float64, fallbackCat string, usd float64) {
	if len(src) == 0 {
		if usd == 0 {
			return
		}
		if e.CatUSD == nil {
			e.CatUSD = map[string]float64{}
		}
		e.CatUSD[fallbackCat] += usd
		return
	}
	if e.CatUSD == nil {
		e.CatUSD = map[string]float64{}
	}
	for c, v := range src {
		e.CatUSD[c] += v
	}
}

// rowFromEntry converts an entry to a row.
func rowFromEntry(name string, e *costEntry) CostRow {
	cat := e.Category
	if cat == "" {
		cat = CategoryForSource(name)
	}
	prov, est := e.ProviderUSD, e.EstimatedUSD
	// Backfill for rows recorded before split tracking (exact+priced legacy).
	if prov == 0 && est == 0 && e.Priced && e.CostUSD != 0 {
		if e.Confidence == ConfExact {
			prov = e.CostUSD
		} else {
			est = e.CostUSD
		}
	}
	var cats map[string]float64
	if len(e.CatUSD) > 0 {
		cats = make(map[string]float64, len(e.CatUSD))
		for c, v := range e.CatUSD {
			cats[c] = v
		}
	}
	return CostRow{Source: name, Category: cat, Calls: e.Calls, InputTok: e.InputTok, OutputTok: e.OutputTok, CachedTok: e.CachedTok, CacheWriteTok: e.CacheWriteTok, CostUSD: e.CostUSD, ProviderUSD: prov, ProviderPriced: e.ProviderPriced || (e.Confidence == ConfExact && e.Priced), EstimatedUSD: est, CatUSD: cats, Priced: e.Priced, Confidence: e.Confidence, UnpricedCalls: e.UnpricedCalls}
}

// CategoryTotals aggregates priced USD by category and provider vs estimated.
type CategoryTotals struct {
	Main, Subagent, Mashura, Aux float64
	Provider, Estimated          float64
	Billed, Total                float64
	InputTok, CachedTok          int64
	CacheShare                   float64
	// Unpriced counts calls across all sources that had no cost. They are
	// excluded from every dollar figure above and shown separately.
	Unpriced int
}

// SnapshotDetail returns category/provider totals plus rows.
func (t *CostTracker) SnapshotDetail() (CategoryTotals, []CostRow) {
	var ct CategoryTotals
	if t == nil {
		return ct, nil
	}
	_, rows := t.Snapshot()
	for _, r := range rows {
		ct.Unpriced += r.UnpricedCalls
		if !r.Priced {
			continue
		}
		ct.Total += r.CostUSD
		ct.Provider += r.ProviderUSD
		ct.Estimated += r.EstimatedUSD
		if r.Confidence == ConfExact {
			ct.Billed += r.CostUSD
		}
		// Split by the per-record category buckets. A row without buckets
		// (legacy or restored without them) is attributed to its own category.
		cats := r.CatUSD
		if len(cats) == 0 {
			cats = map[string]float64{r.Category: r.CostUSD}
		}
		for c, v := range cats {
			switch c {
			case CostCatSubagent:
				ct.Subagent += v
			case CostCatMashura:
				ct.Mashura += v
			case CostCatAux:
				ct.Aux += v
			default:
				ct.Main += v
			}
		}
	}
	// Cache share across all rows (including unpriced).
	_, all := t.Snapshot()
	for _, r := range all {
		ct.InputTok += r.InputTok
		ct.CachedTok += r.CachedTok
	}
	if ct.InputTok > 0 {
		ct.CacheShare = float64(ct.CachedTok) / float64(ct.InputTok)
	}
	return ct, rows
}

// ExportRows returns rows for session persistence.
func (t *CostTracker) ExportRows() []CostRow {
	if t == nil {
		return nil
	}
	_, rows := t.Snapshot()
	return rows
}

// RestoreRows merges rows into the tracker: rows with matching keys are
// added (calls, tokens, cost), new keys are inserted. Used on resume; empty
// = no-op. Restoring the same rows twice double-counts — callers must pass
// each row's contribution once.
func (t *CostTracker) RestoreRows(rows []CostRow) {
	if t == nil || len(rows) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.src == nil {
		t.src = map[string]*costEntry{}
	}
	for _, r := range rows {
		e := &costEntry{Calls: r.Calls, InputTok: r.InputTok, OutputTok: r.OutputTok, CachedTok: r.CachedTok, CacheWriteTok: r.CacheWriteTok, CostUSD: r.CostUSD, ProviderUSD: r.ProviderUSD, ProviderPriced: r.ProviderPriced, EstimatedUSD: r.EstimatedUSD, Category: r.Category, Priced: r.Priced, Confidence: r.Confidence, UnpricedCalls: r.UnpricedCalls}
		if e.Category == "" {
			e.Category = CategoryForSource(r.Source)
		}
		e.CatUSD = nil
		if r.Priced {
			mergeCatUSD(e, r.CatUSD, e.Category, r.CostUSD)
		}
		if existing := t.src[r.Source]; existing != nil {
			mergeCatUSD(existing, e.CatUSD, e.Category, 0)
			// Merge to avoid double-count on repeated restore.
			existing.Calls += e.Calls
			existing.InputTok += e.InputTok
			existing.OutputTok += e.OutputTok
			existing.CachedTok += e.CachedTok
			existing.CacheWriteTok += e.CacheWriteTok
			existing.CostUSD += e.CostUSD
			existing.ProviderUSD += e.ProviderUSD
			existing.EstimatedUSD += e.EstimatedUSD
			existing.UnpricedCalls += e.UnpricedCalls
			if e.Priced {
				existing.Priced = true
			}
			if e.ProviderPriced {
				existing.ProviderPriced = true
			}
			existing.Confidence = WeakerConfidence(existing.Confidence, e.Confidence)
		} else {
			t.src[r.Source] = e
		}
	}
}

// Snapshot returns the session total (sum of priced sources only — an unpriced
// source contributes calls but no dollars) and the per-source rows sorted by
// cost descending, ties broken by source name for a stable order.
func (t *CostTracker) Snapshot() (total float64, rows []CostRow) {
	if t == nil {
		return 0, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for name, e := range t.src {
		rows = append(rows, rowFromEntry(name, e))
		if e.Priced {
			total += e.CostUSD
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CostUSD != rows[j].CostUSD {
			return rows[i].CostUSD > rows[j].CostUSD
		}
		return rows[i].Source < rows[j].Source
	})
	return total, rows
}

// SnapshotSplit returns billing-categorized totals and the full per-source row
// set. billedTotal sums exact+priced rows (real charges you owe an external
// provider); estimatedTotal sums modeled/approx+priced rows (compute-cost
// estimates that must not be conflated with billed amounts). anyBilled and
// anyEstimated flag whether each category has any rows at all — including
// unpriced ones — so the sidebar can show a subtotal line even when cost is "—".
// Rows are sorted cost-descending (ties by source name) like Snapshot.
func (t *CostTracker) SnapshotSplit() (billedTotal, estimatedTotal float64, anyBilled, anyEstimated bool, rows []CostRow) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for name, e := range t.src {
		rows = append(rows, rowFromEntry(name, e))
		if e.Confidence == ConfExact {
			anyBilled = true
			if e.Priced {
				billedTotal += e.CostUSD
			}
		} else {
			anyEstimated = true
			if e.Priced {
				estimatedTotal += e.CostUSD
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CostUSD != rows[j].CostUSD {
			return rows[i].CostUSD > rows[j].CostUSD
		}
		return rows[i].Source < rows[j].Source
	})
	return
}

// confidenceRank orders tiers from most certain (0) to least (2). Unknown values
// are treated as modeled so a stray tag never masquerades as exact.
func confidenceRank(c string) int {
	switch c {
	case ConfExact:
		return 0
	case ConfApprox:
		return 2
	default: // ConfModeled and unknown
		return 1
	}
}

// WeakerConfidence returns the less-certain of two tiers (empty = no opinion).
func WeakerConfidence(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	case confidenceRank(b) > confidenceRank(a):
		return b
	default:
		return a
	}
}

// ApproxTokens estimates a token count from a character count at ~4 chars per
// token — the fallback when the proxy reports no usage chunk.
func ApproxTokens(chars int) int64 {
	if chars <= 0 {
		return 0
	}
	return int64((chars + 3) / 4)
}

// CostCell renders a row's cost: "—" when unpriced (never "$0.00"), else a
// compact dollar figure that stays within the narrow sidebar column.
func CostCell(r CostRow) string {
	if !r.Priced {
		return "—"
	}
	return FmtUSDCompact(r.CostUSD)
}

// FmtUSDCompact keeps a dollar amount to at most 5 visible chars so it fits the
// sidebar: cents below $10, one decimal below $100, whole dollars above.
func FmtUSDCompact(v float64) string {
	switch {
	case v >= 100:
		return fmt.Sprintf("$%.0f", v)
	case v >= 10:
		return fmt.Sprintf("$%.1f", v)
	default:
		return fmt.Sprintf("$%.2f", v)
	}
}

// FmtUSDSplit formats the per-source split and the billed totals: four decimals
// below $1 so small subagent and Mashūra costs stay visible ($0.0034), two
// decimals from $1 up. Other sidebar cells keep FmtUSDCompact.
func FmtUSDSplit(v float64) string {
	// Decide the precision on the value as it will be printed, so a value that
	// rounds up to $1.0000 uses the two-decimal form instead.
	if s := fmt.Sprintf("$%.4f", v); s != "$1.0000" && v < 1 {
		return s
	}
	if v < 1 {
		return "$1.00"
	}
	return fmt.Sprintf("$%.2f", v)
}

// CostGlyphStyle is the glyph and 256-colour code for a confidence tier. A solid
// dot marks exact (billed-grade); hollow dots mark modeled (amber) and approx
// (dim grey) so neither can be mistaken for a billed figure.
func CostGlyphStyle(conf string) (glyph, color string) {
	switch conf {
	case ConfExact:
		return "●", "2" // solid green
	case ConfApprox:
		return "○", "240" // hollow, dim grey
	default: // ConfModeled
		return "○", "214" // hollow amber
	}
}
