package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/prompts"
)

func proxyMessageForDump() []proxy.Message {
	return []proxy.Message{
		{Role: "system", Content: StrPtr("## Context upkeep: pruning is on\nDo this while you work. A mark takes effect after your next tool call at the earliest.")},
		{Role: "user", Content: StrPtr("task [Wakil context upkeep] Do this while you work, and keep what you are still using.")},
		{Role: "tool", ToolCallID: "c1", Content: StrPtr("[r1] result [context · unmarked for 3+ steps: r0 (~1k tokens)]")},
	}
}

func proxyToolForDump() []proxy.Tool {
	return []proxy.Tool{contextMarkTool(true)}
}

// The pruning section is the FIRST section of the system prompt when pruning
// is on, in full and core mode alike. Measured section sizes are logged so
// the final report carries executed numbers, not estimates.
func TestPruneSectionFirstInPreamble(t *testing.T) {
	for _, mode := range []string{"full", "core"} {
		app := preambleTestApp()
		app.Cfg.ContextPrune = "on"
		app.AgentPrompt = "FULL-OPERATING-INSTRUCTIONS"
		app.AgentCorePrompt = "CORE-OPERATING-INSTRUCTIONS"
		app.AgentExtensionPrompt = "EXTENSION"
		if err := app.SetPromptMode(mode); err != nil {
			t.Fatalf("%s: SetPromptMode: %v", mode, err)
		}
		got := app.buildPreamble("Monday, 1 January 2026")
		pruneSec := app.pruneSystemSection()
		if !strings.HasPrefix(got, "## Context upkeep: pruning is on") {
			t.Errorf("%s: preamble does not start with the pruning section:\n%.200q", mode, got)
		}
		if !strings.HasPrefix(got, pruneSec) {
			t.Errorf("%s: preamble does not start with the exact pruning section text", mode)
		}
		// The operating instructions follow the pruning section, never precede it.
		rest := got[len(pruneSec):]
		if mode == "full" && !strings.HasPrefix(rest, "\nFULL-OPERATING-INSTRUCTIONS") {
			t.Errorf("full: operating instructions do not follow the pruning section: %q", rest[:min(120, len(rest))])
		}
		if mode == "core" && !strings.HasPrefix(rest, "\nCORE-OPERATING-INSTRUCTIONS") {
			t.Errorf("core: operating instructions do not follow the pruning section: %q", rest[:min(120, len(rest))])
		}
		if strings.Contains(rest, "## Context upkeep: pruning is on") {
			t.Errorf("%s: pruning section appears twice", mode)
		}
		// Section sizes for the report (characters, measured).
		t.Logf("%s mode: pruning section %d chars, preamble total %d chars", mode, len(pruneSec), len(got))
	}

	// On-trigger mode keeps the same heading and the mode sentence.
	app := preambleTestApp()
	app.Cfg.ContextPrune = "on"
	app.Cfg.ContextPruneMarks = "on-trigger"
	sec := app.pruneSystemSection()
	if !strings.HasPrefix(sec, "## Context upkeep: pruning is on\nTool results are tagged [rN]") {
		t.Errorf("on-trigger section heading: %.80q", sec)
	}
	if !strings.Contains(sec, "A mark takes effect when the context fills up, not before; until then the result stays in full.") {
		t.Errorf("on-trigger section lacks the take-effect sentence: %s", sec)
	}
	if strings.Contains(sec, "after your next tool call") {
		t.Errorf("on-trigger section has the immediate sentence: %s", sec)
	}

	// The rater sentence appears only when a rater is configured.
	app.Cfg.ContextPruneMarks = "immediate"
	app.Cfg.ContextPruneRater = "off"
	if strings.Contains(app.pruneSystemSection(), "may be rated by a later pass") {
		t.Error("rater sentence present with rater off")
	}
	app.Cfg.ContextPruneRater = "main"
	if !strings.Contains(app.pruneSystemSection(), "A round you leave unmarked may be rated by a later pass when pruning is needed.") {
		t.Error("rater sentence missing with rater configured")
	}
}

// Off mode is byte-identical to master: no pruning section anywhere.
func TestPruneSectionAbsentWhenOff(t *testing.T) {
	app := preambleTestApp()
	app.AgentPrompt = "FULL-OPERATING-INSTRUCTIONS"
	got := app.buildPreamble("Monday, 1 January 2026")
	if strings.Contains(got, "Context upkeep") || strings.Contains(got, "context_mark") {
		t.Errorf("off: preamble carries pruning text:\n%s", got)
	}
	if !strings.HasPrefix(got, "FULL-OPERATING-INSTRUCTIONS") {
		t.Errorf("off: preamble does not start with operating instructions: %q", got[:min(120, len(got))])
	}
}

