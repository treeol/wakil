package verify

// parse.go: structured failure extraction from verification command output.
//
// v1 scope is deliberately narrow and conservative (per the Mashūra plan
// review): Go `go test` output only, top-level test names only, and a rerun
// suggestion that is *never executed here* — it is a shell string for the
// caller to surface/run through the normal consent gate.
//
// The parser is pure (no I/O, no globals) so it stays unit-testable inside
// this package, matching detect.go and summarize.go.
//
// BEST-EFFORT CONTRACT: the parser reads plain-text `go test` output, which
// is not a stable, spoof-proof format. A test's own output can print a line
// that looks like `--- FAIL: TestFake` and be misread as a failure. Extracted
// failures are therefore evidence, not guaranteed facts; `go test -json` is
// the known upgrade path that eliminates this and the regex fragility.
// Unrecognized input yields an empty (nil) result, never a fabricated one.

import (
	"regexp"
	"strings"
)

// maxFailures caps the number of parsed failures retained per command. A
// suite with hundreds of failures would otherwise produce a huge -run regex
// (arg-length limits) and bloat the workflow briefing (which competes for a
// ~16 KB budget — see OutputCap). Excess failures are dropped; the caller's
// summary carries the raw (capped) output for the rest, and RerunCommand's
// omission marker reports the shortfall via the retained count.
const maxFailures = 20

// maxNameLen caps a single parsed test name. Go identifiers are unbounded in
// principle; a pathological name would blow the -run arg and the summary.
// A name longer than this is dropped as unparseable (best-effort).
const maxNameLen = 256

// maxScanBytes bounds the raw output the parser will scan. Output is capped
// at OutputCap (4 KB) for storage, but ParseFailures receives the RAW output,
// which a hostile or pathological runner could make arbitrarily large.
const maxScanBytes = 1 << 20 // 1 MiB

// goTestNameRe matches a top-level `--- FAIL: Name` line at column 0.
//
// Anchored at line start: subtest failures are printed with leading
// indentation (`    --- FAIL: Parent/sub`) and are deliberately NOT matched —
// a subtest rerun still targets its top-level parent. `(?m)` so the anchor
// applies per line. The name group is [A-Za-z_][A-Za-z0-9_]* — a strict
// ASCII identifier charset — so a hostile repo cannot smuggle shell/regex
// metacharacters into a parsed name. (ASCII-only is a documented limitation:
// Go permits Unicode identifiers, which this v1 parser will not see.)
var goTestNameRe = regexp.MustCompile(`(?m)^--- FAIL: ([A-Za-z_][A-Za-z0-9_]*)\s`)

// ansiRe strips the common CSI/SGR sequences (color codes) found in TTY and
// CI-colored output, so colored and plain output parse the same. It does NOT
// claim to strip every control sequence (OSC hyperlinks etc.) — those are
// out of scope for v1.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

// ParseFailures extracts structured failures from a verification command's
// raw (untruncated) output.
//
// Supported today: a plain `go test` command (see isGoTestCommand) whose
// output contains top-level `--- FAIL:` lines. Everything else — `go vet`,
// build failures, pytest, jest, cargo, wrappers like `make test`/`gotestsum`,
// environment prefixes, compound shell commands — returns nil. This is
// intentional: emitting a guess as a structured fact is worse than emitting
// nothing.
//
// Guarantees: pure; deterministic; results are de-duplicated and
// order-preserving (first occurrence wins); capped at maxFailures by count
// AND by bytes; bounded scanning (never scans more than maxScanBytes); never
// panics on malformed input.
func ParseFailures(cmd, output string) []Failure {
	if !isGoTestCommand(cmd) || output == "" {
		return nil
	}

	// Bound the scan: strip ANSI and normalize CRLF only up to maxScanBytes.
	if len(output) > maxScanBytes {
		output = output[:maxScanBytes]
	}
	clean := ansiRe.ReplaceAllString(output, "")
	clean = strings.ReplaceAll(clean, "\r\n", "\n")

	seen := make(map[string]bool, maxFailures)
	var failures []Failure
	var nameBytes int

	// Manual cursor over matches so scanning stops as soon as the bounded
	// result is complete, instead of FindAllStringSubmatch collecting every
	// match up front. clean is already capped at maxScanBytes above.
	var idx int
	for len(failures) < maxFailures {
		loc := goTestNameRe.FindStringSubmatchIndex(clean[idx:])
		if loc == nil {
			break
		}
		name := clean[idx+loc[2] : idx+loc[3]]
		idx += loc[1] // advance past this match

		if len(name) > maxNameLen || seen[name] {
			continue
		}
		seen[name] = true
		// Aggregate byte budget: many long names would still bloat the -run
		// arg and the summary. Stop once the running total would exceed a
		// generous per-command budget.
		if nameBytes+len(name) > maxNameLen*maxFailures {
			break
		}
		nameBytes += len(name)
		failures = append(failures, Failure{Test: name})
	}
	return failures
}

