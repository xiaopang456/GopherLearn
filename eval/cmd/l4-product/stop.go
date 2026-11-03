package main

import (
	"context"
	"fmt"
	"os"
	"time"
)

// watchStopFile fails closed on unreadable control paths. It cancels in-flight
// preparation/questions as well as checking before the first resource opens.
func watchStopFile(parent context.Context, path string) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	check := func() {
		if path == "" {
			return
		}
		_, err := os.Stat(path)
		if err == nil {
			cancel(fmt.Errorf("stop file present"))
		} else if !os.IsNotExist(err) {
			cancel(fmt.Errorf("stop file cannot be inspected"))
		}
	}
	check()
	if path != "" {
		go func() {
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					check()
				}
			}
		}()
	}
	return ctx, func() { cancel(context.Canceled) }
}
