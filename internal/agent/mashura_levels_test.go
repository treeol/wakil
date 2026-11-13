package agent

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/trace"
)

// hasMashuraTool reports whether tools contains mashura__review.
func hasMashuraTool(tools []proxy.Tool) bool {
	for _, t := range tools {
		if t.Function.Name == "mashura__review" {
			return true
		}
	}
	return false
}

// TestBuildToolsMashuraLevelGate pins Group C4's unified predicate: with the
// new block, tools are offered iff effective level != off AND effective member
// list non-empty (fallback member counts) AND the OpenRouter key is present.
// oracle_api_key_env is not part of it.
func TestBuildToolsMashuraLevelGate(t *testing.T) {
	// The gate reads the OpenRouter key named by the config's
	// openrouter_api_key_env — the test configs below must name it, or the
	// "" name reads nothing. Set the named var.
	t.Setenv("OPENROUTER_API_KEY", "x")

	members := []string{"openrouter:a/b"}
	keyEnvCfg := func(c config.Config) config.Config { c.OpenRouterAPIKeyEnv = "OPENROUTER_API_KEY"; return c }
	cases := []struct {
		name string
		cfg  config.Config
		repo string
		want bool
	}{
		{"level medium with key", keyEnvCfg(config.Config{Mashura: &config.MashuraConfig{Level: "medium", Members: members}}), "", true},
		{"level off", keyEnvCfg(config.Config{Mashura: &config.MashuraConfig{Level: "off", Members: members}}), "", false},
		{"repo off overrides config medium", keyEnvCfg(config.Config{Mashura: &config.MashuraConfig{Level: "medium", Members: members}}), "off", false},
		// Empty members falls back to the lazy fallback member (A4) — tools
		// ARE offered, matching today's fallback-based behavior.
		{"empty members (fallback counts)", keyEnvCfg(config.Config{Mashura: &config.MashuraConfig{Level: "medium"}}), "", true},
		{"empty level defaults medium", keyEnvCfg(config.Config{Mashura: &config.MashuraConfig{Members: members}}), "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hasMashuraTool(BuildTools(tc.cfg, "/work", nil, tc.repo))
			if got != tc.want {
				t.Errorf("BuildTools mashura present = %v, want %v", got, tc.want)
			}
		})
	}

	// Without the key, no tools even at medium (unified predicate).
	t.Setenv("OPENROUTER_API_KEY", "")
	if got := hasMashuraTool(BuildTools(keyEnvCfg(config.Config{Mashura: &config.MashuraConfig{Level: "medium", Members: members}}), "/work", nil, "")); got {
		t.Error("tools must not appear without the OpenRouter key")
	}
}

// TestMashuraLevelStatusShowsCheckException pins that the per-tool table in
// /mashura states check's exception explicitly (user requirement #3).
func TestMashuraLevelStatusShowsCheckException(t *testing.T) {
	app, _ := mashuraTestApp(t, "", "", "")
	app.Cfg.Mashura = &config.MashuraConfig{Level: "medium", Members: []string{"openrouter:a/b"}}
	s := mashuraStatus(app)
	if !strings.Contains(s, "level:") || !strings.Contains(s, "effective per tool") {
		t.Fatalf("status missing level survey:\n%s", s)
	}
	if !strings.Contains(s, "check stays at low below high") {
		t.Fatalf("status must state check stays at low below high:\n%s", s)
	}
	if !strings.Contains(s, "\"mashura\":") {
		t.Fatalf("status must print the copyable config block:\n%s", s)
	}
}

