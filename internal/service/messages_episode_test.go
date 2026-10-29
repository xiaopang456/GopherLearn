package service

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// recordingEpisodePublisher records every conversation-completion event the
// turn pipeline offers, so the trigger's facts - owner, session, and the
// stored assistant message id - are observable without a running worker.
type recordingEpisodePublisher struct {
	mu     sync.Mutex
	events [][3]string
	err    error
}

func (r *recordingEpisodePublisher) PublishConversationCompleted(_ context.Context, userID, sessionID, latestMessageID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, [3]string{userID, sessionID, latestMessageID})
	return r.err
}

func (r *recordingEpisodePublisher) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

func (r *recordingEpisodePublisher) last() [3]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.events[len(r.events)-1]
}

// TestEpisodeTriggerFiresAfterTheAssistantRowIsStored pins the production
// trigger on both turn paths: the publish carries the id of the assistant
// message that was actually stored, and a failed turn publishes nothing.
func TestEpisodeTriggerFiresAfterTheAssistantRowIsStored(t *testing.T) {
	publisher := &recordingEpisodePublisher{}
	svc, store, userID, sessionID := newMessageFixture(t, &fixedResponder{answer: "已收到。"})
	// Re-wire the fixture's service with the trigger installed.
	svc.episodes = publisher

	// A completed turn publishes exactly once, with the stored id.
	pair, err := svc.Send(context.Background(), userID, sessionID, "我决定用 JSON 编解码器。", domain.KnowledgeOptions{})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if publisher.count() != 1 {
		t.Fatalf("publish count = %d, want 1", publisher.count())
	}
	event := publisher.last()
	if event[0] != userID || event[1] != sessionID || event[2] != pair.Assistant.ID {
		t.Fatalf("publish event = %v, want the stored assistant row", event)
	}
	stored, err := store.ListMessages(context.Background(), userID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if stored[len(stored)-1].ID != event[2] {
		t.Fatal("the published id is not the stored assistant message")
	}

	// The streamed path triggers the same way, after the terminal frame.
	if err := svc.Stream(context.Background(), userID, sessionID, "再问一个。", domain.KnowledgeOptions{},
		func(StreamEvent) error { return nil }); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if publisher.count() != 2 {
		t.Fatalf("publish count after stream = %d, want 2", publisher.count())
	}

	// A failed model call stores no answer and publishes nothing.
	before := publisher.count()
	failingSvc, _, _, _ := newMessageFixture(t, &fixedResponder{err: errors.New("private provider trace")})
	failingSvc.episodes = publisher
	if _, err := failingSvc.Send(context.Background(), userID, sessionID, "这次会失败。", domain.KnowledgeOptions{}); err == nil {
		t.Fatal("the failing turn unexpectedly succeeded")
	}
	if publisher.count() != before {
		t.Fatal("a failed turn published a completion event")
	}
}

// TestEpisodeTriggerNeverFailsTheTurn pins the non-blocking rule: a publisher
// that errors must not turn a completed turn into a failure, because the
// reconciler's gap scan is what repairs a lost publish.
func TestEpisodeTriggerNeverFailsTheTurn(t *testing.T) {
	publisher := &recordingEpisodePublisher{err: context.DeadlineExceeded}
	svc, _, userID, sessionID := newMessageFixture(t, &fixedResponder{answer: "已收到。"})
	svc.episodes = publisher
	pair, err := svc.Send(context.Background(), userID, sessionID, "我决定用 JSON 编解码器。", domain.KnowledgeOptions{})
	if err != nil {
		t.Fatalf("a failing publisher failed the turn: %v", err)
	}
	if pair.Assistant.ID == "" {
		t.Fatal("the turn lost its answer")
	}
}
