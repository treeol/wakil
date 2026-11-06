package project

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, content string) {
	t.Helper()
	p := filepath.Join(dir, ".wakil", "sandbox.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const full = `{"hisn":{
  "services":[{"name":"db","exec":["pg"],"socket":".wakil/db.sock"}],
  "forward":[{"name":"docker","socket":"/var/run/docker.sock","expose":"/run/docker.sock"}],
  "host_services":[{"name":"h","exec":["h"],"socket":"/run/h.sock","expose":"/run/h.sock"}],
  "env_set":{"FOO":"bar"},
  "mounts_ro":["/opt/data"],
  "mounts_rw":["/srv/out"],
  "risk_ack":"root-equivalent"
}}`

func TestLoadAbsentAndEmpty(t *testing.T) {
	dir := t.TempDir()
	l, err := Load(dir)
	if err != nil || l.Present || l.Needs() {
		t.Fatalf("absent: %+v %v", l, err)
	}
	write(t, dir, "  \n")
	l, err = Load(dir)
	if err != nil || !l.Present || l.Needs() {
		t.Fatalf("blank file asks for nothing and needs no approval: %+v %v", l, err)
	}
}

func TestLoadFullSchema(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, full)
	l, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := l.Surface
	if len(s.Services) != 1 || len(s.Forward) != 1 || len(s.HostServices) != 1 || s.EnvSet["FOO"] != "bar" ||
		s.MountsRO[0] != "/opt/data" || s.MountsRW[0] != "/srv/out" || s.RiskAck != "root-equivalent" {
		t.Errorf("surface not decoded: %+v", s)
	}
	if !l.Needs() || l.Hash == "" {
		t.Error("a file asking for things needs approval and has a hash")
	}
	if got := strings.Join(Summary(l), "\n"); !strings.Contains(got, "RUNS ON THE HOST") || !strings.Contains(got, "READ-WRITE mount: /srv/out") {
		t.Errorf("summary hides the dangerous parts:\n%s", got)
	}
}

func TestStrictDecoding(t *testing.T) {
	for name, body := range map[string]string{
		"unknown top":     `{"docker":{}}`,
		"unknown hisn":    `{"hisn":{"risk-ack":"x"}}`,
		"unknown service": `{"hisn":{"services":[{"name":"a","exec":["x"],"restart":"always"}]}}`,
		"trailing":        `{"hisn":{}} {}`,
		"not json":        `hisn:`,
	} {
		dir := t.TempDir()
		write(t, dir, body)
		if _, err := Load(dir); err == nil {
			t.Errorf("%s must be rejected", name)
		}
	}
	dir := t.TempDir()
	write(t, dir, `{"hisn":{"nope":1}}`)
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("the error must name the unknown key: %v", err)
	}
}

func TestSymlinkedProjectFileRefused(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".wakil"), 0o755); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "x.json")
	_ = os.WriteFile(other, []byte(`{}`), 0o644)
	if err := os.Symlink(other, filepath.Join(dir, ".wakil", "sandbox.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Error("a symlinked sandbox.json must be refused")
	}
}

func TestApprovalLifecycle(t *testing.T) {
	state, ws := t.TempDir(), t.TempDir()
	write(t, ws, full)
	l, _ := Load(ws)
	if Approved(state, "k1", l.Hash) {
		t.Fatal("nothing approved yet")
	}
	if err := Approve(state, "k1", l.Hash); err != nil {
		t.Fatal(err)
	}
	if !Approved(state, "k1", l.Hash) {
		t.Error("approved hash must verify")
	}
	if Approved(state, "k2", l.Hash) {
		t.Error("approval is per workspace")
	}
	// The agent edits the file: the approval no longer matches.
	write(t, ws, strings.Replace(full, "/srv/out", "/home/u/.ssh", 1))
	l2, _ := Load(ws)
	if l2.Hash == l.Hash || Approved(state, "k1", l2.Hash) {
		t.Error("a changed file must need approval again")
	}
	if Approved(state, "", l.Hash) || Approved(state, "k1", "") {
		t.Error("empty key/hash never approve")
	}
	if err := Approve(state, "k1", ""); err == nil {
		t.Error("approving nothing must fail")
	}
	var na *ErrNotApproved
	if !errors.As(error(&ErrNotApproved{Path: "p"}), &na) || !strings.Contains(na.Error(), "wakil sandbox trust") {
		t.Error("the error must point at the user-run command")
	}
}

// TestRootfsProjectNameOnly: the project file takes a rootfs store name only;
// the absolute path form is global-only and is refused by the loader.
func TestRootfsProjectNameOnly(t *testing.T) {
	if _, err := Parse([]byte(`{"hisn":{"rootfs":"wakil-dev"}}`)); err != nil {
		t.Fatalf("store name must parse: %v", err)
	}
	_, err := Parse([]byte(`{"hisn":{"rootfs":"/srv/rootfs"}}`))
	if err == nil || !strings.Contains(err.Error(), "global only") {
		t.Fatalf("path form must be refused in the project file, got %v", err)
	}
	_, err = Parse([]byte(`{"hisn":{"rootfs":"bad name!"}}`))
	if err == nil || !strings.Contains(err.Error(), "hisn.rootfs") {
		t.Fatalf("invalid name must be refused, got %v", err)
	}
}
