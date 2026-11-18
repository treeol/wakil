package agent

// /zdr and /region commands for the data policy (Step 4).
//
// Both switches are per-repo overrides stored in RepoState (the user's side,
// keyed by the workspace — never in the repository). /zdr on|off|auto writes
// the per-repo override; /zdr reset removes it; bare /zdr shows the
// effective value, where it comes from, and the last request's decision. The
// /region command follows the same pattern. Changes apply from the next
// request: applyDataPolicyOverride re-points the session proxy client.

import (
	"fmt"
	"strings"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/exec"
)

// handleDataSwitchCommand is the shared implementation for /zdr and /region.
// kind is "zdr" or "region".
func handleDataSwitchCommand(fields []string, app *App, kind string) (handled, quit bool, cmd func() Msg) {
	note := func(text string) func() Msg { return NoteCmd(text) }

	if len(fields) < 2 {
		return true, false, note(app.dataSwitchStatus(kind))
	}
	arg := fields[1]

	switch arg {
	case "reset":
		app.stateMu.Lock()
		if kind == "zdr" {
			app.ZDROverride = ""
		} else {
			app.RegionOverride = ""
		}
		app.applyDataPolicyToClientLocked()
		app.stateMu.Unlock()
		if kind == "zdr" {
			app.saveRepoState(func(s *RepoState) { s.ZDR = "" })
		} else {
			app.saveRepoState(func(s *RepoState) { s.Region = "" })
		}
		return true, false, note(app.dataSwitchStatus(kind))
	case "on", "off", "auto", "eu", "us":
		// Both switches accept off; zdr accepts on|auto, region accepts eu|us.
		if kind == "zdr" && (arg == "eu" || arg == "us") {
			return true, false, note("usage: /zdr [on|off|auto|reset]")
		}
		if kind == "region" && arg == "auto" {
			return true, false, note("usage: /region [eu|us|off|reset]")
		}
		val := arg
		if arg == "on" && kind == "region" {
			// /region on is not a value.
			return true, false, note("usage: /region [eu|us|off|reset]")
		}
		// Validate via the shared spelling (clear message at switch time).
		if err := config.ValidateDataPolicy(map[string]string{"zdr": val, "region": ""}[kind], map[string]string{"zdr": "", "region": val}[kind]); err != nil {
			return true, false, note(err.Error())
		}
		var prev string
		if kind == "zdr" {
			prev = app.EffectiveZDR()
		} else {
			prev = app.EffectiveRegion()
		}
		app.stateMu.Lock()
		if kind == "zdr" {
			app.ZDROverride = val
		} else {
			app.RegionOverride = val
		}
		app.applyDataPolicyToClientLocked()
		app.stateMu.Unlock()
		if kind == "zdr" {
			app.saveRepoState(func(s *RepoState) { s.ZDR = val })
		} else {
			app.saveRepoState(func(s *RepoState) { s.Region = val })
		}
		msg := app.dataSwitchStatus(kind)
		// A value that actually changed after the session has already sent a
		// request resets the prompt cache (the request prefix changes under
		// it). Re-setting the same value changes nothing, so it says so.
		if app.Client != nil && app.Client.RequestsSent() > 0 {
			if val == prev {
				msg += "\nValue unchanged — no prompt cache reset."
			} else {
				msg += "\nChanging this mid-session resets the prompt cache."
			}
		}
		if kind == "zdr" && val == "off" {
			msg += "\nZDR may still be enforced by your OpenRouter account settings."
		}
		if kind == "region" && (val == "eu" || val == "us") {
			msg += "\nNeeds an OpenRouter Business or Enterprise plan."
		}
		return true, false, note(msg)
	default:
		if kind == "zdr" {
			return true, false, note("usage: /zdr [on|off|auto|reset]")
		}
		return true, false, note("usage: /region [eu|us|off|reset]")
	}
}

