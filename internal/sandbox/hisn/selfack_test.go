package hisn

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/sandbox"
)

// The self-ack decision table (0.5.0): Wakil's own default home-persist mount
// is acknowledged by Wakil; every user-configured label still needs the ack.
func TestSelfAckDecisionTable(t *testing.T) {
	root := "/state/hisn-home/abc"
	cases := []struct {
		name     string
		d        explainDoc
		persist  string
		homeRoot string
		want     string // the label checkAck must accept with ack, "" = refuse without ack
	}{
		{
			name:    "default home-persist under wakil root is self-acked",
			d:       explainDoc{Risk: "data", Capabilities: capsOf("home-persist", "data")},
			persist: root, homeRoot: root, want: "none",
		},
		{
			name:    "home-persist outside wakil root still needs the ack",
			d:       explainDoc{Risk: "data", Capabilities: capsOf("home-persist", "data")},
			persist: "/home/u/shared", homeRoot: root, want: "data",
		},
		{
			name:    "rootfs (data) is not self-acked even with the home mount",
			d:       explainDoc{Risk: "data", Capabilities: append(capsOf("home-persist", "data"), capsOf("rootfs", "data")...)},
			persist: root, homeRoot: root, want: "data",
		},
		{
			name:    "ssh-agent (signing) needs the ack",
			d:       explainDoc{Risk: "signing", Capabilities: capsOf("ssh-agent", "signing")},
			persist: root, homeRoot: root, want: "signing",
		},
		{
			name:    "docker forward (root-equivalent) needs the ack",
			d:       explainDoc{Risk: "root-equivalent", Capabilities: capsOf("forward", "root-equivalent")},
			persist: root, homeRoot: root, want: "root-equivalent",
		},
		{
			name:    "no persist at all: data needs the ack",
			d:       explainDoc{Risk: "data", Capabilities: capsOf("home-persist", "data")},
			persist: "", homeRoot: root, want: "data",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			need := ackFor(c.d, c.persist, c.homeRoot)
			if need != c.want {
				t.Fatalf("ackFor = %q, want %q", need, c.want)
			}
			// Refused without an ack unless it is none.
			err := checkAck(need, "", c.d.Risk)
			if need == "none" {
				if err != nil {
					t.Fatalf("self-acked label must pass with no ack: %v", err)
				}
				return
			}
			var re *RiskAckError
			if !errors.As(err, &re) {
				t.Fatalf("label %q must need the ack, got %v", need, err)
			}
			if checkAck(need, need, c.d.Risk) != nil {
				t.Fatalf("matching ack %q must pass", need)
			}
		})
	}
}

func capsOf(capability, label string) []sandbox.Capability {
	return []sandbox.Capability{{Capability: capability, Label: label}}
}

// The default profile (no project file, no sandbox.json) starts without any
// acknowledgement: its only label is Wakil's own home-persist mount.
func TestDefaultProfileStartsWithoutAck(t *testing.T) {
	fe := newFakeEnv(t)
	setVersion(t, fe, "0.4.2")
	_ = os.WriteFile(filepath.Join(fe.state, "explain_risk"), []byte("data"), 0o600)
	b, err := openWith(t, fe, nil)
	if err != nil {
		t.Fatalf("default profile must open without an ack: %v", err)
	}
	if err := startChat(t, b, fe, "dflt1"); err != nil {
		t.Fatalf("default profile must start without an ack: %v", err)
	}
	st := b.SandboxStatus(ctxT(t))
	marked := false
	for _, c := range st.Capabilities {
		if c.Capability == "home-persist" && strings.Contains(c.Label, "(wakil default)") {
			marked = true
		}
	}
	if !marked {
		t.Errorf("banner must mark the home mount as the wakil default: %+v", st.Capabilities)
	}
}
