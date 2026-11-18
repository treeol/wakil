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