// dataSwitchStatus renders the /zdr or /region status: the effective value,
// where it comes from, and the decision of the last request.
func (a *App) dataSwitchStatus(kind string) string {
	var eff, src string
	if kind == "zdr" {
		eff = a.EffectiveZDR()
		src = "config default"
		if a.ZDROverride != "" {
			src = "repo override"
		}
		if a.Cfg.ZDR != "" && a.ZDROverride == "" {
			src = "config"
		}
	} else {
		eff = a.EffectiveRegion()
		src = "config default"
		if a.RegionOverride != "" {
			src = "repo override"
		}
		if a.Cfg.Region != "" && a.RegionOverride == "" {
			src = "config"
		}
	}
	dec, model := "", ""
	if a.Client != nil {
		dec, model = a.Client.LastPolicyDecision()
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s: %s (%s)", kind, eff, src)
	if dec != "" {
		fmt.Fprintf(&sb, " · last request: %s", dec)
		if model != "" {
			fmt.Fprintf(&sb, " [%s]", model)
		}
	} else if a.Client != nil {
		sb.WriteString(" · last request: no request yet")
	}
	return sb.String()
}

// dataPolicyStatusLines renders the two /mode status lines: zdr (mode, last
// decision) and region (value, host in use).
func (a *App) dataPolicyStatusLines() string {
	zdrDec, _ := "", ""
	if a.Client != nil {
		zdrDec, _ = a.Client.LastPolicyDecision()
	}
	host := "openrouter.ai"
	if a.EffectiveRegion() == "eu" {
		host = "eu.openrouter.ai"
	} else if a.EffectiveRegion() == "us" {
		host = "us.openrouter.ai"
	}
	zdr := a.EffectiveZDR()
	if zdrDec != "" {
		zdr += " (last: " + zdrDec + ")"
	}
	return fmt.Sprintf("zdr: %s · region: %s (host: %s)", zdr, a.EffectiveRegion(), host)
}

// handleNetworkCommand implements /network [host|open|reset]. With no argument
// it shows the current effective value and where it comes from. A change
// applies from the NEXT session start (not the current one): the hisn profile
// is written at session start, so a running session keeps its network mode.
func handleNetworkCommand(fields []string, app *App) (handled, quit bool, cmd func() Msg) {
	note := func(text string) func() Msg { return NoteCmd(text) }

	if len(fields) < 2 {
		return true, false, note(app.networkStatus())
	}
	arg := fields[1]

	switch arg {
	case "reset":
		app.stateMu.Lock()
		app.NetworkOverride = ""
		app.invalidatePreambleLocked()
		app.stateMu.Unlock()
		app.saveRepoState(func(s *RepoState) { s.Network = "" })
		// Resetting must reach the backend too: without it the backend keeps
		// the previous override for the next session start while the status
		// line reports the config/default (same push as "host"|"open").
		if ns, ok := app.Exec.(exec.NetworkSetter); ok {
			ns.SetNetworkMode(app.EffectiveNetwork())
		}
		msg := app.networkStatus()
		msg += "\n" + app.networkApplyLine()
		return true, false, note(msg)
	case "host", "open":
		app.stateMu.Lock()
		app.NetworkOverride = arg
		app.invalidatePreambleLocked()
		app.stateMu.Unlock()
		app.saveRepoState(func(s *RepoState) { s.Network = arg })
		// Push to the backend so the NEXT session start uses it. A running
		// session keeps its mode; the value applies when a new session starts.
		if ns, ok := app.Exec.(exec.NetworkSetter); ok {
			ns.SetNetworkMode(arg)
		}
		msg := app.networkStatus()
		msg += "\n" + app.networkApplyLine()
		return true, false, note(msg)
	default:
		return true, false, note("usage: /network [host|open|reset]")
	}
}

// networkApplyLine describes when a /network change takes effect, naming the
// live session's current mode.
func (a *App) networkApplyLine() string {
	return fmt.Sprintf("applies at the next session; current session is still %s", a.sessionNetworkMode())
}

// networkStatus renders the /network status: the effective value and where it
// comes from. The source names the value that actually decided the effective
// mode: a set override always wins (even "host" over an "open" config), else
// the config, else the built-in default.
func (a *App) networkStatus() string {
	eff := a.EffectiveNetwork()
	src := "config default"
	if a.NetworkOverride != "" {
		src = "repo override"
	} else if a.Cfg.Sandbox.Hisn.NetworkMode == "open" {
		src = "config"
	}
	if a.NetworkOverride == "" && a.Cfg.Sandbox.Hisn.NetworkMode == "" {
		src = "default"
	}
	return fmt.Sprintf("network: %s (%s)", eff, src)
}
