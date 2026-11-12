package counsel

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/treeol/wakil/internal/diag"
)

// panelDebugEnvVar is the directory that receives one panel-debug file per
// Mashūra/counsel call. It is the same variable as the main-loop request dump
// (WAKIL_DEBUG_REQUESTS). Unset means no capture and no cost.
const panelDebugEnvVar = "WAKIL_DEBUG_REQUESTS"

// panelDebugTail is how many raw stream lines before [DONE] the capture keeps,
// in addition to every line that carries a "usage" object.
const panelDebugTail = 3

// panelDebugCounter numbers capture files across the process.
var panelDebugCounter atomic.Int64

// panelDebugWarned logs one warning per directory, not one per call.
var panelDebugWarned sync.Map

// panelCapture gathers the raw facts of one panel call. It never holds headers
// or the API key. The request body is replaced after Exec with the body as
// actually sent (the data policy may have added the provider ZDR fields, or
// the auto fallback may have stripped them) via withSentBody; the capture's
// initial body is only what orChat built pre-policy.
type panelCapture struct {
	dir    string
	label  string
	body   []byte // pre-policy body; replaced by sentBody on write
	sentBody []byte // the body as sent, recorded via observeSent
	usages []string // raw JSON of every data line that contains "usage", in order
	tail   []string // the last panelDebugTail raw data lines (any content)
	served string
	status int
}

// withSentBody replaces the capture's request body with the body as sent
// (i.e. after the data policy ran). Called by orChat from the wire body
// Exec actually transmitted.
func (c *panelCapture) withSentBody(b []byte) *panelCapture {
	if c == nil {
		return nil
	}
	c.sentBody = append([]byte(nil), b...)
	return c
}

// newPanelCapture returns a capture for one call, or nil when the debug
// variable is unset. The directory is created 0700 if missing.
func newPanelCapture(label string, body []byte) *panelCapture {
	dir := strings.TrimSpace(os.Getenv(panelDebugEnvVar))
	if dir == "" {
		return nil
	}
	return &panelCapture{dir: dir, label: label, body: append([]byte(nil), body...)}
}

// observe records one raw SSE data line (already joined, before parsing).
// It is safe to call on a nil capture. The served model is taken from the
// stream's model field (the provider reports it on every chunk).
func (c *panelCapture) observe(raw string) {
	if c == nil {
		return
	}
	if strings.Contains(raw, `"usage"`) {
		c.usages = append(c.usages, raw)
	}
	var model struct {
		Model string `json:"model"`
	}
	if json.Unmarshal([]byte(raw), &model) == nil && model.Model != "" {
		c.served = model.Model
	}
	c.tail = append(c.tail, raw)
	if len(c.tail) > panelDebugTail {
		c.tail = c.tail[len(c.tail)-panelDebugTail:]
	}
}

// write stores the capture as one 0600 file named
// panel-<label>-NNNNNN-<timestamp>.json. Failures log one warning per
// directory and never fail the call.
func (c *panelCapture) write(status int, served string, err error) {
	if c == nil {
		return
	}
	c.status = status
	if served != "" {
		c.served = served
	}
	served = c.served
	n := panelDebugCounter.Add(1)
	doc := struct {
		Label       string          `json:"label"`
		Served      string          `json:"served_model"`
		HTTPStatus  int             `json:"http_status"`
		Error       string          `json:"error,omitempty"`
		RequestBody json.RawMessage `json:"request_body"`
		UsageSeen   []string        `json:"usage_objects"`
		LastLines   []string        `json:"last_lines_before_done"`
	}{
		Label:      c.label,
		Served:     served,
		HTTPStatus: status,
		UsageSeen:  c.usages,
		LastLines:  c.tail,
	}
	if err != nil {
		doc.Error = err.Error()
	}
	wireBody := c.body
	if len(c.sentBody) > 0 {
		wireBody = c.sentBody
	}
	if json.Valid(wireBody) {
		doc.RequestBody = json.RawMessage(wireBody)
	} else {
		doc.RequestBody = json.RawMessage(`"<non-json body>"`)
	}
	raw, merr := json.MarshalIndent(doc, "", "  ")
	if merr != nil {
		panelDebugWarn(c.dir, "marshal: %v", merr)
		return
	}
	if merr := os.MkdirAll(c.dir, 0o700); merr != nil {
		panelDebugWarn(c.dir, "mkdir: %v", merr)
		return
	}
	name := fmt.Sprintf("panel-%s-%06d-%s.json", safeLabel(c.label), n, time.Now().UTC().Format("20060102T150405.000000000"))
	path := filepath.Join(c.dir, name)
	f, ferr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if ferr != nil {
		panelDebugWarn(c.dir, "create: %v", ferr)
		return
	}
	_, werr := f.Write(append(raw, '\n'))
	cerr := f.Close()
	if werr != nil || cerr != nil {
		panelDebugWarn(c.dir, "write: %v", firstErr(werr, cerr))
	}
}

