package wiring

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/sandbox/hisn"
	"github.com/treeol/wakil/internal/sandbox/project"
	"github.com/treeol/wakil/internal/timeline"
	"golang.org/x/term"
)

// loadProjectSurface reads <workspace>/.wakil/sandbox.json and returns the
// surface to start hisn with. A file that asks for anything is honoured ONLY
// when the user approved exactly this content (see package project): an agent
// that wrote it cannot approve it. The file itself is masked inside the sandbox
// when it exists (defence in depth; the executor refuses writes to it too).
func loadProjectSurface(cfg config.Config) (hisn.Surface, project.Loaded, error) {
	ws := cfg.HostWorkDir
	endLoad := timeline.Accum("project file load + sha256 (.wakil/sandbox.json)")
	l, err := project.Load(ws)
	endLoad()
	if err != nil {
		return hisn.Surface{}, l, err
	}
	if l.Needs() {
		endAppr := timeline.Accum("project file approval check")
		key := agent.WorkspaceKey(cfg.WorkspacePath())
		approved := project.Approved(filepath.Join(hisnStateDir(), "hisn"), key, l.Hash)
		endAppr()
		if !approved {
			return hisn.Surface{}, l, &project.ErrNotApproved{Path: l.Path}
		}
	}
	s := l.Surface
	if l.Present && s.Profile == "" {
		s.Mask = []string{l.Path}
	}
	return s, l, nil
}

// SandboxTrust shows the project's sandbox file and, after an interactive
// "yes" read from the controlling terminal, records its approval. There is
// deliberately no flag that skips the question and no non-terminal path: the
// approval is the user's decision, and a process an agent can drive must not
// be able to make it.
func SandboxTrust(out io.Writer, dir string, openTTY func() (io.ReadCloser, error)) error {
	if dir == "" {
		dir = "."
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	ws, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return fmt.Errorf("workspace %q: %w", dir, err)
	}
	l, err := project.Load(ws)
	if err != nil {
		return err
	}
	if !l.Present || bootstrapEligible(l.Raw) {
		cfg, err := loadTrustConfig(ws)
		if err != nil {
			return err
		}
		return trustBootstrap(out, cfg, l, openTTY, func(c config.Config, sf hisn.Surface) (hisn.Risk, error) {
			return productionRisk(c, sf)
		})
	}
	if !l.Needs() {
		fmt.Fprintf(out, "%s needs no approval (absent, empty or asks for nothing).\n", project.RelPath)
		return nil
	}
	key := agent.WorkspaceKey(ws)
	if key == "" {
		return fmt.Errorf("cannot derive a workspace key for %s", ws)
	}
	stateDir := filepath.Join(hisnStateDir(), "hisn")
	if project.Approved(stateDir, key, l.Hash) {
		fmt.Fprintf(out, "%s is already approved (sha256 %s).\n", project.RelPath, l.Hash[:16])
		return nil
	}
	fmt.Fprintf(out, "%s in %s\n\n%s\n\nIt asks for:\n", project.RelPath, ws, strings.TrimSpace(string(l.Raw)))
	for _, line := range project.Summary(l) {
		fmt.Fprintf(out, "  - %s\n", line)
	}
	fmt.Fprintf(out, "\nsha256 %s\nApprove this exact content for this workspace? Type 'yes' to approve: ", l.Hash)
	tty, err := openTTY()
	if err != nil {
		fmt.Fprintln(out)
		return fmt.Errorf("approval needs an interactive terminal (/dev/tty): %w", err)
	}
	defer tty.Close()
	line, _ := bufio.NewReader(tty).ReadString('\n')
	if strings.TrimSpace(line) != "yes" {
		fmt.Fprintln(out, "not approved.")
		return fmt.Errorf("not approved")
	}
	if err := project.Approve(stateDir, key, l.Hash); err != nil {
		return err
	}
	fmt.Fprintln(out, "approved.")
	return nil
}

// ackBootstrapForm is the only file content Wakil rewrites on its own: the
// file the bootstrap wrote, {"hisn":{"risk_ack":"<label>"}} (whitespace
// insensitive). Anything else is hand-written and is never re-marshalled.
type ackBootstrapForm struct {
	Hisn struct {
		RiskAck *string `json:"risk_ack"`
	} `json:"hisn"`
}

// bootstrapContent returns the bootstrap bytes for ack, and whether raw (when
// present) is exactly the bootstrap form, i.e. safe for Wakil to rewrite.
func bootstrapFormOK(raw []byte) (current string, ok bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var f ackBootstrapForm
	if err := dec.Decode(&f); err != nil || f.Hisn.RiskAck == nil {
		return "", false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "", false
	}
	// The object must hold exactly one key ("hisn") with exactly one key ("risk_ack").
	var shape struct {
		Hisn map[string]json.RawMessage `json:"hisn"`
	}
	if json.Unmarshal(raw, &shape) != nil || len(shape.Hisn) != 1 {
		return "", false
	}
	return *f.Hisn.RiskAck, true
}

