package config

import "testing"

// TestReasoningBudgetFor verifies the effort→budget mapping and that
// MaxTokens takes precedence over Effort.
func TestReasoningBudgetFor(t *testing.T) {
	tests := []struct {
		name     string
		spec     *ReasoningSpec
		wantZero bool
		want     int
	}{
		{"nil spec", nil, true, 0},
		{"zero spec", &ReasoningSpec{}, true, 0},
		{"effort none", &ReasoningSpec{Effort: "none"}, true, 0},
		{"effort empty", &ReasoningSpec{Effort: ""}, true, 0},
		{"effort low", &ReasoningSpec{Effort: "low"}, false, EffortToBudget["low"]},
		{"effort medium", &ReasoningSpec{Effort: "medium"}, false, EffortToBudget["medium"]},
		{"effort high", &ReasoningSpec{Effort: "high"}, false, EffortToBudget["high"]},
		{"max_tokens wins", &ReasoningSpec{Effort: "low", MaxTokens: 9999}, false, 9999},
		{"max_tokens only", &ReasoningSpec{MaxTokens: 5000}, false, 5000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ReasoningBudgetFor(tt.spec)
			if tt.wantZero {
				if got != 0 {
					t.Errorf("ReasoningBudgetFor(%v) = %d, want 0", tt.spec, got)
				}
				return
			}
			if got != tt.want {
				t.Errorf("ReasoningBudgetFor(%v) = %d, want %d", tt.spec, got, tt.want)
			}
		})
	}
}

// TestReasoningSpecIsZero verifies the IsZero method.
func TestReasoningSpecIsZero(t *testing.T) {
	if !(&ReasoningSpec{}).IsZero() {
		t.Error("zero ReasoningSpec should be IsZero")
	}
	if !(&ReasoningSpec{Effort: ""}).IsZero() {
		t.Error("empty-effort ReasoningSpec should be IsZero")
	}
	if (&ReasoningSpec{Effort: "medium"}).IsZero() {
		t.Error("medium-effort ReasoningSpec should not be IsZero")
	}
	if (&ReasoningSpec{MaxTokens: 100}).IsZero() {
		t.Error("max_tokens ReasoningSpec should not be IsZero")
	}
}

// TestValidateReasoningSpec verifies that valid and invalid effort levels
// are correctly validated.
func TestValidateReasoningSpec(t *testing.T) {
	valid := []string{"", "none", "minimal", "low", "medium", "high", "xhigh", "max", "off"}
	for _, effort := range valid {
		if err := validateReasoningSpec("test", ReasoningSpec{Effort: effort}); err != nil {
			t.Errorf("validateReasoningSpec(%q) unexpectedly failed: %v", effort, err)
		}
	}
	invalid := []string{"turbo", "extra", "ultra"}
	for _, effort := range invalid {
		if err := validateReasoningSpec("test", ReasoningSpec{Effort: effort}); err == nil {
			t.Errorf("validateReasoningSpec(%q) should have failed", effort)
		}
	}
	if err := validateReasoningSpec("test", ReasoningSpec{MaxTokens: -1}); err == nil {
		t.Error("validateReasoningSpec with negative max_tokens should have failed")
	}
}
