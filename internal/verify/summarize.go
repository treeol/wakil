package verify

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// OutputCap is the maximum bytes of command output retained per result.
// Verification output feeds into the oracle final-review briefing (capped at
// ~16 KB by WFBriefingMaxBytes), so unbounded output would evict the step
// log. This per-command cap keeps each result's output to a reasonable size;
// Summarize further truncates when composing the full log entry.
const OutputCap = 4 * 1024 // 4 KB per command

// CapOutput truncates output to at most n bytes, appending a marker when
// truncation occurs. Used by the agent runner before storing the result.
// Truncation is rune-safe: if the byte boundary falls in the middle of a
// multi-byte UTF-8 sequence, the partial rune is dropped.
func CapOutput(output string, n int) string {
	if len(output) <= n {
		return output
	}
	// Find the last valid rune boundary at or before n bytes.
	cut := n
	for cut > 0 && !utf8.RuneStart(output[cut]) {
		cut--
	}
	return output[:cut] + "\n[output truncated]"
}

// Summarize renders an Outcome as a concise multi-line string suitable for
// appending to the workflow step log (## Step log). Each result is one
// status line followed by an indented output tail (for failures).
//
// Example:
//
//	VERIFY: go test ./... — PASS (1.2s)
//	VERIFY: go vet ./... — PASS (0.3s)
//
//	VERIFY: npm test — FAIL exit=1 (12.4s)
//	  ✗ npm ERR! Test failed. See above for more details.
func (o Outcome) Summarize() string {
	if len(o.Results) == 0 {
		return "VERIFY: skipped — no verification commands configured or detected"
	}
	var sb strings.Builder
	allPass := true
	for _, r := range o.Results {
		icon := "✓"
		if r.Status != StatusPass {
			allPass = false
			icon = "✗"
		}
		sb.WriteString(fmt.Sprintf("VERIFY: %s — %s", r.Command.Cmd, strings.ToUpper(string(r.Status))))
		if r.DurationMs > 0 {
			sb.WriteString(fmt.Sprintf(" (%s)", formatDuration(r.DurationMs)))
		}
		if r.ExitCode != 0 {
			sb.WriteString(fmt.Sprintf(" exit=%d", r.ExitCode))
		}
		if r.Reason != "" && r.Status != StatusPass {
			sb.WriteString(" — " + r.Reason)
		}
		sb.WriteString("\n")
		// Include output tail for failures (and errors/timeouts).
		if r.Status != StatusPass && r.Output != "" {
			for _, line := range strings.Split(strings.TrimSpace(r.Output), "\n") {
				if line = strings.TrimSpace(line); line != "" {
					sb.WriteString("  " + icon + " " + line + "\n")
				}
			}
		}
	}
	if allPass {
		sb.WriteString("VERIFY: all checks passed ✓\n")
	} else {
		sb.WriteString("VERIFY: failures detected — workflow remains open\n")
	}
	return sb.String()
}

// formatDuration renders milliseconds as a human-friendly duration string.
func formatDuration(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return fmt.Sprintf("%.1fs", float64(ms)/1000)
}
