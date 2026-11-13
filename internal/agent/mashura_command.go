package agent

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/treeol/wakil/internal/config"
)

// handleMashuraCommand implements the /mashura slash command for runtime
// Mashūra counsel configuration. It allows the user to manage panels, tool
// mappings, default model, max tokens, and timeout — all without editing
// the config file. Changes take effect immediately (resolvePanel reads
// a.Cfg at call time) and persist to repo-state across TUI sessions.
//
// Subcommands:
//
//	/mashura                        Show status (level, members, per-tool table)
//	/mashura panel add <name> <models> [--mode panel|fallback|debate]  (legacy)
//	/mashura panel rm <name>         Remove a named panel
//	/mashura panel <name>            Show panel details
//	/mashura panel <name> --mode <mode>  Set panel mode
//	/mashura map <tool> <panel>      Map a tool (review|debug|decide|check) to a panel
//	/mashura map <tool>              Show current mapping for a tool
//	/mashura model <model-id>        Set the default model
//	/mashura maxtokens <N>           Set max tokens for mashura responses
//	/mashura timeout <seconds>       Set timeout for mashura calls
//
// Boundary: /counsel auto|suggest|off retains control of auto-counsel
// (struggle detection). /mashura controls panel composition and limits.
func handleMashuraCommand(fields []string, app *App) (handled, quit bool, cmd Cmd) {
	note := func(text string) Cmd { return NoteCmd(text) }

	if len(fields) < 2 {
		return true, false, note(mashuraStatus(app))
	}

	// With the new config block present, legacy panel/map/model/maxtokens/
	// timeout commands would write values the block path ignores. Report the
	// fact instead of appearing to succeed (round-2 finding).
	legacyBlocked := func(sub string) bool {
		return app.Cfg.Mashura != nil && (sub == "panel" || sub == "map" || sub == "model" || sub == "maxtokens" || sub == "timeout")
	}

	switch fields[1] {
	case config.MashuraLevelOff, config.MashuraLevelLow, config.MashuraLevelMedium, config.MashuraLevelHigh:
		level := fields[1]
		prev := app.MashuraLevelOverride
		app.MashuraLevelOverride = level
		app.saveRepoState(func(s *RepoState) { s.MashuraLevel = level })
		return true, false, note(mashuraLevelSetNote(app, prev, level))
	case "reset":
		prev := app.MashuraLevelOverride
		app.MashuraLevelOverride = ""
		// Reset also clears the counsel choice (session + per-repo) so the
		// level decides again, and clears the stale legacy RepoState mashura
		// fields so a pre-block repo file cannot resurrect them later.
		app.stateMu.Lock()
		app.CounselMode = ""
		app.MaxCounsel = 0
		app.CounselMaxSet = false
		app.stateMu.Unlock()
		app.saveRepoState(func(s *RepoState) {
			s.MashuraLevel = ""
			s.CounselMode = ""
			s.MaxCounsel = 0
			s.MaxCounselCapSet = false
			s.MashuraPanels = nil
			s.MashuraToolPanels = nil
			s.MashuraDefaultModel = ""
			s.MashuraMaxTokens = 0
			s.MashuraTimeoutSeconds = 0
		})
		return true, false, note(mashuraLevelSetNote(app, prev, ""))
	case "panel":
		if legacyBlocked("panel") {
			return true, false, note("ignored: the new mashura config block is present; /mashura panel and friends do not affect calls while it is")
		}
		return handleMashuraPanel(fields, app)
	case "map":
		if legacyBlocked("map") {
			return true, false, note("ignored: the new mashura config block is present; /mashura map does not affect calls while it is")
		}
		return handleMashuraMap(fields, app)
	case "model":
		if legacyBlocked("model") {
			return true, false, note("ignored: the new mashura config block is present; /mashura model does not affect calls while it is")
		}
		if len(fields) < 3 {
			return true, false, note("current default model: " + app.Cfg.OracleModel)
		}
		model := strings.Join(fields[2:], " ")
		app.Cfg.OracleModel = model
		app.saveRepoState(func(s *RepoState) { s.MashuraDefaultModel = model })
		return true, false, note("default model set: " + model)
	case "maxtokens":
		if legacyBlocked("maxtokens") {
			return true, false, note("ignored: the new mashura config block is present; /mashura maxtokens does not affect calls while it is")
		}
		if len(fields) < 3 {
			return true, false, note(fmt.Sprintf("current max tokens: %d", app.Cfg.OracleMaxTokens))
		}
		n, err := strconv.Atoi(fields[2])
		if err != nil || n <= 0 {
			return true, false, note("usage: /mashura maxtokens <N> (positive integer)")
		}
		app.Cfg.OracleMaxTokens = n
		app.saveRepoState(func(s *RepoState) { s.MashuraMaxTokens = n })
		return true, false, note(fmt.Sprintf("max tokens set: %d", n))
	case "timeout":
		if legacyBlocked("timeout") {
			return true, false, note("ignored: the new mashura config block is present; /mashura timeout does not affect calls while it is")
		}
		if len(fields) < 3 {
			return true, false, note(fmt.Sprintf("current timeout: %ds", app.Cfg.OracleTimeoutSeconds))
		}
		n, err := strconv.Atoi(fields[2])
		if err != nil || n <= 0 {
			return true, false, note("usage: /mashura timeout <seconds> (positive integer)")
		}
		app.Cfg.OracleTimeoutSeconds = n
		app.saveRepoState(func(s *RepoState) { s.MashuraTimeoutSeconds = n })
		return true, false, note(fmt.Sprintf("timeout set: %ds", n))
	default:
		return true, false, note(mashuraHelpText)
	}
}

