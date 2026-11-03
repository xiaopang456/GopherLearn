package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStopFileCancelsBeforeAndDuringWork(t *testing.T) {
	path := filepath.Join(t.TempDir(), "STOP")
	ctx, cancel := watchStopFile(context.Background(), path)
	defer cancel()
	if ctx.Err() != nil {
		t.Fatal("absent file stopped work")
	}
	if err := os.WriteFile(path, []byte("stop"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("stop was not observed")
	}
	before, done := watchStopFile(context.Background(), path)
	defer done()
	if before.Err() == nil || context.Cause(before).Error() != "stop file present" {
		t.Fatal("preexisting stop file ignored")
	}
}
