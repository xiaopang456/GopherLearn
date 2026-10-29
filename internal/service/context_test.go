package service

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/responder"
)

func stored(role, content string) domain.Message {
	return domain.Message{Role: role, Content: content}
}

func turnsOf(messages ...domain.Message) []responder.Turn {
	turns := make([]responder.Turn, 0, len(messages))
	for _, message := range messages {
		turns = append(turns, responder.Turn{Role: message.Role, Content: message.Content})
	}
	return turns
}

func TestRecentTurnsAppliesBothLimitsAndKeepsConversationOrder(t *testing.T) {
	// Every message below is eight runes, so a completed pair costs sixteen and the
	// budgets are arithmetic a reader can check by hand.
	first := stored("user", "第一问aaaaa")
	firstAnswer := stored("assistant", "第一答bbbbb")
	second := stored("user", "第二问ccccc")
	secondAnswer := stored("assistant", "第二答ddddd")
	messages := []domain.Message{first, firstAnswer, second, secondAnswer}

	cases := []struct {
		name     string
		messages []domain.Message
		budget   ContextBudget
		want     []domain.Message
	}{
		{"unset budget uses the defaults", messages, ContextBudget{}, messages},
		{"message cap keeps the newest pair", messages, ContextBudget{MaxMessages: 2, MaxRunes: 1000}, []domain.Message{second, secondAnswer}},
		{"odd message cap takes whole pairs only", messages, ContextBudget{MaxMessages: 3, MaxRunes: 1000}, []domain.Message{second, secondAnswer}},
		{"rune cap for one pair", messages, ContextBudget{MaxMessages: 100, MaxRunes: 16}, []domain.Message{second, secondAnswer}},
		{"rune cap below one pair sends nothing", messages, ContextBudget{MaxMessages: 100, MaxRunes: 15}, nil},
		{"budget smaller than one message", messages, ContextBudget{MaxMessages: 5, MaxRunes: 7}, nil},
		{"empty history", nil, ContextBudget{MaxMessages: 3, MaxRunes: 100}, nil},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := recentTurns(testCase.messages, testCase.budget)
			if !reflect.DeepEqual(got, turnsOf(testCase.want...)) {
				t.Fatalf("window = %#v, want %#v", got, turnsOf(testCase.want...))
			}
			budget := testCase.budget.Normalize()
			if len(got) > budget.MaxMessages {
				t.Fatalf("window holds %d turns against a limit of %d", len(got), budget.MaxMessages)
			}
			used := 0
			for _, turn := range got {
				used += len([]rune(turn.Content))
			}
			if used > budget.MaxRunes {
				t.Fatalf("window used %d runes against a budget of %d", used, budget.MaxRunes)
			}
		})
	}
}

// TestRecentTurnsExcludesUnfinishedTurns is the Phase 4A.1 rule: a turn whose
// assistant half never arrived stays out of the model's context, while remaining
// visible to the history endpoint. The transcript is append-only; this filter
// decides what counts as a completed exchange.
func TestRecentTurnsExcludesUnfinishedTurns(t *testing.T) {
	first := stored("user", "第一问")
	firstAnswer := stored("assistant", "第一答")
	abandoned := stored("user", "模型失败的那次提问")
	second := stored("user", "第二问")
	secondAnswer := stored("assistant", "第二答")
	orphanAnswer := stored("assistant", "没有提问的孤立回答")
	const roomy = 1000

	cases := []struct {
		name     string
		messages []domain.Message
		want     []domain.Message
	}{
		{"no history yet", nil, nil},
		{"a lone unanswered question", []domain.Message{abandoned}, nil},
		{"trailing unanswered question after a good turn", []domain.Message{first, firstAnswer, abandoned}, []domain.Message{first, firstAnswer}},
		{"several abandoned attempts", []domain.Message{first, firstAnswer, abandoned, abandoned}, []domain.Message{first, firstAnswer}},
		{"abandoned attempt in the middle", []domain.Message{first, abandoned, second, secondAnswer}, []domain.Message{second, secondAnswer}},
		{"answer without a question", []domain.Message{orphanAnswer, first, firstAnswer}, []domain.Message{first, firstAnswer}},
		{"two completed exchanges keep their order", []domain.Message{first, firstAnswer, second, secondAnswer}, []domain.Message{first, firstAnswer, second, secondAnswer}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := recentTurns(testCase.messages, ContextBudget{MaxMessages: 12, MaxRunes: roomy})
			if !reflect.DeepEqual(got, turnsOf(testCase.want...)) {
				t.Fatalf("window = %#v, want %#v", got, turnsOf(testCase.want...))
			}
			for _, turn := range got {
				if turn.Content == abandoned.Content {
					t.Fatalf("an unanswered question reached the model context: %#v", got)
				}
			}
		})
	}
}

