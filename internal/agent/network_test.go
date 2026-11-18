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

type networkGetterSetter struct {
	*fakeExecutor
	mode string
}

func (n *networkGetterSetter) SetNetworkMode(mode string) { n.mode = mode }
func (n *networkGetterSetter) NetworkMode() string        { return n.mode }

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

// TestSystemPromptNetworkMode verifies both modes ("host" and "open") appear
// as the final line of the system prompt's per-session suffix, reflect
// per-repo /network overrides and backend-reported modes, and update in place
// on /network changes for the next request without disturbing the static prefix.
func TestSystemPromptNetworkMode(t *testing.T) {
	const staticPrefix = "STATIC AGENT PROMPT PREFIX"
	wantHost := "Network mode: host (LAN and internet reachable)."
	wantOpen := "Network mode: open (public internet over TCP only; LAN and private addresses are refused with a permission error, do not retry them)."

	lastLine := func(s string) string {
		lines := strings.Split(s, "\n")
		return lines[len(lines)-1]
	}

	// 1. Default ("host") mode ends with the host line, static prefix first.
	app := &App{
		Cfg:         config.Config{},
		AgentPrompt: staticPrefix,
		InjectDate:  true,
	}
	app.ensurePreamble()
	if len(app.Conv) != 1 || app.Conv[0].Role != "system" {
		t.Fatalf("Conv = %+v, want 1 system message", app.Conv)
	}
	got := DerefStr(app.Conv[0].Content)
	if !strings.HasPrefix(got, staticPrefix) {
		t.Errorf("system prompt must start with static prefix, got:\n%s", got)
	}
	if ll := lastLine(got); ll != wantHost {
		t.Errorf("host last line = %q, want %q", ll, wantHost)
	}

	// 2. /network open updates Conv[0] in place on the next ensurePreamble().
	handled, _, cmd := handleNetworkCommand([]string{"/network", "open"}, app)
	if !handled || cmd == nil {
		t.Fatal("/network open must be handled")
	}
	cmd()
	app.ensurePreamble()
	if len(app.Conv) != 1 {
		t.Fatalf("after /network open: len(Conv) = %d, want 1 (in-place refresh)", len(app.Conv))
	}
	got = DerefStr(app.Conv[0].Content)
	if !strings.HasPrefix(got, staticPrefix) {
		t.Errorf("after /network open: static prefix lost")
	}
	if ll := lastLine(got); ll != wantOpen {
		t.Errorf("open last line = %q, want %q", ll, wantOpen)
	}

	// 3. /network host switches it back on the next ensurePreamble().
	handled, _, cmd = handleNetworkCommand([]string{"/network", "host"}, app)
	if !handled || cmd == nil {
		t.Fatal("/network host must be handled")
	}
	cmd()
	app.ensurePreamble()
	if ll := lastLine(DerefStr(app.Conv[0].Content)); ll != wantHost {
		t.Errorf("after /network host: last line = %q, want %q", ll, wantHost)
	}

	// 4. Config default "open" + /network reset restores "open".
	appOpenCfg := &App{
		Cfg:         config.Config{Sandbox: config.SandboxConfig{Hisn: config.HisnConfig{NetworkMode: "open"}}},
		AgentPrompt: staticPrefix,
		InjectDate:  true,
	}
	appOpenCfg.ensurePreamble()
	if ll := lastLine(DerefStr(appOpenCfg.Conv[0].Content)); ll != wantOpen {
		t.Errorf("config open: last line = %q, want %q", ll, wantOpen)
	}
	handleNetworkCommand([]string{"/network", "host"}, appOpenCfg)
	appOpenCfg.ensurePreamble()
	if ll := lastLine(DerefStr(appOpenCfg.Conv[0].Content)); ll != wantHost {
		t.Errorf("override host over open config: last line = %q, want %q", ll, wantHost)
	}
	handleNetworkCommand([]string{"/network", "reset"}, appOpenCfg)
	appOpenCfg.ensurePreamble()
	if ll := lastLine(DerefStr(appOpenCfg.Conv[0].Content)); ll != wantOpen {
		t.Errorf("reset back to open config: last line = %q, want %q", ll, wantOpen)
	}

	// 5. Executor reporting "open" via NetworkGetter (e.g. .wakil/sandbox.json narrowing).
	ngs := &networkGetterSetter{fakeExecutor: newFakeExecutor(), mode: "open"}
	appBackend := &App{
		Cfg:        config.Config{},
		Exec:       ngs,
		InjectDate: true,
	}
	appBackend.ensurePreamble()
	if ll := lastLine(DerefStr(appBackend.Conv[0].Content)); ll != wantOpen {
		t.Errorf("backend-narrowed open: last line = %q, want %q", ll, wantOpen)
	}
	handleNetworkCommand([]string{"/network", "host"}, appBackend)
	appBackend.ensurePreamble()
	if ll := lastLine(DerefStr(appBackend.Conv[0].Content)); ll != wantHost {
		t.Errorf("after /network host on backend: last line = %q, want %q", ll, wantHost)
	}

	// 6. Per-repo RepoState restore sets the mode in the system prompt.
	repoDir := t.TempDir()
	if err := updateRepoState(repoDir, func(s *RepoState) { s.Network = "open" }); err != nil {
		t.Fatalf("save repo state: %v", err)
	}
	appRepo := &App{
		Cfg:        config.Config{WorkDir: repoDir},
		InjectDate: true,
	}
	RestoreRepoState(appRepo)
	appRepo.ensurePreamble()
	if ll := lastLine(DerefStr(appRepo.Conv[0].Content)); ll != wantOpen {
		t.Errorf("per-repo restored open: last line = %q, want %q", ll, wantOpen)
	}
}
