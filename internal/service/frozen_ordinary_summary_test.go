package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/responder"
	"github.com/xiaopang456/GopherLearn/internal/summary"
)

// frozenOrdinaryScriptedSummarizer stands in for the model that writes summaries. It counts calls,
// can fail on cue, and can run an action at the moment of the call - which is how a test
// checks what was already stored before any model was contacted, rather than trusting a
// comment that says so.
type frozenOrdinaryScriptedSummarizer struct {
	mu      sync.Mutex
	calls   []summary.Request
	err     error
	hook    func()
	replies []string
}

func (s *frozenOrdinaryScriptedSummarizer) Summarize(ctx context.Context, request summary.Request) (summary.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, request)
	if s.hook != nil {
		hook := s.hook
		s.hook = nil
		hook()
	}
	if s.err != nil {
		return summary.Result{}, s.err
	}
	index := len(s.calls) - 1
	content := fmt.Sprintf("摘要第%d版", index+1)
	if index < len(s.replies) && s.replies[index] != "" {
		content = s.replies[index]
	}
	return summary.Result{Content: content}, nil
}

func (s *frozenOrdinaryScriptedSummarizer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *frozenOrdinaryScriptedSummarizer) requestAt(index int) summary.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[index]
}

func frozenOrdinaryNewSummaryFixture(t *testing.T, assistant responder.Responder, summarizer summary.ConversationSummarizer, mode domain.SessionMode) (
	*MessageService, *repository.InMemoryRepository, string, string) {
	t.Helper()
	store := repository.NewInMemoryRepository()
	user, err := store.CreateUser(context.Background(), &domain.User{Email: "owner@example.com", DisplayName: "Owner"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.CreateSession(context.Background(), &domain.Session{
		UserID: user.ID, Title: "会话", Mode: mode,
	})
	if err != nil {
		t.Fatal(err)
	}
	options := []MessageServiceOption{WithCollectionAccess(frozenOrdinaryOpenAccess{}), WithRollingSummary(summarizer, summary.Policy{})}
	return newFrozenOrdinaryMessageService(store, assistant, options...), store, user.ID, session.ID
}

// frozenOrdinarySeedPairs appends count complete turns directly to the store, the way earlier
// conversation would already be there when a new turn arrives.
func frozenOrdinarySeedPairs(t *testing.T, store *repository.InMemoryRepository, userID, sessionID string, count int) []domain.Message {
	t.Helper()
	ctx := context.Background()
	stored := make([]domain.Message, 0, count*2)
	for index := 1; index <= count; index++ {
		for _, entry := range []struct{ role, content string }{
			{roleUser, fmt.Sprintf("旧问题%d", index)},
			{roleAssistant, fmt.Sprintf("旧回答%d", index)},
		} {
			message, err := store.AppendMessage(ctx, userID, sessionID, &domain.Message{Role: entry.role, Content: entry.content})
			if err != nil {
				t.Fatal(err)
			}
			stored = append(stored, *message)
		}
	}
	return stored
}

func frozenOrdinaryLastRequest(t *testing.T, assistant *frozenOrdinaryFixedResponder) responder.Request {
	t.Helper()
	select {
	case request := <-assistant.respondRequest:
		return request
	default:
		t.Fatal("the responder never received a request")
		return responder.Request{}
	}
}

func TestFrozenOrdinaryShortConversationIsNotSummarized(t *testing.T) {
	// Five pairs is ten messages, under the twelve-message trigger: the recent window
	// still holds everything, so a model call would only lose detail.
	summarizer := &frozenOrdinaryScriptedSummarizer{}
	assistant := &frozenOrdinaryFixedResponder{answer: "回答", respondRequest: make(chan responder.Request, 1)}
	service, store, userID, sessionID := frozenOrdinaryNewSummaryFixture(t, assistant, summarizer, domain.SessionModeChat)
	frozenOrdinarySeedPairs(t, store, userID, sessionID, 5)

	if _, err := service.Send(context.Background(), userID, sessionID, "新问题", domain.KnowledgeOptions{}); err != nil {
		t.Fatal(err)
	}
	if summarizer.count() != 0 {
		t.Fatalf("the summarizer ran %d times below the threshold", summarizer.count())
	}
	if request := frozenOrdinaryLastRequest(t, assistant); request.Summary != "" {
		t.Fatalf("a turn with no summary reported one: %q", request.Summary)
	}
	if _, err := store.GetSessionSummary(context.Background(), userID, sessionID); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("a skipped refresh stored a row: %v", err)
	}
}

func TestFrozenOrdinaryThresholdRefreshSummarizesTheOlderPairsOnly(t *testing.T) {
	// Six pairs = twelve uncovered messages, which is exactly the trigger. Eight of
	// them must stay verbatim, so two pairs are compressed and the boundary lands on
	// the second answer.
	summarizer := &frozenOrdinaryScriptedSummarizer{}
	assistant := &frozenOrdinaryFixedResponder{answer: "回答", respondRequest: make(chan responder.Request, 1)}
	service, store, userID, sessionID := frozenOrdinaryNewSummaryFixture(t, assistant, summarizer, domain.SessionModeChat)
	seeded := frozenOrdinarySeedPairs(t, store, userID, sessionID, 6)

	if _, err := service.Send(context.Background(), userID, sessionID, "当前问题", domain.KnowledgeOptions{}); err != nil {
		t.Fatal(err)
	}
	if summarizer.count() != 1 {
		t.Fatalf("calls = %d, want exactly one model call per refresh", summarizer.count())
	}
	request := summarizer.requestAt(0)
	if request.Previous != "" {
		t.Fatalf("a first refresh was given a previous summary: %q", request.Previous)
	}
	if len(request.Turns) != 4 {
		t.Fatalf("compressed %d messages, want the four oldest", len(request.Turns))
	}
	if request.Turns[0].Content != "旧问题1" || request.Turns[3].Content != "旧回答2" {
		t.Fatalf("the compress window is not the oldest two pairs: %+v", request.Turns)
	}
	for _, turn := range request.Turns {
		if strings.Contains(turn.Content, "当前问题") {
			t.Fatal("the question being answered this turn was fed into its own summary")
		}
	}
	stored, err := store.GetSessionSummary(context.Background(), userID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Version != 1 || stored.Content != "摘要第1版" || stored.CoveredMessageID != seeded[3].ID {
		t.Fatalf("stored = %+v, want version 1 covering the second answer (%s)", stored, seeded[3].ID)
	}
	// The turn that triggered it is answered from the new summary plus the eight
	// verbatim messages - the four compressed ones must not also be there.
	responderRequest := frozenOrdinaryLastRequest(t, assistant)
	if responderRequest.Summary != "摘要第1版" {
		t.Fatalf("the strategies were handed %q, want the summary this turn wrote", responderRequest.Summary)
	}
	frozenOrdinaryAssertNoOverlap(t, responderRequest, seeded[:4])
	frozenOrdinaryAssertVerbatim(t, responderRequest, seeded[4:])
}

func TestFrozenOrdinarySecondRefreshIsIncremental(t *testing.T) {
	// After the first refresh the boundary sits at answer 2. Four more pairs bring the
	// uncovered count back to twelve, and the second call must be handed the old text
	// plus only the new exchanges - never the whole transcript again.
	summarizer := &frozenOrdinaryScriptedSummarizer{replies: []string{"摘要A", "摘要B"}}
	assistant := &frozenOrdinaryFixedResponder{answer: "回答", respondRequest: make(chan responder.Request, 4)}
	service, store, userID, sessionID := frozenOrdinaryNewSummaryFixture(t, assistant, summarizer, domain.SessionModeChat)
	// Ten pairs is twenty uncovered messages, so the first send refreshes twelve of
	// them and leaves a boundary at answer 6. The answer to that send is a complete
	// pair by the time the second send reads the session, and one more pair after that
	// is what brings the count back over twelve: the refresh is driven by what has
	// actually accumulated since the boundary, not by how long the session is.
	seeded := frozenOrdinarySeedPairs(t, store, userID, sessionID, 10)
	if _, err := service.Send(context.Background(), userID, sessionID, "第一轮提问", domain.KnowledgeOptions{}); err != nil {
		t.Fatal(err)
	}
	<-assistant.respondRequest
	if summarizer.count() != 1 {
		t.Fatalf("calls = %d after the first refresh, want 1", summarizer.count())
	}
	// Two more complete pairs are needed before twelve messages are uncovered again.
	// Nothing may be generated in between: the threshold is a floor, not a timer.
	for _, question := range []string{"第二轮提问", "第三轮提问"} {
		if _, err := service.Send(context.Background(), userID, sessionID, question, domain.KnowledgeOptions{}); err != nil {
			t.Fatal(err)
		}
		<-assistant.respondRequest
	}
	if summarizer.count() != 2 {
		t.Fatalf("calls = %d, want exactly one more refresh once twelve messages accrued", summarizer.count())
	}
	second := summarizer.requestAt(1)
	if second.Previous != "摘要A" {
		t.Fatalf("the incremental refresh started from %q, want the stored summary", second.Previous)
	}
	// The first refresh covered six pairs, so those twelve messages must not be offered
	// again - otherwise the summary is rewritten from scratch every time and can drift.
	for _, turn := range second.Turns {
		for index := 0; index < 12; index++ {
			if turn.Content == seeded[index].Content {
				t.Fatalf("the second refresh re-compressed %q, which the first summary covers", turn.Content)
			}
		}
	}
	stored, err := store.GetSessionSummary(context.Background(), userID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Version != 2 || stored.Content != "摘要B" {
		t.Fatalf("stored = %+v, want version 2 with the new text", stored)
	}
	if stored.CoveredMessageID == seeded[11].ID || stored.CoveredMessageID == "" {
		t.Fatalf("the boundary did not advance past the first refresh: %q", stored.CoveredMessageID)
	}
}

func TestFrozenOrdinarySummaryAndRecentWindowNeverOverlap(t *testing.T) {
	// A summary written earlier, and no refresh this turn: the window must still start
	// after the stored boundary, or the model reads the same exchange twice - once
	// compressed, once verbatim - and counts it as two independent facts.
	summarizer := &frozenOrdinaryScriptedSummarizer{}
	assistant := &frozenOrdinaryFixedResponder{answer: "回答", respondRequest: make(chan responder.Request, 1)}
	service, store, userID, sessionID := frozenOrdinaryNewSummaryFixture(t, assistant, summarizer, domain.SessionModeChat)
	seeded := frozenOrdinarySeedPairs(t, store, userID, sessionID, 8)
	if _, err := store.CommitSessionSummary(context.Background(), &domain.SessionSummary{
		SessionID: sessionID, UserID: userID, Content: "已覆盖前四问", CoveredMessageID: seeded[7].ID,
	}, 0); err != nil {
		t.Fatal(err)
	}

	if _, err := service.Send(context.Background(), userID, sessionID, "追问", domain.KnowledgeOptions{}); err != nil {
		t.Fatal(err)
	}
	request := frozenOrdinaryLastRequest(t, assistant)
	if request.Summary != "已覆盖前四问" {
		t.Fatalf("the stored summary did not reach the strategies: %q", request.Summary)
	}
	frozenOrdinaryAssertNoOverlap(t, request, seeded[:8])
	frozenOrdinaryAssertVerbatim(t, request, seeded[8:])
}

func TestFrozenOrdinaryWindowStartsStrictlyAfterTheBoundaryWhateverItNames(t *testing.T) {
	// The service cuts its own window from the boundary id, independently of the
	// planner, so the cut needs its own guard. A boundary naming a question and one
	// naming an answer must both resume *after* that message: restarting at it would
	// hand the model a covered exchange twice, once inside the summary.
	summarizer := &frozenOrdinaryScriptedSummarizer{}
	assistant := &frozenOrdinaryFixedResponder{answer: "回答", respondRequest: make(chan responder.Request, 1)}
	service, store, userID, sessionID := frozenOrdinaryNewSummaryFixture(t, assistant, summarizer, domain.SessionModeChat)
	seeded := frozenOrdinarySeedPairs(t, store, userID, sessionID, 8)
	if _, err := store.CommitSessionSummary(context.Background(), &domain.SessionSummary{
		SessionID: sessionID, UserID: userID, Content: "已覆盖前四问", CoveredMessageID: seeded[6].ID,
	}, 0); err != nil {
		t.Fatal(err)
	}

	if _, err := service.Send(context.Background(), userID, sessionID, "追问", domain.KnowledgeOptions{}); err != nil {
		t.Fatal(err)
	}
	request := frozenOrdinaryLastRequest(t, assistant)
	frozenOrdinaryAssertNoOverlap(t, request, seeded[:7])
	if len(request.History) == 0 || request.History[0].Content != "旧问题5" || request.History[0].Role != roleUser {
		t.Fatalf("window = %+v, want it to open at the first complete pair after the boundary", request.History)
	}
}

func TestFrozenOrdinaryCurrentQuestionAppearsOnce(t *testing.T) {
	summarizer := &frozenOrdinaryScriptedSummarizer{}
	assistant := &frozenOrdinaryFixedResponder{answer: "回答", respondRequest: make(chan responder.Request, 1)}
	service, store, userID, sessionID := frozenOrdinaryNewSummaryFixture(t, assistant, summarizer, domain.SessionModeChat)
	frozenOrdinarySeedPairs(t, store, userID, sessionID, 3)

	if _, err := service.Send(context.Background(), userID, sessionID, "唯一的问题", domain.KnowledgeOptions{}); err != nil {
		t.Fatal(err)
	}
	request := frozenOrdinaryLastRequest(t, assistant)
	if request.Content != "唯一的问题" {
		t.Fatalf("content = %q", request.Content)
	}
	occurrences := 0
	for _, turn := range request.History {
		if turn.Content == request.Content {
			occurrences++
		}
	}
	if occurrences != 0 {
		t.Fatalf("the question appeared in history %d times as well", occurrences)
	}
}

func TestFrozenOrdinaryFailedTurnIsNotSummarizedLater(t *testing.T) {
	// A model failure leaves a question with no answer behind. It must not be answered
	// by the summary either: the orphan is skipped when pairs are counted and never
	// handed to the summarizer.
	summarizer := &frozenOrdinaryScriptedSummarizer{}
	assistant := &frozenOrdinaryFixedResponder{answer: "回答", respondRequest: make(chan responder.Request, 4)}
	service, store, userID, sessionID := frozenOrdinaryNewSummaryFixture(t, assistant, summarizer, domain.SessionModeChat)
	frozenOrdinarySeedPairs(t, store, userID, sessionID, 6)
	failing := &frozenOrdinaryFixedResponder{err: ErrInternal, respondRequest: make(chan responder.Request, 1)}
	service.responder = failing
	if _, err := service.Send(context.Background(), userID, sessionID, "失败的提问", domain.KnowledgeOptions{}); err == nil {
		t.Fatal("a failed turn returned no error")
	}
	<-failing.respondRequest
	service.responder = assistant
	if _, err := service.Send(context.Background(), userID, sessionID, "补一轮提问", domain.KnowledgeOptions{}); err != nil {
		t.Fatal(err)
	}

	for index := 0; index < summarizer.count(); index++ {
		for _, turn := range summarizer.requestAt(index).Turns {
			if strings.Contains(turn.Content, "失败的提问") {
				t.Fatalf("call %d summarized an unanswered question", index+1)
			}
		}
	}
	for _, turn := range frozenOrdinaryLastRequest(t, assistant).History {
		if turn.Content == "失败的提问" {
			t.Fatal("the orphaned question reached the recent window as an answered turn")
		}
	}
}

func TestFrozenOrdinarySummaryFailureEndsTheTurnWithoutAnAnswer(t *testing.T) {
	// The refresh was due and the model refused it. The turn ends as a model failure
	// does today: the question stays in the transcript, no answer is stored, and no
	// stale summary is presented as though the refresh had succeeded.
	summarizer := &frozenOrdinaryScriptedSummarizer{err: errors.New("provider said no")}
	assistant := &frozenOrdinaryFixedResponder{answer: "回答", respondRequest: make(chan responder.Request, 1)}
	service, store, userID, sessionID := frozenOrdinaryNewSummaryFixture(t, assistant, summarizer, domain.SessionModeChat)
	frozenOrdinarySeedPairs(t, store, userID, sessionID, 6)

	if _, err := service.Send(context.Background(), userID, sessionID, "提问", domain.KnowledgeOptions{}); !errors.Is(err, ErrInternal) {
		t.Fatalf("error = %v, want the turn to end as an internal failure", err)
	}
	messages, err := store.ListMessages(context.Background(), userID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if messages[len(messages)-1].Role != roleUser {
		t.Fatalf("the last stored message is %q; a failed summary must not invent an answer", messages[len(messages)-1].Role)
	}
	if _, err := store.GetSessionSummary(context.Background(), userID, sessionID); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("a failed refresh stored a summary anyway: %v", err)
	}
	select {
	case request := <-assistant.respondRequest:
		t.Fatalf("the answer strategy still ran after the summary failed: %+v", request)
	default:
	}
}

func TestFrozenOrdinaryACancelledTurnDuringTheRefreshEndsAsACancellation(t *testing.T) {
	// The caller hanging up while the summary is being written must not be reported as
	// an internal failure, and must not leave a half-written summary behind. The
	// question stays in the transcript, and the next refresh must not treat the
	// cancelled question as an answered exchange.
	summarizer := &frozenOrdinaryScriptedSummarizer{}
	assistant := &frozenOrdinaryFixedResponder{answer: "回答", respondRequest: make(chan responder.Request, 1)}
	service, store, userID, sessionID := frozenOrdinaryNewSummaryFixture(t, assistant, summarizer, domain.SessionModeChat)
	frozenOrdinarySeedPairs(t, store, userID, sessionID, 6)

	ctx, cancel := context.WithCancel(context.Background())
	// The transport reports its own failure, but the caller already hung up: whose
	// event this is has to be decided from the context, not from the error text, or a
	// cancellation is booked as a server fault.
	summarizer.err = errors.New("connection reset by peer")
	summarizer.hook = cancel
	_, sendErr := service.Send(ctx, userID, sessionID, "被取消的提问", domain.KnowledgeOptions{})
	if !errors.Is(sendErr, context.Canceled) {
		t.Fatalf("error = %v, want the cancellation to stay a cancellation", sendErr)
	}
	if errors.Is(sendErr, ErrInternal) {
		t.Fatal("a cancelled caller was folded into an internal failure")
	}
	if _, err := store.GetSessionSummary(context.Background(), userID, sessionID); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("a cancelled refresh stored a summary: %v", err)
	}
	select {
	case request := <-assistant.respondRequest:
		t.Fatalf("the answer strategy ran after the caller hung up: %+v", request)
	default:
	}

	// The next turn's refresh is the real test: the cancelled question is an orphan, so
	// it belongs neither to the summary nor to the window.
	working := &frozenOrdinaryScriptedSummarizer{}
	service.summarizer = working
	if _, err := service.Send(context.Background(), userID, sessionID, "下一轮提问", domain.KnowledgeOptions{}); err != nil {
		t.Fatal(err)
	}
	if working.count() != 1 {
		t.Fatalf("summarizer calls = %d, want one refresh on the next turn", working.count())
	}
	for _, turn := range working.requestAt(0).Turns {
		if strings.Contains(turn.Content, "被取消的提问") || strings.Contains(turn.Content, "下一轮提问") {
			t.Fatalf("a cancelled or unanswered question entered the summary material: %+v", working.requestAt(0).Turns)
		}
	}
}

func TestFrozenOrdinaryCommitConflictAdoptsTheNewerSummaryWithoutPayingTwice(t *testing.T) {
	// Another writer commits while this turn's generation is in flight. The loser must
	// adopt the newer summary, not overwrite it, and must not regenerate: a second
	// model call for text that is already stored is exactly the double spend a version
	// check exists to prevent.
	summarizer := &frozenOrdinaryScriptedSummarizer{}
	store := repository.NewInMemoryRepository()
	user, err := store.CreateUser(context.Background(), &domain.User{Email: "owner@example.com", DisplayName: "Owner"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.CreateSession(context.Background(), &domain.Session{UserID: user.ID, Title: "会话", Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}
	seeded := frozenOrdinarySeedPairs(t, store, user.ID, session.ID, 8)
	// Eight pairs with a boundary at the first answer leaves fourteen uncovered
	// messages, so a refresh is due. The first commit is this turn's read of
	// "version 1"; the hook then stands for the concurrent writer that moved the row to
	// version 2 before this turn could commit.
	if _, err := store.CommitSessionSummary(context.Background(), &domain.SessionSummary{
		SessionID: session.ID, UserID: user.ID, Content: "别人的旧版", CoveredMessageID: seeded[1].ID,
	}, 0); err != nil {
		t.Fatal(err)
	}
	summarizer.hook = func() {
		if _, err := store.CommitSessionSummary(context.Background(), &domain.SessionSummary{
			SessionID: session.ID, UserID: user.ID, Content: "别人更新版", CoveredMessageID: seeded[3].ID,
		}, 1); err != nil {
			t.Errorf("the concurrent commit failed: %v", err)
		}
	}
	assistant := &frozenOrdinaryFixedResponder{answer: "回答", respondRequest: make(chan responder.Request, 1)}
	service := newFrozenOrdinaryMessageService(store, assistant,
		WithCollectionAccess(frozenOrdinaryOpenAccess{}), WithRollingSummary(summarizer, summary.Policy{}))

	if _, err := service.Send(context.Background(), user.ID, session.ID, "提问", domain.KnowledgeOptions{}); err != nil {
		t.Fatalf("a lost race must not fail the turn: %v", err)
	}
	if summarizer.count() != 1 {
		t.Fatalf("calls = %d, want the loser of the race to generate once and no more", summarizer.count())
	}
	stored, err := store.GetSessionSummary(context.Background(), user.ID, session.ID)
	if err != nil || stored.Content != "别人更新版" || stored.Version != 2 {
		t.Fatalf("the winner's row was overwritten: %+v err %v", stored, err)
	}
	if request := frozenOrdinaryLastRequest(t, assistant); request.Summary != "别人更新版" {
		t.Fatalf("this turn was answered from %q, want the newer stored summary", request.Summary)
	}
}

func TestFrozenOrdinarySummaryBoundaryThatNoLongerExistsEndsTheTurn(t *testing.T) {
	// The stored summary names a message this session does not contain, so what it
	// covers is unknown. The turn is refused rather than answered from "the summary plus
	// whatever the window can carry": that combination either resends material the
	// summary already claims or trusts text covering nothing, and either way the answer
	// comes from a context nobody can describe. No model is asked - not for a summary
	// and not for an answer - and the stored row is left as it was for an operator to
	// look at; silently clearing it would erase the only evidence of the damage.
	for _, testCase := range []struct {
		name       string
		summarizer summary.ConversationSummarizer
	}{
		{"summarizer installed", &frozenOrdinaryScriptedSummarizer{}},
		// The same rule has to hold where no summarizer exists at all: the boundary is
		// what cuts the window, so an unlocatable one is broken context either way.
		{"summaries disabled", nil},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			scripted, _ := testCase.summarizer.(*frozenOrdinaryScriptedSummarizer)
			assistant := &frozenOrdinaryFixedResponder{answer: "回答", respondRequest: make(chan responder.Request, 1)}
			service, store, userID, sessionID := frozenOrdinaryNewSummaryFixture(t, assistant, testCase.summarizer, domain.SessionModeChat)
			frozenOrdinarySeedPairs(t, store, userID, sessionID, 3)
			committed, err := store.CommitSessionSummary(context.Background(), &domain.SessionSummary{
				SessionID: sessionID, UserID: userID, Content: "边界已丢失", CoveredMessageID: "message-deleted-long-ago",
			}, 0)
			if err != nil {
				t.Fatal(err)
			}

			if _, err := service.Send(context.Background(), userID, sessionID, "提问", domain.KnowledgeOptions{}); !errors.Is(err, ErrSummaryBoundaryMissing) {
				t.Fatalf("error = %v, want the boundary-missing integrity failure", err)
			} else if !errors.Is(err, ErrInternal) {
				t.Fatalf("the integrity failure is not an internal error for the API layer: %v", err)
			}
			if scripted != nil && scripted.count() != 0 {
				t.Fatalf("the summarizer was called %d times for a turn that must not reach a model", scripted.count())
			}
			select {
			case request := <-assistant.respondRequest:
				t.Fatalf("an answer was generated from undecidable context: %+v", request)
			default:
			}
			messages, err := store.ListMessages(context.Background(), userID, sessionID)
			if err != nil {
				t.Fatal(err)
			}
			if len(messages) != 7 || messages[len(messages)-1].Role != roleUser {
				t.Fatalf("stored messages = %+v, want the three seeded turns plus this question and no answer", messages)
			}
			still, err := store.GetSessionSummary(context.Background(), userID, sessionID)
			if err != nil || still.Content != committed.Content || still.Version != committed.Version ||
				still.CoveredMessageID != committed.CoveredMessageID {
				t.Fatalf("the row was rewritten instead of reported: %+v err %v", still, err)
			}
		})
	}
}

func TestFrozenOrdinaryStreamReportsASummaryBoundaryFailureWithTheExistingEvent(t *testing.T) {
	summarizer := &frozenOrdinaryScriptedSummarizer{}
	assistant := &frozenOrdinaryFixedResponder{answer: "回答", respondRequest: make(chan responder.Request, 1)}
	service, store, userID, sessionID := frozenOrdinaryNewSummaryFixture(t, assistant, summarizer, domain.SessionModeChat)
	frozenOrdinarySeedPairs(t, store, userID, sessionID, 3)
	if _, err := store.CommitSessionSummary(context.Background(), &domain.SessionSummary{
		SessionID: sessionID, UserID: userID, Content: "边界已丢失", CoveredMessageID: "message-gone",
	}, 0); err != nil {
		t.Fatal(err)
	}

	var events []string
	err := service.Stream(context.Background(), userID, sessionID, "提问", domain.KnowledgeOptions{}, func(event StreamEvent) error {
		events = append(events, event.Name+"/"+event.Code)
		return nil
	})
	if !errors.Is(err, ErrInternal) {
		t.Fatalf("Stream error = %v, want the existing internal failure", err)
	}
	// No new event, and no invented delta: the lifecycle began, so the client is told it
	// failed, which is the same shape any model failure already takes.
	if strings.Join(events, ",") != "message.started/,message.failed/internal_error" {
		t.Fatalf("events = %v, want message.started then message.failed only", events)
	}
	if summarizer.count() != 0 {
		t.Fatalf("summarizer calls = %d, want none", summarizer.count())
	}
	messages, err := store.ListMessages(context.Background(), userID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if messages[len(messages)-1].Role != roleUser {
		t.Fatalf("the last stored message is %q, want the unanswered question", messages[len(messages)-1].Role)
	}
}

func TestFrozenOrdinaryConversationForReportsClosedOutcomes(t *testing.T) {
	// The outcome strings are what an operator would read in a log line, so the three
	// ways a turn can end up with an unchanged summary have to stay distinguishable.
	cases := []struct {
		name       string
		pairs      int
		summarizer summary.ConversationSummarizer
		want       SummaryOutcome
		detail     string
	}{
		{"below threshold", 5, &frozenOrdinaryScriptedSummarizer{}, SummarySkipped, summary.SkipBelowThreshold},
		{"refreshed", 6, &frozenOrdinaryScriptedSummarizer{}, SummaryRefreshed, ""},
		{"disabled", 6, nil, SummaryDisabled, ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			assistant := &frozenOrdinaryFixedResponder{answer: "回答"}
			service, store, userID, sessionID := frozenOrdinaryNewSummaryFixture(t, assistant, testCase.summarizer, domain.SessionModeChat)
			frozenOrdinarySeedPairs(t, store, userID, sessionID, testCase.pairs)
			turn, err := service.openTurn(context.Background(), userID, sessionID, "提问")
			if err != nil {
				t.Fatal(err)
			}
			_, _, outcome, detail, err := service.ordinaryConversationFor(context.Background(), userID, sessionID, turn)
			if err != nil {
				t.Fatal(err)
			}
			if outcome != testCase.want || detail != testCase.detail {
				t.Fatalf("outcome = %q/%q, want %q/%q", outcome, detail, testCase.want, testCase.detail)
			}
		})
	}
}

func TestFrozenOrdinaryStreamKeepsTheEventSetAndStaysSilentDuringRefresh(t *testing.T) {
	// Order the SSE client can see: the question is stored, the lifecycle starts, the
	// summary is written in silence, and only then does answer text arrive. No delta is
	// invented for the summarizing step, and the public event set does not grow.
	var events []string
	// One log fed by both the emitter and the summarizer's hook, so the ordering
	// assertion does not depend on when a goroutine happens to be scheduled.
	var steps []string
	var mutex sync.Mutex
	summarizer := &frozenOrdinaryScriptedSummarizer{}
	assistant := &frozenOrdinaryFixedResponder{answer: "流式回答", chunks: []string{"流式", "回答"}}
	service, store, userID, sessionID := frozenOrdinaryNewSummaryFixture(t, assistant, summarizer, domain.SessionModeChat)
	frozenOrdinarySeedPairs(t, store, userID, sessionID, 6)

	summarizer.hook = func() {
		mutex.Lock()
		steps = append(steps, "summarize")
		mutex.Unlock()
		// Reading inside the hook is the ordering rule this test exists for: any model
		// call, including a summarizing one, happens after this turn's question is
		// already stored.
		messages, err := store.ListMessages(context.Background(), userID, sessionID)
		if err != nil {
			t.Errorf("reading messages during the refresh: %v", err)
			return
		}
		if len(messages) == 0 || messages[len(messages)-1].Content != "提问" {
			t.Errorf("the summarizer ran before the user message was stored: %d messages", len(messages))
		}
	}

	err := service.Stream(context.Background(), userID, sessionID, "提问", domain.KnowledgeOptions{}, func(event StreamEvent) error {
		events = append(events, event.Name)
		mutex.Lock()
		switch event.Name {
		case "message.started":
			steps = append(steps, "started")
		case "message.delta":
			if len(steps) > 0 && steps[len(steps)-1] != "answer" {
				steps = append(steps, "answer")
			}
		}
		mutex.Unlock()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(events, ",") != "message.started,message.delta,message.delta,message.completed" {
		t.Fatalf("events = %v, want the existing set and order with no summary event", events)
	}
	if summarizer.count() != 1 {
		t.Fatalf("calls = %d, want one refresh", summarizer.count())
	}
	mutex.Lock()
	observed := strings.Join(steps, ",")
	mutex.Unlock()
	if observed != "started,summarize,answer" {
		t.Fatalf("steps = %q, want the refresh after the lifecycle start and before any answer text", observed)
	}
}

func TestFrozenOrdinaryStreamReportsASummaryFailureThroughTheExistingEvent(t *testing.T) {
	var events []string
	summarizer := &frozenOrdinaryScriptedSummarizer{err: errors.New("provider refused")}
	assistant := &frozenOrdinaryFixedResponder{answer: "不该出现"}
	service, store, userID, sessionID := frozenOrdinaryNewSummaryFixture(t, assistant, summarizer, domain.SessionModeChat)
	frozenOrdinarySeedPairs(t, store, userID, sessionID, 6)

	err := service.Stream(context.Background(), userID, sessionID, "提问", domain.KnowledgeOptions{}, func(event StreamEvent) error {
		events = append(events, event.Name)
		if event.Name == "message.failed" && event.Code != "internal_error" {
			t.Errorf("failure code = %q, want internal_error", event.Code)
		}
		return nil
	})
	if !errors.Is(err, ErrInternal) {
		t.Fatalf("error = %v, want ErrInternal", err)
	}
	if strings.Join(events, ",") != "message.started,message.failed" {
		t.Fatalf("events = %v, want started then failed and no delta", events)
	}
	messages, storeErr := store.ListMessages(context.Background(), userID, sessionID)
	if storeErr != nil {
		t.Fatal(storeErr)
	}
	if messages[len(messages)-1].Role != roleUser {
		t.Fatal("a failed summary left an assistant message behind")
	}
}

func TestFrozenOrdinarySynchronousAndStreamedTurnsGetTheSameContext(t *testing.T) {
	// The whole point of a shared assembly point: a question answered over SSE has to
	// recall the same summary and the same window as the same question answered
	// synchronously, or the transport would change what the model knows.
	build := func() (*MessageService, *repository.InMemoryRepository, string, string, *frozenOrdinaryFixedResponder) {
		summarizer := &frozenOrdinaryScriptedSummarizer{}
		assistant := &frozenOrdinaryFixedResponder{answer: "回答", chunks: []string{"回答"},
			respondRequest: make(chan responder.Request, 1), streamRequest: make(chan responder.Request, 1)}
		service, store, userID, sessionID := frozenOrdinaryNewSummaryFixture(t, assistant, summarizer, domain.SessionModeChat)
		frozenOrdinarySeedPairs(t, store, userID, sessionID, 6)
		return service, store, userID, sessionID, assistant
	}
	syncService, _, syncUser, syncSession, syncAssistant := build()
	if _, err := syncService.Send(context.Background(), syncUser, syncSession, "同一个问题", domain.KnowledgeOptions{}); err != nil {
		t.Fatal(err)
	}
	streamService, _, streamUser, streamSession, streamAssistant := build()
	if err := streamService.Stream(context.Background(), streamUser, streamSession, "同一个问题", domain.KnowledgeOptions{},
		func(StreamEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	sent := <-syncAssistant.respondRequest
	received := <-streamAssistant.streamRequest
	if sent.Summary != received.Summary || sent.Summary == "" {
		t.Fatalf("the two transports were handed different summaries: %q vs %q", sent.Summary, received.Summary)
	}
	if len(sent.History) != len(received.History) {
		t.Fatalf("history lengths differ: %d vs %d", len(sent.History), len(received.History))
	}
	for index := range sent.History {
		if sent.History[index] != received.History[index] {
			t.Fatalf("history entry %d differs: %+v vs %+v", index, sent.History[index], received.History[index])
		}
	}
}

func TestFrozenOrdinaryEchoDeploymentWithoutASummarizerNeverCallsOne(t *testing.T) {
	// No summarizer installed is the echo configuration: the recent window is the
	// whole context and nothing is ever generated.
	assistant := &frozenOrdinaryFixedResponder{answer: "回声", respondRequest: make(chan responder.Request, 1)}
	store := repository.NewInMemoryRepository()
	user, err := store.CreateUser(context.Background(), &domain.User{Email: "echo@example.com", DisplayName: "Echo"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.CreateSession(context.Background(), &domain.Session{UserID: user.ID, Title: "会话", Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}
	frozenOrdinarySeedPairs(t, store, user.ID, session.ID, 10)
	service := newFrozenOrdinaryMessageService(store, assistant, WithCollectionAccess(frozenOrdinaryOpenAccess{}))

	if _, err := service.Send(context.Background(), user.ID, session.ID, "提问", domain.KnowledgeOptions{}); err != nil {
		t.Fatal(err)
	}
	request := <-assistant.respondRequest
	if request.Summary != "" {
		t.Fatalf("a deployment with no summarizer produced %q", request.Summary)
	}
	if len(request.History) == 0 {
		t.Fatal("the recent window disappeared with the summarizer")
	}
	if _, err := store.GetSessionSummary(context.Background(), user.ID, session.ID); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("no summarizer, yet a row exists: %v", err)
	}
}

func TestFrozenOrdinarySummaryStaysInsideItsOwnSession(t *testing.T) {
	// A second session of the same user, and a second user, both start with nothing:
	// the summary is scoped by session and owned by user, so neither a fresh
	// conversation nor another account inherits a recollection.
	summarizer := &frozenOrdinaryScriptedSummarizer{}
	assistant := &frozenOrdinaryFixedResponder{answer: "回答", respondRequest: make(chan responder.Request, 4)}
	service, store, userID, sessionID := frozenOrdinaryNewSummaryFixture(t, assistant, summarizer, domain.SessionModeChat)
	frozenOrdinarySeedPairs(t, store, userID, sessionID, 6)
	if _, err := service.Send(context.Background(), userID, sessionID, "提问", domain.KnowledgeOptions{}); err != nil {
		t.Fatal(err)
	}
	<-assistant.respondRequest

	other, err := store.CreateSession(context.Background(), &domain.Session{UserID: userID, Title: "另一会话", Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}
	intruder, err := store.CreateUser(context.Background(), &domain.User{Email: "intruder@example.com", DisplayName: "Intruder"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetSessionSummary(context.Background(), userID, other.ID); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("the same user's other session reported a summary: %v", err)
	}
	if _, err := store.GetSessionSummary(context.Background(), intruder.ID, sessionID); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("another user could read the summary: %v", err)
	}
	// Asking for somebody else's session must not reach the model either, so no
	// summarizer call and no responder request are added by the attempt.
	callsBefore, requestsBefore := summarizer.count(), 0
	if _, err := service.Send(context.Background(), intruder.ID, sessionID, "偷看", domain.KnowledgeOptions{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner send error = %v, want ErrNotFound", err)
	}
	if summarizer.count() != callsBefore {
		t.Fatal("a refused turn refreshed another user's summary")
	}
	select {
	case <-assistant.respondRequest:
		t.Fatal("the answer model ran for a turn that was never authorized")
	default:
	}
	if requestsBefore != 0 {
		t.Fatal("unreachable")
	}
}

func frozenOrdinaryAssertNoOverlap(t *testing.T, request responder.Request, covered []domain.Message) {
	t.Helper()
	for _, turn := range request.History {
		for _, message := range covered {
			if turn.Content == message.Content {
				t.Fatalf("the window still carries %q, which the summary covers:\n%+v", message.Content, request.History)
			}
		}
	}
}

func frozenOrdinaryAssertVerbatim(t *testing.T, request responder.Request, kept []domain.Message) {
	t.Helper()
	for _, message := range kept {
		found := false
		for _, turn := range request.History {
			if turn.Content == message.Content {
				found = true
			}
		}
		if !found {
			t.Fatalf("%q should still be in the window verbatim: %+v", message.Content, request.History)
		}
	}
}
