package counsel

import (
	"os"
	"testing"
	"time"
)

// TestMain shortens the 429/5xx retry backoff (5–15 s in production) so tests
// that simulate provider failures with 5xx statuses stay fast. TestBackoffRange
// checks the production range via prodRetryBackoff.
func TestMain(m *testing.M) {
	prodRetryBackoff = retryBackoff
	retryBackoff = func() time.Duration { return time.Millisecond }
	os.Exit(m.Run())
}

var prodRetryBackoff func() time.Duration
