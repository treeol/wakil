package tui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// tuiLocalCommands are handled in tui.go before HandleTUICommand is called.
var tuiLocalCommands = []string{"/info", "/queue", "/ask"}

// dispatcherCommands parses agent/commands.go and returns every "/command"
// string-literal case label of the top-level `switch fields[0]` inside
// HandleTUICommand. AST-based so formatting changes can't hide a command.
func dispatcherCommands(t *testing.T) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "../agent/commands.go", nil, 0)
	if err != nil {
		t.Fatalf("parse dispatcher source: %v", err)
	}
	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "HandleTUICommand" {
			fn = fd
		}
	}
	if fn == nil {
		t.Fatal("HandleTUICommand not found in agent/commands.go")
	}
	out := map[string]bool{}
	for _, stmt := range fn.Body.List {
		sw, ok := stmt.(*ast.SwitchStmt)
		if !ok {
			continue
		}
		idx, ok := sw.Tag.(*ast.IndexExpr)
		if !ok {
			continue
		}
		if id, ok := idx.X.(*ast.Ident); !ok || id.Name != "fields" {
			continue
		}
		for _, cc := range sw.Body.List {
			for _, e := range cc.(*ast.CaseClause).List {
				lit, ok := e.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Errorf("non-literal case label in HandleTUICommand switch at %s", fset.Position(e.Pos()))
					continue
				}
				s, err := strconv.Unquote(lit.Value)
				if err == nil && strings.HasPrefix(s, "/") {
					out[s] = true
				}
			}
		}
	}
	return out
}

// TestAllTUICommandsMatchDispatcher keeps the "/" picker list in sync with the
// commands the dispatcher actually handles. It parses the case labels of
// agent.HandleTUICommand from source so a new command can't silently miss the
// picker again (the /prompt regression).
func TestAllTUICommandsMatchDispatcher(t *testing.T) {
	handled := dispatcherCommands(t)
	if len(handled) < 20 {
		t.Fatalf("parsed only %d commands from HandleTUICommand — parser out of date?", len(handled))
	}
	for _, c := range tuiLocalCommands {
		handled[c] = true
	}

	listed := map[string]bool{}
	for _, c := range allTUICommands {
		if listed[c.name] {
			t.Errorf("allTUICommands lists %s twice", c.name)
		}
		listed[c.name] = true
	}

	var missing, stale []string
	for c := range handled {
		if !listed[c] {
			missing = append(missing, c)
		}
	}
	for c := range listed {
		if !handled[c] {
			stale = append(stale, c)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) > 0 {
		t.Errorf("commands handled but missing from allTUICommands: %v", missing)
	}
	if len(stale) > 0 {
		t.Errorf("allTUICommands entries with no handler: %v", stale)
	}
}

// TestHelpTextMentionsAllCommands ensures /help documents every picker command.
func TestHelpTextMentionsAllCommands(t *testing.T) {
	src, err := os.ReadFile("../agent/commands.go")
	if err != nil {
		t.Fatalf("read dispatcher source: %v", err)
	}
	s := string(src)
	i := strings.Index(s, "const helpTextTUI = `")
	if i < 0 {
		t.Fatal("helpTextTUI not found in commands.go")
	}
	help := s[i+len("const helpTextTUI = `"):]
	if j := strings.Index(help, "`"); j >= 0 {
		help = help[:j]
	}
	for _, c := range allTUICommands {
		re := regexp.MustCompile(`(^|[\s,(])` + regexp.QuoteMeta(c.name) + `($|[\s,\]])`)
		if !re.MatchString(help) {
			t.Errorf("/help text does not mention %s", c.name)
		}
	}
}

func TestSlashArgCompletionKeywords(t *testing.T) {
	cases := []struct {
		input string
		want  []string
	}{
		{"/prompt ", []string{"core", "full", "status"}},
		{"/prompt c", []string{"core"}},
		{"/mode long-", []string{"long-session"}},
		{"/grants ", []string{"clear"}},
		{"/compact ", []string{"force"}},
		{"/sessions ", []string{"all"}},
		{"/repomap ", []string{"--symbols"}},
		{"/mode long", []string{"long", "long-session"}},
	}
	for _, tc := range cases {
		st := computeSlashCompletion(newTA(tc.input), compSources{}, nil)
		if !st.active {
			t.Errorf("%q: picker should be active", tc.input)
			continue
		}
		var got []string
		for _, c := range st.cands {
			got = append(got, c.name)
		}
		sort.Strings(got)
		if len(got) != len(tc.want) {
			t.Errorf("%q: got %v, want %v", tc.input, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%q: got %v, want %v", tc.input, got, tc.want)
				break
			}
		}
	}
}

// Free-text arguments must not be covered by a "no matches" picker box.
func TestSlashArgCompletionFreeText(t *testing.T) {
	for _, input := range []string{
		"/plan fix the login",                    // past first arg
		"/plan refactor",                         // free-form command, non-keyword
		"/image ./shot.png",                      // path, not a keyword
		"/keep 3",                                // number
		"/mashura panel add foo",                 // past first arg
		"/backend openrouter/anthropic/claude-x", // backend/model form
		"/model some-unlisted-model",             // unlisted model ID
	} {
		if st := computeSlashCompletion(newTA(input), compSources{}, nil); st.active {
			t.Errorf("%q: picker should be closed, got %+v", input, st.cands)
		}
	}
}

func TestDuplicateRememberRemoved(t *testing.T) {
	st := computeSlashCompletion(newTA("/rem"), compSources{}, nil)
	n := 0
	for _, c := range st.cands {
		if c.name == "/remember" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("/remember appears %d times in picker, want 1", n)
	}
}