// Resumed session (InstallSession) clears preambleDay while Conv[0] is the
// stored preamble. ensurePreamble must refresh it in place so a session saved
// before this change does not send the old text beneath the new one.
func TestResumedSessionPreambleRefreshedInPlace(t *testing.T) {
	app := pruneTestApp()
	app.Cfg.ContextPrune = "on"
	app.InjectDate = true
	old := "OLD-PREAMBLE with stale wording"
	app.InstallSession(&Session{ChatID: "resumed", Conv: []proxy.Message{{Role: "system", Content: StrPtr(old)}}})
	app.ensurePreamble()
	if len(app.Conv) != 1 {
		t.Fatalf("Conv has %d messages, want 1 (no second system message)", len(app.Conv))
	}
	got := DerefStr(app.Conv[0].Content)
	if got == old || !strings.HasPrefix(got, "## Context upkeep: pruning is on") {
		t.Errorf("resumed preamble not refreshed: %.160q", got)
	}
}

// The three texts carry no discouraging sentence and no contradictions.
func TestPruneProminenceTexts(t *testing.T) {
	app := preambleTestApp()
	app.Cfg.ContextPrune = "on"
	sec := app.pruneSystemSection()
	desc := contextMarkTool(true).Function.Description
	for name, text := range map[string]string{"system section": sec, "user suffix": pruneReminder, "tool description": desc} {
		if strings.Contains(text, "not a step to perform after every result") || strings.Contains(text, "not a step after every result") {
			t.Errorf("%s still carries the removed discouraging sentence", name)
		}
		if strings.Contains(text, "wakil.pro") {
			t.Errorf("%s contains the forbidden domain", name)
		}
	}
	for _, s := range []string{
		"## Context upkeep: pruning is on",
		"Do this while you work",
		"Marking finished results is part of the task, not an extra action",
		"Keep results you are still working with or will cite.",
		"The full result stays retrievable from the archive path in its stub.",
		"Write rounds are only stubbed, never dropped.",
		"The line on the newest tool result names results that have been unmarked for a while, and warns you when you reread something you marked.",
		`The "prev" argument on any tool call can rate the previous round in one line; for large results use context_mark.`,
	} {
		if !strings.Contains(sec, s) {
			t.Errorf("system section lacks %q", s)
		}
	}
	wantSuffix := "[Wakil context upkeep] Tool results are tagged [rN] and are re-sent in full on every step until you mark them. When you are done with a result, mark it with context_mark: drop it, or stub it with the facts you still need, with file:line. Do this while you work, and keep what you are still using."
	if pruneReminder != "\n\n"+wantSuffix {
		t.Errorf("user suffix:\n got %q\nwant %q", pruneReminder, "\n\n"+wantSuffix)
	}
	for _, s := range []string{
		"drop = nothing in the result is needed any more",
		"stub = the note replaces the result and afterwards only the note remains",
		"every fact you will still use, per file and with file:line",
		"a note too thin for a large result is rejected",
		"Reading a file again after stubbing it costs more than keeping it, so keep what you will still cite and cannot carry in a note.",
		"Marking right before the final answer saves nothing.",
	} {
		if !strings.Contains(desc, s) {
			t.Errorf("tool description lacks %q", s)
		}
	}
	t.Logf("system section %d chars, user suffix %d chars, tool description %d chars", len(sec), len(pruneReminder)-2, len(desc))
}

// Measured system-prompt section sizes with the real prompts (embedded
// prompts/*.txt), so the report cites executed numbers.
func TestPruneProminenceSectionSizes(t *testing.T) {
	embeddedAgent := strings.TrimRight(prompts.EmbeddedAgentPrompt, "\n")
	embeddedCore := strings.TrimRight(prompts.EmbeddedCorePrompt, "\n")
	embeddedExt := strings.TrimRight(prompts.EmbeddedExtensionPrompt, "\n")

	for _, mode := range []string{"full", "core"} {
		app := preambleTestApp()
		app.Cfg.ContextPrune = "on"
		app.AgentPrompt = embeddedAgent
		app.AgentCorePrompt = embeddedCore
		app.AgentExtensionPrompt = embeddedExt
		if err := app.SetPromptMode(mode); err != nil {
			t.Fatalf("%s: SetPromptMode: %v", mode, err)
		}
		got := app.buildPreamble("Monday, 1 January 2026")
		pruneSec := app.pruneSystemSection()
		if !strings.HasPrefix(got, "## Context upkeep: pruning is on") {
			t.Errorf("%s: preamble does not start with the pruning section", mode)
		}
		opLen := len(embeddedAgent)
		if mode == "core" {
			opLen = len(embeddedCore)
		}
		t.Logf("%s mode: pruning %d, operating instructions %d, preamble total %d",
			mode, len(pruneSec), opLen, len(got))
	}
}

