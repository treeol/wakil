package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/initcmd"
)

func TestFirstRunTTYYesRunsInit(t *testing.T) {
	var out, errb bytes.Buffer
	ran := false
	retry, code := firstRun(&config.NoConfigError{Path: "/x/config.json"}, true, strings.NewReader("y\n"), &out, &errb, func(o initcmd.Options) int {
		ran = true
		if o.Force {
			t.Error("a missing config must not need --force")
		}
		return 0
	})
	if !retry || code != 0 || !ran {
		t.Errorf("retry=%v code=%d ran=%v", retry, code, ran)
	}
	if !strings.Contains(out.String(), "No config found. Run setup now? [Y/n]") {
		t.Errorf("prompt: %q", out.String())
	}
}

func TestFirstRunTTYEnterMeansYes(t *testing.T) {
	var out, errb bytes.Buffer
	ran := false
	retry, _ := firstRun(&config.NoConfigError{}, true, strings.NewReader("\n"), &out, &errb, func(initcmd.Options) int { ran = true; return 0 })
	if !retry || !ran {
		t.Error("an empty answer is the default, Yes")
	}
}

func TestFirstRunTTYNoPrintsHintAndExits1(t *testing.T) {
	var out, errb bytes.Buffer
	retry, code := firstRun(&config.NoConfigError{}, true, strings.NewReader("n\n"), &out, &errb, func(initcmd.Options) int {
		t.Fatal("init must not run on No")
		return 0
	})
	if retry || code != 1 || !strings.Contains(errb.String(), "wakil init") {
		t.Errorf("retry=%v code=%d err=%q", retry, code, errb.String())
	}
}

func TestFirstRunNoTTY(t *testing.T) {
	var out, errb bytes.Buffer
	retry, code := firstRun(&config.NoConfigError{}, false, strings.NewReader("y\n"), &out, &errb, func(initcmd.Options) int {
		t.Fatal("init must not run without a TTY")
		return 0
	})
	if retry || code != 1 {
		t.Errorf("retry=%v code=%d", retry, code)
	}
	if strings.TrimSpace(errb.String()) != "no config: run `wakil init` (or see README Quickstart)" {
		t.Errorf("message = %q", errb.String())
	}
	if out.Len() != 0 {
		t.Errorf("nothing may be printed to stdout: %q", out.String())
	}
}

func TestFirstRunStubDefaultsToNo(t *testing.T) {
	var out, errb bytes.Buffer
	retry, code := firstRun(&config.NoConfigError{Path: "/x/config.json", FileExists: true}, true, strings.NewReader("\n"), &out, &errb, func(initcmd.Options) int {
		t.Fatal("an existing file must only be replaced on an explicit y")
		return 0
	})
	if retry || code != 1 || !strings.Contains(out.String(), "[y/N]") {
		t.Errorf("retry=%v code=%d prompt=%q", retry, code, out.String())
	}
}

func TestFirstRunStubOffersForceWithBackup(t *testing.T) {
	var out, errb bytes.Buffer
	var got initcmd.Options
	_, _ = firstRun(&config.NoConfigError{Path: "/x/config.json", FileExists: true}, true, strings.NewReader("y\n"), &out, &errb, func(o initcmd.Options) int { got = o; return 0 })
	if !got.Force || !strings.Contains(out.String(), "backed up first") {
		t.Errorf("force=%v prompt=%q", got.Force, out.String())
	}
}
