package agent

import (
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
//	/mashura                        Show status (panels, tool mappings, limits)
//	/mashura panel add <name> <models> [--mode panel|fallback|fusion|debate]
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

	switch fields[1] {
	case "panel":
		return handleMashuraPanel(fields, app)
	case "map":
		return handleMashuraMap(fields, app)
	case "model":
		if len(fields) < 3 {
			return true, false, note("current default model: " + app.Cfg.OracleModel)
		}
		model := strings.Join(fields[2:], " ")
		app.Cfg.OracleModel = model
		app.saveRepoState(func(s *RepoState) { s.MashuraDefaultModel = model })
		return true, false, note("default model set: " + model)
	case "maxtokens":
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
	sb.WriteString(fmt.Sprintf("default model:  %s\n", app.Cfg.OracleModel))
	sb.WriteString(fmt.Sprintf("max tokens:     %d\n", app.Cfg.OracleMaxTokens))
	sb.WriteString(fmt.Sprintf("timeout:        %ds\n", app.Cfg.OracleTimeoutSeconds))
	sb.WriteString(fmt.Sprintf("enabled:        %v\n", app.mashuraAvailable()))

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
			sb.WriteString(fmt.Sprintf("  %-15s  [%s]  %s\n", name, mode, strings.Join(p.Models, ", ")))
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
			return true, false, note("usage: /mashura panel add <name> <model1>[,model2,...] [--mode panel|fallback|fusion|debate]")
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
		case "panel", "fallback", "fusion", "debate":
		default:
			return true, false, note(fmt.Sprintf("invalid mode %q (expected panel, fallback, fusion, or debate)", mode))
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
				case "panel", "fallback", "fusion", "debate":
				default:
					return true, false, note(fmt.Sprintf("invalid mode %q (expected panel, fallback, fusion, or debate)", newMode))
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
  /mashura                        Show status (panels, tool mappings, limits)
  /mashura panel add <name> <models> [--mode panel|fallback|fusion|debate]
  /mashura panel rm <name>         Remove a named panel
  /mashura panel <name>            Show panel details
  /mashura panel <name> --mode <mode>  Set panel mode
  /mashura map <tool> <panel>      Map a tool (review|debug|decide|check) to a panel
  /mashura map <tool>              Show current mapping for a tool
  /mashura model <model-id>        Set the default model
  /mashura maxtokens <N>           Set max tokens for mashura responses
  /mashura timeout <seconds>       Set timeout for mashura calls`
