package agent

import (
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/proxy"
)

// userMsgAt builds a user message stamped with a turn.
func userMsgAt(turn int, text string) proxy.Message {
	return proxy.Message{Role: "user", Turn: turn, Content: StrPtr(text)}
}

func TestPruneReminderOffIsByteIdentical(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	_ = app.SetContextMode("long-session")
	app.Cfg.ContextPrune = "off"
	app.curTurn = 1
	app.appendConv(userMsgAt(1, "do the task"))
	app.appendConv(tcMsg("c1", "read_file", `{"path":"a.go"}`))
	app.appendConv(trMsg("c1", "read_file", bigContent))
	msgs := app.requestMessages()
	if len(msgs) != len(app.Conv) {
		t.Fatalf("len %d != %d", len(msgs), len(app.Conv))
	}
	for i := range msgs {
		if DerefStr(msgs[i].Content) != DerefStr(app.Conv[i].Content) {
			t.Errorf("msg %d changed with prune off: %q", i, DerefStr(msgs[i].Content))
		}
	}
	if strings.Contains(DerefStr(msgs[0].Content), "context upkeep") {
		t.Error("reminder present with prune off")
	}
}

func TestPruneReminderAppendedToRealUserPromptOnly(t *testing.T) {
	app := pruneTestApp()
	app.curTurn = 1
	app.appendConv(userMsgAt(1, "refresh the docs"))
	app.appendConv(tcMsg("c1", "read_file", `{"path":"a.go"}`))
	app.appendConv(trMsg("c1", "read_file", bigContent))
	// Injected user-role message inside the same turn (async envelope etc.).
	app.appendConv(userMsgAt(1, "## Async task results"))
	app.curTurn = 2
	app.appendConv(userMsgAt(2, "second prompt"))

	msgs := app.requestMessages()
	var users []string
	for _, m := range msgs {
		if m.Role == "user" {
			users = append(users, DerefStr(m.Content))
		}
	}
	if len(users) != 3 {
		t.Fatalf("users = %d, want 3", len(users))
	}
	if users[0] != "refresh the docs"+pruneReminder {
		t.Errorf("first prompt not suffixed exactly once: %q", users[0])
	}
	if users[1] != "## Async task results" {
		t.Errorf("injected user message was changed: %q", users[1])
	}
	if users[2] != "second prompt"+pruneReminder {
		t.Errorf("second prompt not suffixed: %q", users[2])
	}
	if strings.Count(users[0], "context upkeep") != 1 {
		t.Error("reminder repeated within one prompt")
	}
}

func TestPruneReminderNeverStored(t *testing.T) {
	app := pruneTestApp()
	app.curTurn = 1
	app.appendConv(userMsgAt(1, "original prompt"))
	_ = app.requestMessages()
	_ = app.requestMessages()
	if got := DerefStr(app.Conv[0].Content); got != "original prompt" {
		t.Errorf("stored prompt mutated: %q", got)
	}
}

func TestPruneReminderStableAcrossRequests(t *testing.T) {
	// Prompt-cache stability: the suffixed bytes of an earlier prompt must be
	// identical on every later request, regardless of how the tail grows.
	app := pruneTestApp()
	app.curTurn = 1
	app.appendConv(userMsgAt(1, "task"))
	first := DerefStr(app.requestMessages()[0].Content)
	for i := 0; i < 5; i++ {
		appendRound(app, 1, "c"+string(rune('a'+i)), "read_file", `{"path":"a.go"}`, bigContent)
	}
	if got := DerefStr(app.requestMessages()[0].Content); got != first {
		t.Errorf("prefix bytes changed between requests:\n%q\n%q", first, got)
	}
}

func TestPruneReminderSkipsCollapsedAndHandoffMessages(t *testing.T) {
	app := pruneTestApp()
	app.collapsed = map[int]CollapseRecord{1: {Turn: 1}}
	in := []proxy.Message{
		{Role: "user", Turn: 0, Content: StrPtr("handoff seed")},
		userMsgAt(1, "collapsed prompt"),
		userMsgAt(2, "[older request 2 — details archived. x]"),
		userMsgAt(3, "live prompt"),
	}
	out := app.withPruneReminder(in)
	for i, want := range []string{"handoff seed", "collapsed prompt", "[older request 2 — details archived. x]", "live prompt" + pruneReminder} {
		if got := DerefStr(out[i].Content); got != want {
			t.Errorf("msg %d = %q, want %q", i, got, want)
		}
	}
	// Input slice untouched.
	if DerefStr(in[3].Content) != "live prompt" {
		t.Error("input message mutated")
	}
}

func TestPruneReminderTextMentionsMarkingContract(t *testing.T) {
	for _, s := range []string{"context_mark", "[rN]", "stub", "drop", "not at the end"} {
		if !strings.Contains(pruneReminder, s) {
			t.Errorf("reminder lacks %q", s)
		}
	}
	if strings.Contains(pruneReminder, "wakil.pro") {
		t.Error("forbidden domain")
	}
}

func TestPruneMarksCounterSurvivesResume(t *testing.T) {
	app := pruneTestApp()
	app.pruneMarkCount = 3
	s := &Session{PruneMarkCount: app.pruneMarkCount}
	app2 := pruneTestApp()
	app2.RestoreTurnState(s)
	if app2.pruneMarkCount != 3 {
		t.Errorf("marks after resume = %d, want 3", app2.pruneMarkCount)
	}
	if !strings.Contains(app2.pruneStatusLine(), "marks 3") {
		t.Errorf("status: %s", app2.pruneStatusLine())
	}
}

func TestPruneMissingPrevCountsRepliesWithoutPrev(t *testing.T) {
	app := pruneTestApp()
	app.curTurn = 1
	// Two earlier rounds so the obligation applies.
	appendRound(app, 1, "c1", "read_file", `{"path":"a"}`, "x")
	appendRound(app, 1, "c2", "read_file", `{"path":"b"}`, "x")

	call := func(name, args string) proxy.Message {
		return proxy.Message{Role: "assistant", ToolCalls: []proxy.ToolCall{{ID: "n", Type: "function", Function: proxy.FunctionCall{Name: name, Arguments: args}}}}
	}
	// A reply without prev counts once.
	m := call("read_file", `{"path":"c"}`)
	app.stripPrevFromMessage(&m)
	if app.pruneMissingPrev != 1 {
		t.Fatalf("missing = %d, want 1", app.pruneMissingPrev)
	}
	// A reply carrying prev does not count.
	m = call("read_file", `{"path":"d","prev":"drop"}`)
	app.stripPrevFromMessage(&m)
	if app.pruneMissingPrev != 1 {
		t.Errorf("reply with prev counted: %d", app.pruneMissingPrev)
	}
	// A pure context_mark reply owes no rating.
	m = call("context_mark", `{"drop":["r1"]}`)
	app.stripPrevFromMessage(&m)
	if app.pruneMissingPrev != 1 {
		t.Errorf("context_mark reply counted: %d", app.pruneMissingPrev)
	}
	// A final answer with no tool calls is not a tool reply.
	app.stripPrevFromMessage(&proxy.Message{Role: "assistant", Content: StrPtr("done")})
	if app.pruneMissingPrev != 1 {
		t.Errorf("final answer counted: %d", app.pruneMissingPrev)
	}
}