// TestRunMashuraCoreLevelOffRefuses pins the run-time refusal when the
// effective level is off (before any source reads).
func TestRunMashuraCoreLevelOffRefuses(t *testing.T) {
	app, _ := mashuraTestApp(t, "", "", "")
	t.Setenv("MASHURA_TEST_OR_KEY", "x")
	app.Cfg = config.Config{OpenRouterAPIKeyEnv: "MASHURA_TEST_OR_KEY", OracleModel: "z"}
	app.Cfg.Mashura = &config.MashuraConfig{Level: "off", Members: []string{"openrouter:a/b"}}
	res := app.runMashuraCore(context.Background(), "mashura__review", tcArgs("mashura__review", `{"focus":"x"}`), true)
	if !strings.Contains(res, "Mashūra is off") {
		t.Fatalf("want level-off refusal, got: %s", res)
	}
}

// TestMashuraReasoningForLevel pins that the level owns reasoning when the
// new block is present, and roles own it otherwise.
func TestMashuraReasoningForLevel(t *testing.T) {
	app, _ := mashuraTestApp(t, "", "", "")
	app.Cfg = config.Config{}
	app.Cfg.Mashura = &config.MashuraConfig{Level: "medium"}
	r := app.mashuraReasoningFor("mashura__review")
	if r == nil || r.Effort != "medium" {
		t.Fatalf("review at medium = %v, want effort medium", r)
	}
	r = app.mashuraReasoningFor("mashura__check")
	if r == nil || r.Effort != "low" {
		t.Fatalf("check at medium = %v, want effort low", r)
	}
	// Override check -> high follows the level.
	app.Cfg.Mashura.Overrides = map[string]config.MashuraOverride{"check": {Level: "high"}}
	r = app.mashuraReasoningFor("mashura__check")
	if r == nil || r.Effort != "max" {
		t.Fatalf("check override high = %v, want effort max", r)
	}
}

