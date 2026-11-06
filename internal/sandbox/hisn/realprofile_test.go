package hisn

import (
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests run the profiles Wakil GENERATES through the REAL hisn >= 0.3.1
// `profile validate` / `profile explain`. Both are pure file checks: they need
// no user namespaces, so unlike the TestIntegration* session tests they run on
// any host that has the binary (CI, this build sandbox via WAKIL_HISN_BIN).
// They are what keeps the hand-written fake honest.
func realHisnOffline(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("WAKIL_HISN_BIN")
	if bin == "" {
		p, err := osexec.LookPath("hisn")
		if err != nil {
			t.Skip("hisn not found")
		}
		bin = p
	}
	out, err := osexec.Command(bin, "--version").Output()
	if err != nil {
		t.Skipf("hisn unusable: %v", err)
	}
	if v, err := ParseVersion(string(out)); err != nil || !VersionAtLeast(v, [3]int{0, 3, 1}) {
		t.Skipf("hisn >= 0.3.1 required, got %q", strings.TrimSpace(string(out)))
	}
	return bin
}

func hisnRun(t *testing.T, bin string, args ...string) (string, error) {
	t.Helper()
	cmd := osexec.Command(bin, args...)
	cmd.Dir = t.TempDir()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// realWorkspace returns existing, resolved directories OUTSIDE /tmp/hisn-<uid>
// (hisn forbids paths under its runtime dir).
func realDirs(t *testing.T, names ...string) []string {
	t.Helper()
	base, err := os.MkdirTemp(os.Getenv("GOTMPDIR"), "rp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	base, _ = filepath.EvalSymlinks(base)
	var out []string
	for _, n := range names {
		d := filepath.Join(base, n)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func renderForReal(t *testing.T, ws, home, grant string, s Surface) string {
	t.Helper()
	surf, err := s.Normalize(ws)
	if err != nil {
		t.Fatal(err)
	}
	prof, err := buildProfile(policyParams{Cwd: ws, Home: home, Hostname: "wakil-0123456789abcdef", GrantableRW: []string{grant},
		EnvPass: []string{"GOFLAGS", "CI"}, ShmSize: 64, TmpSize: 1024}, surf, "")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(filepath.Dir(ws), "gen.profile.toml") // outside ws, like the state dir
	if err := os.WriteFile(p, prof, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRealHisnAcceptsGeneratedBaseProfile(t *testing.T) {
	bin := realHisnOffline(t)
	d := realDirs(t, "ws", "home", "out")
	p := renderForReal(t, d[0], d[1], d[2], Surface{})
	if out, err := hisnRun(t, bin, "profile", "validate", p); err != nil {
		t.Fatalf("real hisn rejects Wakil's base profile: %v\n%s\n--- profile\n%s", err, out, mustRead(t, p))
	}
	out, err := hisnRun(t, bin, "profile", "explain", p)
	if err != nil {
		t.Fatalf("explain: %v\n%s", err, out)
	}
	if doc, err := parseExplain(out); err != nil || doc.Risk != "none" {
		t.Errorf("explain: %+v %v\n%s", doc, err, out)
	}
}

func TestRealHisnAcceptsFullSurface(t *testing.T) {
	bin := realHisnOffline(t)
	d := realDirs(t, "ws", "home", "out", "ro", "rw")
	ws := d[0]
	s := Surface{
		Services:     []Service{{Name: "kvr", Exec: []string{"/usr/bin/env", "A=b", "/usr/bin/true"}, Socket: ".wakil/kvr.sock", StopSignal: "TERM", StopGrace: 5}},
		Forward:      []Forward{{Name: "docker", Socket: "/var/run/docker.sock", Expose: "/run/docker.sock"}},
		HostServices: []HostService{{Name: "h", Exec: []string{"/usr/bin/true"}, Socket: "/run/user/1000/h.sock", Expose: "/run/h.sock", ReadySocket: "/run/user/1000/h.sock", ReadyTimeout: 5, Risk: "host-exec"}},
		EnvSet:       map[string]string{"APP_ENV": "dev"},
		MountsRO:     []string{d[3]},
		MountsRW:     []string{d[4]},
		Mask:         []string{filepath.Join(ws, ".wakil", "sandbox.json")},
	}
	// The mask target must exist for hisn to apply it; hisn skips a missing one.
	_ = os.MkdirAll(filepath.Join(ws, ".wakil"), 0o755)
	_ = os.WriteFile(filepath.Join(ws, ".wakil", "sandbox.json"), []byte("{}"), 0o644)

	// Without the user's ack real hisn refuses it, with its own error code.
	p := renderForReal(t, ws, d[1], d[2], s)
	out, err := hisnRun(t, bin, "profile", "validate", p)
	if err == nil || !strings.Contains(out, "E_RISK_ACK_") {
		t.Fatalf("a root-equivalent/host-exec profile without risk_ack must be refused by hisn itself: %v\n%s", err, out)
	}
	var re *RiskAckError
	if e := validateError(err, out); !errors.As(e, &re) {
		t.Errorf("hisn's %s must map to RiskAckError, got %T", strings.TrimSpace(out), e)
	}

	// With host-exec acked on a root-equivalent forward hisn says MISMATCH.
	s.RiskAck = "host-exec"
	p = renderForReal(t, ws, d[1], d[2], s)
	if out, err := hisnRun(t, bin, "profile", "validate", p); err == nil || !strings.Contains(out, "E_RISK_ACK_MISMATCH") {
		t.Fatalf("wrong ack must be E_RISK_ACK_MISMATCH: %v\n%s", err, out)
	}
	// host_services + forward with mixed labels: one ack cannot satisfy both
	// (documented limit) — the matching one for the highest still fails the other.
	s.RiskAck = "root-equivalent"
	p = renderForReal(t, ws, d[1], d[2], s)
	out, err = hisnRun(t, bin, "profile", "validate", p)
	if err == nil && !strings.Contains(out, "version") {
		t.Fatalf("unexpected: %v\n%s", err, out)
	}
	t.Logf("mixed labels with one ack: err=%v\n%s", err, firstNonEmpty(out))
}

func firstNonEmpty(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			return l
		}
	}
	return ""
}

func mustRead(t *testing.T, p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRealHisnAckMatrix(t *testing.T) {
	bin := realHisnOffline(t)
	d := realDirs(t, "ws", "home", "out")
	fwd := func(sock, risk string) Surface {
		return Surface{Forward: []Forward{{Name: "f", Socket: sock, Expose: "/run/f.sock", Risk: risk}}}
	}
	cases := []struct {
		name    string
		s       Surface
		ack     string
		wantOK  bool
		wantErr string
		risk    string
	}{
		{"docker without ack", fwd("/var/run/docker.sock", ""), "", false, "E_RISK_ACK_REQUIRED", ""},
		{"docker mismatch", fwd("/var/run/docker.sock", ""), "signing", false, "E_RISK_ACK_MISMATCH", ""},
		{"docker ok", fwd("/var/run/docker.sock", ""), "root-equivalent", true, "", "root-equivalent"},
		{"ssh-agent ok", fwd("/run/user/1000/ssh-agent.sock", ""), "signing", true, "", "signing"},
		{"dbus ok", fwd("/run/dbus/system_bus_socket", ""), "host-exec", true, "", "host-exec"},
		{"unknown socket without risk", fwd("/run/user/1000/x.sock", ""), "host-exec", false, "E_RISK_ACK_REQUIRED", ""},
		// data needs no ack in hisn; Wakil copies a SUPERFLUOUS ack in — real hisn must accept that.
		{"data, no ack", fwd("/run/user/1000/x.sock", "data"), "", true, "", "data"},
		{"data, superfluous ack", fwd("/run/user/1000/x.sock", "data"), "data", true, "", "data"},
		{"service only", Surface{Services: []Service{{Name: "s", Exec: []string{"/usr/bin/true"}, Socket: ".wakil/s.sock"}}}, "", true, "", "none"},
		{"service with a superfluous ack", Surface{Services: []Service{{Name: "s", Exec: []string{"/usr/bin/true"}}}}, "root-equivalent", true, "", "none"},
	}
	for _, c := range cases {
		s := c.s
		s.RiskAck = c.ack
		p := renderForReal(t, d[0], d[1], d[2], s)
		out, err := hisnRun(t, bin, "profile", "validate", p)
		if c.wantOK {
			if err != nil {
				t.Errorf("%s: real hisn refused what Wakil generates: %v\n%s\n--- profile\n%s", c.name, err, out, mustRead(t, p))
				continue
			}
			eo, err := hisnRun(t, bin, "profile", "explain", p)
			if err != nil {
				t.Errorf("%s: explain: %v\n%s", c.name, err, eo)
				continue
			}
			if doc, err := parseExplain(eo); err != nil || doc.Risk != c.risk {
				t.Errorf("%s: explain risk = %+v (%v), want %s\n%s", c.name, doc, err, c.risk, eo)
			}
			continue
		}
		if err == nil || !strings.Contains(out, c.wantErr) {
			t.Errorf("%s: want %s, got err=%v\n%s", c.name, c.wantErr, err, out)
		}
	}
}