// bootstrapBytes is the content written for an ack.
func bootstrapBytes(ack string) []byte {
	b, _ := json.Marshal(map[string]map[string]string{"hisn": {"risk_ack": ack}})
	return append(b, '\n')
}

// riskFunc computes the generated profile's risk for a config (injected so
// tests need no hisn). It is hisn.PlannedRisk in production.
type riskFunc func(cfg config.Config, surface hisn.Surface) (hisn.Risk, error)

// fileAck returns hisn.risk_ack of a file that already parsed (project.Load is
// strict and refuses anything else first), or "" when absent.
func fileAck(raw []byte) string {
	f, err := project.Parse(raw)
	if err != nil {
		return ""
	}
	return f.Hisn.RiskAck
}

// bootstrapEligible reports whether an existing file is one Wakil may rewrite:
// exactly the bootstrap form. A hand-written file is never rewritten.
func bootstrapEligible(raw []byte) bool {
	_, ok := bootstrapFormOK(raw)
	return ok
}

// loadTrustConfig returns the config startup would use in ws: the real global
// configuration (LoadConfig with ws as the working directory, exactly as
// `wakil run` run from that repo resolves it), so the trust-time risk is the
// startup risk. The working directory is restored before returning.
func loadTrustConfig(ws string) (config.Config, error) {
	prev, err := os.Getwd()
	if err != nil {
		return config.Config{}, err
	}
	if err := os.Chdir(ws); err != nil {
		return config.Config{}, err
	}
	defer func() { _ = os.Chdir(prev) }()
	cfg, err := config.LoadConfig(nil)
	if err != nil {
		return config.Config{}, err
	}
	return cfg, nil
}

// productionRisk is the real plan-time risk: hisn.PlannedRisk over the same
// options startup builds (hisnOptions), so the labels match what startup
// would refuse without an ack.
var productionRiskFn = func(cfg config.Config) (hisn.Risk, error) {
	surface, _, err := loadProjectSurface(cfg)
	if err != nil {
		return hisn.Risk{}, err
	}
	return productionRisk(cfg, surface)
}

// productionRisk is the plan-time risk of cfg with the given project surface.
func productionRisk(cfg config.Config, surface hisn.Surface) (hisn.Risk, error) {
	opts, err := hisnOptionsWith(cfg, surface)
	if err != nil {
		return hisn.Risk{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return hisn.PlannedRisk(ctx, opts)
}

// trustBootstrap is the no-file / bootstrap-form path of `wakil sandbox trust`.
// It shows the labels and the risk, asks for an explicit yes, and only then
// writes the ack and approves it. Nothing is written before the answer, so any
// other answer leaves the workspace untouched.
// A bootstrap-form file whose ack already matches the risk needs nothing.
func trustBootstrap(out io.Writer, cfg config.Config, l project.Loaded, openTTY func() (io.ReadCloser, error), risk riskFunc) error {
	r, err := risk(cfg, l.Surface)
	if err != nil {
		return err
	}
	if r.Ack == "none" {
		fmt.Fprintf(out, "%s needs no approval: the profile's risk is %q and needs no acknowledgement.\n", project.RelPath, r.Label)
		return nil
	}
	if l.Present {
		cur, _ := bootstrapFormOK(l.Raw)
		if cur == r.Ack && approvedNow(cfg, l) {
			fmt.Fprintf(out, "%s already acknowledges %q and is approved.\n", project.RelPath, r.Ack)
			return nil
		}
	}
	printLabels(out, r)
	path := l.Path
	if l.Present {
		// A hand-written file is never rewritten: say which key to change.
		if !bootstrapEligible(l.Raw) {
			fmt.Fprintf(out, "\n%s is hand-written, so Wakil will not rewrite it.\n", project.RelPath)
			fmt.Fprintf(out, "Set \"hisn\": {\"risk_ack\": %q} in it yourself, then run `wakil sandbox trust` again.\n", r.Ack)
			return fmt.Errorf("%s is not in the bootstrap form; change hisn.risk_ack to %q yourself", project.RelPath, r.Ack)
		}
		cur, _ := bootstrapFormOK(l.Raw)
		fmt.Fprintf(out, "\nthe acknowledgement in %s is %q; the profile now needs %q.\n", project.RelPath, cur, r.Ack)
	} else {
		fmt.Fprintf(out, "\n%s does not exist yet.\n", project.RelPath)
	}
	fmt.Fprint(out, "Approve this exact content for this workspace? Type 'yes': ")
	tty, err := openTTY()
	if err != nil {
		fmt.Fprintln(out)
		return fmt.Errorf("approval needs an interactive terminal (/dev/tty): %w", err)
	}
	defer tty.Close()
	line, _ := bufio.NewReader(tty).ReadString('\n')
	if strings.TrimSpace(line) != "yes" {
		fmt.Fprintln(out, "not approved; nothing was written.")
		return fmt.Errorf("not approved")
	}
	content := bootstrapBytes(r.Ack)
	if err := writeAndApprove(out, cfg, path, content, l); err != nil {
		return err
	}
	return nil
}

// approvedNow reports whether the loaded file is already approved for this
// workspace under the key startup checks (WorkspacePath, as loadProjectSurface).
func approvedNow(cfg config.Config, l project.Loaded) bool {
	key := agent.WorkspaceKey(cfg.WorkspacePath())
	return project.Approved(filepath.Join(hisnStateDir(), "hisn"), key, l.Hash)
}

// writeAndApprove writes content (only when it differs from what is on disk),
// then approves EXACTLY those bytes: the approval hash is sha256(content), and
// the reloaded file must hash to it, or the write is refused and nothing is
// approved. The approval key is WorkspacePath, the one startup checks.
func writeAndApprove(out io.Writer, cfg config.Config, path string, content []byte, prior project.Loaded) error {
	if !prior.Present || !bytes.Equal(prior.Raw, content) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			return err
		}
	}
	got, err := project.Load(cfg.HostWorkDir)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(content)
	if got.Hash != hex.EncodeToString(sum[:]) {
		return fmt.Errorf("%s changed while it was being written; nothing was approved — run `wakil sandbox trust` again", project.RelPath)
	}
	key := agent.WorkspaceKey(cfg.WorkspacePath())
	if key == "" {
		return fmt.Errorf("cannot derive a workspace key for %s", path)
	}
	if err := project.Approve(filepath.Join(hisnStateDir(), "hisn"), key, got.Hash); err != nil {
		return err
	}
	fmt.Fprintln(out, "approved.")
	return nil
}

