package conversation

import (
	"context"
	"testing"

	"GopherAI/model"
)

// The adapter the harness builds its read windows over must forward the
// builder's newest-first fetch to the store's newest-first method. Wiring it
// to the chronological prefix instead was silent: the call still returned
// messages, the window still assembled, and only the content was wrong - the
// oldest part of a long unsummarized tail, with every message newer than the
// fetch reached left out of the prompt. The fakes below record which method
// the adapter actually touched.

type recordingHistoryStore struct {
	rows        []model.Message
	recentCalls int
	afterCalls  int
	afterID     uint
	limit       int
}

func (s *recordingHistoryStore) ListRecentAfter(_ context.Context, _ string, afterID uint, limit int) ([]model.Message, error) {
	s.recentCalls++
	s.afterID, s.limit = afterID, limit
	out := make([]model.Message, len(s.rows))
	copy(out, s.rows)
	return out, nil
}

func (s *recordingHistoryStore) ListAfter(_ context.Context, _ string, afterID uint, limit int) ([]model.Message, error) {
	s.afterCalls++
	s.afterID, s.limit = afterID, limit
	out := make([]model.Message, len(s.rows))
	copy(out, s.rows)
	return out, nil
}

func TestTheAdapterSendsTheNewestFetchToTheNewestMethod(t *testing.T) {
	rows := []model.Message{
		{ID: 71, IsUser: true, Content: "newest user"},
		{ID: 72, IsUser: false, Content: "newest assistant"},
	}
	store := &recordingHistoryStore{rows: rows}
	adapter := historyAdapter{store: store, sessionID: "session"}

	returned, err := adapter.ListRecentAfter(context.Background(), "session", 70, 8)
	if err != nil {
		t.Fatalf("ListRecentAfter: %v", err)
	}
	if store.recentCalls != 1 {
		t.Fatalf("the newest fetch reached the store %d times through the newest method, want 1", store.recentCalls)
	}
	if store.afterCalls != 0 {
		t.Fatalf("the newest fetch reached the chronological prefix %d times, want 0", store.afterCalls)
	}
	if store.afterID != 70 || store.limit != 8 {
		t.Fatalf("watermark/limit forwarded as %d/%d, want 70/8", store.afterID, store.limit)
	}
	if len(returned) != len(rows) || returned[0].ID != 71 || returned[1].ID != 72 {
		t.Fatalf("rows did not pass through unchanged: %+v", returned)
	}
}
