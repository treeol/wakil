package counsel

// Stream-collection plumbing for Mashūra calls.
//
// Every model call in this package goes through the single transport
// (internal/proxy, see proxy.Exec): this file no longer opens HTTP requests
// of its own. It only carries the stream timeouts and the sentinel that the
// retry machinery matches on.

import (
	"errors"
	"time"
)

// ErrBackendStream is a sentinel for retryable backend stream errors from
// counsel streaming calls. Mirrors proxy.ErrBackendStream but is local to
// avoid a package dependency.
var ErrBackendStream = errors.New("counsel backend stream error")

// counselStreamIdleTimeout is the default idle timeout for streaming Mashūra
// calls: if no bytes arrive for this duration, the stream is considered
// stalled and a retryable error is returned. Reasoning models can
// legitimately take minutes to produce the first token, but a silent
// connection for 120s is a stall.
const counselStreamIdleTimeout = 120 * time.Second

// counselStreamTotalCap is the absolute upper bound on a single streaming
// Mashūra call. A call that runs this long without completing is cancelled.
const counselStreamTotalCap = 30 * time.Minute
