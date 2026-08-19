package main

// run.go is the CLI shim for the "wakil run" subcommand (card #148 chunk 7,
// plan D19). Argument parsing and stderr usage live here; all bootstrap/runtime
// policy (App construction, session host, event projection, teardown) lives in
// internal/wiring. This file must NOT import internal/agent or internal/tui.
//
// Exit codes are re-exported from wiring so existing tests keep compiling.

import (
	"fmt"
	"os"
	"strings"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/wiring"
)

const (
	ExitOK             = wiring.ExitOK
	ExitDeclined       = wiring.ExitDeclined
	ExitGaps           = wiring.ExitGaps
	ExitError          = wiring.ExitError
	ExitBackendFailure = wiring.ExitBackendFailure
)

// RunFlags is the parsed form of the run-subcommand flags. It is a local
// mirror of wiring.HeadlessOptions, kept here so parseRunArgs (and its tests)
// stay in package main without importing wiring's runtime policy types.
type RunFlags struct {
	Auto             bool
	AllowDestructive bool
	NoOracle         bool
	TranscriptFile   string
	AllowExternal    bool
	AutoCounsel      bool
	MaxCounsel       int
	AttachImage      string
	PolicyPath       string
	ProfileName      string
	Verify           bool
}

// parseRunArgs parses the args that follow "run":
//
//	[--plan] [--auto] [--allow-destructive] [--allow-external]
//	[--auto-counsel] [--max-counsel N] [--no-oracle] [--transcript <file>]
//	[--attach-image <path>] [--policy <path>] [--profile <name>] [--verify] "<task>"
func parseRunArgs(args []string) (task string, planMode bool, flags RunFlags, err error) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--plan":
			planMode = true
		case "--auto":
			flags.Auto = true
		case "--allow-destructive":
			flags.AllowDestructive = true
		case "--allow-external":
			flags.AllowExternal = true
		case "--auto-counsel":
			flags.AutoCounsel = true
		case "--max-counsel":
			i++
			if i >= len(args) {
				return "", false, flags, fmt.Errorf("--max-counsel requires an integer")
			}
			if n, sErr := fmt.Sscanf(args[i], "%d", &flags.MaxCounsel); n != 1 || sErr != nil {
				return "", false, flags, fmt.Errorf("--max-counsel requires an integer, got %q", args[i])
			}
		case "--no-oracle":
			flags.NoOracle = true
		case "--transcript":
			i++
			if i >= len(args) {
				return "", false, flags, fmt.Errorf("--transcript requires a file path")
			}
			flags.TranscriptFile = args[i]
		case "--attach-image":
			i++
			if i >= len(args) {
				return "", false, flags, fmt.Errorf("--attach-image requires a file path")
			}
			flags.AttachImage = args[i]
		case "--policy":
			i++
			if i >= len(args) {
				return "", false, flags, fmt.Errorf("--policy requires a file path")
			}
			flags.PolicyPath = args[i]
		case "--profile":
			i++
			if i >= len(args) {
				return "", false, flags, fmt.Errorf("--profile requires a name")
			}
			flags.ProfileName = args[i]
		case "--verify":
			flags.Verify = true
		default:
			if strings.HasPrefix(args[i], "-") {
				return "", false, flags, fmt.Errorf("unknown flag: %s", args[i])
			}
			if task != "" {
				return "", false, flags, fmt.Errorf("unexpected argument: %s", args[i])
			}
			task = args[i]
		}
	}
	if task == "" {
		return "", false, flags, fmt.Errorf(
			"usage: wakil run [--plan] [--auto] [--allow-destructive] [--allow-external] [--auto-counsel [--max-counsel N]] [--no-oracle] [--transcript <file>] [--attach-image <path>] [--policy <path>] [--profile <name>] [--verify] \"<task>\"")
	}
	// Default cap: 3 auto-counsel calls when --auto-counsel is set without --max-counsel.
	if flags.AutoCounsel && flags.MaxCounsel == 0 {
		flags.MaxCounsel = 3
	}
	return task, planMode, flags, nil
}

// RunHeadless is the CLI entry point for "wakil run". It parses the flags and
// delegates to wiring.RunHeadless. Returns the process exit code.
func RunHeadless(cfg config.Config, args []string) int {
	task, planMode, flags, err := parseRunArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return ExitError
	}
	return wiring.RunHeadless(cfg, task, wiring.HeadlessOptions{
		PlanMode:         planMode,
		Auto:             flags.Auto,
		AllowDestructive: flags.AllowDestructive,
		AllowExternal:    flags.AllowExternal,
		NoOracle:         flags.NoOracle,
		AutoCounsel:      flags.AutoCounsel,
		MaxCounsel:       flags.MaxCounsel,
		AttachImage:      flags.AttachImage,
		PolicyPath:       flags.PolicyPath,
		ProfileName:      flags.ProfileName,
		Verify:           flags.Verify,
		TranscriptFile:   flags.TranscriptFile,
	})
}
