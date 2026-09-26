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
	"strconv"
	"strings"
	"time"

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
	BudgetUSD        float64
	Continuous       *wiring.ContinuousOptions
	verifyCmds       []string
	continueFlag     bool
	maxTurns         int
	maxTime          time.Duration
	sawVerifyCmd     bool
	sawMaxTurns      bool
	sawMaxTime       bool
}

// parseDurationStrict parses a Go duration with no leading/trailing junk.
func parseDurationStrict(s string) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("requires a positive duration like 30m, got %q", s)
	}
	return d, nil
}

// parseRunArgs parses the args that follow "run":
//
//	[--plan] [--auto] [--allow-destructive] [--allow-external]
//	[--auto-counsel] [--max-counsel N] [--no-oracle] [--transcript <file>]
//	[--attach-image <path>] [--policy <path>] [--profile <name>] [--verify]
//	[--budget $N] [--continue --max-turns N --max-time D --verify-cmd CMD] "<task>"
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
		case "--budget":
			i++
			if i >= len(args) {
				return "", false, flags, fmt.Errorf("--budget requires a dollar amount")
			}
			v, sErr := fmt.Sscanf(args[i], "%f", &flags.BudgetUSD)
			if sErr != nil || v != 1 {
				return "", false, flags, fmt.Errorf("--budget requires a number, got %q", args[i])
			}
			if flags.BudgetUSD <= 0 {
				return "", false, flags, fmt.Errorf("--budget must be > 0 (got %.2f)", flags.BudgetUSD)
			}
		case "--continue":
			flags.continueFlag = true
		case "--max-turns":
			i++
			if i >= len(args) {
				return "", false, flags, fmt.Errorf("--max-turns requires an integer")
			}
			n, sErr := strconv.Atoi(args[i])
			if sErr != nil || n <= 0 {
				return "", false, flags, fmt.Errorf("--max-turns requires a positive integer, got %q", args[i])
			}
			flags.maxTurns, flags.sawMaxTurns = n, true
		case "--max-time":
			i++
			if i >= len(args) {
				return "", false, flags, fmt.Errorf("--max-time requires a duration")
			}
			d, dErr := parseDurationStrict(args[i])
			if dErr != nil {
				return "", false, flags, fmt.Errorf("--max-time %v", dErr)
			}
			flags.maxTime, flags.sawMaxTime = d, true
		case "--verify-cmd":
			i++
			if i >= len(args) {
				return "", false, flags, fmt.Errorf("--verify-cmd requires a command")
			}
			if strings.TrimSpace(args[i]) == "" {
				return "", false, flags, fmt.Errorf("--verify-cmd requires a non-empty command")
			}
			flags.verifyCmds = append(flags.verifyCmds, args[i])
			flags.sawVerifyCmd = true
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
			"usage: wakil run [--plan] [--auto] [--allow-destructive] [--allow-external] [--auto-counsel [--max-counsel N]] [--no-oracle] [--transcript <file>] [--attach-image <path>] [--policy <path>] [--profile <name>] [--verify] [--budget $N] " +
				"[--continue --max-turns N --max-time D [--verify-cmd CMD]] \"<task>\"")
	}
	// Default cap: 3 auto-counsel calls when --auto-counsel is set without --max-counsel.
	if flags.AutoCounsel && flags.MaxCounsel == 0 {
		flags.MaxCounsel = 3
	}
	// Continuous-only flags are rejected without --continue, so bounds can never
	// be silently ignored.
	if !flags.continueFlag {
		if flags.sawMaxTurns || flags.sawMaxTime || flags.sawVerifyCmd {
			return "", false, flags, fmt.Errorf("--max-turns, --max-time and --verify-cmd require --continue")
		}
		return task, planMode, flags, nil
	}
	if !flags.sawMaxTurns || !flags.sawMaxTime {
		return "", false, flags, fmt.Errorf("--continue requires both --max-turns and --max-time")
	}
	flags.Continuous = &wiring.ContinuousOptions{
		MaxTurns:       flags.maxTurns,
		MaxTime:        flags.maxTime,
		VerifyCommands: flags.verifyCmds,
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
		BudgetUSD:        flags.BudgetUSD,
		Continuous:       flags.Continuous,
	})
}
