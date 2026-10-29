package service

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/responder"
)

type frozenOrdinaryFixedResponder struct {
	answer         string
	err            error
	chunks         []string
	entered        chan struct{}
	release        chan struct{}
	respondRequest chan responder.Request
	streamRequest  chan responder.Request
	sources        []domain.Source
}

// frozenOrdinaryOpenAccess stands in for the knowledge authorization gate in tests that are
// about turn resolution rather than ownership. The real gate is covered by
// TestGroundedTurnEnforcesCollectionOwnership.
type frozenOrdinaryOpenAccess struct{}

func (frozenOrdinaryOpenAccess) EnsureReadable(context.Context, string, []string) error { return nil }

// frozenOrdinaryForbiddenAccess refuses one sentinel id, mirroring how the real gate reports a
// collection owned by somebody else.
type frozenOrdinaryForbiddenAccess struct{}

func (frozenOrdinaryForbiddenAccess) EnsureReadable(_ context.Context, _ string, collectionIDs []string) error {
	for _, collectionID := range collectionIDs {
		if strings.HasPrefix(collectionID, "foreign-") {
			return ErrNotFound
		}
	}
	return nil
}

func (f *frozenOrdinaryFixedResponder) Respond(ctx context.Context, request responder.Request) (responder.Answer, error) {
	if f.respondRequest != nil {
		f.respondRequest <- request
	}
	if f.entered != nil {
		select {
		case f.entered <- struct{}{}:
		default:
		}
	}
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return responder.Answer{}, ctx.Err()
		}
	}
	return responder.Answer{Content: f.answer, Sources: f.sources}, f.err
}

func (f *frozenOrdinaryFixedResponder) Stream(ctx context.Context, request responder.Request, emit func(string) error) (responder.Answer, error) {
	if f.streamRequest != nil {
		f.streamRequest <- request
	}
	if f.entered != nil {
		select {
		case f.entered <- struct{}{}:
		default:
		}
	}
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return responder.Answer{}, ctx.Err()
		}
	}
	if f.err != nil {
		return responder.Answer{}, f.err
	}
	for _, chunk := range f.chunks {
		if err := emit(chunk); err != nil {
			return responder.Answer{}, err
		}
	}
	return responder.Answer{Content: f.answer, Sources: f.sources}, nil
}

func frozenOrdinaryNewMessageFixture(t *testing.T, assistant responder.Responder, modes ...domain.SessionMode) (*MessageService, *repository.InMemoryRepository, string, string) {
	t.Helper()
	store := repository.NewInMemoryRepository()
	user, err := store.CreateUser(context.Background(), &domain.User{Email: "owner@example.com", DisplayName: "Owner"})
	if err != nil {
		t.Fatal(err)
	}
	mode := domain.SessionModeChat
	if len(modes) > 0 {
		mode = modes[0]
	}
	session, err := store.CreateSession(context.Background(), &domain.Session{UserID: user.ID, Title: "Chat", Mode: mode})
	if err != nil {
		t.Fatal(err)
	}
	return newFrozenOrdinaryMessageService(store, assistant, WithCollectionAccess(frozenOrdinaryForbiddenAccess{})), store, user.ID, session.ID
}

// frozenOrdinaryTurnContextCase describes one stored session plus the per-turn option and the
// responder request the server must derive from them.
type frozenOrdinaryTurnContextCase struct {
	name         string
	mode         domain.SessionMode
	knowledge    domain.KnowledgeOptions
	wantMode     domain.SessionMode
	wantStrategy domain.AnswerStrategy
	wantIDs      []string
}

func (c frozenOrdinaryTurnContextCase) assert(t *testing.T, path string, got responder.Request) {
	t.Helper()
	if got.SessionMode != c.wantMode {
		t.Fatalf("%s responder mode=%q, want the stored mode %q", path, got.SessionMode, c.wantMode)
	}
	if got.AnswerStrategy != c.wantStrategy {
		t.Fatalf("%s responder strategy=%q, want %q", path, got.AnswerStrategy, c.wantStrategy)
	}
	if !got.AnswerStrategy.IsValid() {
		t.Fatalf("%s responder strategy %q is outside the closed set", path, got.AnswerStrategy)
	}
	wantEnabled := c.wantStrategy == domain.AnswerStrategyKnowledgeQA
	if got.KnowledgeEnabled != wantEnabled {
		t.Fatalf("%s knowledge enabled=%v, want %v", path, got.KnowledgeEnabled, wantEnabled)
	}
	if !reflect.DeepEqual(got.KnowledgeCollectionIDs, c.wantIDs) {
		t.Fatalf("%s collection ids=%#v, want %#v", path, got.KnowledgeCollectionIDs, c.wantIDs)
	}
}

