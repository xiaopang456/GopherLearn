package service

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/modelcall"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/responder"
	"github.com/xiaopang456/GopherLearn/internal/summary"
)

// providerFault is one way a provider turn can go wrong, paired with what the
// task record must say once the service has mapped the caller's error.
type providerFault struct {
	name       string
	reply      func(timeout time.Duration) scriptedReply
	wantReason string
	wantOut    modelcall.Outcome
	wantUsage  bool
}

var providerFaults = []providerFault{
	{
		name:       "provider status",
		reply:      func(time.Duration) scriptedReply { return scriptStatus(http.StatusServiceUnavailable) },
		wantReason: reasonProviderStatus, wantOut: modelcall.OutcomeProviderError,
	},
	{
		name:       "model timeout",
		reply:      func(time.Duration) scriptedReply { return scriptDelayed() },
		wantReason: reasonTimeout, wantOut: modelcall.OutcomeTimeout,
	},
	{
		// Not parseable as a body and not parseable as a stream frame, so the same
		// fault means the same thing on all three shapes.
		name:       "malformed response",
		reply:      func(time.Duration) scriptedReply { return scriptRaw("{invalid") },
		wantReason: reasonMalformedResponse, wantOut: modelcall.OutcomeMalformed,
	},
	{
		name: "empty answer",
		reply: func(time.Duration) scriptedReply {
			return scriptCompletion("", usageBody(400, 1, 401))
		},
		wantReason: reasonEmptyCompletion, wantOut: modelcall.OutcomeEmpty, wantUsage: true,
	},
}

// TestTaskKeepsTheRealCauseAcrossTheInternalMapping is the regression for the
// mapping defect. Send, Stream and StreamSteps answer ErrInternal for any model
// failure, so a record built from the returned error called four different
// problems "internal".
func TestTaskKeepsTheRealCauseAcrossTheInternalMapping(t *testing.T) {
	const clientTimeout = 120 * time.Millisecond

	for _, shape := range []string{"send", "stream", "stream_steps"} {
		for _, fault := range providerFaults {
			t.Run(shape+"/"+fault.name, func(t *testing.T) {
				script := newProviderScriptWithTimeout(t, clientTimeout, fault.reply(clientTimeout))
				sessionField := func(f *metricsFixture) string { return f.sessionID }
				var assistant responder.Responder
				if shape == "stream_steps" {
					assistant, _ = newAgentResponder(t, script.client)
					sessionField = func(f *metricsFixture) string { return f.agentID }
				} else {
					direct, err := responder.NewModelStrategy(domain.AnswerStrategyDirectChat, script.client, "回答。")
					if err != nil {
						t.Fatal(err)
					}
					assistant, err = responder.NewStrategyResponder(direct)
					if err != nil {
						t.Fatal(err)
					}
				}
				fixture := newMetricsFixture(t, assistant, true)
				ctx := t.Context()
				sessionID := sessionField(fixture)

				var outward error
				switch shape {
				case "send":
					_, outward = fixture.service.Send(ctx, fixture.userID, sessionID, "问题", domain.KnowledgeOptions{})
				case "stream":
					outward = fixture.service.Stream(ctx, fixture.userID, sessionID, "问题",
						domain.KnowledgeOptions{}, func(StreamEvent) error { return nil })
				case "stream_steps":
					outward = fixture.service.StreamSteps(ctx, fixture.userID, sessionID, "问题",
						domain.KnowledgeOptions{}, func(StreamEvent) error { return nil })
				}
				// The contract the HTTP layer already had is untouched.
				if !errors.Is(outward, ErrInternal) {
					t.Fatalf("outward error = %v, want the existing ErrInternal", outward)
				}

				record := fixture.sink.one(t)
				if record.ReasonCode != fault.wantReason {
					t.Fatalf("reason = %q, want the real cause %q rather than the mapped %q",
						record.ReasonCode, fault.wantReason, reasonInternal)
				}
				if record.Outcome != fault.wantOut {
					t.Fatalf("outcome = %s, want %s", record.Outcome, fault.wantOut)
				}
				if record.FailureStage != stageModel {
					t.Fatalf("stage = %q, want %q", record.FailureStage, stageModel)
				}
				if len(record.Calls) != 1 {
					t.Fatalf("calls = %+v, want the failed provider call kept", record.Calls)
				}
				call := record.Calls[0]
				if call.Outcome == modelcall.OutcomeOK {
					t.Fatalf("call outcome = %s, want a failure", call.Outcome)
				}
				if !call.HTTPDispatched {
					t.Fatal("the request left the process but the record says it did not")
				}
				if call.Purpose != modelcall.PurposeDialogue {
					t.Fatalf("purpose = %s, want dialogue", call.Purpose)
				}
				if fault.wantUsage && call.Usage.Total.Value != 401 {
					t.Fatalf("usage = %+v, want the tokens of the empty-but-billed call kept", call.Usage)
				}
				if record.Totals.Dispatches != 1 {
					t.Fatalf("dispatches = %d, want the one attempt counted", record.Totals.Dispatches)
				}
			})
		}
	}
}

