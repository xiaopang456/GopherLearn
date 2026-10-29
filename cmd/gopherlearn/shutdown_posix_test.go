//go:build !windows

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

// TestSIGTERMReachesTheDrainPath sends the real signal to this process, so the wiring main uses
// - signal.NotifyContext feeding serveUntil - is what is under test rather than a context a
// test cancelled by hand.
func TestSIGTERMReachesTheDrainPath(t *testing.T) {
	server, _, url, serve := listeningStream(t, countingStream(8, 30*time.Millisecond))
	started := make(chan struct{}, 1)
	client := make(chan streamResult, 1)
	go func() { client <- readStream(url, started) }()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- serveUntil(ctx, server, 2*time.Second, serve) }()
	<-started

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := <-served; err != nil {
		t.Fatalf("after SIGTERM serveUntil = %v, want a clean drained stop", err)
	}
	result := <-client
	if result.events != 8 || !result.finished || result.err != nil {
		t.Fatalf("SIGTERM cut the stream short: %+v", result)
	}
}