func TestFrozenOrdinarySendPersistsMessagesInRoleOrder(t *testing.T) {
	service, store, userID, sessionID := frozenOrdinaryNewMessageFixture(t, &frozenOrdinaryFixedResponder{answer: "answer"})
	pair, err := service.Send(context.Background(), userID, sessionID, "question", domain.KnowledgeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pair.User.Role != "user" || pair.Assistant.Role != "assistant" {
		t.Fatalf("unexpected roles: %#v", pair)
	}
	messages, err := store.ListMessages(context.Background(), userID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Role != "user" || messages[1].Role != "assistant" {
		t.Fatalf("messages were not persisted in order: %#v", messages)
	}
}

func TestFrozenOrdinaryResponderReceivesServerResolvedTurnContext(t *testing.T) {
	cases := []frozenOrdinaryTurnContextCase{
		{
			name:         "chat without knowledge",
			mode:         domain.SessionModeChat,
			wantMode:     domain.SessionModeChat,
			wantStrategy: domain.AnswerStrategyDirectChat,
		},
		{
			name:      "chat with knowledge normalizes the collection list",
			mode:      domain.SessionModeChat,
			knowledge: domain.KnowledgeOptions{Enabled: true, CollectionIDs: []string{" collection-2 ", "collection-1", "collection-2"}},
			wantMode:  domain.SessionModeChat,
			// The option is a Chat intent, not a stored mode: knowledge_qa can
			// only appear as a strategy.
			wantStrategy: domain.AnswerStrategyKnowledgeQA,
			wantIDs:      []string{"collection-2", "collection-1"},
		},
		{
			name:         "learning agent",
			mode:         domain.SessionModeLearningAgent,
			wantMode:     domain.SessionModeLearningAgent,
			wantStrategy: domain.AnswerStrategyAgent,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			assistant := &frozenOrdinaryFixedResponder{
				answer:         "answer",
				chunks:         []string{"answer"},
				respondRequest: make(chan responder.Request, 1),
				streamRequest:  make(chan responder.Request, 1),
			}
			service, _, userID, sessionID := frozenOrdinaryNewMessageFixture(t, assistant, testCase.mode)

			pair, err := service.Send(context.Background(), userID, sessionID, "question", testCase.knowledge)
			if err != nil {
				t.Fatal(err)
			}
			sync := <-assistant.respondRequest
			testCase.assert(t, "sync", sync)

			if err := service.Stream(context.Background(), userID, sessionID, "question", testCase.knowledge, func(StreamEvent) error { return nil }); err != nil {
				t.Fatal(err)
			}
			streamed := <-assistant.streamRequest
			testCase.assert(t, "stream", streamed)

			// Same rule as everywhere else: the resolved turn must agree across
			// routes, and only the history window may move, because the first call
			// stored a pair the second one can now see. The stored id of the question
			// being answered is normalized out for the same reason history is: these
			// are two different turns, so agreeing on it would be the bug.
			syncContext, streamContext := sync, streamed
			syncContext.History, streamContext.History = nil, nil
			syncContext.UserMessageID, streamContext.UserMessageID = "", ""
			if !reflect.DeepEqual(syncContext, streamContext) {
				t.Fatalf("sync and stream resolved different turn contexts: %#v vs %#v", sync, streamed)
			}
			// Each route still hands its own turn down, and that is the value a
			// propose_memory call may cite: nothing else, including the answer that
			// has not been written yet.
			if sync.UserMessageID != pair.User.ID {
				t.Fatalf("sync request cites %q, want the question this turn stored (%q)", sync.UserMessageID, pair.User.ID)
			}
			if streamed.UserMessageID == "" || streamed.UserMessageID == sync.UserMessageID {
				t.Fatalf("streamed request cites %q, want its own stored question (this turn's was %q)",
					streamed.UserMessageID, sync.UserMessageID)
			}
			if len(sync.History) != 0 || len(streamed.History) != 2 {
				t.Fatalf("history window = %d then %d, want 0 then the stored pair: %#v / %#v",
					len(sync.History), len(streamed.History), sync.History, streamed.History)
			}
			if streamed.History[0].Role != "user" || streamed.History[1].Role != "assistant" {
				t.Fatalf("history roles = %#v, want stored order", streamed.History)
			}
		})
	}
}

func TestFrozenOrdinaryInvalidKnowledgeOptionsAreRejectedBeforeAnyWrite(t *testing.T) {
	cases := []struct {
		name      string
		mode      domain.SessionMode
		knowledge domain.KnowledgeOptions
	}{
		{"learning agent cannot use the chat toggle", domain.SessionModeLearningAgent, domain.KnowledgeOptions{Enabled: true, CollectionIDs: []string{"collection-1"}}},
		{"chat knowledge requires a collection", domain.SessionModeChat, domain.KnowledgeOptions{Enabled: true}},
		{"disabled knowledge cannot carry collections", domain.SessionModeChat, domain.KnowledgeOptions{CollectionIDs: []string{"collection-1"}}},
		{"blank collection ids are rejected", domain.SessionModeChat, domain.KnowledgeOptions{Enabled: true, CollectionIDs: []string{"  "}}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			assistant := &frozenOrdinaryFixedResponder{answer: "answer", chunks: []string{"answer"}}
			service, store, userID, sessionID := frozenOrdinaryNewMessageFixture(t, assistant, testCase.mode)

			if _, err := service.Send(context.Background(), userID, sessionID, "question", testCase.knowledge); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("sync error=%v, want ErrInvalidRequest", err)
			}
			var events []string
			err := service.Stream(context.Background(), userID, sessionID, "question", testCase.knowledge, func(event StreamEvent) error {
				events = append(events, event.Name)
				return nil
			})
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("stream error=%v, want ErrInvalidRequest", err)
			}
			if len(events) != 0 {
				t.Fatalf("stream emitted events before rejecting: %#v", events)
			}
			messages, listErr := store.ListMessages(context.Background(), userID, sessionID)
			if listErr != nil {
				t.Fatal(listErr)
			}
			if len(messages) != 0 {
				t.Fatalf("rejected turn persisted %d messages", len(messages))
			}
		})
	}
}

