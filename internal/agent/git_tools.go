package agent

// git_tools.go — handlers for the structured read-only git tools (card #137).
//
// Each handler builds a git command from structured JSON arguments and runs it
// via Exec.RunShell, then parses the output into structured JSON (status/log/
// blame) or returns capped raw text (diff/show). Security model (folded from
// Mashūra plan review):
//
//   - No model-supplied flags: the command string is assembled from fixed
//     subcommand + allowlisted options; refs and paths are the only free inputs.
//   - Refs are validated (reject leading '-', ':', '^', whitespace, NUL) so a
//     value like "--output=/x" cannot become a git option (shellQuote stops
//     shell metacharacters, NOT git option parsing — validation is the git-level
//     barrier). The `--` end-of-options separator precedes every path.
//   - Paths are confined via Exec.ConfinePath before use.
//   - Repo-config code-execution surface is neutralized: `-c core.fsmonitor=false`
//     and `-c core.pager=cat` (no hook/pager), `--no-ext-diff --no-textconv`
//     (diff/show/log never invoke external diff/textconv drivers), and
//     `GIT_PAGER=cat GIT_TERMINAL_PROMPT=0` (no pager, no credential prompts).
//     "Read-only" therefore means no network, no prompts, no external helpers.
//   - Output is bounded in-command (git_log `-n`; diff/show/blame piped through
//     `head -c`), not merely truncated after the fact.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/treeol/wakil/internal/proxy"
	wtools "github.com/treeol/wakil/internal/tools"
)

const (
	// gitDiffCapBytes caps diff/show raw output. In-command via `head -c`, so a
	// huge diff never transits the executor as unbounded output.
	gitDiffCapBytes = 64 * 1024
	// gitBlameCapBytes caps blame raw porcelain output. Larger than diff because
	// porcelain blame is verbose (metadata per commit block); the parsed hunks
	// are additionally capped by gitBlameMaxHunks.
	gitBlameCapBytes = 256 * 1024
	// gitLogDefaultCount is the default `-n`; gitLogMaxCount is the hard cap.
	gitLogDefaultCount = 20
	gitLogMaxCount     = 100
	// gitStatusMaxEntries caps the number of status records parsed (still far
	// more than a model can act on in one turn).
	gitStatusMaxEntries = 200
	// gitBlameMaxHunks caps the parsed blame hunks (a defensive bound on the
	// JSON result size independent of the raw byte cap).
	gitBlameMaxHunks = 500
)

// gitBaseEnv returns the hardening env prefix applied to every git command.
// LC_ALL=C makes error messages locale-independent (the non-repo/fatal markers
// we string-match are English in C locale). GIT_OPTIONAL_LOCKS=0 prevents
// opportunistic index-refresh/lock writes; GIT_NO_LAZY_FETCH=1 prevents
// partial-clone lazy fetches (network) for supported git versions (harmlessly
// ignored otherwise). GIT_PAGER/GIT_TERMINAL_PROMPT suppress the pager and any
// credential prompt.
func gitBaseEnv() string {
	return "LC_ALL=C GIT_OPTIONAL_LOCKS=0 GIT_NO_LAZY_FETCH=1 GIT_PAGER=cat GIT_TERMINAL_PROMPT=0 "
}

// gitBaseArgs returns the hardening flags shared by every git command.
// `-c core.fsmonitor=false -c core.pager=cat -c log.showSignature=false`
// neutralize repo-config hooks, pagers, and the gpg signature-program vector;
// `--no-pager` is belt-and-suspenders on top of the pager config.
func gitBaseArgs() string {
	return "--no-pager -c core.fsmonitor=false -c core.pager=cat -c log.showSignature=false"
}

// gitFatal reports whether git output carries a fatal/error marker, used to
// classify git failures from CONTENT (the pipeline in gitRunCapped masks the
// exit status — see below — so content is the reliable signal in both paths).
func gitFatal(msg string) bool {
	return strings.Contains(msg, "fatal:") ||
		strings.Contains(msg, "not a git repository") ||
		strings.Contains(msg, "does not have any commits")
}