// --- Debug dump (WAKIL_DEBUG_REQUESTS) ---

type debugReqBody struct {
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Tools []map[string]interface{} `json:"tools"`
}

func contentText(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return string(raw)
	}
	return s
}

func readDebugDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func parseDebugFile(t *testing.T, path string) (raw []byte, body debugReqBody) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("%s: not JSON: %v", path, err)
	}
	return raw, body
}

func TestDebugRequestDumpOffWritesNothing(t *testing.T) {
	t.Setenv("WAKIL_DEBUG_REQUESTS", "") // unset: no dump, no cost
	resetDebugRequestCounter()
	app := pruneTestApp()
	app.Client.ChatID = "debug-off-session"
	for i := 0; i < 3; i++ {
		app.dumpDebugRequest(nil, nil)
	}
	if got := debugRequestCounter.Load(); got != 0 {
		t.Errorf("off: counter moved to %d", got)
	}
}

func TestDebugRequestDumpWarnOnce(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(bad, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAKIL_DEBUG_REQUESTS", bad)
	resetDebugRequestCounter()
	debugRequestWarned.Delete(bad)
	app := pruneTestApp()
	app.Client.ChatID = "debug-warn-once"
	app.dumpDebugRequest(proxyMessageForDump(), nil)
	if _, ok := debugRequestWarned.Load(bad); !ok {
		t.Error("first failure did not record a warning")
	}
	// Second failure: no new warning (the map entry already exists), the
	// hook still returns without erroring the request.
	app.dumpDebugRequest(proxyMessageForDump(), nil)
}

func TestDebugRequestDumpWritesOneFilePerRequest(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WAKIL_DEBUG_REQUESTS", dir)
	app := pruneTestApp()
	app.Client.ChatID = "debug-on-session"
	resetDebugRequestCounter()

	msgs := proxyMessageForDump()
	tools := proxyToolForDump()
	for i := 0; i < 2; i++ {
		app.dumpDebugRequest(msgs, tools)
	}
	names := readDebugDir(t, dir)
	if len(names) != 2 {
		t.Fatalf("want 2 files, got %d: %v", len(names), names)
	}
	for _, name := range names {
		if !strings.HasPrefix(name, "debug-on-session-000") {
			t.Errorf("file %q lacks session id + running number", name)
		}
		if !strings.HasSuffix(name, ".json") {
			t.Errorf("file %q lacks .json suffix", name)
		}
		raw, body := parseDebugFile(t, filepath.Join(dir, name))
		if len(body.Messages) != 3 || len(body.Tools) != 1 {
			t.Errorf("%s: %d messages, %d tools, want 3+1", name, len(body.Messages), len(body.Tools))
		}
		// No headers, no key material: the body carries no Authorization.
		if strings.Contains(string(raw), "Authorization") || strings.Contains(string(raw), "Bearer") || strings.Contains(string(raw), "api_key") {
			t.Errorf("%s carries header/key material", name)
		}
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %o, want 0600", name, info.Mode().Perm())
		}
	}
	// The three reminders are visible when prune is on: system section,
	// user suffix, backlog line.
	sysText := ""
	userText := ""
	for _, name := range names {
		_, body := parseDebugFile(t, filepath.Join(dir, name))
		for _, m := range body.Messages {
			c := contentText(m.Content)
			switch m.Role {
			case "system":
				sysText += c
			case "user":
				userText += c
			}
		}
	}
	if !strings.Contains(sysText, "## Context upkeep: pruning is on") || !strings.Contains(sysText, "Do this while you work") {
		t.Errorf("system section not in dump: %.200q", sysText)
	}
	if !strings.Contains(userText, "[Wakil context upkeep]") || !strings.Contains(userText, "keep what you are still using") {
		t.Errorf("user suffix not in dump: %.200q", userText)
	}
	if !strings.Contains(sysText, "A mark takes effect after your next tool call") {
		t.Errorf("take-effect sentence not in dump")
	}
	// Running number is monotonic per request.
	if names[0] >= names[1] {
		t.Errorf("file names not in running order: %v", names)
	}
}

