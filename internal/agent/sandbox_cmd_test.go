package agent

import (
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/sandbox"
)

func TestFormatSandboxStatus(t *testing.T) {
	out := FormatSandboxStatus(sandbox.Status{
		Backend: "hisn", Mode: "generated profile", ProfilePath: "/state/hisn/c1.profile.toml", Risk: "root-equivalent",
		Capabilities: []sandbox.Capability{{Capability: "forward:docker", Label: "root-equivalent"}},
		Services:     []sandbox.ServiceState{{Name: "kvr", Socket: "/w/.wakil/kvr.sock", State: "ready"}},
		Forwards:     []string{"docker: /var/run/docker.sock → /run/docker.sock"},
		EnvSet:       []string{"FOO"},
		Grants:       []sandbox.GrantEntry{{Path: "/srv/out", RW: true}},
		KVR:          "on",
	})
	for _, want := range []string{
		"sandbox: backend=hisn · risk=root-equivalent", "profile file: /state/hisn/c1.profile.toml",
		"capability: forward:docker [root-equivalent]", "service: kvr (ready) /w/.wakil/kvr.sock",
		"forward: docker:", "env set: FOO", "grant: /srv/out (rw)", "staging (kvr): on",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "risk_ack") {
		t.Error("status must never print an acknowledgement")
	}
}

func TestSandboxCommandOnPlainExecutor(t *testing.T) {
	app := newTestApp("", newFakeExecutor(), nil)
	handled, quit, _ := HandleTUICommand("/sandbox", app)
	if !handled || quit {
		t.Fatal("/sandbox must be handled")
	}
	if got := app.renderSandboxState(); !strings.HasPrefix(got, "sandbox: ") {
		t.Errorf("an executor without a status still answers: %q", got)
	}
	if handled, _, _ := HandleTUICommand("/sandbox extra", app); !handled {
		t.Error("a stray argument is answered with usage, not ignored")
	}
}
