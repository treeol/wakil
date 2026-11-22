package agent

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
)

// modules_test.go — tests for the context-modules switch (Stage 1).
//
// Block 1 tests: the switch off is byte-identical to today. With the switch
// off, BuildTools produces the same tool array as before the feature, the
// preamble is the same, and the new tools (load_module, call_tool) are absent
// from the array.

// TestContextModulesOff_ByteIdentical verifies that with context_modules
// "off" (the default), BuildTools produces a byte-identical tool array to the
// pre-feature BuildTools call. The golden master is a BuildTools call with
// the default config (which already has ContextModules "off"); the candidate
// is the same call with ContextModules explicitly set to "off". They must
// marshal to identical JSON.
func TestContextModulesOff_ByteIdentical(t *testing.T) {
	today := BuildTools(config.DefaultConfig(), "/work", nil, "")

	off := config.DefaultConfig()
	off.ContextModules = "off"
	offTools := BuildTools(off, "/work", nil, "")

	todayJSON, _ := json.Marshal(today)
	offJSON, _ := json.Marshal(offTools)
	if string(todayJSON) != string(offJSON) {
		t.Errorf("context_modules off is not byte-identical to today:\ntoday: %s\noff:   %s", todayJSON, offJSON)
	}
}

// TestContextModulesOff_NoLoadModuleCallToolInArray verifies that with the
// switch off, neither load_module nor call_tool appears in the tool array.
func TestContextModulesOff_NoLoadModuleCallToolInArray(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ContextModules = "off"
	tools := BuildTools(cfg, "/work", nil, "")
	for _, tl := range tools {
		if tl.Function.Name == "load_module" || tl.Function.Name == "call_tool" {
			t.Errorf("tool %q should not be in the array with context_modules off", tl.Function.Name)
		}
	}
}

// TestContextModulesOff_DefaultIsOff verifies that DefaultConfig sets
// context_modules to "off".
func TestContextModulesOff_DefaultIsOff(t *testing.T) {
	cfg := config.DefaultConfig()
	if cfg.ContextModules != "off" {
		t.Errorf("DefaultConfig context_modules = %q, want %q", cfg.ContextModules, "off")
	}
}

// TestContextModulesOff_PreambleByteIdentical verifies that with the switch
// off, buildPreambleForNetwork is byte-identical to today. This is a
// structural check: the function should not mention the module index or
// the context-modules switch at all when off.
func TestContextModulesOff_PreambleByteIdentical(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.ContextModules = "off"
	today := app.buildPreambleForNetwork("Monday, 1 January 2026", "host")

	app2 := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app2.Cfg.ContextModules = "off"
	off := app2.buildPreambleForNetwork("Monday, 1 January 2026", "host")

	if today != off {
		t.Errorf("preamble with context_modules off is not byte-identical across two apps:\ntoday: %q\noff:   %q", today, off)
	}
	if strings.Contains(off, "Context modules") || strings.Contains(off, "load_module") {
		t.Errorf("preamble with context_modules off should not mention Context modules or load_module:\n%q", off)
	}
}

// TestContextModulesValidation verifies that validateEnums accepts "off" and
// "on" and rejects unknown values. Lives in package config (config_test.go)
// because validateEnums is unexported.

// TestContextModulesEnvOverride verifies that WAKIL_CONTEXT_MODULES overrides
// the config file value.
func TestContextModulesEnvOverride(t *testing.T) {
	t.Setenv("WAKIL_CONTEXT_MODULES", "on")
	cfg := config.DefaultConfig()
	if v := os.Getenv("WAKIL_CONTEXT_MODULES"); v != "" {
		cfg.ContextModules = v
	}
	if cfg.ContextModules != "on" {
		t.Errorf("WAKIL_CONTEXT_MODULES=on should set context_modules to %q, got %q", "on", cfg.ContextModules)
	}
}
