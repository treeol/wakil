//go:build race

package main

import "time"

// raceEnabled reports whether the test binary was built with -race. The e2e
// suite runs many daemon round-trips; under the race detector they are much
// slower, so the RPC budget scales up.
const raceEnabled = true

// e2eRPCTimeout bounds each e2e test's context. Scaled under -race.
const e2eRPCTimeout = 60 * time.Second
