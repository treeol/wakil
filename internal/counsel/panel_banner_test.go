package counsel

// panel_banner_test.go pins B2: the NOT COMPLETED banner appears only when
// no member produced a usable answer. When a later member succeeded, each
// failure is named inline (member + why) instead.

import (
	"strings"
	"testing"
)

func usableResult(model string) PanelMemberResult {
	return PanelMemberResult{PrefixedModel: "openrouter:" + model, Model: model, Answer: "a usable review"}
}

func voidResult(model, reason string) PanelMemberResult {
	return PanelMemberResult{
		PrefixedModel: "openrouter:" + model, Model: model,
		Void: true, Err: &VoidError{Reason: reason},
	}
}

func budgetResult(model, reason string) PanelMemberResult {
	return PanelMemberResult{
		PrefixedModel: "openrouter:" + model, Model: model,
		BudgetStopped: true, BudgetNote: reason, Err: &BudgetError{Reason: reason},
	}
}

// TestMixedPanelNoBannerNamesFailures: one usable member + one VOID member
// shows no NOT COMPLETED banner and names the failed member with its reason.
func TestMixedPanelNoBannerNamesFailures(t *testing.T) {
	out := FormatPanelResult([]PanelMemberResult{
		usableResult("m1"),
		voidResult("m2", "truncated reply"),
	})
	if strings.Contains(out, "NOT COMPLETED") {
		t.Errorf("mixed panel must not show NOT COMPLETED, got:\n%s", out)
	}
	if !strings.Contains(out, "m2") || !strings.Contains(out, "did not produce a review") {
		t.Errorf("failed member must be named inline, got:\n%s", out)
	}
	if !strings.Contains(out, "truncated reply") {
		t.Errorf("inline failure must carry the reason, got:\n%s", out)
	}
}

// TestMixedPanelBudgetFailureNamedInline: a budget-stopped member beside a
// usable one names the stopped member without the VOID banner. (The member
// is named via its BudgetStop verdict line, which happens to carry budget
// wording; the panel-level budget banner — the "review budget reached; what
// was obtained is kept" text — must not appear.)
func TestMixedPanelBudgetFailureNamedInline(t *testing.T) {
	out := FormatPanelResult([]PanelMemberResult{
		usableResult("m1"),
		budgetResult("m2", "review budget reached"),
	})
	if strings.Contains(out, "what was obtained is kept") {
		t.Errorf("mixed panel must not show the budget banner, got:\n%s", out)
	}
	if !strings.Contains(out, "m2") || !strings.Contains(out, "review budget reached") {
		t.Errorf("stopped member must be named inline with its reason, got:\n%s", out)
	}
}

// TestAllVoidPanelKeepsBanner: when no member is usable, the VOID banner
// still appears.
func TestAllVoidPanelKeepsBanner(t *testing.T) {
	out := FormatPanelResult([]PanelMemberResult{
		voidResult("m1", "empty reply"),
		voidResult("m2", "truncated reply"),
	})
	if !strings.Contains(out, "NOT COMPLETED") {
		t.Errorf("all-void panel must show NOT COMPLETED, got:\n%s", out)
	}
}

// TestAllBudgetPanelKeepsBanner: when every member is budget-stopped, the
// budget banner still appears (matched on its "what was obtained is kept"
// text — the per-member BudgetStop line carries budget wording of its own).
func TestAllBudgetPanelKeepsBanner(t *testing.T) {
	out := FormatPanelResult([]PanelMemberResult{
		budgetResult("m1", "review budget reached"),
	})
	if !strings.Contains(out, "what was obtained is kept") {
		t.Errorf("all-budget panel must show the budget banner, got:\n%s", out)
	}
}
