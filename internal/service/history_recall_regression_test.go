package service

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/responder"
)

// historyProbeProvider records the pre-turn fact supplied by MessageService. It
// deliberately returns an empty supply: these tests verify routing, not recall.
type historyProbeProvider struct {
	mu      sync.Mutex
	history []bool
	err     error
}

func (p *historyProbeProvider) ContextRecall(ctx context.Context, userID, sessionID, turnID, question string) (EpisodeContextSupply, error) {
	return p.ContextRecallWithHistory(ctx, userID, sessionID, turnID, question, false)
}

func (p *historyProbeProvider) ContextRecallWithHistory(_ context.Context, _, _, _, _ string, hasHistory bool) (EpisodeContextSupply, error) {
	p.mu.Lock()
	p.history = append(p.history, hasHistory)
	p.mu.Unlock()
	if p.err != nil {
		return EpisodeContextSupply{}, p.err
	}
	return EpisodeContextSupply{}, nil
}

func (p *historyProbeProvider) values() []bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]bool(nil), p.history...)
}

// countingListRepository proves Chat and Knowledge do not acquire a second
// ListMessages read merely because the Episode provider is installed.
type countingListRepository struct {
	repository.Repository
	mu    sync.Mutex
	calls int
}

func (r *countingListRepository) ListMessages(ctx context.Context, userID, sessionID string) ([]domain.Message, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	return r.Repository.ListMessages(ctx, userID, sessionID)
}

func (r *countingListRepository) listCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

type failingListRepository struct {
	repository.Repository
	err error
}

func (r failingListRepository) ListMessages(context.Context, string, string) ([]domain.Message, error) {
	return nil, r.err
}

func newAgentHistoryFixture(t *testing.T, assistant responder.Responder, repo repository.Repository) (string, string) {
	t.Helper()
	ctx := context.Background()
	user, err := repo.CreateUser(ctx, &domain.User{Email: "history-regression@example.com", DisplayName: "History"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := repo.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "Agent", Mode: domain.SessionModeLearningAgent})
	if err != nil {
		t.Fatal(err)
	}
	_ = NewMessageService(repo, assistant, WithCollectionAccess(openAccess{}))
	return user.ID, session.ID
}

func TestContextRecallLegacyInterfaceUsesListMessagesResult(t *testing.T) {
	system, store, _ := newEpisodeTestSystem(t)
	user, err := store.CreateUser(t.Context(), &domain.User{Email: "legacy-recall@example.com", DisplayName: "Legacy"})
	if err != nil {
		t.Fatal(err)
	}
	empty, err := store.CreateSession(t.Context(), &domain.Session{UserID: user.ID, Title: "Empty", Mode: domain.SessionModeLearningAgent})
	if err != nil {
		t.Fatal(err)
	}
	withHistory, err := store.CreateSession(t.Context(), &domain.Session{UserID: user.ID, Title: "History", Mode: domain.SessionModeLearningAgent})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendMessage(t.Context(), user.ID, withHistory.ID, &domain.Message{Role: "user", Content: "已有历史"}); err != nil {
		t.Fatal(err)
	}

	got, err := system.ContextRecall(t.Context(), user.ID, empty.ID, "turn-empty", "继续上次")
	if err != nil {
		t.Fatalf("empty legacy recall: %v", err)
	}
	if got.Trace.Reason != "continue_empty_session" {
		t.Fatalf("empty reason = %q, want continue_empty_session", got.Trace.Reason)
	}
	got, err = system.ContextRecall(t.Context(), user.ID, withHistory.ID, "turn-history", "继续上次")
	if err != nil {
		t.Fatalf("history legacy recall: %v", err)
	}
	if got.Trace.Reason != "current_session_has_history" {
		t.Fatalf("history reason = %q, want current_session_has_history", got.Trace.Reason)
	}
	if _, err := system.ContextRecall(t.Context(), user.ID, empty.ID, "turn-failed", "继续上次"); err != nil {
		// A valid empty session is not a probe failure; this call is retained as the
		// positive control that the old interface still performs its normal branch.
		t.Fatalf("repeat empty legacy recall: %v", err)
	}
	if _, err := system.ContextRecall(context.WithValue(t.Context(), struct{}{}, struct{}{}), user.ID, "missing-session", "turn-failed", "继续上次"); err == nil {
		t.Fatal("foreign/missing session probe unexpectedly succeeded")
	}
}

