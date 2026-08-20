package main

// signal.go: graceful shutdown handling for wakild (card #148 P2d).
// SIGTERM and SIGINT trigger a graceful drain: stop accepting new connections,
// drain running turns up to --shutdown-timeout, then close the host and store.

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// waitForSignal blocks until SIGTERM or SIGINT is received, then returns a
// context cancelled with the signal. The context is also cancelled if the
// parent context is cancelled (e.g. during startup failure).
func waitForSignal(ctx context.Context) context.Context {
	ctx, cancel := context.WithCancel(ctx)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		select {
		case <-ch:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx
}
