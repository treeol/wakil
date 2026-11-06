package hisn

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/treeol/wakil/internal/sandbox"
)

// riskRank orders hisn's closed label set.
var riskRank = map[string]int{"none": 0, "data": 1, "signing": 2, "host-exec": 3, "root-equivalent": 4, "kernel-surface": 5}

// explainDoc is `hisn profile explain` output (schema hisn-profile-explain/v1).
type explainDoc struct {
	Risk         string               `json:"risk"`
	Capabilities []sandbox.Capability `json:"capabilities"`
	Notes        []string             `json:"notes"`
}

func parseExplain(out string) (explainDoc, error) {
	var d explainDoc
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &d); err != nil {
		return d, fmt.Errorf("hisn profile explain: unexpected output: %w", err)
	}
	if _, ok := riskRank[d.Risk]; !ok {
		return d, fmt.Errorf("hisn profile explain: unknown risk label %q", d.Risk)
	}
	return d, nil
}

// checkAck enforces the user's acknowledgement: any label above `none` must be
// acknowledged by the project file with exactly that label. hisn itself only
// demands an ack for signing, host-exec and root-equivalent; Wakil asks for
// `data` and `kernel-surface` too. Wakil never supplies the ack.
func checkAck(label, ack string) error {
	if label == "none" || label == ack {
		return nil
	}
	if ack == "" {
		return &RiskAckError{Label: label, Detail: fmt.Sprintf("the profile's risk is %q; set hisn.risk_ack to %q once you have reviewed it", label, label)}
	}
	return &RiskAckError{Label: label, Detail: fmt.Sprintf("hisn.risk_ack is %q but the profile's risk is %q", ack, label)}
}

// evaluateRisk is the plan-time check: it asks hisn for the profile's label
// (`hisn profile explain`) and compares it with the project's acknowledgement.
// For a generated profile it explains the profile exactly as it would start;
// for a named profile it explains NAME. On 0.2.x (no 0.3 surface) the label is
// none and nothing is asked.
func (b *Backend) evaluateRisk(ctx context.Context, target string) (explainDoc, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := b.output(ctx, false, "profile", "explain", target)
	if err != nil {
		return explainDoc{}, validateError(err, out)
	}
	d, err := parseExplain(out)
	if err != nil {
		return d, err
	}
	return d, checkAck(d.Risk, b.opts.Surface.RiskAck)
}

// inspectDoc is the part of `hisn inspect --json` Wakil reads.
type inspectDoc struct {
	OK       bool                 `json:"ok"`
	Risk     []sandbox.Capability `json:"risk"`
	Services []struct {
		Name   string `json:"name"`
		Socket string `json:"socket"`
		State  string `json:"state"`
	} `json:"services"`
}

func (b *Backend) inspect(ctx context.Context, id string) (inspectDoc, error) {
	var d inspectDoc
	out, err := b.output(ctx, false, "inspect", "--session", id, "--json")
	if err != nil {
		return d, fmt.Errorf("hisn inspect %s: %w: %s", id, err, strings.TrimSpace(out))
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &d); err != nil || !d.OK {
		return d, fmt.Errorf("hisn inspect %s: unexpected output", id)
	}
	return d, nil
}

func highestOf(caps []sandbox.Capability) string {
	h := "none"
	for _, c := range caps {
		if riskRank[c.Label] > riskRank[h] {
			h = c.Label
		}
	}
	return h
}

// verifyRisk is the post-start check: the running session's own risk snapshot
// (`inspect --json`) must not exceed what was acknowledged. A session that
// runs at a label the user did not acknowledge is stopped by the caller.
func (b *Backend) verifyRisk(ctx context.Context, id string) (inspectDoc, error) {
	d, err := b.inspect(ctx, id)
	if err != nil {
		return d, err
	}
	if h := highestOf(d.Risk); h != "none" {
		if err := checkAck(h, b.opts.Surface.RiskAck); err != nil {
			return d, err
		}
	}
	return d, nil
}