// isGoTestCommand reports whether the command string is a plain `go test`
// invocation that ParseFailures can interpret. The test is deliberately
// strict:
//
//   - Only a bare "go test" or "go test <args>" prefix is accepted. A leading
//     path (`./bin/go test`), `go -C dir test`, wrappers, and environment
//     prefixes are all rejected — their output format is not guaranteed to
//     be plain go test output.
//   - Shell operators, redirections, newlines, and pipelines are rejected:
//     appending `-run` to `go test ... && other` would bind the filter to the
//     wrong command. Compound commands are therefore unsupported.
func isGoTestCommand(cmd string) bool {
	trimmed := strings.TrimSpace(cmd)
	if trimmed == "go test" {
		return true
	}
	if !strings.HasPrefix(trimmed, "go test ") {
		return false
	}
	// Reject shell metacharacters and compound syntax. This is a conservative
	// deny-list of the characters sh -c interprets structurally; anything
	// containing them is not a plain command we can safely rewrite.
	if strings.ContainsAny(trimmed, "|&;<>`$()\n\r\t") {
		return false
	}
	return true
}

// goIdentifierRe is the strict ASCII Go test-function charset for rerun
// safety. Unicode identifiers are rejected (documented false-negative).
var goIdentifierRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// RerunCommand builds a targeted rerun command for a failed `go test` run,
// returning (command, ok). ok is false when a safe targeted command cannot be
// produced, in which case command is empty and the caller should fall back to
// re-running the original command (or skip a targeted rerun entirely).
//
// Safety contract (per the Mashūra reviews — test names are repo-controlled
// bytes, an injection surface):
//   - Only plain `go test` commands are rewritable (see isGoTestCommand);
//     everything else returns ok=false.
//   - Every test name must match goIdentifierRe (strict ASCII identifier); a
//     name that fails this check makes the whole call return ok=false — never
//     a partial, possibly-injectable command.
//   - Names are regexp.QuoteMeta'd and single-quoted for the shell, so no
//     name can escape into shell syntax.
//   - The original command's flags and package scope are preserved verbatim;
//     only a `-run '...'` flag is appended.
//   - If the command already carries a `-run`/`-run=` argument, or an `-args`
//     argument (after which test flags are passed to the test binary and a
//     trailing -run would NOT filter go test itself), ok=false.
//   - The aggregate -run regex is byte-capped; if the caller passes more
//     failures than fit, ok=false (never emit an unbounded command).
//
// The returned string is a SUGGESTION. It must never be executed without
// passing through the same consent gate as every other verification command
// (app.Confirm), which is the caller's responsibility.
func RerunCommand(cmd Command, failures []Failure) (string, bool) {
	if !isGoTestCommand(cmd.Cmd) || len(failures) == 0 {
		return "", false
	}
	if hasRunFlag(cmd.Cmd) || hasArgsFlag(cmd.Cmd) {
		return "", false
	}

	names := make([]string, 0, len(failures))
	var total int
	for _, f := range failures {
		if !goIdentifierRe.MatchString(f.Test) || len(f.Test) > maxNameLen {
			return "", false
		}
		names = append(names, f.Test)
		total += len(f.Test)
	}
	if len(names) == 0 || total > maxNameLen*maxFailures {
		return "", false
	}

	// Build the anchored, escaped regex: ^(TestA|TestB)$
	quoted := make([]string, 0, len(names))
	for _, n := range names {
		quoted = append(quoted, regexp.QuoteMeta(n))
	}
	runArg := "'^(" + strings.Join(quoted, "|") + ")$'"

	return strings.TrimSpace(cmd.Cmd) + " -run " + runArg, true
}

// hasRunFlag reports whether the plain `go test` command string already
// carries a -run or -run=... argument. Tokenized rather than a raw substring
// check so a package path or value merely containing the text "-run" is not
// misread as a filter flag.
func hasRunFlag(cmd string) bool {
	for _, tok := range strings.Fields(cmd) {
		if tok == "-run" || strings.HasPrefix(tok, "-run=") {
			return true
		}
	}
	return false
}

// hasArgsFlag reports whether the command carries an `-args` argument, after
// which go test stops interpreting flags and passes the rest to the test
// binary. Appending `-run` after `-args` would not filter tests, so such
// commands are not safely rewritable.
func hasArgsFlag(cmd string) bool {
	for _, tok := range strings.Fields(cmd) {
		if tok == "-args" {
			return true
		}
	}
	return false
}