// mashuraStatus renders the current mashura configuration as a readable string.
func mashuraStatus(app *App) string {
	var sb strings.Builder
	sb.WriteString("Mashūra Counsel Status\n")
	sb.WriteString("──────────────────────\n")
	if app.Cfg.Mashura != nil {
		sb.WriteString(mashuraLevelStatus(app))
		sb.WriteString(fmt.Sprintf("enabled:        %v\n", app.mashuraAvailable()))
	} else {
		sb.WriteString(fmt.Sprintf("default model:  %s\n", app.Cfg.OracleModel))
		sb.WriteString(fmt.Sprintf("max tokens:     %d\n", app.Cfg.OracleMaxTokens))
		sb.WriteString(fmt.Sprintf("timeout:        %ds\n", app.Cfg.OracleTimeoutSeconds))
		sb.WriteString(fmt.Sprintf("enabled:        %v\n", app.mashuraAvailable()))
	}

	// Panels
	if len(app.Cfg.MashuraPanels) == 0 {
		sb.WriteString("\npanels: (none — using built-in default)\n")
	} else {
		names := make([]string, 0, len(app.Cfg.MashuraPanels))
		for name := range app.Cfg.MashuraPanels {
			names = append(names, name)
		}
		sort.Strings(names)
		sb.WriteString(fmt.Sprintf("\npanels (%d):\n", len(names)))
		for _, name := range names {
			p := app.Cfg.MashuraPanels[name]
			mode := p.Mode
			if mode == "" {
				mode = "panel"
			}
			line := fmt.Sprintf("  %-15s  [%s]  %s", name, mode, strings.Join(p.Models, ", "))
			if _, keyErr := app.mashuraPanelKeys(p); keyErr != nil {
				line += fmt.Sprintf("\n                    ⚠ NOT USABLE: %v", keyErr)
			}
			sb.WriteString(line + "\n")
		}
	}

	// Tool → panel mappings
	if len(app.Cfg.MashuraToolPanels) == 0 {
		sb.WriteString("\ntool mappings: (none — all use default panel)\n")
	} else {
		sb.WriteString("\ntool mappings:\n")
		tools := []string{"review", "debug", "decide", "check"}
		for _, t := range tools {
			if panel, ok := app.Cfg.MashuraToolPanels[t]; ok {
				sb.WriteString(fmt.Sprintf("  %-8s → %s\n", t, panel))
			}
		}
	}

	return sb.String()
}