func TestFrozenOrdinaryValidateTurnAgreesWithSendAndStreamPreChecks(t *testing.T) {
	assistant := &frozenOrdinaryFixedResponder{answer: "answer", chunks: []string{"answer"}}
	service, _, userID, sessionID := frozenOrdinaryNewMessageFixture(t, assistant, domain.SessionModeChat)

	cases := []struct {
		name      string
		userID    string
		sessionID string
		content   string
		knowledge domain.KnowledgeOptions
		want      error
	}{
		{"valid direct turn", userID, sessionID, "question", domain.KnowledgeOptions{}, nil},
		{"valid grounded turn", userID, sessionID, "question", domain.KnowledgeOptions{Enabled: true, CollectionIDs: []string{"collection-1"}}, nil},
		{"grounding without a collection", userID, sessionID, "question", domain.KnowledgeOptions{Enabled: true}, ErrInvalidRequest},
		{"blank content", userID, sessionID, "   ", domain.KnowledgeOptions{}, ErrInvalidRequest},
		{"cross-owner", "different-user", sessionID, "question", domain.KnowledgeOptions{}, ErrNotFound},
		{"unknown session", userID, "missing-session", "question", domain.KnowledgeOptions{}, ErrNotFound},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := service.ValidateTurn(context.Background(), testCase.userID, testCase.sessionID, testCase.content, testCase.knowledge)
			if !errors.Is(err, testCase.want) {
				t.Fatalf("error=%v, want %v", err, testCase.want)
			}
		})
	}
}