// TestLearningReasonSeparatesProviderFailureFromRejectedOutput: both arrive as
// ErrLearningGeneration or ErrLearningGrading, and only one of them means the
// model wrote something unusable.
func TestLearningReasonSeparatesProviderFailureFromRejectedOutput(t *testing.T) {
	t.Run("generation over a provider outage", func(t *testing.T) {
		script := newProviderScript(t,
			scriptStatus(http.StatusServiceUnavailable),
			scriptStatus(http.StatusServiceUnavailable),
			scriptStatus(http.StatusServiceUnavailable),
		)
		fixture := newQuizMetricsFixture(t, script.client)
		_, err := fixture.service.Start(t.Context(), fixture.userID, fixture.agentChat, "Redis 持久化", nil)
		if !errors.Is(err, ErrLearningGeneration) {
			t.Fatalf("error = %v, want the existing generation refusal", err)
		}
		record := fixture.sink.one(t)
		if record.ReasonCode != reasonProviderStatus {
			t.Fatalf("reason = %q, want the outage rather than %q", record.ReasonCode, reasonGenerationRejected)
		}
		if record.FailureStage != stageGeneration {
			t.Fatalf("stage = %q, want %q", record.FailureStage, stageGeneration)
		}
		if record.Outcome != modelcall.OutcomeProviderError {
			t.Fatalf("outcome = %s, want provider_error", record.Outcome)
		}
	})

	t.Run("generation rejected by the shape rules", func(t *testing.T) {
		script := newProviderScript(t,
			scriptCompletion("not json", usageBody(10, 1, 11)),
			scriptCompletion("still not json", usageBody(10, 1, 11)),
			scriptCompletion("nope", usageBody(10, 1, 11)),
		)
		fixture := newQuizMetricsFixture(t, script.client)
		if _, err := fixture.service.Start(t.Context(), fixture.userID, fixture.agentChat, "Redis 持久化", nil); err == nil {
			t.Fatal("an unusable question set was accepted")
		}
		record := fixture.sink.one(t)
		if record.ReasonCode != reasonGenerationRejected {
			t.Fatalf("reason = %q, want the business rejection", record.ReasonCode)
		}
		if record.Totals.Calls != 3 || record.Totals.Prompt.Value != 30 {
			t.Fatalf("totals = %+v, want all three attempts counted", record.Totals)
		}
	})

	t.Run("grading over a provider outage", func(t *testing.T) {
		script := newProviderScript(t,
			scriptCompletion(learningQuestions([]int{1, 2}, []int{3}, []int{4, 5}), usageBody(10, 1, 11)),
			scriptStatus(http.StatusServiceUnavailable),
		)
		fixture := newQuizMetricsFixture(t, script.client)
		quiz, err := fixture.service.Start(t.Context(), fixture.userID, fixture.agentChat, "Redis 持久化", nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = fixture.service.Answer(t.Context(), fixture.userID, quiz.ID, "sub-1", 1, "答案内容")
		if !errors.Is(err, ErrLearningGrading) {
			t.Fatalf("error = %v, want the existing grading refusal", err)
		}
		record := fixture.sink.all()[1]
		if record.ReasonCode != reasonProviderStatus {
			t.Fatalf("reason = %q, want the outage rather than %q", record.ReasonCode, reasonGradingRejected)
		}
		if record.FailureStage != stageGrading {
			t.Fatalf("stage = %q, want %q", record.FailureStage, stageGrading)
		}
		if record.Totals.ByPurpose[modelcall.PurposeQuizGrading].Calls != 1 {
			t.Fatalf("purposes = %+v, want the failed grading call counted", record.Totals.ByPurpose)
		}
	})

	t.Run("grading rejected by the answer contract", func(t *testing.T) {
		script := newProviderScript(t,
			scriptCompletion(learningQuestions([]int{1, 2}, []int{3}, []int{4, 5}), usageBody(10, 1, 11)),
			// A grade that reports its own total is refused: the server does the arithmetic.
			scriptCompletion(`{"correctness":50,"completeness":20,"clarity":10,"total":80,`+
				`"feedback":"x","missing_points":[],"confidence":0.8}`, usageBody(12, 2, 14)),
		)
		fixture := newQuizMetricsFixture(t, script.client)
		quiz, err := fixture.service.Start(t.Context(), fixture.userID, fixture.agentChat, "Redis 持久化", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.service.Answer(t.Context(), fixture.userID, quiz.ID, "sub-1", 1, "答案内容"); err == nil {
			t.Fatal("a grade carrying its own total was accepted")
		}
		record := fixture.sink.all()[1]
		if record.ReasonCode != reasonGradingRejected {
			t.Fatalf("reason = %q, want the grading contract rejection", record.ReasonCode)
		}
	})
}

// TestSummaryFailureKeepsItsOwnCause: the turn still ends as ErrInternal, while
// the record says which summary call failed and why.
func TestSummaryFailureKeepsItsOwnCause(t *testing.T) {
	script := newProviderScript(t, scriptStatus(http.StatusServiceUnavailable))
	direct, err := responder.NewModelStrategy(domain.AnswerStrategyDirectChat, script.client, "回答。")
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := responder.NewStrategyResponder(direct)
	if err != nil {
		t.Fatal(err)
	}
	summarizer, err := summary.NewModelSummarizer(script.client, summaryPolicy())
	if err != nil {
		t.Fatal(err)
	}
	fixture := newMetricsFixture(t, assistant, true, WithRollingSummary(summarizer, summaryPolicy()))
	for index := range 4 {
		fixture.storeTurn(t, fixture.sessionID, "旧问题", "旧答案")
		_ = index
	}
	if _, err := fixture.service.Send(t.Context(), fixture.userID, fixture.sessionID, "新问题",
		domain.KnowledgeOptions{}); !errors.Is(err, ErrInternal) {
		t.Fatalf("error = %v, want the unchanged internal mapping", err)
	}
	record := fixture.sink.one(t)
	if record.FailureStage != stageSummary {
		t.Fatalf("stage = %q, want the summary phase", record.FailureStage)
	}
	if record.ReasonCode != reasonProviderStatus {
		t.Fatalf("reason = %q, want the outage behind the mapped error", record.ReasonCode)
	}
	if len(record.Calls) != 1 || record.Calls[0].Purpose != modelcall.PurposeSummary {
		t.Fatalf("calls = %+v, want the summary call kept", record.Calls)
	}
}

// failingAppendStore chooses which stored write fails: the first AppendMessage is
// openTurn's question, the second is the answer.
type failingAppendStore struct {
	repository.Repository
	failOn int
	calls  int
}

func newFailingAppendStore(t *testing.T, failOn int) (*failingAppendStore, string, string) {
	t.Helper()
	ctx := t.Context()
	inner := repository.NewInMemoryRepository()
	user, err := inner.CreateUser(ctx, &domain.User{Email: "stage@example.com", Username: "stage"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := inner.CreateSession(ctx, &domain.Session{UserID: user.ID, Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}
	return &failingAppendStore{Repository: inner, failOn: failOn}, user.ID, session.ID
}

func (s *failingAppendStore) AppendMessage(ctx context.Context, userID, sessionID string,
	message *domain.Message) (*domain.Message, error) {
	s.calls++
	if s.calls == s.failOn {
		return nil, repository.ErrConflict
	}
	return s.Repository.AppendMessage(ctx, userID, sessionID, message)
}

// TestStagesMatchWhereTheOperationActuallyStopped is the stage regression: the
// record used to name a phase the failure had already moved past.
func TestStagesMatchWhereTheOperationActuallyStopped(t *testing.T) {
	t.Run("lock wait cancelled", func(t *testing.T) {
		entered := make(chan struct{}, 1)
		release := make(chan struct{})
		holder := &fixedResponder{answer: "答案", chunks: []string{"答案"}, entered: entered, release: release}
		fixture := newMetricsFixture(t, holder, true)

		holderDone := make(chan error, 1)
		go func() {
			_, err := fixture.service.Send(t.Context(), fixture.userID, fixture.sessionID, "占位问题",
				domain.KnowledgeOptions{})
			holderDone <- err
		}()
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("the holding turn never reached the responder")
		}

		ctx, cancel := context.WithCancel(t.Context())
		go func() {
			time.Sleep(40 * time.Millisecond)
			cancel()
		}()
		err := fixture.service.Stream(ctx, fixture.userID, fixture.sessionID, "排队问题",
			domain.KnowledgeOptions{}, func(StreamEvent) error { return nil })
		close(release)
		if holderErr := <-holderDone; holderErr != nil {
			t.Fatalf("the holding turn failed: %v", holderErr)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want the waiting turn cancelled", err)
		}

		// Select by operation rather than by position: the waiter finishes first,
		// because it is the one that gave up.
		records := fixture.sink.all()
		if len(records) != 2 {
			t.Fatalf("records = %d, want the holder and the waiter", len(records))
		}
		var waiter, held modelcall.TaskRecord
		for _, candidate := range records {
			if candidate.Operation == taskOperationMessageStream {
				waiter = candidate
			} else {
				held = candidate
			}
		}
		if held.Outcome != modelcall.OutcomeOK {
			t.Fatalf("holding turn = %+v, want it to have succeeded", held)
		}
		if waiter.FailureStage != stageLock {
			t.Fatalf("stage = %q, want the session-lock wait", waiter.FailureStage)
		}
		if waiter.Outcome != modelcall.OutcomeCancelled || waiter.ReasonCode != reasonCancelled {
			t.Fatalf("waiter = %s/%s, want cancelled", waiter.Outcome, waiter.ReasonCode)
		}
		if waiter.Totals.Calls != 0 {
			t.Fatalf("calls = %d, want no model call while queued", waiter.Totals.Calls)
		}
		if waiter.AnswerStored != nil || waiter.Delivered != nil {
			t.Fatalf("waiter stored=%v delivered=%v, want both unset: nothing was written or streamed",
				waiter.AnswerStored, waiter.Delivered)
		}
	})

	for _, fault := range []struct {
		name      string
		failOn    int
		wantStage string
		wantCalls int
	}{
		{name: "open turn failure", failOn: 1, wantStage: stageOpenTurn, wantCalls: 0},
		{name: "answer save failure", failOn: 2, wantStage: stageSave, wantCalls: 1},
	} {
		t.Run(fault.name, func(t *testing.T) {
			store, userID, sessionID := newFailingAppendStore(t, fault.failOn)
			script := newProviderScript(t, scriptCompletion("答案文本。", usageBody(12, 3, 15)))
			direct, err := responder.NewModelStrategy(domain.AnswerStrategyDirectChat, script.client, "回答。")
			if err != nil {
				t.Fatal(err)
			}
			assistant, err := responder.NewStrategyResponder(direct)
			if err != nil {
				t.Fatal(err)
			}
			sink := &sinkRecorder{}
			service := NewMessageService(store, assistant, WithCollectionAccess(openAccess{}),
				WithTaskMetrics(modelcall.NewPublisher(true, sink.write)))

			err = service.Stream(t.Context(), userID, sessionID, "问题", domain.KnowledgeOptions{},
				func(StreamEvent) error { return nil })
			if err == nil {
				t.Fatal("a store that refuses the write let the turn succeed")
			}
			record := sink.one(t)
			if record.FailureStage != fault.wantStage {
				t.Fatalf("stage = %q, want %q", record.FailureStage, fault.wantStage)
			}
			if record.Totals.Calls != fault.wantCalls {
				t.Fatalf("calls = %d, want %d", record.Totals.Calls, fault.wantCalls)
			}
			if record.AnswerStored != nil {
				t.Fatalf("answer_stored = %v, want null: no row was written", *record.AnswerStored)
			}
			if fault.wantStage == stageSave && script.dispatches() != 1 {
				t.Fatalf("provider saw %d requests, want the paid call kept in the story", script.dispatches())
			}
		})
	}

	t.Run("primary cause survives an unwritable notice", func(t *testing.T) {
		// The save fails first; the failure notice that follows cannot be written
		// either. The record must keep the save as the cause and say delivery
		// failed, not quietly re-label the turn as a delivery problem.
		store, userID, sessionID := newFailingAppendStore(t, 2)
		script := newProviderScript(t, scriptCompletion("答案文本。", usageBody(12, 3, 15)))
		direct, err := responder.NewModelStrategy(domain.AnswerStrategyDirectChat, script.client, "回答。")
		if err != nil {
			t.Fatal(err)
		}
		assistant, err := responder.NewStrategyResponder(direct)
		if err != nil {
			t.Fatal(err)
		}
		sink := &sinkRecorder{}
		service := NewMessageService(store, assistant, WithCollectionAccess(openAccess{}),
			WithTaskMetrics(modelcall.NewPublisher(true, sink.write)))

		err = service.Stream(t.Context(), userID, sessionID, "问题", domain.KnowledgeOptions{},
			func(event StreamEvent) error {
				if event.Name == "message.failed" {
					return errors.New("connection dropped")
				}
				return nil
			})
		if err == nil {
			t.Fatal("a store that refused the answer let the turn report success")
		}
		record := sink.one(t)
		if record.FailureStage != stageSave {
			t.Fatalf("stage = %q, want the primary save failure kept, not the notice's own",
				record.FailureStage)
		}
		if record.Delivered == nil || *record.Delivered {
			t.Fatalf("delivered = %v, want false: the notice never left either", record.Delivered)
		}
		if record.ReasonCode != reasonConflict {
			t.Fatalf("reason = %q, want the store conflict behind the mapped error", record.ReasonCode)
		}
		if record.Totals.Calls != 1 {
			t.Fatalf("calls = %d, want the paid model call kept", record.Totals.Calls)
		}
	})
}
