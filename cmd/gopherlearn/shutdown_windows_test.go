//go:build windows

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"testing"
)

// TestSIGTERMReachesTheDrainPath has no Windows equivalent of "send SIGTERM to this process",
// so the case says so instead of passing vacuously. What is still proven here is the wiring
// Windows does have: signal.NotifyContext for SIGTERM returns a context that stays live until
// something asks for a stop. The signal itself is delivered by the container runtime on Linux,
// and that path is verified against the real stack with `docker stop`.
func TestSIGTERMReachesTheDrainPath(t *testing.T) {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer cancel()
	notify := make(chan os.Signal, 1)
	signal.Notify(notify, syscall.SIGTERM)
	signal.Stop(notify)
	if ctx.Err() != nil {
		t.Fatalf("the stop context is already done before anything asked for a stop: %v", ctx.Err())
	}
	t.Skip("Windows delivers no SIGTERM to a Go process; docker stop covers the drain path")
}