func TestNormalizeRefusesAnUnboundedWindow(t *testing.T) {
	budget := ContextBudget{MaxMessages: -1, MaxRunes: 0}.Normalize()
	if budget.MaxMessages != DefaultContextMaxMessages || budget.MaxRunes != DefaultContextMaxRunes {
		t.Fatalf("Normalize() = %#v, want the defaults for unusable limits", budget)
	}
}

// capturingResponder records the request each turn resolves and answers with a
// deterministic echo so a test can tell an assistant turn from a user turn in the
// history that follows. failFirst makes that many turns fail the way a provider
// outage does, which is how a test reaches an unfinished turn.
type capturingResponder struct {
	requests  []responder.Request
	calls     int
	failFirst int
	err       error
	// streamHalfway makes a streamed turn emit one delta and then fail, which is
	// the mid-stream case the persistence rules have to survive.
	streamHalfway bool
}

var errModelDown = errors.New("provider is down")

func (c *capturingResponder) Respond(_ context.Context, request responder.Request) (responder.Answer, error) {
	c.requests = append(c.requests, request)
	c.calls++
	if c.calls <= c.failFirst {
		if c.err != nil {
			return responder.Answer{}, c.err
		}
		return responder.Answer{}, errModelDown
	}
	return responder.Answer{Content: "echo:" + request.Content}, nil
}

func (c *capturingResponder) Stream(ctx context.Context, request responder.Request, emit func(string) error) (responder.Answer, error) {
	c.requests = append(c.requests, request)
	c.calls++
	if c.calls <= c.failFirst {
		if c.streamHalfway {
			if err := emit("半句"); err != nil {
				return responder.Answer{}, err
			}
		}
		if c.err != nil {
			return responder.Answer{}, c.err
		}
		return responder.Answer{}, errModelDown
	}
	answer := responder.Answer{Content: "echo:" + request.Content}
	return answer, emit(answer.Content)
}

// flakyRepository fails one numbered AppendMessage call, which is the only way a
// test reaches "the answer arrived but could not be stored".
type flakyRepository struct {
	repository.Repository
	failAppend int
	appends    int
}

func (f *flakyRepository) AppendMessage(ctx context.Context, userID, sessionID string, message *domain.Message) (*domain.Message, error) {
	f.appends++
	if f.appends == f.failAppend {
		return nil, errors.New("assistant message write failed")
	}
	return f.Repository.AppendMessage(ctx, userID, sessionID, message)
}