// gitRun runs a git command (no output pipe) with the hardening prefix and
// returns the raw output. Non-repo and other fatal errors are surfaced as a
// stable, model-actionable error rather than raw git stderr. The exit status
// here IS git's (no pipeline), so err is reliable.
func (a *App) gitRun(ctx context.Context, cmd string) (string, error) {
	full := gitBaseEnv() + "git " + cmd
	out, err := a.Exec.RunShell(ctx, full)
	msg := strings.TrimSpace(out)
	if err != nil || gitFatal(msg) {
		if gitFatal(msg) {
			return "", fmt.Errorf("not a git repository (or no commits yet, or invalid revision)")
		}
		return "", fmt.Errorf("%s", firstLineOrEmpty(msg, err.Error()))
	}
	return out, nil
}

// validateGitRef rejects revision strings that could be parsed as git options
// or that carry shell/formatting hazards. It is intentionally conservative:
// useful expressions like HEAD~2, main...feature, A..B, and @ are allowed;
// anything starting with '-', ':', '^', containing whitespace/NUL, or empty
// (callers use "" to mean "no rev") is rejected.
func validateGitRef(rev string) error {
	if rev == "" {
		return nil
	}
	if strings.ContainsAny(rev, " \t\n\r\x00") {
		return fmt.Errorf("revision must not contain whitespace or NUL: %q", rev)
	}
	if strings.HasPrefix(rev, "-") || strings.HasPrefix(rev, ":") || strings.HasPrefix(rev, "^") {
		return fmt.Errorf("revision must not start with '-', ':' or '^': %q", rev)
	}
	return nil
}

// firstLineOrEmpty returns the first non-empty line of s, else fallback.
func firstLineOrEmpty(s, fallback string) string {
	for _, l := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			return t
		}
	}
	return fallback
}

// handleGitStatus implements git_status.
func (a *App) handleGitStatus(ctx context.Context, tc proxy.ToolCall) string {
	out, err := a.gitRun(ctx, gitBaseArgs()+" status --porcelain=v1 -z --branch")
	if err != nil {
		return "ERROR: " + err.Error()
	}
	res := wtools.ParseGitStatus(out)
	if len(res.Entries) > gitStatusMaxEntries {
		res.Entries = res.Entries[:gitStatusMaxEntries]
		res.Truncated = true
	}
	b, err := json.Marshal(res)
	if err != nil {
		return "ERROR: could not marshal status: " + err.Error()
	}
	return string(b)
}

