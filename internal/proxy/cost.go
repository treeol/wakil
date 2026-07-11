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
// Scope (P24): capture + display only, per-session. Two future hooks are
// intentionally NOT implemented here:
//   - budget enforcement: refuse a call once a configured ceiling is crossed.
//   - per-turn alerts: warn when one turn's modeled spend exceeds a threshold.
// Cross-session persistence is also deferred (this tracker dies with the App).

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

// costEntry accumulates one source's calls, tokens, and modeled spend.
type costEntry struct {
	Calls      int
	InputTok   int64
	OutputTok  int64
	CachedTok  int64 // subset of InputTok served from the backend's prompt cache; 0 when never reported
	CacheWriteTok int64 // tokens written to the cache this turn; 0 when never reported
	CostUSD    float64
	Priced     bool   // false → render "—" rather than a fake "$0.00"
	Confidence string // ConfExact | ConfModeled | ConfApprox (weakest seen wins)
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
	if t == nil {
		return
	}
	var d config.TokenDetail
	if len(detail) > 0 {
		d = detail[0]
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.src == nil {
		t.src = map[string]*costEntry{}
	}
	e := t.src[source]
	if e == nil {
		e = &costEntry{}
		t.src[source] = e
	}
	e.Calls++
	e.InputTok += inTok
	e.OutputTok += outTok
	e.CachedTok += d.CachedTok
	e.CacheWriteTok += d.CacheWriteTok
	e.CostUSD += costUSD
	if priced {
		e.Priced = true
	}
	e.Confidence = WeakerConfidence(e.Confidence, confidence)
}

// CostRow is one source's snapshot for rendering.
type CostRow struct {
	Source     string
	Calls      int
	InputTok   int64
	OutputTok  int64
	CachedTok  int64 // subset of InputTok served from the backend's prompt cache; 0 when never reported
	CacheWriteTok int64 // tokens written to the cache; 0 when never reported
	CostUSD    float64
	Priced     bool
	Confidence string
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
		rows = append(rows, CostRow{
			Source:     name,
			Calls:      e.Calls,
			InputTok:   e.InputTok,
			OutputTok:  e.OutputTok,
			CachedTok:  e.CachedTok,
			CacheWriteTok: e.CacheWriteTok,
			CostUSD:    e.CostUSD,
			Priced:     e.Priced,
			Confidence: e.Confidence,
		})
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
		rows = append(rows, CostRow{
			Source:     name,
			Calls:      e.Calls,
			InputTok:   e.InputTok,
			OutputTok:  e.OutputTok,
			CachedTok:  e.CachedTok,
			CacheWriteTok: e.CacheWriteTok,
			CostUSD:    e.CostUSD,
			Priced:     e.Priced,
			Confidence: e.Confidence,
		})
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
