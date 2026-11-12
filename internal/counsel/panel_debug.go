package counsel

import (
	"context"
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
// or the API key: only the request body as sent, and what came back.
type panelCapture struct {
	dir    string
	label  string
	body   []byte
	usages []string // raw JSON of every data line that contains "usage", in order
	tail   []string // the last panelDebugTail raw data lines (any content)
	served string
	status int
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
// It is safe to call on a nil capture.
func (c *panelCapture) observe(raw string) {
	if c == nil {
		return
	}
	if strings.Contains(raw, `"usage"`) {
		c.usages = append(c.usages, raw)
	}
	c.tail = append(c.tail, raw)
	if len(c.tail) > panelDebugTail {
		c.tail = c.tail[len(c.tail)-panelDebugTail:]
	}
}

// panelCaptureKey carries the capture through the request context, so
// streamRequest's parser hook reaches the capture of its own call only.
type panelCaptureKey struct{}

func withPanelCapture(ctx context.Context, c *panelCapture) context.Context {
	if c == nil {
		return ctx
	}
	return context.WithValue(ctx, panelCaptureKey{}, c)
}

func panelCaptureFrom(ctx context.Context) *panelCapture {
	c, _ := ctx.Value(panelCaptureKey{}).(*panelCapture)
	return c
}

// write stores the capture as one 0600 file named
// panel-<label>-NNNNNN-<timestamp>.json. Failures log one warning per
// directory and never fail the call.
func (c *panelCapture) write(status int, served string, err error) {
	if c == nil {
		return
	}
	c.status = status
	c.served = served
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
	if json.Valid(c.body) {
		doc.RequestBody = json.RawMessage(c.body)
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