// plan is the plan-time risk check, run once by Open before any session
// exists: it renders the profile exactly as sessions will start it (a named
// profile is explained by name), asks hisn for its label and compares it with
// the project's acknowledgement. A refusal is a hard error naming hisn.risk_ack.
func (b *Backend) plan(ctx context.Context) error {
	s := b.opts.Surface
	b.planned = explainDoc{Risk: "none", Capabilities: []sandbox.Capability{}}
	if !b.profileMode() || s.Empty() {
		return nil // no 0.3 surface requested: the 0.2 sandbox, label none
	}
	target := s.Profile
	if target == "" {
		if err := os.MkdirAll(b.opts.StateDir, 0o700); err != nil {
			return fmt.Errorf("hisn state dir: %w", err)
		}
		pp := policyParams{Cwd: b.opts.HostCwd, Home: b.opts.HomeRoot, Hostname: "wakil-plan",
			GrantableRW: b.opts.GrantableRW, EnvPass: b.opts.EnvPass, ShmSize: b.opts.ShmSizeMB, TmpSize: b.opts.TmpSizeMB}
		if err := checkProfileTrust(b.profilePath("plan"), pp.Cwd, pp.Home, s.MountsRW); err != nil {
			return err
		}
		homeP, err := b.homePersist(s)
		if err != nil {
			return err
		}
		prof, err := buildProfile(pp, s, homeP)
		if err != nil {
			return err
		}
		target = filepath.Join(b.opts.StateDir, "plan.profile.toml")
		if err := os.WriteFile(target, prof, 0o600); err != nil {
			return fmt.Errorf("writing hisn profile: %w", err)
		}
		defer func() { _ = os.Remove(target) }()
	}
	d, err := b.evaluateRisk(ctx, target)
	b.planned = d
	return err
}

// SandboxStatus implements sandbox.StatusReporter.
func (b *Backend) SandboxStatus(ctx context.Context) sandbox.Status {
	s := b.opts.Surface
	st := sandbox.Status{Backend: "hisn", Risk: b.planned.Risk, Capabilities: b.planned.Capabilities, Notes: b.planned.Notes}
	if st.Risk == "" {
		st.Risk = "none"
	}
	switch {
	case s.Profile != "":
		st.Mode, st.Profile = "named profile", s.Profile
	case b.profileMode():
		st.Mode = "generated profile"
	default:
		st.Mode = "bare policy (hisn < " + ProfileMinVersion + ")"
	}
	for _, f := range s.Forward {
		st.Forwards = append(st.Forwards, fmt.Sprintf("%s: %s → %s", f.Name, f.Socket, f.Expose))
	}
	for _, h := range s.HostServices {
		st.HostServices = append(st.HostServices, fmt.Sprintf("%s: %s → %s (runs on the host)", h.Name, h.Socket, h.Expose))
	}
	for k := range s.EnvSet {
		st.EnvSet = append(st.EnvSet, k)
	}
	sort.Strings(st.EnvSet)
	st.Grants = b.pm.Grants()
	st.KVR = b.kvrNote
	live := map[string]sandbox.ServiceState{}
	b.mu.RLock()
	cur := b.cur
	b.mu.RUnlock()
	if cur != nil {
		if s.Profile == "" {
			st.ProfilePath = b.profilePath(cur.id)
		}
		ictx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if d, err := b.inspect(ictx, cur.id); err == nil {
			for _, v := range d.Services {
				live[v.Name] = sandbox.ServiceState{Name: v.Name, Socket: v.Socket, State: v.State}
			}
		}
		cancel()
	}
	for _, v := range s.Services {
		ss, ok := live[v.Name]
		if !ok {
			ss = sandbox.ServiceState{Name: v.Name, Socket: v.Socket, State: "not started"}
		}
		st.Services = append(st.Services, ss)
	}
	return st
}

var _ sandbox.StatusReporter = (*Backend)(nil)
