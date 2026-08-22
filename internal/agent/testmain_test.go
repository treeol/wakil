package agent

// TestMain isolates every test in this package from the real on-disk session
// store: without it, any test that constructs an App and sends a turn (or
// otherwise triggers SaveSession) without its own t.Setenv wrote transcripts
// into the user's actual ~/.local/share/wakil/sessions directory — hundreds
// of leaked stubs were found there. Tests that need their own store keep
// working: t.Setenv(WAKIL_SESSIONS_DIR, ...) overrides this for the test's
// duration and restores it afterwards.

import (
	"log"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "wakil-agent-test-sessions")
	if err != nil {
		log.Printf("agent: cannot create temp sessions dir for test isolation: %v", err)
	} else {
		if err := os.Setenv("WAKIL_SESSIONS_DIR", dir); err != nil {
			log.Printf("agent: cannot pin WAKIL_SESSIONS_DIR for tests: %v", err)
		}
	}
	code := m.Run()
	if dir != "" {
		os.RemoveAll(dir)
	}
	os.Exit(code)
}
