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
//   - Output is bounded (git_log `-n`; diff/show/blame byte-capped via a
//     temp-file capture that preserves git's real exit status), and git
//     failures are classified by exit code — never by matching output content
//     (a successful command may legitimately emit the phrases the classifier
//     once matched, e.g. the tools' own source).

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

// classifyGitError maps a git FAILURE to a stable, model-actionable message.
// It is called ONLY when git already exited non-zero (err != nil) — never to
// DETECT failure from the content of a successful command. Content matching on
// success was the bug: git_show/git_diff/git_blame legitimately emit the repo
// files being inspected, and those files now contain "fatal:"/"not a git
// repository" as string literals, so the classifier saw its own source text
// and concluded the repo was broken.
func classifyGitError(out string, err error) string {
	msg := strings.TrimSpace(out)
	if strings.Contains(msg, "not a git repository") {
		return "not a git repository"
	}
	if strings.Contains(msg, "does not have any commits") {
		return "repository has no commits yet"
	}
	// Surface git's own first line (LC_ALL=C makes it locale-stable) — the
	// model needs it for bad-object/ambiguous-ref/merge-base failures.
	return firstLineOrEmpty(msg, err.Error())
}

// gitRun runs a git command (no output pipe) with the hardening prefix and
// returns the raw output. The exit status here IS git's (no pipeline), so err
// is the reliable failure signal — success output is never content-matched.
func (a *App) gitRun(ctx context.Context, cmd string) (string, error) {
	full := gitBaseEnv() + "git " + cmd
	out, err := a.Exec.RunShell(ctx, full)
	if err != nil {
		return "", fmt.Errorf("%s", classifyGitError(out, err))
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
	return a.gitRunCapped(ctx, cmd, "diff", gitDiffCapBytes)
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
	return a.gitRunCapped(ctx, cmd, "show", gitDiffCapBytes)
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
	// Cap the raw porcelain output (temp-file + real exit status) AND the
	// parsed hunks, so a huge generated file cannot produce unbounded output.
	out := a.gitRunCapped(ctx, cmd, "blame", gitBlameCapBytes)
	if strings.HasPrefix(out, "ERROR:") {
		return out
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

// gitRunCapped runs a raw-text git command, capturing its REAL exit status
// (temp file + `exit $rc`) and then byte-capping the output in Go. The cap is
// applied before the result reaches the transcript so a huge diff never floods
// context.
//
// Why not `| head -c`: the executor's shell is dash (`sh`), which has no
// `pipefail`, so a pipeline's exit status is the LAST command's — head's 0 —
// and a git failure (bad ref, non-repo) was invisible. Worse, the old code
// papered over that with CONTENT matching, which misfired the moment the repo
// files under inspection contained "fatal:"/"not a git repository" as string
// literals (the git-tools source itself) — git_show HEAD broke on this repo.
// The temp-file pattern preserves git's exit code while still capping output.
func (a *App) gitRunCapped(ctx context.Context, cmd, label string, capBytes int) string {
	// temp=$(mktemp); git ... > "$temp" 2>&1; rc=$?; head -c <cap+1> "$temp";
	// rm -f "$temp"; exit $rc
	// head -c cap+1 lets us detect truncation: if we got cap+1 bytes, output
	// exceeded the cap.
	full := gitBaseEnv() + fmt.Sprintf(
		`tmp=$(mktemp) && git %s > "$tmp" 2>&1; rc=$?; head -c %d "$tmp"; rm -f "$tmp"; exit $rc`,
		cmd, capBytes+1)
	out, err := a.Exec.RunShell(ctx, full)
	if err != nil {
		// git failed; out holds git's stderr (trimmed to cap+1 bytes). Classify
		// the FAILURE only (we already know it failed from err), never the
		// content of a successful command.
		return "ERROR: " + classifyGitError(out, err)
	}
	truncated := len(out) > capBytes
	if truncated {
		out = out[:capBytes]
		out = truncateUTF8(out, capBytes)
		out += fmt.Sprintf("\n… [%s output truncated at %d bytes — narrow with path/rev]", label, capBytes)
	}
	if strings.ContainsRune(out, 0) {
		return "ERROR: binary content — not readable as text."
	}
	if strings.TrimSpace(out) == "" {
		return "(no changes)"
	}
	return out
}