func budgetedService(t *testing.T, assistant responder.Responder, budget ContextBudget) (*MessageService, string, string) {
	t.Helper()
	store := repository.NewInMemoryRepository()
	user, err := store.CreateUser(context.Background(), &domain.User{Email: "owner@example.com", DisplayName: "Owner"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.CreateSession(context.Background(), &domain.Session{UserID: user.ID, Title: "Chat", Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}
	return NewMessageService(store, assistant, WithConversationBudget(budget)), user.ID, session.ID
}

// TestTurnContextSeesStoredHistoryButNotTheCurrentQuestion is the ordering rule the
// model prompt depends on: the window is read before this turn's user message is
// stored, so a question reaches the model exactly once even though it is both the
// request content and, moments later, a stored message.
func TestTurnContextSeesStoredHistoryButNotTheCurrentQuestion(t *testing.T) {
	ctx := context.Background()
	assistant := &capturingResponder{}
	service, userID, sessionID := budgetedService(t, assistant, ContextBudget{MaxMessages: 6, MaxRunes: 500})

	for _, content := range []string{"第一问", "第二问", "第三问"} {
		if _, err := service.Send(ctx, userID, sessionID, content, domain.KnowledgeOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if len(assistant.requests) != 3 {
		t.Fatalf("responder calls = %d", len(assistant.requests))
	}
	if got := assistant.requests[0].History; len(got) != 0 {
		t.Fatalf("the first turn saw history %#v, want none", got)
	}
	want := turnsOf(
		stored("user", "第一问"), stored("assistant", "echo:第一问"),
		stored("user", "第二问"), stored("assistant", "echo:第二问"),
	)
	if got := assistant.requests[2].History; !reflect.DeepEqual(got, want) {
		t.Fatalf("third turn history = %#v, want the four stored messages in order", got)
	}
	for _, request := range assistant.requests {
		if occurrencesOf(request.History, request.Content) != 0 {
			t.Fatalf("the current question was also passed as history: %#v", request)
		}
	}
}

// TestConversationBudgetBoundsWhatTheModelSees proves the limit is enforced end to
// end rather than only inside the helper: a long history cannot smuggle itself into
// a turn.
func TestConversationBudgetBoundsWhatTheModelSees(t *testing.T) {
	ctx := context.Background()
	assistant := &capturingResponder{}
	service, userID, sessionID := budgetedService(t, assistant, ContextBudget{MaxMessages: 2, MaxRunes: 1000})

	for _, content := range []string{"一", "二", "三", "四"} {
		if _, err := service.Send(ctx, userID, sessionID, content, domain.KnowledgeOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	last := assistant.requests[len(assistant.requests)-1]
	if len(last.History) != 2 {
		t.Fatalf("history = %#v, want the two newest stored messages only", last.History)
	}
	if last.History[0].Content != "三" || last.History[1].Content != "echo:三" {
		t.Fatalf("window = %#v, want the newest pair", last.History)
	}
	// The streamed route gets the same window, so a browser using SSE cannot see a
	// differently-scoped answer.
	if err := service.Stream(ctx, userID, sessionID, "五", domain.KnowledgeOptions{}, func(StreamEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	streamed := assistant.requests[len(assistant.requests)-1]
	if len(streamed.History) != 2 || streamed.History[0].Content != "四" {
		t.Fatalf("stream window = %#v, want the two newest turns", streamed.History)
	}
}

// TestFailedTurnsNeverEnterTheNextPrompt is the Phase 4A.1 rule measured through
// the real turn pipeline, on every way a turn can end without an answer: the model
// fails, the stream breaks after a delta, the answer cannot be stored - and then
// what the next turn is actually shown.
//
// The failed question must stay in the transcript (the user did ask it) and out of
// the model's context (nobody answered it).
func TestFailedTurnsNeverEnterTheNextPrompt(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		// failFirst counts model failures from the first turn; failAppend is the
		// ordinal of the AppendMessage call that must fail.
		failFirst  int
		failStream bool
		failAppend int
	}{
		{"synchronous model failure", 1, false, 0},
		{"stream broke mid answer", 1, true, 0},
		{"assistant message could not be stored", 0, false, 2},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			assistant := &capturingResponder{failFirst: testCase.failFirst, streamHalfway: testCase.failStream}
			store := repository.NewInMemoryRepository()
			user, err := store.CreateUser(ctx, &domain.User{Email: "owner@example.com", DisplayName: "Owner"})
			if err != nil {
				t.Fatal(err)
			}
			session, err := store.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "Chat", Mode: domain.SessionModeChat})
			if err != nil {
				t.Fatal(err)
			}
			var persisted repository.Repository = store
			if testCase.failAppend > 0 {
				persisted = &flakyRepository{Repository: store, failAppend: testCase.failAppend}
			}
			service := NewMessageService(persisted, assistant, WithConversationBudget(ContextBudget{MaxMessages: 12, MaxRunes: 1000}))

			first := "第一次提问的那句话"
			if testCase.failStream {
				err = service.Stream(ctx, user.ID, session.ID, first, domain.KnowledgeOptions{}, func(StreamEvent) error { return nil })
			} else {
				_, err = service.Send(ctx, user.ID, session.ID, first, domain.KnowledgeOptions{})
			}
			if err == nil {
				t.Fatal("the failed turn reported success")
			}

			// The transcript keeps the question that was actually asked; nothing is
			// quietly deleted to keep history tidy.
			stored, err := store.ListMessages(ctx, user.ID, session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(stored) != 1 || stored[0].Role != "user" || stored[0].Content != first {
				t.Fatalf("transcript = %#v, want the unanswered question kept as history", stored)
			}

			if _, err := service.Send(ctx, user.ID, session.ID, "重问一次", domain.KnowledgeOptions{}); err != nil {
				t.Fatalf("the retry turn failed: %v", err)
			}
			retry := assistant.requests[len(assistant.requests)-1]
			if len(retry.History) != 0 {
				t.Fatalf("the retry was shown %#v, want no context from a turn that never completed", retry.History)
			}
			if occurrencesOf(retry.History, first) != 0 {
				t.Fatal("the failed question re-entered the model context")
			}
			// And the retried question itself appears exactly once.
			if retry.Content != "重问一次" {
				t.Fatalf("retry content = %q", retry.Content)
			}
		})
	}
}

// TestCompletedTurnsStillChainAfterFailures proves the filter is about unfinished
// turns only: once an exchange completes it is normal context again, and the
// transcript still tells the whole truth, failures included.
func TestCompletedTurnsStillChainAfterFailures(t *testing.T) {
	ctx := context.Background()
	assistant := &capturingResponder{failFirst: 1}
	store := repository.NewInMemoryRepository()
	user, err := store.CreateUser(ctx, &domain.User{Email: "owner@example.com", DisplayName: "Owner"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "Chat", Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}
	service := NewMessageService(store, assistant, WithConversationBudget(ContextBudget{MaxMessages: 4, MaxRunes: 1000}))

	if _, err := service.Send(ctx, user.ID, session.ID, "失败的一问", domain.KnowledgeOptions{}); err == nil {
		t.Fatal("the failing turn reported success")
	}
	for _, content := range []string{"第一问", "第二问"} {
		if _, err := service.Send(ctx, user.ID, session.ID, content, domain.KnowledgeOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	last := assistant.requests[len(assistant.requests)-1]
	if want := turnsOf(stored("user", "第一问"), stored("assistant", "echo:第一问")); !reflect.DeepEqual(last.History, want) {
		t.Fatalf("window = %#v, want the one completed exchange and not the failed one", last.History)
	}
	storedMessages, err := store.ListMessages(ctx, user.ID, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(storedMessages) != 5 || storedMessages[0].Content != "失败的一问" {
		t.Fatalf("transcript = %#v, want all five messages with the failed question first", storedMessages)
	}
	for _, message := range storedMessages[1:] {
		if message.Role == "user" && message.Content == "失败的一问" {
			t.Fatal("history lost a stored fact")
		}
	}
}

func occurrencesOf(history []responder.Turn, content string) int {
	var count int
	for _, turn := range history {
		if turn.Content == content {
			count++
		}
	}
	return count
}
