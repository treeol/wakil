package config

import "testing"

func TestBudgetConfigRejectsReserveNotBelowClass(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SubagentBudgetReserve = 25 // equals the default search budget (25)
	cfg.SubagentBudgetSearch = 0
	if err := validateContextLimits(cfg); err == nil {
		t.Fatal("reserve == effective search budget must be rejected")
	}
	cfg = DefaultConfig()
	cfg.SubagentBudgetRead = 7 // below default reserve 8
	if err := validateContextLimits(cfg); err == nil {
		t.Fatal("class below reserve must be rejected")
	}
	if err := validateContextLimits(DefaultConfig()); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
}
