package agent

import (
	"testing"

	"github.com/treeol/wakil/internal/proxy"
)

// TestResumePathsAgree is the Group 3 parity pin: resuming the same saved
// session through each entry point must yield the same cost totals, turn
// state and approvals. The immediate surfaces are InstallSession (headless
// and direct-id target implementation) and ResumeSessionMsg (in-process
// /resume + TUI picker target implementation) — both of which must delegate
// to InstallSession so the state is installed identically.
//
// Pre-state models "the session before": the App carries cost rows, a grant,
// and an approval set (auto + reads + destructive on) that must carry over
// for auto/reads/grants, while destructive must not carry over, and cost
// rows must be REPLACED (not merged) by the resumed session's rows.
func TestResumePathsAgree(t *testing.T) {
	saved := &Session{
		ChatID: "resumed-1",
		Conv: []proxy.Message{
			{Role: "user", Content: StrPtr("hello"), Turn: 1},
			{Role: "assistant", Content: StrPtr("world"), Turn: 1},
			{Role: "user", Content: StrPtr("again"), Turn: 2},
		},
		CostRows: []proxy.CostRow{
			{Source: "oldsrc", Calls: 1, InputTok: 50, OutputTok: 25, CostUSD: 0.10, Priced: true, Confidence: "exact", Category: proxy.CategoryForSource("oldsrc")},
		},
		Keep: []int{2},
	}

	seed := func() *App {
		a := &App{}
		a.Costs = proxy.NewCostTracker()
		a.Costs.Record("src-a", 100, 50, 0.30, true, "exact")
		a.Client = &proxy.Client{ChatID: "prev"}
		a.SetConsent(ConsentSnapshot{AutoApprove: true, AllowDestructive: true, AllowReads: true})
		a.AddGrant("read_file")
		return a
	}

	assertInstalled := func(t *testing.T, name string, a *App) {
		t.Helper()
		// Transcript.
		if len(a.ConvSnapshot()) != 3 {
			t.Errorf("%s: conv len = %d, want 3", name, len(a.ConvSnapshot()))
		}
		// Cost rows replaced with the saved session's rows exactly.
		total, rows := a.Costs.Snapshot()
		if total != 0.10 {
			t.Errorf("%s: cost total = %v, want 0.10 (replaced)", name, total)
		}
		if len(rows) != 1 || rows[0].Source != "oldsrc" {
			t.Errorf("%s: cost rows = %v, want exactly [oldsrc]", name, rows)
		}
		// Turn state restored.
		a.stateMu.RLock()
		ts := a.turnSeq
		kept := a.keep[2]
		a.stateMu.RUnlock()
		if ts != 2 {
			t.Errorf("%s: turnSeq = %d, want 2 (max Turn in conv)", name, ts)
		}
		if !kept {
			t.Errorf("%s: keep set = false for turn 2, want restored", name)
		}
		// Approvals: auto + reads + grants carry over; destructive does not.
		c := a.Consent()
		if !c.AutoApprove {
			t.Errorf("%s: AutoApprove = false, want carried over", name)
		}
		if !c.AllowReads {
			t.Errorf("%s: AllowReads = false, want carried over", name)
		}
		if c.AllowDestructive {
			t.Errorf("%s: AllowDestructive = true, must not carry over", name)
		}
		if len(a.Grants()) != 1 || a.Grants()[0].Tool != "read_file" {
			t.Errorf("%s: grants = %v, want [read_file] carried over", name, a.Grants())
		}
		// Session identity.
		if a.Client.ChatID != "resumed-1" {
			t.Errorf("%s: ChatID = %q, want resumed-1", name, a.Client.ChatID)
		}
	}

	// Entry point 1: InstallSession (the one implementation).
	a1 := seed()
	a1.InstallSession(saved)
	assertInstalled(t, "InstallSession", a1)

	// Entry point 2: ResumeSessionMsg (in-process /resume + TUI picker).
	a2 := seed()
	ResumeSessionMsg(a2, saved)
	assertInstalled(t, "ResumeSessionMsg", a2)
}

// TestNewConversationApprovalsCarryOver pins the /new side of the decided
// behaviour: auto mode and read access that are active stay active in the
// new session; the destructive grant does not carry over.
func TestNewConversationApprovalsCarryOver(t *testing.T) {
	a := &App{}
	a.Client = &proxy.Client{}
	a.SetConsent(ConsentSnapshot{AutoApprove: true, AllowDestructive: true, AllowReads: true})
	a.NewConversation("c-1")
	c := a.Consent()
	if !c.AutoApprove {
		t.Error("NewConversation: AutoApprove lost, want carried over")
	}
	if !c.AllowReads {
		t.Error("NewConversation: AllowReads lost, want carried over")
	}
	if c.AllowDestructive {
		t.Error("NewConversation: AllowDestructive carried over, must not")
	}
}
