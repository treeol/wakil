package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/sandbox"
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

func (n *networkGetterSetter) SetNetworkMode(mode string)      { n.mode = mode }
func (n *networkGetterSetter) NetworkMode() string             { return n.mode }
func (n *networkGetterSetter) LiveNetworkMode() (string, bool) { return "", false }

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
		got := app.networkStatus()
		first, rest, _ := strings.Cut(got, "\n")
		if first != c.want {
			t.Errorf("%s: networkStatus: want %q, got %q", c.name, c.want, first)
		}
		// The trust/kernel sentence appears exactly when open is in effect.
		if hasNote := rest == networkOpenNote; hasNote != strings.HasPrefix(c.want, "network: open") {
			t.Errorf("%s: open note present=%v, status %q", c.name, hasNote, got)
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
	wantOpen := "Network mode: open (public internet over TCP only; LAN and private addresses are refused with a permission error, do not retry them; open is meant for code you trust and needs kernel 5.14+)."

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

	// 7. Concurrent invalidation during ensurePreamble must not be lost:
	// if preambleGen advances before ensurePreamble finishes, preambleDay
	// stays stale so the next ensurePreamble rebuilds with the new mode.
	appGen := &App{
		Cfg:        config.Config{WorkDir: t.TempDir()},
		InjectDate: true,
	}
	appGen.ensurePreamble()
	appGen.stateMu.Lock()
	appGen.invalidatePreambleLocked()
	genAfterFirst := appGen.preambleGen
	appGen.stateMu.Unlock()
	if genAfterFirst == 0 {
		t.Fatalf("expected preambleGen > 0 after invalidatePreambleLocked")
	}
	appGen.ensurePreamble()
	appGen.stateMu.RLock()
	dayAfterRebuild := appGen.preambleDay
	appGen.stateMu.RUnlock()
	if dayAfterRebuild == "" || dayAfterRebuild == preambleRefreshSentinel {
		t.Errorf("preambleDay after uncontested rebuild = %q, want today", dayAfterRebuild)
	}
}

// runningSessionNetworkExec models a sandbox backend (such as hisn) whose
// running session keeps its original network mode until replaced.
type runningSessionNetworkExec struct {
	*fakeExecutor
	running bool
	live    string
	queued  string
}

func (e *runningSessionNetworkExec) SetNetworkMode(mode string) {
	if mode != "open" {
		mode = "host"
	}
	e.queued = mode
}

func (e *runningSessionNetworkExec) NetworkMode() string {
	if e.running {
		return e.live
	}
	return e.queued
}

func (e *runningSessionNetworkExec) LiveNetworkMode() (string, bool) {
	if !e.running {
		return "", false
	}
	return e.live, true
}

func (e *runningSessionNetworkExec) replaceSession() {
	e.running = true
	e.live = e.queued
}

func (e *runningSessionNetworkExec) SandboxStatus(_ context.Context) sandbox.Status {
	cur := e.queued
	if e.running {
		cur = e.live
	}
	st := sandbox.Status{Backend: "hisn", Risk: "none", Network: cur}
	if e.running && e.queued != "" && e.queued != e.live {
		st.NextNetwork = e.queued
	}
	return st
}

