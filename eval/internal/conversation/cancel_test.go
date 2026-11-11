package conversation

import (
	"context"
	"errors"
	"testing"
)

func TestCancelledWorkCannotOpenProductResources(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, calls, err := PrepareOfflineProduct(ctx, "missing-checkout", nil, AttemptInput{}); !errors.Is(err, context.Canceled) || len(calls) != 0 {
		t.Fatalf("prepare calls=%v err=%v", calls, err)
	}
	result, err := RunOfflineProduct(ctx, "missing-checkout", Snapshot{}, AttemptInput{Questions: []string{"question"}})
	if !errors.Is(err, context.Canceled) || result.Error == "" || result.BackgroundAudit.Valid {
		t.Fatalf("cancelled result %+v err=%v", result, err)
	}
}