func TestSendAndStreamUsePreTurnHistoryForEpisodeRouting(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream bool
	}{
		{name: "send"},
		{name: "stream", stream: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := repository.NewInMemoryRepository()
			assistant := &fixedResponder{answer: "ok"}
			userID, sessionID := newAgentHistoryFixture(t, assistant, store)
			probe := &historyProbeProvider{}
			svc := NewMessageService(store, assistant, WithCollectionAccess(openAccess{}), WithLearningContextSupply(nil, probe))
			if tc.stream {
				if err := svc.Stream(context.Background(), userID, sessionID, "继续上次", domain.KnowledgeOptions{}, func(StreamEvent) error { return nil }); err != nil {
					t.Fatal(err)
				}
			} else if _, err := svc.Send(context.Background(), userID, sessionID, "继续上次", domain.KnowledgeOptions{}); err != nil {
				t.Fatal(err)
			}
			if got := probe.values(); len(got) != 1 || got[0] {
				t.Fatalf("new-session history = %#v, want [false]", got)
			}

			probe = &historyProbeProvider{}
			svc = NewMessageService(store, assistant, WithCollectionAccess(openAccess{}), WithLearningContextSupply(nil, probe))
			if tc.stream {
				if err := svc.Stream(context.Background(), userID, sessionID, "继续上次", domain.KnowledgeOptions{}, func(StreamEvent) error { return nil }); err != nil {
					t.Fatal(err)
				}
			} else if _, err := svc.Send(context.Background(), userID, sessionID, "继续上次", domain.KnowledgeOptions{}); err != nil {
				t.Fatal(err)
			}
			if got := probe.values(); len(got) != 1 || !got[0] {
				t.Fatalf("existing-session history = %#v, want [true]", got)
			}
		})
	}
}

func TestChatAndKnowledgeDoNotAddHistoryProbe(t *testing.T) {
	for _, knowledge := range []domain.KnowledgeOptions{{}, {Enabled: true, CollectionIDs: []string{"collection-1"}}} {
		store := repository.NewInMemoryRepository()
		base := store
		counting := &countingListRepository{Repository: base}
		assistant := &fixedResponder{answer: "ok"}
		ctx := context.Background()
		user, err := counting.CreateUser(ctx, &domain.User{Email: "chat-history@example.com", DisplayName: "Chat"})
		if err != nil {
			t.Fatal(err)
		}
		session, err := counting.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "Chat", Mode: domain.SessionModeChat})
		if err != nil {
			t.Fatal(err)
		}
		userID, sessionID := user.ID, session.ID
		svc := NewMessageService(counting, assistant, WithCollectionAccess(openAccess{}), WithLearningContextSupply(nil, &historyProbeProvider{}))
		if knowledge.Enabled {
			knowledge.CollectionIDs = []string{"collection-1"}
		}
		if _, err := svc.Send(context.Background(), userID, sessionID, "普通问题", knowledge); err != nil {
			t.Fatal(err)
		}
		if got := counting.listCalls(); got != 1 {
			t.Fatalf("ListMessages calls = %d, want one openTurn read", got)
		}
	}
}

func TestOpenTurnHistoryFailureStopsAnswerAndDoesNotWriteQuestion(t *testing.T) {
	sentinel := errors.New("history probe failed")
	base := repository.NewInMemoryRepository()
	store := &failingListRepository{Repository: base, err: sentinel}
	assistant := &fixedResponder{answer: "must not run"}
	userID, sessionID := newAgentHistoryFixture(t, assistant, base)
	// The session exists in base; only the history read is made to fail.
	svc := NewMessageService(store, assistant, WithCollectionAccess(openAccess{}), WithLearningContextSupply(nil, &historyProbeProvider{}))
	if _, err := svc.Send(context.Background(), userID, sessionID, "不会写入", domain.KnowledgeOptions{}); err == nil {
		t.Fatal("Send unexpectedly succeeded")
	}
	if len(baseMessages(t, base, userID, sessionID)) != 0 {
		t.Fatal("openTurn history failure wrote the current question")
	}
}

func baseMessages(t *testing.T, repo repository.Repository, userID, sessionID string) []domain.Message {
	t.Helper()
	messages, err := repo.ListMessages(context.Background(), userID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	return messages
}