// handleMashuraPanel handles the /mashura panel subcommand.
func handleMashuraPanel(fields []string, app *App) (handled, quit bool, cmd Cmd) {
	note := func(text string) Cmd { return NoteCmd(text) }

	if len(fields) < 3 {
		return true, false, note("usage: /mashura panel add|rm|<name> ...\n" + mashuraHelpText)
	}

	sub := fields[2]
	switch sub {
	case "add":
		if len(fields) < 5 {
			return true, false, note("usage: /mashura panel add <name> <model1>[,model2,...] [--mode panel|fallback|debate]")
		}
		name := fields[3]
		modelsStr := fields[4]
		mode := "panel"
		// Parse optional --mode flag
		for i := 5; i < len(fields); i++ {
			if fields[i] == "--mode" && i+1 < len(fields) {
				mode = fields[i+1]
				break
			}
		}
		// Validate mode
		switch mode {
		case "panel", "fallback", "debate":
		default:
			return true, false, note(fmt.Sprintf("invalid mode %q (expected panel, fallback, or debate)", mode))
		}
		models := strings.Split(modelsStr, ",")
		for i, m := range models {
			models[i] = strings.TrimSpace(m)
		}
		if app.Cfg.MashuraPanels == nil {
			app.Cfg.MashuraPanels = make(map[string]config.MashuraPanelConfig)
		}
		app.Cfg.MashuraPanels[name] = config.MashuraPanelConfig{
			Models: models,
			Mode:   mode,
		}
		app.saveRepoState(func(s *RepoState) {
			if s.MashuraPanels == nil {
				s.MashuraPanels = make(map[string]config.MashuraPanelConfig)
			}
			s.MashuraPanels[name] = config.MashuraPanelConfig{
				Models: models,
				Mode:   mode,
			}
		})
		return true, false, note(fmt.Sprintf("panel %q added: [%s] %s", name, mode, strings.Join(models, ", ")))

	case "rm":
		if len(fields) < 4 {
			return true, false, note("usage: /mashura panel rm <name>")
		}
		name := fields[3]
		if _, ok := app.Cfg.MashuraPanels[name]; !ok {
			return true, false, note(fmt.Sprintf("panel %q not found", name))
		}
		delete(app.Cfg.MashuraPanels, name)
		// Also remove any tool mappings pointing to it
		for tool, panel := range app.Cfg.MashuraToolPanels {
			if panel == name {
				delete(app.Cfg.MashuraToolPanels, tool)
			}
		}
		app.saveRepoState(func(s *RepoState) {
			if s.MashuraPanels != nil {
				delete(s.MashuraPanels, name)
			}
			if s.MashuraToolPanels != nil {
				for tool, panel := range s.MashuraToolPanels {
					if panel == name {
						delete(s.MashuraToolPanels, tool)
					}
				}
			}
		})
		return true, false, note(fmt.Sprintf("panel %q removed", name))

	default:
		// /mashura panel <name> — show details or set mode
		name := sub
		panel, ok := app.Cfg.MashuraPanels[name]
		if !ok {
			return true, false, note(fmt.Sprintf("panel %q not found", name))
		}
		mode := panel.Mode
		if mode == "" {
			mode = "panel"
		}
		// Check for --mode flag
		for i := 3; i < len(fields); i++ {
			if fields[i] == "--mode" && i+1 < len(fields) {
				newMode := fields[i+1]
				switch newMode {
				case "panel", "fallback", "debate":
				default:
					return true, false, note(fmt.Sprintf("invalid mode %q (expected panel, fallback, or debate)", newMode))
				}
				panel.Mode = newMode
				app.Cfg.MashuraPanels[name] = panel
				app.saveRepoState(func(s *RepoState) {
					if s.MashuraPanels == nil {
						s.MashuraPanels = make(map[string]config.MashuraPanelConfig)
					}
					s.MashuraPanels[name] = panel
				})
				return true, false, note(fmt.Sprintf("panel %q mode set: %s", name, newMode))
			}
		}
		return true, false, note(fmt.Sprintf("panel %q [%s]:\n  models: %s", name, mode, strings.Join(panel.Models, ", ")))
	}
}