// writeVoidCapture stores one VOID member's diagnostics as one 0600 file
// (no request body, no headers/key). It no-ops when the debug variable is
// unset or when the result is not VOID. Failures log one warning per
// directory and never fail the call.
func writeVoidCapture(res PanelMemberResult) {
	if !res.Void {
		return
	}
	dir := strings.TrimSpace(os.Getenv(panelDebugEnvVar))
	if dir == "" {
		return
	}
	n := panelDebugCounter.Add(1)
	doc := struct {
		Label                string  `json:"label"`
		Cause                string  `json:"cause"`
		FinishReason         string  `json:"finish_reason"`
		ReasoningTokens      int64   `json:"reasoning_tokens"`
		AnswerTokens         int64   `json:"answer_tokens"`
		Attempts             int     `json:"attempts"`
		CostUSD              float64 `json:"cost_usd"`
		HasProviderCost      bool    `json:"has_provider_cost"`
		FinalTextLength      int     `json:"final_text_length"`
		CoverageNotConfirmed bool    `json:"coverage_not_confirmed"`
		BudgetStop           string  `json:"budget_stop"`
		ServedModel          string  `json:"served_model"`
	}{
		Label:           res.PrefixedModel,
		CostUSD:         res.Usage.ProviderCostUSD,
		HasProviderCost: res.Usage.HasProviderCost,
		FinalTextLength: len(res.Answer),
		ServedModel:     res.Usage.ServedModel,
	}
	doc.Attempts = 1
	doc.ReasoningTokens = res.Usage.ReasoningTokens
	doc.AnswerTokens = max(res.Usage.OutputTokens-res.Usage.ReasoningTokens, 0)
	doc.Cause, doc.FinishReason = "unknown", "unknown"
	if res.Log != nil {
		if res.Log.VoidCause != "" {
			doc.Cause = res.Log.VoidCause
		}
		if res.Log.VoidFinish != "" {
			doc.FinishReason = res.Log.VoidFinish
		}
		if res.Log.VoidReasoning != 0 {
			doc.ReasoningTokens = res.Log.VoidReasoning
		}
		if res.Log.VoidAnswer != 0 {
			doc.AnswerTokens = res.Log.VoidAnswer
		}
		if res.Log.VoidAttempts > 0 {
			doc.Attempts = res.Log.VoidAttempts
		}
		doc.CoverageNotConfirmed = res.Log.CoverageNotConfirmed
		doc.BudgetStop = res.Log.BudgetStop
	}
	raw, merr := json.MarshalIndent(doc, "", "  ")
	if merr != nil {
		panelDebugWarn(dir, "void marshal: %v", merr)
		return
	}
	if merr := os.MkdirAll(dir, 0o700); merr != nil {
		panelDebugWarn(dir, "void mkdir: %v", merr)
		return
	}
	name := fmt.Sprintf("panel-void-%s-%06d-%s.json", safeLabel(res.PrefixedModel), n, time.Now().UTC().Format("20060102T150405.000000000"))
	path := filepath.Join(dir, name)
	f, ferr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if ferr != nil {
		panelDebugWarn(dir, "void create: %v", ferr)
		return
	}
	_, werr := f.Write(append(raw, '\n'))
	cerr := f.Close()
	if werr != nil || cerr != nil {
		panelDebugWarn(dir, "void write: %v", firstErr(werr, cerr))
	}
}

func panelDebugWarn(dir, format string, args ...any) {
	if _, loaded := panelDebugWarned.LoadOrStore(dir, true); loaded {
		return
	}
	diag.Printf("wakil: panel debug capture disabled for %s: "+format+"\n", append([]any{dir}, args...)...)
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// safeLabel keeps a file-name part to letters, digits, dash and underscore.
func safeLabel(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "call"
	}
	return b.String()
}