// TestSystemPromptNetworkMode_StaysLiveUntilSessionReplaced verifies that
// running /network open on a live host session keeps the system prompt's
// "Network mode:" line at "host", tells the user in /network output that the
// current session is still host, shows both in /sandbox, and updates the
// prompt line to "open" once the sandbox session is replaced.
func TestSystemPromptNetworkMode_StaysLiveUntilSessionReplaced(t *testing.T) {
	wantHost := "Network mode: host (LAN and internet reachable)."
	wantOpen := "Network mode: open (public internet over TCP only; LAN and private addresses are refused with a permission error, do not retry them; open is meant for code you trust and needs kernel 5.14+)."

	lastLine := func(s string) string {
		lines := strings.Split(s, "\n")
		return lines[len(lines)-1]
	}

	ex := &runningSessionNetworkExec{
		fakeExecutor: newFakeExecutor(),
		running:      true,
		live:         "host",
		queued:       "host",
	}
	app := &App{
		Cfg:         config.Config{WorkDir: t.TempDir()},
		Exec:        ex,
		AgentPrompt: "STATIC PROMPT",
		InjectDate:  true,
	}

	// Initial prompt reports the running session's live mode ("host").
	app.ensurePreamble()
	if ll := lastLine(DerefStr(app.Conv[0].Content)); ll != wantHost {
		t.Fatalf("initial prompt last line = %q, want %q", ll, wantHost)
	}

	// /network open queues "open" for the next session; the running session is still "host".
	handled, _, cmd := handleNetworkCommand([]string{"/network", "open"}, app)
	if !handled || cmd == nil {
		t.Fatal("/network open must be handled")
	}
	msg := cmd().(SysNoteMsg).Text
	if !strings.Contains(msg, "applies at the next session; current session is still host") {
		t.Errorf("/network output = %q, want live-session notice", msg)
	}

	// Next request before session replacement must STILL say "host".
	app.ensurePreamble()
	if ll := lastLine(DerefStr(app.Conv[0].Content)); ll != wantHost {
		t.Errorf("prompt before session replace = %q, want %q", ll, wantHost)
	}

	// /sandbox shows both live and queued next-session modes when they differ.
	sbOut := app.renderSandboxState()
	if !strings.Contains(sbOut, "network: host (next session: open)") {
		t.Errorf("/sandbox output missing live+next network:\n%s", sbOut)
	}

	// Once the session is replaced (picking up the queued "open" mode),
	// ensurePreamble updates the prompt line to "open" automatically.
	ex.replaceSession()
	app.ensurePreamble()
	if ll := lastLine(DerefStr(app.Conv[0].Content)); ll != wantOpen {
		t.Errorf("prompt after session replace = %q, want %q", ll, wantOpen)
	}

	// /sandbox now shows only the active "open" mode (no next-session mismatch).
	sbOut2 := app.renderSandboxState()
	if strings.Contains(sbOut2, "next session:") || !strings.Contains(sbOut2, "network: open") {
		t.Errorf("/sandbox output after replace = \n%s", sbOut2)
	}
}

