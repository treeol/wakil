//go:build !race

package main

import "time"

// raceEnabled reports whether the test binary was built with -race.
const raceEnabled = false

// e2eRPCTimeout bounds each e2e test's context.
const e2eRPCTimeout = 10 * time.Second
