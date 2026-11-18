package agent

import (
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
)

// TestNetworkCommand: /network shows the current value with no argument,
// sets the override with an argument, and rejects invalid values.
func TestNetworkCommand(t *testing.T) {
	app := &App{
		Cfg: config.Config{},
	}

	// No argument: shows the current value (default "host").
	handled, _, cmd := handleNetworkCommand([]string{"/network"}, app)
	if !handled || cmd == nil {
		t.Fatal("/network must be handled")
	}
	msg := cmd().(SysNoteMsg)
	if !strings.Contains(msg.Text, "host") {
		t.Errorf("/network bare: want %q in %q", "host", msg.Text)
	}

	// /network open: sets the override.
	handled, _, cmd = handleNetworkCommand([]string{"/network", "open"}, app)
	if !handled || cmd == nil {
		t.Fatal("/network open must be handled")
	}
	msg = cmd().(SysNoteMsg)
	if !strings.Contains(msg.Text, "open") {
		t.Errorf("/network open: want %q in %q", "open", msg.Text)
	}
	if app.NetworkOverride != "open" {
		t.Errorf("NetworkOverride: want %q, got %q", "open", app.NetworkOverride)
	}

	// /network host: clears the override (back to config default).
	handled, _, cmd = handleNetworkCommand([]string{"/network", "host"}, app)
	if !handled || cmd == nil {
		t.Fatal("/network host must be handled")
	}
	cmd()
	if app.NetworkOverride != "host" {
		t.Errorf("NetworkOverride: want %q, got %q", "host", app.NetworkOverride)
	}

	// /network reset: clears the override.
	handled, _, cmd = handleNetworkCommand([]string{"/network", "reset"}, app)
	if !handled || cmd == nil {
		t.Fatal("/network reset must be handled")
	}
	cmd()
	if app.NetworkOverride != "" {
		t.Errorf("NetworkOverride after reset: want %q, got %q", "", app.NetworkOverride)
	}

	// Invalid value: usage message.
	handled, _, cmd = handleNetworkCommand([]string{"/network", "bogus"}, app)
	if !handled || cmd == nil {
		t.Fatal("/network bogus must be handled")
	}
	msg = cmd().(SysNoteMsg)
	if !strings.Contains(msg.Text, "usage") {
		t.Errorf("/network bogus: want %q in %q", "usage", msg.Text)
	}
}

// networkSetter is a fake NetworkSetter for tests: it records the last mode.
type networkSetter struct {
	*fakeExecutor
	last string
}

func newNetworkSetter() *networkSetter { return &networkSetter{fakeExecutor: newFakeExecutor()} }

func (n *networkSetter) SetNetworkMode(mode string) { n.last = mode }

// TestNetworkResetReachesBackend: /network reset must push the effective mode
// (override cleared → config/default) to the backend, so the next session
// start uses the reset value instead of the previous override.
func TestNetworkResetReachesBackend(t *testing.T) {
	app := &App{Cfg: config.Config{}}
	app.Exec = newNetworkSetter()
	// Simulate a previous /network open, then reset.
	handled, _, cmd := handleNetworkCommand([]string{"/network", "open"}, app)
	if !handled || cmd == nil {
		t.Fatal("/network open must be handled")
	}
	cmd()
	if got := app.Exec.(*networkSetter).last; got != "open" {
		t.Fatalf("/network open: backend mode: want %q, got %q", "open", got)
	}
	handled, _, cmd = handleNetworkCommand([]string{"/network", "reset"}, app)
	if !handled || cmd == nil {
		t.Fatal("/network reset must be handled")
	}
	cmd()
	if got := app.Exec.(*networkSetter).last; got != "host" {
		t.Errorf("/network reset: backend mode: want %q (effective after reset), got %q", "host", got)
	}
	// With config "open", reset must push "open" (not the stale "host").
	app2 := &App{Cfg: config.Config{Sandbox: config.SandboxConfig{Hisn: config.HisnConfig{NetworkMode: "open"}}}}
	app2.Exec = newNetworkSetter()
	handled, _, cmd = handleNetworkCommand([]string{"/network", "host"}, app2)
	if !handled || cmd == nil {
		t.Fatal("/network host must be handled")
	}
	cmd()
	if got := app2.Exec.(*networkSetter).last; got != "host" {
		t.Fatalf("/network host: backend mode: want %q, got %q", "host", got)
	}
	handled, _, cmd = handleNetworkCommand([]string{"/network", "reset"}, app2)
	if !handled || cmd == nil {
		t.Fatal("/network reset must be handled")
	}
	cmd()
	if got := app2.Exec.(*networkSetter).last; got != "open" {
		t.Errorf("/network reset (config open): backend mode: want %q, got %q", "open", got)
	}
}

// TestNetworkStatusSource: the status line names the source that decided the
// effective mode: a set override (host or open) is "repo override", else an
// "open" config, else the built-in default.
func TestNetworkStatusSource(t *testing.T) {
	cases := []struct {
		name     string
		override string
		config   string
		want     string
	}{
		{"override host over config open", "host", "open", "network: host (repo override)"},
		{"override open over config host", "open", "host", "network: open (repo override)"},
		{"config open, no override", "", "open", "network: open (config)"},
		{"nothing set", "", "", "network: host (default)"},
		{"config host, no override", "", "host", "network: host (config default)"},
	}
	for _, c := range cases {
		app := &App{Cfg: config.Config{Sandbox: config.SandboxConfig{Hisn: config.HisnConfig{NetworkMode: c.config}}}}
		app.NetworkOverride = c.override
		if got := app.networkStatus(); got != c.want {
			t.Errorf("%s: networkStatus: want %q, got %q", c.name, c.want, got)
		}
	}
}

// TestEffectiveNetwork: the effective network resolves from the per-repo
// override, then the config, then the default "host".
func TestEffectiveNetwork(t *testing.T) {
	// Default: host.
	app := &App{Cfg: config.Config{}}
	if got := app.EffectiveNetwork(); got != "host" {
		t.Errorf("default: want %q, got %q", "host", got)
	}

	// Config says open.
	app = &App{Cfg: config.Config{Sandbox: config.SandboxConfig{Hisn: config.HisnConfig{NetworkMode: "open"}}}}
	if got := app.EffectiveNetwork(); got != "open" {
		t.Errorf("config open: want %q, got %q", "open", got)
	}

	// Override wins over config.
	app = &App{Cfg: config.Config{Sandbox: config.SandboxConfig{Hisn: config.HisnConfig{NetworkMode: "open"}}}}
	app.NetworkOverride = "host"
	if got := app.EffectiveNetwork(); got != "host" {
		t.Errorf("override host over config open: want %q, got %q", "host", got)
	}

	// Override open wins over config host.
	app = &App{Cfg: config.Config{}}
	app.NetworkOverride = "open"
	if got := app.EffectiveNetwork(); got != "open" {
		t.Errorf("override open over config default: want %q, got %q", "open", got)
	}
}
