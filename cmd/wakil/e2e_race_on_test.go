//go:build race

package main

import "time"

// e2eRPCTimeout bounds each e2e test's context. Under -race the daemon
// round-trips are much slower, so the budget scales up.
const e2eRPCTimeout = 60 * time.Second