// handleGitDiff implements git_diff.
func (a *App) handleGitDiff(ctx context.Context, tc proxy.ToolCall) string {
	var args struct {
		Staged bool   `json:"staged"`
		Rev    string `json:"rev"`
		Path   string `json:"path"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	if err := validateGitRef(args.Rev); err != nil {
		return "ERROR: " + err.Error()
	}
	cmd := gitBaseArgs() + " diff --no-ext-diff --no-textconv"
	if args.Staged {
		cmd += " --cached"
	}
	if args.Rev != "" {
		cmd += " " + shellQuote(args.Rev)
	}
	if args.Path != "" {
		canonical, err := a.Exec.ConfinePath(ctx, args.Path)
		if err != nil {
			return "ERROR: " + err.Error()
		}
		cmd += " -- " + shellQuote(canonical)
	}
	return a.gitRunCapped(ctx, cmd, "diff")
}

// handleGitLog implements git_log.
func (a *App) handleGitLog(ctx context.Context, tc proxy.ToolCall) string {
	var args struct {
		Count int    `json:"count"`
		Rev   string `json:"rev"`
		Path  string `json:"path"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	if err := validateGitRef(args.Rev); err != nil {
		return "ERROR: " + err.Error()
	}
	n := args.Count
	if n <= 0 {
		n = gitLogDefaultCount
	}
	if n > gitLogMaxCount {
		n = gitLogMaxCount
	}
	cmd := fmt.Sprintf("%s log --no-ext-diff --no-textconv -n %d --format=%s", gitBaseArgs(), n, shellQuote(wtools.GitLogFormat()))
	if args.Rev != "" {
		cmd += " " + shellQuote(args.Rev)
	}
	if args.Path != "" {
		canonical, err := a.Exec.ConfinePath(ctx, args.Path)
		if err != nil {
			return "ERROR: " + err.Error()
		}
		cmd += " -- " + shellQuote(canonical)
	}
	out, err := a.gitRun(ctx, cmd)
	if err != nil {
		return "ERROR: " + err.Error()
	}
	entries := wtools.ParseGitLog(out)
	b, err := json.Marshal(entries)
	if err != nil {
		return "ERROR: could not marshal log: " + err.Error()
	}
	return string(b)
}

// handleGitShow implements git_show.
func (a *App) handleGitShow(ctx context.Context, tc proxy.ToolCall) string {
	var args struct {
		Rev string `json:"rev"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	if err := validateGitRef(args.Rev); err != nil {
		return "ERROR: " + err.Error()
	}
	rev := args.Rev
	if rev == "" {
		rev = "HEAD"
	}
	cmd := fmt.Sprintf("%s show --no-ext-diff --no-textconv --stat --patch %s", gitBaseArgs(), shellQuote(rev))
	return a.gitRunCapped(ctx, cmd, "show")
}

// handleGitBlame implements git_blame.
func (a *App) handleGitBlame(ctx context.Context, tc proxy.ToolCall) string {
	var args struct {
		Path string `json:"path"`
		Rev  string `json:"rev"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	if args.Path == "" {
		return "ERROR: path is required"
	}
	if err := validateGitRef(args.Rev); err != nil {
		return "ERROR: " + err.Error()
	}
	canonical, err := a.Exec.ConfinePath(ctx, args.Path)
	if err != nil {
		return "ERROR: " + err.Error()
	}
	cmd := fmt.Sprintf("%s blame --porcelain", gitBaseArgs())
	if args.Rev != "" {
		cmd += " " + shellQuote(args.Rev)
	}
	cmd += " -- " + shellQuote(canonical)
	// Cap the raw porcelain output (in-command) AND the parsed hunks, so a
	// huge generated file cannot produce unbounded output or JSON.
	full := gitBaseEnv() + "git " + cmd + fmt.Sprintf(" | head -c %d", gitBlameCapBytes)
	out, err := a.Exec.RunShell(ctx, full)
	msg := strings.TrimSpace(out)
	if gitFatal(msg) {
		return "ERROR: not a git repository (or no commits yet, or invalid revision)"
	}
	if err != nil && msg == "" {
		return "ERROR: " + err.Error()
	}
	if strings.ContainsRune(out, 0) {
		return "ERROR: binary file — blame is only meaningful for text files."
	}
	hunks := wtools.ParseGitBlame(out)
	if len(hunks) > gitBlameMaxHunks {
		hunks = hunks[:gitBlameMaxHunks]
	}
	b, err := json.Marshal(hunks)
	if err != nil {
		return "ERROR: could not marshal blame: " + err.Error()
	}
	return string(b)
}

// gitRunCapped runs a raw-text git command with an in-command byte cap (piped
// through `head -c`), for diff/show whose output the model reads as text. The
// cap is applied in the pipeline so a huge diff never transits the executor as
// unbounded output.
//
// Exit-status caveat: under `sh -c` (no pipefail, see exec.runFromRoot) the
// pipeline's exit status is `head`'s (0), so a git failure is NOT reflected in
// the error return. We therefore classify git failures by CONTENT
// (gitFatal), which CombinedOutput reliably merges. head closing early on
// truncation causes git to SIGPIPE, but that is the intended truncation path
// and head still exits 0 — so err is only meaningful for non-git failures
// (shell/executor errors), and git errors are detected from msg.
func (a *App) gitRunCapped(ctx context.Context, cmd, label string) string {
	full := gitBaseEnv() + "git " + cmd + fmt.Sprintf(" | head -c %d", gitDiffCapBytes)
	out, err := a.Exec.RunShell(ctx, full)
	msg := strings.TrimSpace(out)
	if gitFatal(msg) {
		return "ERROR: not a git repository (or no commits yet, or invalid revision)"
	}
	if err != nil && msg == "" {
		// Executor-level failure (shell couldn't run), not a git error.
		return "ERROR: " + err.Error()
	}
	// Detect truncation: head -c N emits exactly N bytes when the source is
	// longer. We can't distinguish "exactly N bytes of output" from "truncated"
	// reliably from length alone, so we mark truncation whenever the output
	// reached the cap — conservative, since an exactly-cap-sized diff also gets
	// the marker, which is the safe direction (tells the model to narrow rather
	// than trust a possibly-cut diff).
	if len(out) >= gitDiffCapBytes {
		out = truncateUTF8(out, gitDiffCapBytes)
		out += fmt.Sprintf("\n… [%s output truncated at %d bytes — narrow with path/rev]", label, gitDiffCapBytes)
	}
	if strings.ContainsRune(out, 0) {
		return "ERROR: binary content — not readable as text."
	}
	if strings.TrimSpace(out) == "" {
		return "(no changes)"
	}
	return out
}
