package redis

import (
	"context"
	"testing"
	"time"
)

func TestAllowFixedWindowRejectsInvalidConfigurationWithoutRedis(t *testing.T) {
	original := Rdb
	Rdb = nil
	t.Cleanup(func() { Rdb = original })
	if allowed, err := AllowFixedWindow(context.Background(), "rate:test", 1, time.Second); err == nil || allowed {
		t.Fatalf("AllowFixedWindow() = (%v, %v), want fail closed", allowed, err)
	}
}