// handleMashuraMap handles the /mashura map subcommand.
func handleMashuraMap(fields []string, app *App) (handled, quit bool, cmd Cmd) {
	note := func(text string) Cmd { return NoteCmd(text) }

	if len(fields) < 3 {
		return true, false, note("usage: /mashura map <tool> [panel]\n  tools: review, debug, decide, check")
	}
	tool := fields[2]

	// Validate tool name
	validTools := map[string]bool{"review": true, "debug": true, "decide": true, "check": true}
	if !validTools[tool] {
		return true, false, note(fmt.Sprintf("unknown tool %q (expected review, debug, decide, or check)", tool))
	}

	if len(fields) < 4 {
		// Show current mapping
		if panel, ok := app.Cfg.MashuraToolPanels[tool]; ok {
			return true, false, note(fmt.Sprintf("%s → %s", tool, panel))
		}
		return true, false, note(fmt.Sprintf("%s → (default panel)", tool))
	}

	panelName := fields[3]
	// Validate panel exists
	if _, ok := app.Cfg.MashuraPanels[panelName]; !ok {
		return true, false, note(fmt.Sprintf("panel %q not found — create it first with /mashura panel add", panelName))
	}
	if app.Cfg.MashuraToolPanels == nil {
		app.Cfg.MashuraToolPanels = make(map[string]string)
	}
	app.Cfg.MashuraToolPanels[tool] = panelName
	app.saveRepoState(func(s *RepoState) {
		if s.MashuraToolPanels == nil {
			s.MashuraToolPanels = make(map[string]string)
		}
		s.MashuraToolPanels[tool] = panelName
	})
	return true, false, note(fmt.Sprintf("%s → %s", tool, panelName))
}

const mashuraHelpText = `/mashura commands:
  /mashura                        Show status (level, members, per-tool table)
  /mashura off|low|medium|high    Set the per-repo level
  /mashura reset                  Remove the per-repo level
  /mashura panel add <name> <models> [--mode panel|fallback|debate]  (legacy)
  /mashura panel rm <name>         Remove a named panel (legacy)
  /mashura panel <name>            Show panel details (legacy)
  /mashura panel <name> --mode <mode>  Set panel mode (legacy)
  /mashura map <tool> <panel>      Map a tool (review|debug|decide|check) to a panel (legacy)
  /mashura map <tool>              Show current mapping for a tool
  /mashura model <model-id>        Set the default model (legacy)
  /mashura maxtokens <N>           Set max tokens for mashura responses (legacy)
  /mashura timeout <seconds>       Set timeout for mashura calls (legacy)`