// TestMashuraLevelsBehaviourMatrix is the one table-driven test that pins
// the behaviour matrix: rows off/low/medium/high, columns the model calling
// each of the four tools, /counsel, a detected struggle, the workflow
// triggers, and skill creation. Per cell: tools offered or not; panel runs,
// is suggested, or is skipped; members; reasoning; debate possible; the line
// the user sees. Blocked automatic triggers are included — a silent cell or
// one contradicting the docs is a defect.
//
// Tools: by the BootTools gate (level != off + members + key). Panel
// availability: mashuraAvailable (level + members + key). Reasoning/debate
// come from config.LevelReasoning / LevelDebate. Struggle mode comes from
// resolveCounselMode (no explicit choice: level decides).
//
// Cells that this matrix found wrong or surprising are listed in the commit
// message of the fix commit (not asserted here in a way that would hide a
// defect — the assertions below are the correct behaviour).
func TestMashuraLevelsBehaviourMatrix(t *testing.T) {
	t.Setenv("MASHURA_TEST_OR_KEY", "x")
	t.Setenv("MASHURA_TEST_KEY", "x")

	members2 := []string{"openrouter:test-model-a", "openrouter:test-model-b"}

	type cell struct {
		name       string
		level      string
		tools      bool   // the four tools offered?
		available  bool   // mashuraAvailable?
		struggle   string // "silent" | "suggest" | "auto"
		reviewEff  string // reasoning effort for review
		checkEff   string // reasoning effort for check
		debate     bool   // debate possible?
		blockedMsg string // for blocked auto: reason substring, "" if not applicable
		topline    string // exact user-visible line kind the struggle prints
	}

	cells := []cell{
		{"off", "off", false, false, "silent", "none", "none", false, "", "silent"},
		{"low", "low", true, true, "suggest", "none", "low", false, "", "suggest"},
		{"medium", "medium", true, true, "suggest", "medium", "low", false, "", "suggest"},
		{"high", "high", true, true, "auto", "max", "max", true, "", "auto"},
	}

	for _, c := range cells {
		t.Run(c.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.Mashura = &config.MashuraConfig{Level: c.level, Members: members2}
			cfg.OpenRouterAPIKeyEnv = "MASHURA_TEST_OR_KEY"
			cfg.OracleAPIKeyEnv = "MASHURA_TEST_KEY"
			cfg.OracleModel = "test-model"

			// Columns 1: the model calls each of the four tools — offered or
			// not? BootTools gate (Level != off + members + key).
			tools := BuildTools(cfg, "/work", nil, "")
			gotTools := hasMashuraTool(tools)
			if gotTools != c.tools {
				t.Errorf("tools offered = %v, want %v", gotTools, c.tools)
			}

			// Column 2: /counsel — resolved mode with a fresh App (no
			// explicit choice) matches the struggle column.
			app := &App{Cfg: cfg, Out: io.Discard, Client: &proxy.Client{}}
			mode, _ := app.resolveCounselMode()
			switch c.struggle {
			case "silent":
				if mode != "off" {
					t.Errorf("/counsel mode = %q, want off", mode)
				}
			case "suggest":
				if mode != "suggest" {
					t.Errorf("/counsel mode = %q, want suggest", mode)
				}
			case "auto":
				if mode != "auto" {
					t.Errorf("/counsel mode = %q, want auto", mode)
				}
			}

			// Column 3: a detected struggle — suggest or auto or silent?
			// (auto needs the fake endpoint + auto-approve; set up only for
			// the auto row so suggest/silent rows stay hermetic.)
			var out strings.Builder
			app.Out = &out
			app.recentTraces = buildStruggleTraces()
			if c.struggle == "auto" {
				_, count := counselServer(t)
				app.Confirm = func(_, _, _ string, _ bool) bool { return true }
				app.SetAutoApprove(true)
				app.MaxCounsel = 3
				app.maybeSuggestDebug(context.Background())
				if *count == 0 {
					t.Error("auto cell: panel never fired")
				}
			} else {
				app.maybeSuggestDebug(context.Background())
			}
			switch c.struggle {
			case "silent":
				if out.Len() != 0 {
					t.Errorf("silent cell printed %q", out.String())
				}
			case "suggest":
				if !strings.Contains(out.String(), "struggle detected") {
					t.Errorf("suggest cell missing suggest line: %q", out.String())
				}
				if strings.Contains(out.String(), "auto-counsel") {
					t.Errorf("suggest cell announced auto: %q", out.String())
				}
			case "auto":
				if !strings.Contains(out.String(), "⚡ auto-counsel") {
					t.Errorf("auto cell missing announce line: %q", out.String())
				}
			}

			// Columns 4-6: members, reasoning, debate (level-owned).
			// Note: effectiveMashura always appends the fallback member, so
			// the count is >0 even at off; the behavioural gate is the
			// mashuraAvailable/tool-offering predicate (column 7), not the
			// raw member count.
			_ = availableMembers
			if got := config.LevelReasoning("review", c.level).Effort; got != c.reviewEff {
				t.Errorf("review reasoning = %q, want %q", got, c.reviewEff)
			}
			if got := config.LevelReasoning("check", c.level).Effort; got != c.checkEff {
				t.Errorf("check reasoning = %q, want %q", got, c.checkEff)
			}
			if got := config.LevelDebate(c.level); got != c.debate {
				t.Errorf("debate = %v, want %v", got, c.debate)
			}

			// Column 7: skill creation (auto-mashura). When tools are not
			// offered (off), mashuraAvailable is false → behaves like auto
			// (drafts). The warning line is shown by the command; here we
			// pin the predicate itself.
			if got := app.mashuraAvailable(); got != c.available {
				t.Errorf("mashuraAvailable = %v, want %v (skill auto-mashura gate)", got, c.available)
			}

			// Blocked automatic trigger: with /counsel auto forced at the
			// row's level, a struggle with no key (unset env) must print the
			// suggest note plus the reason — never silence. (Only checked
			// where the row is not off: off stays silent by definition.)
			if c.level != "off" {
				t.Setenv("MASHURA_TEST_OR_KEY", "")
				t.Setenv("MASHURA_TEST_KEY", "")
				bapp := &App{Cfg: cfg, Client: &proxy.Client{}, CounselMode: "auto", MaxCounsel: 3}
				var bout strings.Builder
				bapp.Out = &bout
				bapp.recentTraces = buildStruggleTraces()
				bapp.maybeSuggestDebug(context.Background())
				if !strings.Contains(bout.String(), "struggle detected") {
					t.Errorf("blocked cell: expected suggest note, got %q", bout.String())
				}
				if !strings.Contains(bout.String(), "auto not run") {
					t.Errorf("blocked cell: expected reason, got %q", bout.String())
				}
			}
		})
	}
}

