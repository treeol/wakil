package remote

import (
	"errors"
	"testing"

	"connectrpc.com/connect"
)

// TestIsSessionStateUnavailableClassifiesWriteRace pins the classifier for the
// race where the daemon's mux 404s an unmounted SessionStateService path and
// closes the connection while the client is still writing. Depending on
// timing, the client's write fails with EPIPE, and Connect reports that as
// CodeCanceled (not CodeUnavailable). It must still degrade gracefully.
func TestIsSessionStateUnavailableClassifiesWriteRace(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"unimplemented (older daemon)", connect.NewError(connect.CodeUnimplemented, errors.New("404")), true},
		{"unavailable broken pipe", connect.NewError(connect.CodeUnavailable, errors.New("write: broken pipe")), true},
		{"canceled broken pipe (write race)", connect.NewError(connect.CodeCanceled, errors.New("Post ...: write: broken pipe")), true},
		{"canceled, no broken pipe (real cancel)", connect.NewError(connect.CodeCanceled, errors.New("context canceled")), false},
		{"internal error", connect.NewError(connect.CodeInternal, errors.New("boom")), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isSessionStateUnavailable(c.err); got != c.want {
				t.Fatalf("isSessionStateUnavailable(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
