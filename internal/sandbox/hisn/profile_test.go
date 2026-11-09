package hisn

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/treeol/wakil/internal/sandbox"
)

func setVersion(t *testing.T, fe *fakeEnv, v string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(fe.state, "version"), []byte(v), 0o600); err != nil {
		t.Fatal(err)
	}
}

func calls(t *testing.T, fe *fakeEnv) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(fe.state, "calls.log"))
	return string(b)
}

func openWith(t *testing.T, fe *fakeEnv, mut func(*Options)) (*Backend, error) {
	t.Helper()
	o := fe.options()
	if mut != nil {
		mut(&o)
	}
	b, err := Open(ctxT(t), o)
	if err == nil {
		t.Cleanup(func() { _ = b.Close(context.Background()) })
	}
	return b, err
}

func startChat(t *testing.T, b *Backend, fe *fakeEnv, id string) error {
	t.Helper()
	return b.Start(ctxT(t), sandbox.Session{ID: id, HostCwd: fe.cwd})
}

// ---- version gating, both ways ----

func TestProfileModeOn031(t *testing.T) {
	fe := newFakeEnv(t)
	setVersion(t, fe, "0.4.1")
	b, err := openWith(t, fe, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !b.profileMode() {
		t.Fatal("0.3.1 must select profile mode")
	}
	if err := startChat(t, b, fe, "pm1"); err != nil {
		t.Fatal(err)
	}
	c := calls(t, fe)
	if !strings.Contains(c, "profile validate ") || !strings.Contains(c, "--profile ") {
		t.Errorf("profile mode must validate and start with --profile:\n%s", c)
	}
	if strings.Contains(c, "--policy") || strings.Contains(c, "policy validate") {
		t.Errorf("profile mode must not use the bare policy path:\n%s", c)
	}
	if strings.Contains(c, "--network") {
		t.Errorf("the profile's [policy] carries the network, not the CLI:\n%s", c)
	}
	prof := filepath.Join(fe.sdir, "pm1.profile.toml")
	if _, err := os.Stat(prof); err != nil {
		t.Fatalf("profile must live in the state dir: %v", err)
	}
	if sandbox.Within(fe.cwd, prof) {
		t.Fatalf("profile %s is under the project cwd %s", prof, fe.cwd)
	}
	if pc, _ := os.ReadFile(filepath.Join(fe.state, "last_proc_cwd")); strings.TrimSpace(string(pc)) != filepath.Join(fe.sdir, "proc-cwd") {
		realWant, _ := filepath.EvalSymlinks(filepath.Join(fe.sdir, "proc-cwd"))
		if strings.TrimSpace(string(pc)) != realWant {
			t.Errorf("supervisor cwd = %q, want the neutral %q", pc, filepath.Join(fe.sdir, "proc-cwd"))
		}
	}
	if err := b.Stop(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(prof); !os.IsNotExist(err) {
		t.Errorf("generated profile must be removed on stop: %v", err)
	}
}

// Below the 0.4.1 floor there is no bare --policy path any more: Open refuses
// 0.2.x, 0.3.x and 0.4.0 hisn and names the minimum. (The pre-0.3.3 branches
// were removed with the floor; see MinVersion.)
func TestBelowFloorRefusedNamingMinimum(t *testing.T) {
	for _, v := range []string{"0.2.2", "0.3.1", "0.3.2", "0.3.3", "0.4.0"} {
		fe := newFakeEnv(t)
		setVersion(t, fe, v)
		_, err := openWith(t, fe, nil)
		if err == nil || !strings.Contains(err.Error(), "0.4.1") {
			t.Errorf("hisn %s: want a refusal naming 0.4.1, got %v", v, err)
		}
	}
}

func TestProfileEncodingKeepsPolicy(t *testing.T) {
	pp := policyParams{Cwd: "/w", Home: "/h", Hostname: "wakil-x", GrantableRW: []string{"/srv/out"},
		EnvPass: []string{"CI"}, ShmSize: 64, TmpSize: 1024}
	legacy, err := buildPolicy(pp)
	if err != nil {
		t.Fatal(err)
	}
	prof, err := buildProfile(pp, Surface{}, "")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Version string
		Policy  map[string]any
	}
	if _, err := toml.Decode(string(prof), &doc); err != nil {
		t.Fatal(err)
	}
	var leg map[string]any
	if _, err := toml.Decode(string(legacy), &leg); err != nil {
		t.Fatal(err)
	}
	if doc.Version != "hisn-profile/v1" {
		t.Errorf("version = %q", doc.Version)
	}
	var a, b bytes.Buffer
	if err := toml.NewEncoder(&a).Encode(doc.Policy); err != nil {
		t.Fatal(err)
	}
	if err := toml.NewEncoder(&b).Encode(leg); err != nil {
		t.Fatal(err)
	}
	if a.String() != b.String() {
		t.Errorf("[policy] differs from the bare policy:\n--- profile\n%s\n--- legacy\n%s", a.String(), b.String())
	}
	if strings.Contains(string(prof), "services") || strings.Contains(string(prof), "forward") || strings.Contains(string(prof), "risk_ack") {
		t.Errorf("no surface requested, none rendered:\n%s", prof)
	}
}

func TestProfileSurfaceAndAckVerbatim(t *testing.T) {
	s, err := Surface{
		Services:     []Service{{Name: "kvr", Exec: []string{"/usr/bin/env", "x"}, Socket: ".wakil/kvr.sock", ReadySocket: ".wakil/kvr.sock"}},
		Forward:      []Forward{{Name: "docker", Socket: "/var/run/docker.sock", Expose: "/run/docker.sock"}},
		HostServices: []HostService{{Name: "h", Exec: []string{"h"}, Socket: "/run/h.sock", Expose: "/run/h.sock"}},
		RiskAck:      "root-equivalent",
	}.Normalize("/work")
	if err != nil {
		t.Fatal(err)
	}
	out, err := buildProfile(policyParams{Cwd: "/work", Home: "/h", Hostname: "wakil-x", ShmSize: 64, TmpSize: 1024}, s, "")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if _, err := toml.Decode(string(out), &doc); err != nil {
		t.Fatal(err)
	}
	str := string(out)
	for _, want := range []string{`/work/.wakil/kvr.sock`, `restart = "none"`} {
		if !strings.Contains(str, want) {
			t.Errorf("missing %q:\n%s", want, str)
		}
	}
	if n := strings.Count(str, `risk_ack = "root-equivalent"`); n != 2 {
		t.Errorf("ack must be copied verbatim into forward and host_services (got %d):\n%s", n, str)
	}
	// Without an ack in the project, Wakil writes none.
	s.RiskAck = ""
	out, _ = buildProfile(policyParams{Cwd: "/work", Home: "/h", Hostname: "wakil-x", ShmSize: 64, TmpSize: 1024}, s, "")
	if strings.Contains(string(out), "risk_ack") {
		t.Errorf("Wakil must never write a risk_ack on its own:\n%s", out)
	}
}

func TestNormalizeErrors(t *testing.T) {
	cases := map[string]Surface{
		"hisn.profile":       {Profile: "/abs/path.toml"},
		"used as is":         {Profile: "deploy", Services: []Service{{Name: "a", Exec: []string{"x"}}}},
		"duplicate":          {Services: []Service{{Name: "a", Exec: []string{"x"}}, {Name: "a", Exec: []string{"y"}}}},
		"exec must not":      {Services: []Service{{Name: "a"}}},
		"absolute paths":     {Forward: []Forward{{Name: "f", Socket: "rel", Expose: "/e"}}},
		"hisn.env_set":       {EnvSet: map[string]string{"HOME": "x"}},
		"hisn.mounts_rw":     {MountsRW: []string{"relative"}},
		"other than /":       {MountsRO: []string{"/"}},
		"ready_socket must":  {HostServices: []HostService{{Name: "h", Exec: []string{"x"}, Socket: "/s", Expose: "/e", ReadySocket: "rel"}}},
		"invalid name \"a b": {Services: []Service{{Name: "a b", Exec: []string{"x"}}}},
	}
	for want, s := range cases {
		if _, err := s.Normalize("/w"); err == nil || !strings.Contains(err.Error(), strings.Trim(want, `"`)) && !strings.Contains(err.Error(), "invalid name") {
			t.Errorf("%s: got %v", want, err)
		}
	}
}

func TestNormalizeRejectsOversizeDurations(t *testing.T) {
	cases := []struct {
		name string
		s    Surface
		want string
	}{
		{"service ready_timeout", Surface{Services: []Service{{Name: "a", Exec: []string{"x"}, ReadyTimeout: 86401}}}, "ready_timeout"},
		{"service stop_grace", Surface{Services: []Service{{Name: "a", Exec: []string{"x"}, StopGrace: 90000}}}, "stop_grace"},
		{"host ready_timeout", Surface{HostServices: []HostService{{Name: "h", Exec: []string{"x"}, Socket: "/s", Expose: "/e", ReadyTimeout: 86401}}}, "ready_timeout"},
		{"host stop_grace", Surface{HostServices: []HostService{{Name: "h", Exec: []string{"x"}, Socket: "/s", Expose: "/e", StopGrace: 86401}}}, "stop_grace"},
	}
	for _, tc := range cases {
		if _, err := tc.s.Normalize("/w"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want error naming %q, got %v", tc.name, tc.want, err)
		}
	}
	// Exactly 86400 is allowed.
	if _, err := (Surface{Services: []Service{{Name: "a", Exec: []string{"x"}, ReadyTimeout: 86400, StopGrace: 86400}}}).Normalize("/w"); err != nil {
		t.Errorf("86400 must be allowed: %v", err)
	}
}

func TestNormalizeRejectsNUL(t *testing.T) {
	cases := []struct {
		name string
		s    Surface
	}{
		{"service exec", Surface{Services: []Service{{Name: "a", Exec: []string{"x\x00y"}}}}},
		{"service socket", Surface{Services: []Service{{Name: "a", Exec: []string{"x"}, Socket: "/a\x00b"}}}},
		{"service ready_socket", Surface{Services: []Service{{Name: "a", Exec: []string{"x"}, ReadySocket: "/a\x00b"}}}},
		{"host exec", Surface{HostServices: []HostService{{Name: "h", Exec: []string{"a\x00"}, Socket: "/s", Expose: "/e"}}}},
		{"env_set value", Surface{EnvSet: map[string]string{"K": "v\x00"}}},
	}
	for _, tc := range cases {
		if _, err := tc.s.Normalize("/w"); err == nil || !strings.Contains(err.Error(), "NUL") {
			t.Errorf("%s: want NUL error, got %v", tc.name, err)
		}
	}
}

func TestTrustPrecheck(t *testing.T) {
	state := t.TempDir()
	prof := filepath.Join(state, "x.profile.toml")
	if err := checkProfileTrust(prof, filepath.Dir(state), "", nil); err == nil || !strings.Contains(err.Error(), "E_PROFILE_UNTRUSTED") {
		t.Errorf("workspace containing the state dir must be refused up front: %v", err)
	}
	if err := checkProfileTrust(prof, t.TempDir(), "", nil); err != nil {
		t.Errorf("unrelated workspace: %v", err)
	}
	if err := checkProfileTrust(prof, t.TempDir(), "", []string{state}); err == nil {
		t.Error("an rw mount over the state dir must be refused")
	}
	if err := checkProfileTrust(prof, t.TempDir(), filepath.Dir(state), nil); err == nil {
		t.Error("a home containing the state dir must be refused")
	}
}

// ---- named profile ----

func TestNamedProfile(t *testing.T) {
	fe := newFakeEnv(t)
	setVersion(t, fe, "0.4.1")
	pdir := t.TempDir()
	if err := os.WriteFile(filepath.Join(pdir, "deploy.toml"), []byte("version = \"hisn-profile/v1\"\n[policy]\nversion = \"hisn-policy/v1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ro := t.TempDir()
	o := fe.options()
	o.Env = append(o.Env, "HISN_PROFILES="+pdir)
	o.Surface = Surface{Profile: "deploy", MountsRO: []string{ro}}
	b, err := Open(ctxT(t), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	if err := startChat(t, b, fe, "nm1"); err != nil {
		t.Fatal(err)
	}
	c := calls(t, fe)
	if !strings.Contains(c, "--profile deploy") || !strings.Contains(c, "--grant "+ro+":ro") {
		t.Errorf("named profile must be passed by name with grants:\n%s", c)
	}
	if _, err := os.Stat(filepath.Join(fe.sdir, "nm1.profile.toml")); !os.IsNotExist(err) {
		t.Error("Wakil must not generate a profile when a named one is used")
	}
	rec, err := b.readRecord("nm1")
	if err != nil || rec.Mode != "named" || rec.SupPid == 0 || rec.SupStart == "" {
		t.Fatalf("named session must be identified by the supervisor handle: %+v %v", rec, err)
	}
	if err := b.Stop(ctxT(t)); err != nil {
		t.Fatal(err)
	}
}

func TestNamedProfileNotFoundSurfacesHisn(t *testing.T) {
	fe := newFakeEnv(t)
	setVersion(t, fe, "0.4.1")
	// Caught at plan time, in Open, before any session: hisn's own error text.
	_, err := openWith(t, fe, func(o *Options) { o.Surface = Surface{Profile: "nope"} })
	if err == nil || !strings.Contains(err.Error(), "E_PROFILE_NOTFOUND") {
		t.Fatalf("hisn's own error must reach the user, got %v", err)
	}
	if strings.Contains(calls(t, fe), "run --detach") {
		t.Error("no session may be attempted")
	}
}

func TestReconcileNamedUsesSupervisorHandle(t *testing.T) {
	fe := newFakeEnv(t)
	setVersion(t, fe, "0.4.1")
	b, err := openWith(t, fe, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A record of a dead owner, named mode, whose supervisor pid does not match.
	rec := record{ID: "nm3", Pid: 999999, PidStart: "1", Token: "abc", Cwd: fe.cwd, Started: true, Mode: "named", SupPid: os.Getpid(), SupStart: "bogus"}
	if err := b.createRecord(rec); err != nil {
		t.Fatal(err)
	}
	if err := b.Reconcile(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.readRecord("nm3"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("record must be dropped; our own process (starttime mismatch) must NOT be signalled: %v", err)
	}
}

// ---- risk labels ----

var dockerFwd = []Forward{{Name: "docker", Socket: "/var/run/docker.sock", Expose: "/run/docker.sock"}}

func TestRiskRefusedBeforeAnySession(t *testing.T) {
	for name, ack := range map[string]string{"missing": "", "mismatch": "signing"} {
		fe := newFakeEnv(t)
		setVersion(t, fe, "0.4.1")
		_, err := openWith(t, fe, func(o *Options) { o.Surface = Surface{Forward: dockerFwd, RiskAck: ack} })
		var re *RiskAckError
		if !errors.As(err, &re) || !strings.Contains(err.Error(), "wakil sandbox trust") {
			t.Fatalf("%s: want a RiskAckError naming `wakil sandbox trust`, got %v", name, err)
		}
		if got := fe.sessionIDs(); len(got) != 0 {
			t.Errorf("%s: no session may exist after a refusal: %v", name, got)
		}
		if strings.Contains(calls(t, fe), "run --detach") {
			t.Errorf("%s: hisn run must not even be attempted", name)
		}
		if _, err := os.Stat(filepath.Join(fe.sdir, "plan.profile.toml")); !os.IsNotExist(err) {
			t.Errorf("%s: the plan-time profile must be cleaned up", name)
		}
	}
}

func TestRiskAckAccepted(t *testing.T) {
	fe := newFakeEnv(t)
	setVersion(t, fe, "0.4.1")
	b, err := openWith(t, fe, func(o *Options) { o.Surface = Surface{Forward: dockerFwd, RiskAck: "root-equivalent"} })
	if err != nil {
		t.Fatal(err)
	}
	if err := startChat(t, b, fe, "ok1"); err != nil {
		t.Fatal(err)
	}
	st := b.SandboxStatus(ctxT(t))
	if st.Risk != "root-equivalent" || st.Mode != "generated profile" || len(st.Forwards) != 1 || st.Backend != "hisn" {
		t.Errorf("status: %+v", st)
	}
	if st.ProfilePath != filepath.Join(fe.sdir, "ok1.profile.toml") {
		t.Errorf("profile path: %q", st.ProfilePath)
	}
	if got := st.Banner(); got != "sandbox: backend=hisn · risk=root-equivalent" {
		t.Errorf("banner = %q", got)
	}
}

// hisn only demands an ack for signing/host-exec/root-equivalent; Wakil also
// demands one for `data`.
func TestDataLabelNeedsAckInWakil(t *testing.T) {
	fwd := []Forward{{Name: "x", Socket: "/run/user/1/x.sock", Expose: "/run/x.sock", Risk: "data"}}
	fe := newFakeEnv(t)
	setVersion(t, fe, "0.4.1")
	if _, err := openWith(t, fe, func(o *Options) { o.Surface = Surface{Forward: fwd} }); err == nil {
		t.Fatal("a data label without hisn.risk_ack must be refused")
	}
	fe2 := newFakeEnv(t)
	setVersion(t, fe2, "0.4.1")
	if _, err := openWith(t, fe2, func(o *Options) { o.Surface = Surface{Forward: fwd, RiskAck: "data"} }); err != nil {
		t.Fatalf("data with matching ack: %v", err)
	}
}

// With no project surface, the generated profile still carries Wakil's own
// [home] persist (a "data" capability on hisn 0.3.3). It is self-acknowledged:
// Open starts with no risk_ack, and the banner marks it "(wakil default)".
func TestNoSurfaceSelfAckedHomePersist(t *testing.T) {
	fe := newFakeEnv(t)
	setVersion(t, fe, "0.4.1")
	b, err := openWith(t, fe, nil)
	if err != nil {
		t.Fatalf("no surface must start without a risk_ack: %v", err)
	}
	st := b.SandboxStatus(ctxT(t))
	if st.Risk != "data" {
		t.Errorf("risk = %q, want data (the wakil-default home-persist)", st.Risk)
	}
	marked := false
	for _, c := range st.Capabilities {
		if c.Capability == "home-persist" && strings.Contains(c.Label, "(wakil default)") {
			marked = true
		}
	}
	if !marked {
		t.Errorf("home-persist must be marked (wakil default): %+v", st.Capabilities)
	}
}

func TestNamedProfileRiskExplainedByName(t *testing.T) {
	pdir := t.TempDir()
	body := "version = \"hisn-profile/v1\"\n[policy]\nversion = \"hisn-policy/v1\"\n[[forward]]\nname = \"docker\"\nsocket = \"/var/run/docker.sock\"\nexpose = \"/run/docker.sock\"\nrisk_ack = \"root-equivalent\"\n"
	if err := os.WriteFile(filepath.Join(pdir, "dk.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, ack := range []string{"", "root-equivalent"} {
		fe := newFakeEnv(t)
		setVersion(t, fe, "0.4.1")
		o := fe.options()
		o.Env = append(o.Env, "HISN_PROFILES="+pdir)
		o.Surface = Surface{Profile: "dk", RiskAck: ack}
		b, err := Open(ctxT(t), o)
		if ack == "" {
			var re *RiskAckError
			if !errors.As(err, &re) {
				t.Fatalf("a named profile above none needs the project's ack too: %v", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = b.Close(context.Background()) })
		if st := b.SandboxStatus(ctxT(t)); st.Risk != "root-equivalent" || st.Profile != "dk" || st.Mode != "named profile" {
			t.Errorf("status: %+v", st)
		}
	}
}

// If the running session turns out riskier than planned, it is stopped.
func TestPostStartRiskMismatchStopsSession(t *testing.T) {
	fe := newFakeEnv(t)
	setVersion(t, fe, "0.4.1")
	b, err := openWith(t, fe, func(o *Options) { o.Surface = Surface{Forward: dockerFwd, RiskAck: "root-equivalent"} })
	if err != nil {
		t.Fatal(err)
	}
	// hisn reports a label the plan never saw (e.g. a profile edited between
	// plan and start).
	_ = os.WriteFile(filepath.Join(fe.state, "inspect_risk"), []byte("kernel-surface"), 0o600)
	err = startChat(t, b, fe, "pm9")
	var re *RiskAckError
	if !errors.As(err, &re) || re.Label != "kernel-surface" {
		t.Fatalf("want a kernel-surface RiskAckError, got %v", err)
	}
	if got := fe.sessionIDs(); len(got) != 0 {
		t.Errorf("a session riskier than acknowledged must not survive: %v", got)
	}
}

func TestExplainParsing(t *testing.T) {
	if _, err := parseExplain(`{"risk":"bogus","capabilities":[]}`); err == nil {
		t.Error("unknown label must be an error")
	}
	if _, err := parseExplain(`nope`); err == nil {
		t.Error("garbage must be an error")
	}
	d, err := parseExplain(`{"risk":"data","capabilities":[{"capability":"forward:x","label":"data"}],"notes":["n"]}`)
	if err != nil || d.Risk != "data" || len(d.Capabilities) != 1 {
		t.Errorf("%+v %v", d, err)
	}
	if highestOf(d.Capabilities) != "data" || highestOf(nil) != "none" {
		t.Error("highestOf")
	}
}

// Mashūra R2 B3: the state directory (approvals!) must never become writable
// from the sandbox, however the writable surface is configured.
func TestStateDirNeverWritableFromSandbox(t *testing.T) {
	fe := newFakeEnv(t)
	setVersion(t, fe, "0.4.1")
	home := filepath.Dir(fe.sdir) // a prefix that CONTAINS the state dir
	cases := map[string]func(*Options){
		"grantable_rw containing it": func(o *Options) { o.GrantableRW = []string{home} },
		"grantable_rw equal to it":   func(o *Options) { o.GrantableRW = []string{fe.sdir} },
		"grantable_rw below it":      func(o *Options) { o.GrantableRW = []string{filepath.Join(fe.sdir, "approved")} },
		"mounts_rw below it":         func(o *Options) { o.Surface = Surface{MountsRW: []string{filepath.Join(fe.sdir, "approved")}} },
		"mounts_rw containing it":    func(o *Options) { o.Surface = Surface{MountsRW: []string{home}} },
		"home_root containing it":    func(o *Options) { o.HomeRoot = home },
	}
	for name, mut := range cases {
		if _, err := openWith(t, fe, mut); !errors.Is(err, ErrStateReachable) {
			t.Errorf("%s: must be refused with ErrStateReachable, got %v", name, err)
		}
	}
	// Unrelated writable surfaces are untouched.
	ok := t.TempDir()
	if _, err := openWith(t, newFakeEnvVersioned(t, "0.4.1"), func(o *Options) { o.GrantableRW = []string{ok}; o.Surface = Surface{MountsRW: []string{ok}} }); err != nil {
		t.Errorf("an unrelated rw surface must still work: %v", err)
	}
}

func newFakeEnvVersioned(t *testing.T, v string) *fakeEnv {
	t.Helper()
	fe := newFakeEnv(t)
	setVersion(t, fe, v)
	return fe
}

// A dynamic rw grant over the state directory is refused too (the manager's
// prefix list can only be as wide as config allows, but the backend is the last
// line).
func TestDynamicRWGrantOverStateDirRefused(t *testing.T) {
	fe := newFakeEnv(t)
	setVersion(t, fe, "0.4.1")
	b, err := openWith(t, fe, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := startChat(t, b, fe, "gs"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{fe.sdir, filepath.Join(fe.sdir, "approved"), filepath.Dir(fe.sdir)} {
		if err := b.Grant(ctxT(t), p, true); !errors.Is(err, ErrStateReachable) {
			t.Errorf("rw grant of %s must be refused, got %v", p, err)
		}
	}
	if strings.Contains(calls(t, fe), "grant --session gs --rw") {
		t.Error("hisn must never even be asked")
	}
}