// Simulation the audit skipped, executed: 13 rounds of 2–40 KB reads with no
// marks, projection after each round. Reports at which step the first backlog
// line appears and on how many steps one shows. Repeated with a subagent
// result among the rounds. The expected numbers here come from the executed
// run, not from reading the code.
func TestBacklogSimulation13Rounds(t *testing.T) {
	sizes := []int{2048, 3072, 4096, 5120, 6144, 8192, 9216, 10240, 12800, 15360, 20480, 30720, 40960}
	if len(sizes) != 13 {
		t.Fatal("simulation must use 13 rounds")
	}
	run := func(withSubagent bool) (firstStep int, stepsWith int, total int) {
		app := lineTestApp(t)
		for i, size := range sizes {
			name, args, callID := "read_file", fmt.Sprintf(`{"path":"sim%02d.go"}`, i), fmt.Sprintf("sim%d", i)
			if withSubagent && i == 6 {
				name, args = "dispatch_subagent", `{"task":"audit"}`
			}
			appendRound(app, 1, callID, name, args, strings.Repeat("x", size))
			if withSubagent && i == 6 {
				// finalizeToolResult pins dispatch_subagent results.
				for j := range app.Conv {
					if app.Conv[j].Role == "tool" && app.Conv[j].ToolCallID == callID {
						app.Conv[j].Pinned = true
					}
				}
			}
			total++
			got := toolResultOf(app.requestMessages(), callID)
			if strings.Contains(got, backlogMarker) {
				if firstStep == 0 {
					firstStep = i + 1
				}
				stepsWith++
			}
		}
		return firstStep, stepsWith, total
	}
	first, with, total := run(false)
	t.Logf("no-marks run: first backlog on step %d of %d, %d steps show one", first, total, with)
	if first != 4 {
		t.Errorf("no-marks run: first backlog on step %d, want 4", first)
	}
	if with != 10 {
		t.Errorf("no-marks run: %d steps show a backlog line, want 10", with)
	}
	firstSub, withSub, totalSub := run(true)
	t.Logf("with subagent at round 7: first backlog on step %d of %d, %d steps show one", firstSub, totalSub, withSub)
	if firstSub != 4 {
		t.Errorf("subagent run: first backlog on step %d, want 4", firstSub)
	}
	if withSub != 9 {
		t.Errorf("subagent run: %d steps show a backlog line, want 9 (pinned round excluded, stale set unchanged)", withSub)
	}
	// The pinned subagent round is never named in the backlog line.
	app := lineTestApp(t)
	for i, size := range sizes {
		name, args, callID := "read_file", fmt.Sprintf(`{"path":"sim%02d.go"}`, i), fmt.Sprintf("sub%d", i)
		if i == 6 {
			name, args = "dispatch_subagent", `{"task":"audit"}`
		}
		appendRound(app, 1, callID, name, args, strings.Repeat("x", size))
		if i == 6 {
			for j := range app.Conv {
				if app.Conv[j].Role == "tool" && app.Conv[j].ToolCallID == callID {
					app.Conv[j].Pinned = true
				}
			}
		}
	}
	got := toolResultOf(app.requestMessages(), "sub12")
	if strings.Contains(got, "r7 (") {
		t.Errorf("pinned subagent round r7 named in backlog: %q", got[max(0, len(got)-260):])
	}
	t.Logf("subagent run, final backlog line tail: %q", got[max(0, len(got)-260):])
}

func TestDebugRequestDumpCreatesDir(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "nested", "dumps")
	t.Setenv("WAKIL_DEBUG_REQUESTS", dir)
	app := pruneTestApp()
	app.Client.ChatID = "debug-mkdir"
	resetDebugRequestCounter()
	app.dumpDebugRequest(proxyMessageForDump(), nil)
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("dir not created: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %o, want 0700", info.Mode().Perm())
	}
	if names := readDebugDir(t, dir); len(names) != 1 {
		t.Errorf("want 1 file in created dir, got %v", names)
	}
}

func TestDebugRequestDumpFailureDoesNotBreakRequest(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(bad, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAKIL_DEBUG_REQUESTS", bad) // a file, not a directory
	app := pruneTestApp()
	app.Client.ChatID = "debug-fail"
	resetDebugRequestCounter()
	// Must return without panic and without erroring the request.
	app.dumpDebugRequest(proxyMessageForDump(), nil)
	app.dumpDebugRequest(proxyMessageForDump(), nil)
}
