package main

// TestMain isolates every test in this package from the real on-disk session
// store (see internal/agent/testmain_test.go for the rationale). Tests that
// set their own WAKIL_SESSIONS_DIR keep working: t.Setenv overrides this for
// the test's duration and restores it afterwards.

import (
	"log"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "wakil-cmd-test-sessions")
	if err != nil {
		log.Printf("cmd/wakil: cannot create temp sessions dir for test isolation: %v", err)
	} else {
		if err := os.Setenv("WAKIL_SESSIONS_DIR", dir); err != nil {
			log.Printf("cmd/wakil: cannot pin WAKIL_SESSIONS_DIR for tests: %v", err)
		}
	}
	code := m.Run()
	if dir != "" {
		os.RemoveAll(dir)
	}
	os.Exit(code)
}
