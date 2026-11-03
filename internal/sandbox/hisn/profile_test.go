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
	setVersion(t, fe, "0.3.1")
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

func TestPolicyPathOn02x(t *testing.T) {
	fe := newFakeEnv(t)
	setVersion(t, fe, "0.2.2")
	b, err := openWith(t, fe, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b.profileMode() {
		t.Fatal("0.2.2 must keep the bare --policy path")
	}
	if err := startChat(t, b, fe, "pm2"); err != nil {
		t.Fatal(err)
	}
	c := calls(t, fe)
	if !strings.Contains(c, "policy validate ") || !strings.Contains(c, "--policy ") || !strings.Contains(c, "--network host") {
		t.Errorf("0.2.x must use --policy + policy validate:\n%s", c)
	}
	if strings.Contains(c, "--profile") || strings.Contains(c, "profile validate") {
		t.Errorf("0.2.x must never see --profile:\n%s", c)
	}
}

func TestFeaturesNeed031(t *testing.T) {
	fe := newFakeEnv(t)
	setVersion(t, fe, "0.2.2")
	_, err := openWith(t, fe, func(o *Options) {
		o.Surface = Surface{Forward: []Forward{{Name: "docker", Socket: "/var/run/docker.sock", Expose: "/run/docker.sock"}}}
	})
	if err == nil || !strings.Contains(err.Error(), "hisn.forward") || !strings.Contains(err.Error(), "0.3.1") {
		t.Fatalf("want a hard error naming hisn.forward and 0.3.1, got %v", err)
	}
	// env_set / mounts exist in hisn-policy/v1 and work on 0.2.x.
	fe2 := newFakeEnv(t)
	setVersion(t, fe2, "0.2.2")
	b, err := openWith(t, fe2, func(o *Options) { o.Surface = Surface{EnvSet: map[string]string{"A": "b"}} })
	if err != nil {
		t.Fatal(err)
	}
	if err := startChat(t, b, fe2, "e1"); err != nil {
		t.Fatal(err)
	}
	pol, _ := os.ReadFile(filepath.Join(fe2.rt(), "e1", "policy"))
	if !strings.Contains(string(pol), "[env.set]") {
		t.Errorf("env_set must reach the 0.2.x policy:\n%s", pol)
	}
}

// ---- rendering ----

func TestProfileEncodingKeepsPolicy(t *testing.T) {
	pp := policyParams{Cwd: "/w", Home: "/h", Hostname: "wakil-x", GrantableRW: []string{"/srv/out"},
		EnvPass: []string{"CI"}, ShmSize: 64, TmpSize: 1024}
	legacy, err := buildPolicy(pp)
	if err != nil {
		t.Fatal(err)
	}
	prof, err := buildProfile(pp, Surface{})
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
	out, err := buildProfile(policyParams{Cwd: "/work", Home: "/h", Hostname: "wakil-x", ShmSize: 64, TmpSize: 1024}, s)
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
	out, _ = buildProfile(policyParams{Cwd: "/work", Home: "/h", Hostname: "wakil-x", ShmSize: 64, TmpSize: 1024}, s)
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
	setVersion(t, fe, "0.3.1")
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
	setVersion(t, fe, "0.3.1")
	b, err := openWith(t, fe, func(o *Options) { o.Surface = Surface{Profile: "nope"} })
	if err != nil {
		t.Fatal(err)
	}
	err = startChat(t, b, fe, "nm2")
	if err == nil || !strings.Contains(err.Error(), "E_PROFILE_NOTFOUND") {
		t.Fatalf("hisn's own error must reach the user, got %v", err)
	}
}

func TestReconcileNamedUsesSupervisorHandle(t *testing.T) {
	fe := newFakeEnv(t)
	setVersion(t, fe, "0.3.1")
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

// ---- risk errors ----

func TestRiskAckErrorsFromValidate(t *testing.T) {
	fe := newFakeEnv(t)
	setVersion(t, fe, "0.3.1")
	fwd := []Forward{{Name: "docker", Socket: "/var/run/docker.sock", Expose: "/run/docker.sock"}}
	b, err := openWith(t, fe, func(o *Options) { o.Surface = Surface{Forward: fwd} })
	if err != nil {
		t.Fatal(err)
	}
	err = startChat(t, b, fe, "r1")
	var re *RiskAckError
	if !errors.As(err, &re) || !strings.Contains(err.Error(), "hisn.risk_ack") {
		t.Fatalf("missing ack must be a RiskAckError naming hisn.risk_ack, got %v", err)
	}
	if got := fe.sessionIDs(); len(got) != 0 {
		t.Errorf("no session may exist after a refused start: %v", got)
	}
	if strings.Contains(calls(t, fe), "run --detach") {
		t.Error("hisn run must not even be attempted")
	}
	fe2 := newFakeEnv(t)
	setVersion(t, fe2, "0.3.1")
	b2, _ := openWith(t, fe2, func(o *Options) { o.Surface = Surface{Forward: fwd, RiskAck: "signing"} })
	if err := startChat(t, b2, fe2, "r2"); !errors.As(err, &re) {
		t.Fatalf("mismatched ack must be refused, got %v", err)
	}
	fe3 := newFakeEnv(t)
	setVersion(t, fe3, "0.3.1")
	b3, _ := openWith(t, fe3, func(o *Options) { o.Surface = Surface{Forward: fwd, RiskAck: "root-equivalent"} })
	if err := startChat(t, b3, fe3, "r3"); err != nil {
		t.Fatalf("matching ack must start: %v", err)
	}
}