// TestNetworkCommandApplyNotice covers all three /network apply-notice cases:
//  1. No sandbox session running → "no sandbox session is running; the next one starts on <mode>"
//  2. Running session with a different live mode → "applies at the next session; current session is still <live>"
//  3. Running session whose live mode equals the new mode → neither line printed.
func TestNetworkCommandApplyNotice(t *testing.T) {
	// Case 1: no sandbox session running.
	exIdle := &runningSessionNetworkExec{
		fakeExecutor: newFakeExecutor(),
		running:      false,
		queued:       "host",
	}
	appIdle := &App{
		Cfg:  config.Config{WorkDir: t.TempDir()},
		Exec: exIdle,
	}
	handled, _, cmd := handleNetworkCommand([]string{"/network", "open"}, appIdle)
	if !handled || cmd == nil {
		t.Fatal("idle /network open must be handled")
	}
	msgIdle := cmd().(SysNoteMsg).Text
	wantIdle := "no sandbox session is running; the next one starts on open"
	if !strings.Contains(msgIdle, wantIdle) {
		t.Errorf("idle /network open = %q, want %q", msgIdle, wantIdle)
	}
	if strings.Contains(msgIdle, "current session is still") {
		t.Errorf("idle /network open must not mention a running session: %q", msgIdle)
	}
	// Reset while idle also prints the no-running-session line with the effective mode.
	_, _, cmd = handleNetworkCommand([]string{"/network", "reset"}, appIdle)
	msgIdleReset := cmd().(SysNoteMsg).Text
	wantIdleReset := "no sandbox session is running; the next one starts on host"
	if !strings.Contains(msgIdleReset, wantIdleReset) {
		t.Errorf("idle /network reset = %q, want %q", msgIdleReset, wantIdleReset)
	}

	// Case 2: running session whose live mode differs from the new mode.
	exDiff := &runningSessionNetworkExec{
		fakeExecutor: newFakeExecutor(),
		running:      true,
		live:         "host",
		queued:       "host",
	}
	appDiff := &App{
		Cfg:  config.Config{WorkDir: t.TempDir()},
		Exec: exDiff,
	}
	_, _, cmd = handleNetworkCommand([]string{"/network", "open"}, appDiff)
	msgDiff := cmd().(SysNoteMsg).Text
	wantDiff := "applies at the next session; current session is still host"
	if !strings.Contains(msgDiff, wantDiff) {
		t.Errorf("running-different /network open = %q, want %q", msgDiff, wantDiff)
	}
	if strings.Contains(msgDiff, "no sandbox session is running") {
		t.Errorf("running-different must not say no session running: %q", msgDiff)
	}

	// Case 3: running session whose live mode already equals the new mode.
	// Setting /network host on a live host session (or /network reset back to
	// host) must print neither notice line.
	_, _, cmd = handleNetworkCommand([]string{"/network", "host"}, appDiff)
	msgSame := cmd().(SysNoteMsg).Text
	if strings.Contains(msgSame, "current session is still") || strings.Contains(msgSame, "no sandbox session is running") {
		t.Errorf("running-same /network host must print neither apply line, got %q", msgSame)
	}
	_, _, cmd = handleNetworkCommand([]string{"/network", "reset"}, appDiff)
	msgSameReset := cmd().(SysNoteMsg).Text
	if strings.Contains(msgSameReset, "current session is still") || strings.Contains(msgSameReset, "no sandbox session is running") {
		t.Errorf("running-same /network reset must print neither apply line, got %q", msgSameReset)
	}

	// Case 4: executor without NetworkGetter (e.g. direct/docker) or a
	// BackendExecutor wrapping a backend that doesn't support network mode
	// (NetworkMode() == "") must not falsely claim "no sandbox session is running".
	appUnsupported := &App{
		Cfg:  config.Config{WorkDir: t.TempDir()},
		Exec: newFakeExecutor(),
	}
	_, _, cmd = handleNetworkCommand([]string{"/network", "open"}, appUnsupported)
	msgUnsup := cmd().(SysNoteMsg).Text
	if strings.Contains(msgUnsup, "no sandbox session is running") || strings.Contains(msgUnsup, "current session is still") {
		t.Errorf("unsupported executor /network open must not emit sandbox apply notice, got %q", msgUnsup)
	}
	appEmptyGetter := &App{
		Cfg:  config.Config{WorkDir: t.TempDir()},
		Exec: &unsupportedNetworkExec{fakeExecutor: newFakeExecutor()},
	}
	_, _, cmd = handleNetworkCommand([]string{"/network", "open"}, appEmptyGetter)
	msgEmpty := cmd().(SysNoteMsg).Text
	if strings.Contains(msgEmpty, "no sandbox session is running") || strings.Contains(msgEmpty, "current session is still") {
		t.Errorf("unsupported backend (NetworkMode()==\"\") /network open must not emit sandbox apply notice, got %q", msgEmpty)
	}
}

// unsupportedNetworkExec models BackendExecutor wrapping a sandbox backend
// without sandbox.NetworkGetter/NetworkBackend: SetNetworkMode is a no-op and
// NetworkMode/LiveNetworkMode both report unsupported ("", false).
type unsupportedNetworkExec struct {
	*fakeExecutor
}

func (u *unsupportedNetworkExec) SetNetworkMode(string)           {}
func (u *unsupportedNetworkExec) NetworkMode() string             { return "" }
func (u *unsupportedNetworkExec) LiveNetworkMode() (string, bool) { return "", false }