// printLabels shows the capability labels and the resulting risk.
func printLabels(out io.Writer, r hisn.Risk) {
	fmt.Fprintln(out, "the generated profile carries:")
	for _, c := range r.Capabilities {
		fmt.Fprintf(out, "  %s [%s]\n", c.Capability, c.Label)
	}
	fmt.Fprintf(out, "→ risk: %s (acknowledge with hisn.risk_ack %q)\n", r.Label, r.Ack)
}

// IsInteractiveTUI reports whether this process may ask the user an inline
// acknowledgement question: stdin AND stdout are terminals. Any other run
// (wakil run, --print-*, scheduled) is non-interactive and never prompts.
func IsInteractiveTUI() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// AckOffer is the inline first-run offer the TUI shows when startup hits a
// missing or mismatched acknowledgement. It shows the labels, asks
// "acknowledge now? [y/N]", and on y writes and approves exactly as
// `wakil sandbox trust` does. It returns nil when the ack is now in place
// (startup continues) and an error on N or any refusal (startup exits).
func AckOffer(cfg config.Config, in io.Reader, out io.Writer) error {
	l, err := project.Load(cfg.HostWorkDir)
	if err != nil {
		return err
	}
	r, err := productionRiskFn(cfg)
	if err != nil {
		return err
	}
	if r.Ack == "none" {
		return nil
	}
	if l.Present && fileAck(l.Raw) == r.Ack {
		return nil // already acknowledged (any hand-written form too): nothing to ask
	}
	if l.Present && !bootstrapEligible(l.Raw) {
		return fmt.Errorf("%s is hand-written; change hisn.risk_ack to %q yourself, then run `wakil sandbox trust`", project.RelPath, r.Ack)
	}
	printLabels(out, r)
	fmt.Fprint(out, "acknowledge now? [y/N]: ")
	line, _ := bufio.NewReader(in).ReadString('\n')
	if s := strings.ToLower(strings.TrimSpace(line)); s != "y" && s != "yes" {
		fmt.Fprintln(out, "not acknowledged; nothing was written.")
		return fmt.Errorf("risk %q is not acknowledged (run `wakil sandbox trust` to review it)", r.Label)
	}
	return writeAndApprove(out, cfg, l.Path, bootstrapBytes(r.Ack), l)
}

// OpenControllingTTY opens /dev/tty for the approval answer.
func OpenControllingTTY() (io.ReadCloser, error) { return os.Open("/dev/tty") }
