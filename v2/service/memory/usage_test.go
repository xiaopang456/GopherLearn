package memory

import (
	"context"
	"testing"
	"time"
)

type usageMarker struct{ events chan usageEvent }

func (m usageMarker) MarkUsed(_ context.Context, userID int64, ids []string, at time.Time) error {
	m.events <- usageEvent{userID: userID, ids: append([]string(nil), ids...), at: at}
	return nil
}

func TestAsyncUsageRecorderDoesNotBlockRecallPath(t *testing.T) {
	marker := usageMarker{events: make(chan usageEvent, 1)}
	recorder, err := NewAsyncUsageRecorder(marker, 2, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := recorder.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if !recorder.Record(7, []string{"m1", "m2"}, time.Now()) {
		t.Fatal("usage event was not accepted")
	}
	select {
	case event := <-marker.events:
		if event.userID != 7 || len(event.ids) != 2 {
			t.Fatalf("event=%+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("usage event was not processed")
	}
	stopCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := recorder.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
}
