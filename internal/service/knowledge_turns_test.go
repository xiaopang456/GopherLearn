package service

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/knowledge"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/responder"
)

// TestGroundedTurnEnforcesCollectionOwnership checks the authorization gate
// against the real knowledge store: a collection belonging to another user reads
// as missing, and nothing reaches the responder.
func TestGroundedTurnEnforcesCollectionOwnership(t *testing.T) {
	ctx := context.Background()
	store := repository.NewInMemoryRepository()
	owner, err := store.CreateUser(ctx, &domain.User{Email: "kn-owner@example.com", DisplayName: "Owner"})
	if err != nil {
		t.Fatal(err)
	}
	intruder, err := store.CreateUser(ctx, &domain.User{Email: "kn-intruder@example.com", DisplayName: "Intruder"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.CreateSession(ctx, &domain.Session{UserID: owner.ID, Title: "Chat", Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}
	knowledgeStore := knowledge.NewMemoryStore()
	mine, err := knowledgeStore.CreateCollection(ctx, &domain.Collection{UserID: owner.ID, Title: "Mine"})
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := knowledgeStore.CreateCollection(ctx, &domain.Collection{UserID: intruder.ID, Title: "Theirs"})
	if err != nil {
		t.Fatal(err)
	}

	requests := make(chan responder.Request, 4)
	assistant := &fixedResponder{answer: "answer", chunks: []string{"answer"}, respondRequest: requests, streamRequest: requests}
	service := NewMessageService(store, assistant, WithCollectionAccess(NewKnowledgeService(knowledgeStore)))

	if _, err := service.Send(ctx, owner.ID, session.ID, "question", domain.KnowledgeOptions{Enabled: true, CollectionIDs: []string{mine.ID}}); err != nil {
		t.Fatalf("owner grounded turn failed: %v", err)
	}
	// Drain the authorized turn so the count below only reflects refused turns.
	<-requests

	cases := []struct {
		name      string
		knowledge domain.KnowledgeOptions
	}{
		{"foreign collection", domain.KnowledgeOptions{Enabled: true, CollectionIDs: []string{theirs.ID}}},
		{"unknown collection", domain.KnowledgeOptions{Enabled: true, CollectionIDs: []string{"collection-does-not-exist"}}},
		// One refused id poisons the whole scope, so a mixed list cannot partially
		// search the collections the caller does own.
		{"mixed scope", domain.KnowledgeOptions{Enabled: true, CollectionIDs: []string{mine.ID, theirs.ID}}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := service.Send(ctx, owner.ID, session.ID, "question", testCase.knowledge); !errors.Is(err, ErrNotFound) {
				t.Errorf("sync error = %v, want ErrNotFound", err)
			}
			if err := service.Stream(ctx, owner.ID, session.ID, "question", testCase.knowledge, func(StreamEvent) error { return nil }); !errors.Is(err, ErrNotFound) {
				t.Errorf("stream error = %v, want ErrNotFound", err)
			}
		})
	}
	if pending := len(requests); pending != 0 {
		t.Fatalf("refused turns reached the responder %d times", pending)
	}
	messages, err := store.ListMessages(ctx, owner.ID, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 {
		t.Fatalf("refused grounded turns persisted %d messages, want only the authorized pair", len(messages))
	}
}

// TestGroundedTurnFailsClosedWithoutAnAuthorizer pins the fail-closed rule: a
// deployment that forgot to wire knowledge must not answer from collections.
func TestGroundedTurnFailsClosedWithoutAnAuthorizer(t *testing.T) {
	ctx := context.Background()
	store := repository.NewInMemoryRepository()
	user, err := store.CreateUser(ctx, &domain.User{Email: "no-access@example.com", DisplayName: "Owner"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "Chat", Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan responder.Request, 2)
	assistant := &fixedResponder{answer: "answer", respondRequest: requests}
	service := NewMessageService(store, assistant)

	grounded := domain.KnowledgeOptions{Enabled: true, CollectionIDs: []string{"collection-1"}}
	if _, err := service.Send(ctx, user.ID, session.ID, "question", grounded); !errors.Is(err, ErrInternal) {
		t.Fatalf("error = %v, want ErrInternal for an unwired authorizer", err)
	}
	if len(requests) != 0 {
		t.Fatal("an unauthorized grounded turn reached the responder")
	}
	// The same service still answers ungrounded turns, so the missing gate is
	// what fails and not the request shape.
	if _, err := service.Send(ctx, user.ID, session.ID, "question", domain.KnowledgeOptions{}); err != nil {
		t.Fatalf("direct turn with no authorizer = %v, want success", err)
	}
}

// TestUngroundedTurnNeverConsultsTheKnowledgeGate is the zero-retrieval rule for
// direct_chat, asserted on the collaborator that retrieval would go through.
func TestUngroundedTurnNeverConsultsTheKnowledgeGate(t *testing.T) {
	ctx := context.Background()
	store := repository.NewInMemoryRepository()
	user, err := store.CreateUser(ctx, &domain.User{Email: "zero@example.com", DisplayName: "Owner"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "Chat", Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}
	counter := &countingAccess{}
	requests := make(chan responder.Request, 4)
	assistant := &fixedResponder{
		answer:         "answer",
		chunks:         []string{"answer"},
		respondRequest: requests,
		streamRequest:  requests,
	}
	service := NewMessageService(store, assistant, WithCollectionAccess(counter))

	if _, err := service.Send(ctx, user.ID, session.ID, "question", domain.KnowledgeOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := service.Stream(ctx, user.ID, session.ID, "question", domain.KnowledgeOptions{}, func(StreamEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if counter.calls != 0 {
		t.Fatalf("knowledge gate consulted %d times for direct_chat, want 0", counter.calls)
	}
	for range 2 {
		received := <-requests
		if received.AnswerStrategy != domain.AnswerStrategyDirectChat || received.KnowledgeEnabled || len(received.KnowledgeCollectionIDs) != 0 {
			t.Fatalf("direct turn leaked grounding: %#v", received)
		}
	}
}

type countingAccess struct{ calls int }

func (c *countingAccess) EnsureReadable(_ context.Context, _ string, _ []string) error {
	c.calls++
	return nil
}

// TestSourcesSurviveBothResponsePaths asserts the citations that come out of a
// turn are also the citations the stored history carries, on both routes, in the
// same order. That is what lets a refreshed session show the same sources instead
// of an answer that has lost its evidence.
func TestSourcesSurviveBothResponsePaths(t *testing.T) {
	ctx := context.Background()
	cited := []domain.Source{
		{SourceID: "src-1", DocumentID: "doc-1", CollectionID: "col-1", ChunkID: "chunk-1", Title: "Redis", Section: "AOF", Snippet: "first"},
		{SourceID: "src-2", DocumentID: "doc-1", CollectionID: "col-1", ChunkID: "chunk-2", Title: "Redis", Section: "RDB", Snippet: "second"},
	}
	assistant := &fixedResponder{
		answer:  "grounded answer",
		chunks:  []string{"grounded ", "answer"},
		sources: cited,
	}
	service, store, userID, sessionID := newMessageFixture(t, assistant)
	grounded := domain.KnowledgeOptions{Enabled: true, CollectionIDs: []string{"collection-1"}}

	pair, err := service.Send(ctx, userID, sessionID, "question", grounded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pair.Assistant.Sources, cited) {
		t.Fatalf("sync sources = %#v, want them in answer order %#v", pair.Assistant.Sources, cited)
	}
	if len(pair.User.Sources) != 0 {
		t.Fatalf("the user message grew citations: %#v", pair.User.Sources)
	}
	stored, err := store.ListMessages(ctx, userID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 {
		t.Fatalf("messages after one sync turn = %d, want the pair", len(stored))
	}
	if !reflect.DeepEqual(stored[1].Sources, cited) {
		t.Fatalf("stored assistant citations = %#v, want the same sources and order the turn returned", stored[1].Sources)
	}

	var completed []StreamEvent
	if err := service.Stream(ctx, userID, sessionID, "question", grounded, func(event StreamEvent) error {
		if event.Name == "message.completed" {
			completed = append(completed, event)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(completed) != 1 {
		t.Fatalf("completed events = %d, want 1", len(completed))
	}
	if !reflect.DeepEqual(completed[0].Sources, cited) {
		t.Fatalf("stream sources = %#v, want the same citations as the sync path", completed[0].Sources)
	}
	streamed, err := store.ListMessages(ctx, userID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(streamed) != 4 {
		t.Fatalf("messages after both routes = %d, want two turns", len(streamed))
	}
	if !reflect.DeepEqual(streamed[3].Sources, cited) {
		t.Fatalf("stored citations after streaming = %#v, want the completed event's set", streamed[3].Sources)
	}
	for _, message := range []domain.Message{streamed[0], streamed[2]} {
		if len(message.Sources) != 0 {
			t.Fatalf("a user message carries citations: %#v", message)
		}
	}
}
