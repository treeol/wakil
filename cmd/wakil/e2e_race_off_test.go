//go:build !race

package main

import "time"

// e2eRPCTimeout bounds each e2e test's context.
const e2eRPCTimeout = 10 * time.Second