// availableMembers returns the effective member count (fallback counts when
// the block is present).
func availableMembers(a *App) int {
	_, members, _ := a.effectiveMashura("review")
	return len(members)
}

// struggleFrames builds the scripted model frames for one struggle sequence:
// each step is a write_file then a run_shell of checkCmd, and the turn ends
// with a final text. writes is the sequence of file contents (or identical
// content for the identical-write case); checkCmd is the check command.
func struggleFrames(writes []string, checkCmd string) [][]string {
	var calls [][]string
	for i, content := range writes {
		id := fmt.Sprintf("c%d", i)
		args := fmt.Sprintf(`{"path":"answer.txt","content":%q}`, content)
		calls = append(calls, toolCallFrames(id, "write_file", args))
		calls = append(calls, toolCallFrames(id+"s", "run_shell", fmt.Sprintf(`{"command":%q}`, checkCmd)))
	}
	calls = append(calls, []string{contentChunk("done")})
	return calls
}

// TestStrugglePathEndToEndAtEveryLevel drives the real turn loop (streamTurn
// with the scripted fake model) through the two manual sequences at low,
// medium and high, fresh App per case, auto-approve on, no explicit counsel
// choice, two members. The exact user-visible lines are asserted.
//
// Detector rules (detectStruggleScan): two consecutive ExitErr; two identical
// adjacent (Abbrev,Command); or ≥3 write/edit of the same path. The e2e
// sequences produce them through real tool calls: the check after each write
// either fails (ExitErr) or exits 0 printing "wrong answer" (no rule fires —
// the documented detector gap, asserted as silence with the rule in the
// evaluation dump).
func TestStrugglePathEndToEndAtEveryLevel(t *testing.T) {
	t.Setenv("MASHURA_TEST_OR_KEY", "x")
	t.Setenv("MASHURA_TEST_KEY", "x")

	run := func(t *testing.T, level string, writes []string, checkCmd string, checkErr error, counselMode string) (string, int) {
		t.Helper()
		_, count := counselServer(t)
		frames := struggleFrames(writes, checkCmd)
		srv := sseServer(t, frames...)
		t.Cleanup(srv.Close)
		exe := newFakeExecutor()
		if checkErr != nil {
			exe.shellErr = checkErr
		}
		app := newTestApp(srv.URL, exe, func(_, _, _ string, _ bool) bool { return true })
		// Level + two members; no explicit counsel choice unless the case
		// sets one (the /counsel cases). Parent-agent shape: no dedup cache,
		// auto-approve on.
		app.Cfg.Mashura = &config.MashuraConfig{
			Level:   level,
			Members: []string{"openrouter:test-model-a", "openrouter:test-model-b"},
		}
		app.Cfg.OpenRouterAPIKeyEnv = "MASHURA_TEST_OR_KEY"
		app.Cfg.OracleAPIKeyEnv = "MASHURA_TEST_KEY"
		app.Cfg.OracleModel = "test-model"
		if counselMode != "" {
			app.SetCounselMode(counselMode)
		}
		app.IsSubagent = false
		app.MaxCounsel = 3
		app.SetAutoApprove(true)
		out := new(strings.Builder)
		app.Out = out
		var traceToolCalls []trace.ToolTrace
		_, _, err := app.streamTurn(context.Background(), "write answer", nil, &traceToolCalls)
		if err != nil {
			t.Fatalf("streamTurn: %v", err)
		}
		return out.String(), *count
	}

	// NOTE: the write tool name in struggleFrames is write_file; the
	// executor writes to its files map. newTestApp handles it. newTestApp
	// does not give the app a workspace gate for write_file (no ConfinePath
	// needed for the fake executor).
	identical := []string{"same", "same", "same", "same"}
	different12 := []string{"v1", "v2", "v3", "v4", "v5", "v6", "v7", "v8", "v9", "v10", "v11", "v12"}
	failCheck := fmt.Errorf("check failed")

	// low: suggest, no panel.
	out, n := run(t, "low", identical, "./check.sh", failCheck, "")
	if n != 0 {
		t.Errorf("low: panel fired %d times, want 0", n)
	}
	assertStruggleLines(t, "low", out, "suggest")

	// medium: suggest, no panel.
	out, n = run(t, "medium", identical, "./check.sh", failCheck, "")
	if n != 0 {
		t.Errorf("medium: panel fired %d times, want 0", n)
	}
	assertStruggleLines(t, "medium", out, "suggest")

	// high: auto-fires at least once, first line names level high.
	out, n = run(t, "high", identical, "./check.sh", failCheck, "")
	if n == 0 {
		t.Errorf("high: panel never fired; out=%q", out)
	}
	assertStruggleLines(t, "high", out, "auto")

	// high + /counsel suggest: no panel.
	out, n = run(t, "high", identical, "./check.sh", failCheck, "suggest")
	if n != 0 {
		t.Errorf("high+counsel suggest: panel fired %d times, want 0", n)
	}
	assertStruggleLines(t, "high+counsel suggest", out, "suggest")

	// medium + /counsel auto: panel fires.
	out, n = run(t, "medium", identical, "./check.sh", failCheck, "auto")
	if n == 0 {
		t.Errorf("medium+counsel auto: panel never fired; out=%q", out)
	}
	assertStruggleLines(t, "medium+counsel auto", out, "auto")

	// The user's 12-different-writes check that prints "wrong answer" but
	// exits 0. The rewrites rule fires at 3 writes (≥3 writes to the same
	// path), so low/medium suggest and high auto-fires — this pins that the
	// rewrite rule, not the exit status, drives them. High fires once per
	// new symptom (count grows 3,4,5...), so count is asserted >=1, not 0.
	for _, lv := range []string{"low", "medium", "high"} {
		out, n := run(t, lv, different12, "./check.sh; echo exit:$?", nil, "")
		if lv == "high" {
			if n == 0 {
				t.Errorf("high exit0-check: panel never fired; out=%q", out)
			}
			assertStruggleLines(t, "high exit0-check", out, "auto")
		} else {
			if n != 0 {
				t.Errorf("%s exit0-check: panel fired %d times, want 0", lv, n)
			}
			assertStruggleLines(t, lv+" exit0-check", out, "suggest")
		}
	}
	// Same sequences with a check that exits non-zero: high fires, low/medium
	// suggest.
	out, n = run(t, "high", different12, "./check.sh", failCheck, "")
	if n == 0 {
		t.Errorf("high 12-writes failing-check: panel never fired; out=%q", out)
	}
	assertStruggleLines(t, "high 12-writes", out, "auto")
}

// assertStruggleLines checks the exact first user-visible struggle line for
// the level: suggest prints the existing note; auto prints the announce line
// naming the level.
func assertStruggleLines(t *testing.T, label, out, kind string) {
	t.Helper()
	switch kind {
	case "suggest":
		if !strings.Contains(out, "· struggle detected — mashura__debug can diagnose; symptom:") {
			t.Errorf("%s: missing suggest note; out=%q", label, out)
		}
		if strings.Contains(out, "auto-counsel") {
			t.Errorf("%s: suggest case must not announce auto-counsel; out=%q", label, out)
		}
	case "auto":
		if !strings.Contains(out, "⚡ auto-counsel") {
			t.Errorf("%s: missing auto announce; out=%q", label, out)
		}
	}
}