func TestFrozenOrdinaryResponderFailureDoesNotCreateAssistantMessage(t *testing.T) {
	service, store, userID, sessionID := frozenOrdinaryNewMessageFixture(t, &frozenOrdinaryFixedResponder{err: errors.New("private provider trace")})
	if _, err := service.Send(context.Background(), userID, sessionID, "question", domain.KnowledgeOptions{}); err != ErrInternal {
		t.Fatalf("error = %v, want safe internal error", err)
	}
	messages, err := store.ListMessages(context.Background(), userID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Role != "user" {
		t.Fatalf("unexpected messages after failure: %#v", messages)
	}
}

func TestFrozenOrdinaryMessageTurnsAreSerializedPerSession(t *testing.T) {
	responder := &frozenOrdinaryFixedResponder{answer: "answer", entered: make(chan struct{}, 2), release: make(chan struct{})}
	service, store, userID, sessionID := frozenOrdinaryNewMessageFixture(t, responder)
	firstDone := make(chan error, 1)
	go func() {
		_, err := service.Send(context.Background(), userID, sessionID, "first", domain.KnowledgeOptions{})
		firstDone <- err
	}()
	<-responder.entered
	secondDone := make(chan error, 1)
	go func() {
		_, err := service.Send(context.Background(), userID, sessionID, "second", domain.KnowledgeOptions{})
		secondDone <- err
	}()
	time.Sleep(20 * time.Millisecond)
	messages, err := store.ListMessages(context.Background(), userID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Content != "first" {
		t.Fatalf("second turn entered before first completed: %#v", messages)
	}
	responder.release <- struct{}{}
	<-responder.entered
	responder.release <- struct{}{}
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	messages, err = store.ListMessages(context.Background(), userID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	roles := make([]string, len(messages))
	for i, message := range messages {
		roles[i] = message.Role + ":" + message.Content
	}
	want := []string{"user:first", "assistant:answer", "user:second", "assistant:answer"}
	if !reflect.DeepEqual(roles, want) {
		t.Fatalf("turn ordering = %#v, want %#v", roles, want)
	}
}

func TestFrozenOrdinaryStreamEventsAndCancellation(t *testing.T) {
	assistant := &frozenOrdinaryFixedResponder{answer: "hello world", chunks: []string{"hello", " ", "world"}}
	service, store, userID, sessionID := frozenOrdinaryNewMessageFixture(t, assistant)
	var got []string
	err := service.Stream(context.Background(), userID, sessionID, "question", domain.KnowledgeOptions{}, func(event StreamEvent) error {
		got = append(got, event.Name)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"message.started", "message.delta", "message.delta", "message.delta", "message.completed"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %#v, want %#v", got, want)
	}
	messages, err := store.ListMessages(context.Background(), userID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[1].Content != "hello world" {
		t.Fatalf("stream did not persist completed answer: %#v", messages)
	}

	cancelResponder := &frozenOrdinaryFixedResponder{entered: make(chan struct{}, 1), release: make(chan struct{})}
	cancelService, cancelStore, cancelUser, cancelSession := frozenOrdinaryNewMessageFixture(t, cancelResponder)
	ctx, cancel := context.WithCancel(context.Background())
	streamDone := make(chan error, 1)
	go func() {
		streamDone <- cancelService.Stream(ctx, cancelUser, cancelSession, "question", domain.KnowledgeOptions{}, func(StreamEvent) error { return nil })
	}()
	<-cancelResponder.entered
	cancel()
	if err := <-streamDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
	cancelMessages, err := cancelStore.ListMessages(context.Background(), cancelUser, cancelSession)
	if err != nil {
		t.Fatal(err)
	}
	if len(cancelMessages) != 1 || cancelMessages[0].Role != "user" {
		t.Fatalf("canceled stream fabricated a completed answer: %#v", cancelMessages)
	}
}

func TestFrozenOrdinaryStreamRejectsNilEmitter(t *testing.T) {
	service, _, userID, sessionID := frozenOrdinaryNewMessageFixture(t, &frozenOrdinaryFixedResponder{answer: "answer"})
	if err := service.Stream(context.Background(), userID, sessionID, "question", domain.KnowledgeOptions{}, nil); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil emitter error=%v, want ErrInvalidRequest", err)
	}
}

func TestFrozenOrdinaryCrossOwnerMessageAccessIsNotFound(t *testing.T) {
	service, _, _, sessionID := frozenOrdinaryNewMessageFixture(t, &frozenOrdinaryFixedResponder{answer: "answer"})
	if _, err := service.Send(context.Background(), "different-user", sessionID, "question", domain.KnowledgeOptions{}); err != ErrNotFound {
		t.Fatalf("cross-owner send error = %v", err)
	}
}