// mashuraLevelStatus renders the effective level survey for the new block:
// level + source, members, overrides, a per-tool effective table, and the
// equivalent config block as copyable JSON. check's low-cost exception is
// explicit in the table (stays at low below high).
func mashuraLevelStatus(app *App) string {
	level, members, source := app.effectiveMashura("review")
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("level:          %s (from %s)\n", level, source))
	sb.WriteString(fmt.Sprintf("members (%d):   %s\n", len(members), strings.Join(members, ", ")))
	if app.MashuraLevelOverride != "" {
		sb.WriteString(fmt.Sprintf("per-repo level: %s\n", app.MashuraLevelOverride))
	}
	if app.Cfg.Mashura != nil && len(app.Cfg.Mashura.Overrides) > 0 {
		sb.WriteString("overrides:\n")
		tools := make([]string, 0, len(app.Cfg.Mashura.Overrides))
		for t := range app.Cfg.Mashura.Overrides {
			tools = append(tools, t)
		}
		sort.Strings(tools)
		for _, t := range tools {
			ov := app.Cfg.Mashura.Overrides[t]
			parts := []string{}
			if ov.Level != "" {
				parts = append(parts, "level="+ov.Level)
			}
			if len(ov.Members) > 0 {
				parts = append(parts, "members=["+strings.Join(ov.Members, ", ")+"]")
			}
			sb.WriteString(fmt.Sprintf("  %-8s %s\n", t, strings.Join(parts, " ")))
		}
	} else {
		sb.WriteString("overrides:      (none)\n")
	}
	sb.WriteString("effective per tool:\n")
	sb.WriteString("  tool      level    reasoning  debate  struggle (level default)  [source]\n")
	counselMode, counselSrc := app.resolveCounselMode()
	for _, t := range []string{"review", "debug", "decide", "check"} {
		lv, _, src := app.effectiveMashura(t)
		r := config.LevelReasoning(t, lv)
		reasoning := r.Effort
		if r.MaxTokens > 0 {
			reasoning = fmt.Sprintf("%s/%d", r.Effort, r.MaxTokens)
		}
		debate := "no"
		if config.LevelDebate(lv) {
			debate = "yes"
		}
		// The struggle column shows what a detected struggle does at this
		// row's level by default; the effective action is the counsel mode
		// (with its source) on the closing line, which an explicit
		// /counsel or auto_counsel choice can override in either direction.
		struggle := "nothing"
		switch config.LevelAutoCounsel(lv) {
		case "suggest":
			struggle = "suggest"
		case "auto":
			struggle = "start"
		}
		sb.WriteString(fmt.Sprintf("  %-8s  %-8s %-10s %-6s %-20s [%s]\n", t, lv, reasoning, debate, struggle, src))
		if t == "check" && lv != config.MashuraLevelHigh {
			sb.WriteString("    (check stays at low below high)\n")
		}
	}
	sb.WriteString(fmt.Sprintf("counsel mode: %s (source: %s)\n", counselMode, counselSrc))
	if counselMode == "auto" {
		cap := app.autoCounselCap()
		how := "set by user"
		if app.counselCapIsDefault() {
			how = "default"
		}
		sb.WriteString(fmt.Sprintf("auto cap:      %d/turn (%s)\n", cap, how))
	}
	sb.WriteString("\nconfig block (copy into config.json):\n")
	block := *app.Cfg.Mashura
	if len(block.Members) == 0 {
		block.Members = members
	}
	b, _ := json.MarshalIndent(map[string]interface{}{"mashura": block}, "", "  ")
	sb.WriteString(string(b) + "\n")
	return sb.String()
}

// mashuraLevelSetNote composes the confirmation for a /mashura level change.
// When the tool set changes in a session that has already sent a request,
// the prompt-cache line is appended (A6). The tool set changes when the
// effective level crosses the off boundary (tools appear/disappear).
func mashuraLevelSetNote(app *App, prev, next string) string {
	effNext, membersNext, _ := app.effectiveMashura("review")
	var sb strings.Builder
	if next == "" {
		fmt.Fprintf(&sb, "per-repo Mashūra level removed (now %s)\n", effNext)
	} else {
		fmt.Fprintf(&sb, "per-repo Mashūra level set: %s (effective %s, %d member(s))\n", next, effNext, len(membersNext))
	}
	// Tool set changes (tools appear or vanish) only when the effective level
	// crosses the off boundary between the previous and next states.
	var prevEffective string
	if app.Cfg.Mashura == nil {
		prevEffective = prev
	} else if prev == "" {
		// No override before: effective was the config level.
		prevEffective, _, _ = app.Cfg.ResolveEffective("review", "")
	} else {
		prevEffective, _, _ = app.Cfg.ResolveEffective("review", prev)
	}
	toolSetChanges := (prevEffective == config.MashuraLevelOff) != (effNext == config.MashuraLevelOff)
	if toolSetChanges && app.sessionHasSent() {
		sb.WriteString("Changing this mid-session resets the prompt cache.\n")
	}
	return sb.String()
}
