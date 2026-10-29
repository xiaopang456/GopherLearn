package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/modelcall"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/responder"
)

// deliveryCallbackFault is one kind of refusal a frame writer can return, with the
// classification the record must carry.
type deliveryCallbackFault struct {
	name       string
	cause      error
	wantOut    modelcall.Outcome
	wantReason string
}

var deliveryCallbackFaults = []deliveryCallbackFault{
	{"plain refusal", errors.New("browser hung up"), modelcall.OutcomeCallbackFailed, reasonDeliveryFailure},
	{"bare cancellation", context.Canceled, modelcall.OutcomeCancelled, reasonCancelled},
	{"wrapped cancellation", fmt.Errorf("write failed: %w", context.Canceled), modelcall.OutcomeCancelled, reasonCancelled},
	{"deadline", context.DeadlineExceeded, modelcall.OutcomeTimeout, reasonTimeout},
	{"wrapped model timeout", fmt.Errorf("stalled: %w", model.ErrTimeout), modelcall.OutcomeTimeout, reasonTimeout},
}

func newStreamFixture(t *testing.T, usage string) (*metricsFixture, *providerScript) {
	t.Helper()
	script := newProviderScript(t, scriptCompletion("第一段文字。", usage))
	direct, err := responder.NewModelStrategy(domain.AnswerStrategyDirectChat, script.client, "回答。")
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := responder.NewStrategyResponder(direct)
	if err != nil {
		t.Fatal(err)
	}
	return newMetricsFixture(t, assistant, true), script
}

// TestStreamDeltaWriteFailureIsNotAModelFailure is the delivery-cause regression:
// the refused write travels back out through the model-call error path, and a
// record built from that error called it a model fault.
func TestStreamDeltaWriteFailureIsNotAModelFailure(t *testing.T) {
	for _, fault := range deliveryCallbackFaults {
		t.Run("message.delta/"+fault.name, func(t *testing.T) {
			fixture, script := newStreamFixture(t, usageBody(12, 3, 15))
			frames := 0
			outward := fixture.service.Stream(t.Context(), fixture.userID, fixture.sessionID, "问题",
				domain.KnowledgeOptions{}, func(StreamEvent) error {
					frames++
					if frames == 2 {
						// The first frame is message.started; the second is the
						// first text fragment, and that write is what fails.
						return fault.cause
					}
					return nil
				})
			if outward == nil {
				t.Fatal("an unwritable delta reported success")
			}

			record := fixture.sink.one(t)
			if record.FailureStage != stageDeliver {
				t.Fatalf("stage = %q, want %q rather than the model phase it passed through",
					record.FailureStage, stageDeliver)
			}
			if record.Outcome != fault.wantOut || record.ReasonCode != fault.wantReason {
				t.Fatalf("record = %s/%s, want %s/%s", record.Outcome, record.ReasonCode,
					fault.wantOut, fault.wantReason)
			}
			if record.Delivered == nil || *record.Delivered {
				t.Fatal("delivered must say the stream did not finish")
			}
			if record.FirstEventMillis == nil {
				t.Fatal("the started frame did leave the process, so it must be timed")
			}
			if record.FirstTextMillis != nil {
				t.Fatal("no text was ever delivered, so first_text must stay null")
			}
			// The provider answered and was billed: the call and its usage stay.
			if record.Totals.Calls != 1 || record.Calls[0].Usage.Total.Value != 15 {
				t.Fatalf("calls = %+v, want the completed provider call and its usage kept", record.Calls)
			}
			if record.Calls[0].Purpose != modelcall.PurposeDialogue {
				t.Fatalf("purpose = %s, want dialogue", record.Calls[0].Purpose)
			}
			if record.AnswerStored != nil {
				t.Fatalf("answer_stored = %v, want null: the abort happened before any write",
					*record.AnswerStored)
			}
			if script.dispatches() != 1 {
				t.Fatalf("provider saw %d requests, want the one call", script.dispatches())
			}
		})
	}
}

// TestStreamStepsTextDeltaWriteFailureIsNotAModelFailure: same rule on the step
// channel, where the answer arrives as agent.text.delta frames rather than
// message.delta. The turn is a real two-call agent run — a decide round that asks
// for the probe tool, then the answer round — so the refusal lands partway through
// the second provider call.
func TestStreamStepsTextDeltaWriteFailureIsNotAModelFailure(t *testing.T) {
	for _, fault := range deliveryCallbackFaults {
		t.Run("agent.text.delta/"+fault.name, func(t *testing.T) {
			script := newProviderScript(t,
				scriptToolCall("call-1", "probe_search", "{}", usageBody(20, 4, 24)),
				scriptCompletion("第一段答案文字。", usageBody(30, 6, 36)),
			)
			assistant, tool := newAgentResponder(t, script.client)
			fixture := newMetricsFixture(t, assistant, true)

			outward := fixture.service.StreamSteps(t.Context(), fixture.userID, fixture.agentID, "问题",
				domain.KnowledgeOptions{}, func(event StreamEvent) error {
					if event.Name == string(responder.TextDelta) {
						return fault.cause
					}
					return nil
				})
			if outward == nil {
				t.Fatal("an unwritable text fragment reported success")
			}
			record := fixture.sink.one(t)
			if record.FailureStage != stageDeliver {
				t.Fatalf("stage = %q, want the delivery phase, not the model phase it surfaced through",
					record.FailureStage)
			}
			if record.Outcome != fault.wantOut || record.ReasonCode != fault.wantReason {
				t.Fatalf("record = %s/%s, want %s/%s", record.Outcome, record.ReasonCode,
					fault.wantOut, fault.wantReason)
			}
			if record.FirstTextMillis != nil {
				t.Fatal("no text ever reached the learner, so first_text must stay null")
			}
			if record.FirstEventMillis == nil {
				t.Fatal("frames before the refusal did leave the process and must be timed")
			}
			// The provider was asked twice and billed for both rounds; a delivery
			// refusal cannot unspend the first call.
			if record.Totals.Calls != 2 || script.dispatches() != 2 {
				t.Fatalf("calls=%d dispatches=%d, want the two agent rounds kept",
					record.Totals.Calls, script.dispatches())
			}
			if record.Calls[1].Usage.Total.Value != 36 {
				t.Fatalf("answer round usage = %+v, want the reported 36 tokens kept",
					record.Calls[1].Usage)
			}
			if tool.executions() != 1 {
				t.Fatalf("tool runs = %d, want the probe executed once before the answer round",
					tool.executions())
			}
			if record.AnswerStored != nil || record.Delivered == nil || *record.Delivered {
				t.Fatalf("stored=%v delivered=%v, want nil and false", record.AnswerStored, record.Delivered)
			}
		})
	}
}

// TestFirstFrameFailureAndTerminalFrameFailureStayDeliveryFailures pins the two
// ends of the stream that R1 already distinguished, now with reason codes.
func TestFirstFrameFailureAndTerminalFrameFailureStayDeliveryFailures(t *testing.T) {
	t.Run("first frame", func(t *testing.T) {
		fixture, script := newStreamFixture(t, usageBody(9, 2, 11))
		outward := fixture.service.Stream(t.Context(), fixture.userID, fixture.sessionID, "问题",
			domain.KnowledgeOptions{}, func(StreamEvent) error { return errors.New("gone") })
		if outward == nil {
			t.Fatal("a refused first frame reported success")
		}
		record := fixture.sink.one(t)
		if record.FailureStage != stageDeliver || record.ReasonCode != reasonDeliveryFailure ||
			record.Outcome != modelcall.OutcomeCallbackFailed {
			t.Fatalf("record = %s/%s/%s, want deliver + delivery_failure + callback_failed",
				record.Outcome, record.FailureStage, record.ReasonCode)
		}
		if record.FirstEventMillis != nil || record.FirstTextMillis != nil {
			t.Fatal("nothing left the process, so no stream timing may be set")
		}
		if record.Totals.Calls != 0 || script.dispatches() != 0 {
			t.Fatalf("calls=%d dispatches=%d, want the provider never asked",
				record.Totals.Calls, script.dispatches())
		}
	})

	t.Run("terminal frame", func(t *testing.T) {
		fixture, _ := newStreamFixture(t, usageBody(9, 2, 11))
		outward := fixture.service.Stream(t.Context(), fixture.userID, fixture.sessionID, "问题",
			domain.KnowledgeOptions{}, func(event StreamEvent) error {
				if event.Name == "message.completed" {
					return errors.New("connection closed")
				}
				return nil
			})
		if outward == nil {
			t.Fatal("a refused completion frame reported success")
		}
		record := fixture.sink.one(t)
		if record.FailureStage != stageDeliver || record.ReasonCode != reasonDeliveryFailure {
			t.Fatalf("record = %s/%s, want the delivery phase named", record.FailureStage, record.ReasonCode)
		}
		if record.AnswerStored == nil || !*record.AnswerStored {
			t.Fatal("the row was written before the frame failed and must say so")
		}
		if record.Delivered == nil || *record.Delivered {
			t.Fatal("delivered must be false: the learner never got the confirmation")
		}
		if record.FirstTextMillis == nil {
			t.Fatal("text did reach the client and must be timed")
		}
	})
}

// TestPrimaryCauseSurvivesAnUnwritableNotice covers both primary failures: the
// delivery note that follows must never overwrite them.
func TestPrimaryCauseSurvivesAnUnwritableNotice(t *testing.T) {
	t.Run("provider outage then unwritable notice", func(t *testing.T) {
		script := newProviderScript(t, scriptStatus(http.StatusServiceUnavailable))
		direct, err := responder.NewModelStrategy(domain.AnswerStrategyDirectChat, script.client, "回答。")
		if err != nil {
			t.Fatal(err)
		}
		assistant, err := responder.NewStrategyResponder(direct)
		if err != nil {
			t.Fatal(err)
		}
		fixture := newMetricsFixture(t, assistant, true)
		outward := fixture.service.Stream(t.Context(), fixture.userID, fixture.sessionID, "问题",
			domain.KnowledgeOptions{}, func(event StreamEvent) error {
				if event.Name == "message.failed" {
					return errors.New("connection dropped")
				}
				return nil
			})
		if !errors.Is(outward, ErrInternal) {
			t.Fatalf("outward error = %v, want the unchanged internal mapping", outward)
		}
		record := fixture.sink.one(t)
		if record.FailureStage != stageModel || record.ReasonCode != reasonProviderStatus {
			t.Fatalf("record = %s/%s, want the provider outage kept as the primary cause",
				record.FailureStage, record.ReasonCode)
		}
		if record.Delivered == nil || *record.Delivered {
			t.Fatalf("delivered = %v, want false: the notice could not be written either", record.Delivered)
		}
	})

	t.Run("store fault then unwritable notice", func(t *testing.T) {
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
		outward := service.Stream(t.Context(), userID, sessionID, "问题", domain.KnowledgeOptions{},
			func(event StreamEvent) error {
				if event.Name == "message.failed" {
					return errors.New("connection dropped")
				}
				return nil
			})
		if outward == nil {
			t.Fatal("a store that refused the answer let the turn succeed")
		}
		record := sink.one(t)
		if record.FailureStage != stageSave || record.ReasonCode != reasonConflict {
			t.Fatalf("record = %s/%s, want the store conflict kept as the primary cause",
				record.FailureStage, record.ReasonCode)
		}
		if record.Delivered == nil || *record.Delivered {
			t.Fatal("delivered must still record that the notice never left")
		}
		if record.Totals.Calls != 1 {
			t.Fatalf("calls = %d, want the paid model call kept", record.Totals.Calls)
		}
	})
}

// TestRepositorySentinelsAreNotLumpedTogether is the classification regression:
// three different store refusals used to collapse into "conflict".
func TestRepositorySentinelsAreNotLumpedTogether(t *testing.T) {
	for _, testCase := range []struct {
		name string
		err  error
		want string
	}{
		{"service not found", ErrNotFound, reasonNotFound},
		{"store not found", repository.ErrNotFound, reasonNotFound},
		{"store not found wrapped", fmt.Errorf("read the session: %w", repository.ErrNotFound), reasonNotFound},
		{"service conflict", ErrConflict, reasonConflict},
		{"store conflict", repository.ErrConflict, reasonConflict},
		{"store conflict wrapped", fmt.Errorf("state moved: %w", repository.ErrConflict), reasonConflict},
		{
			name: "memory state conflict matches the conflict sentinel",
			err:  &repository.MemoryStateConflict{MemoryID: "mem-1", Status: domain.MemoryStatusActive},
			want: reasonConflict,
		},
		{"store invalid argument", repository.ErrInvalidArgument, reasonStoreInvalidArgument},
		{
			name: "store invalid argument wrapped",
			err:  fmt.Errorf("append: %w", repository.ErrInvalidArgument),
			want: reasonStoreInvalidArgument,
		},
		// The caller's own bad input must not borrow the store's code, or vice
		// versa: one is a client mistake, the other is this service breaking an
		// invariant.
		{"invalid request stays invalid_request", ErrInvalidRequest, reasonInvalidRequest},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := reasonFor(testCase.err); got != testCase.want {
				t.Fatalf("reason = %q, want %q", got, testCase.want)
			}
		})
	}
}

// failingReadStore injects a chosen store failure at a chosen call site, so a
// classification can be checked where it is actually mapped rather than in a unit
// table alone.
type failingReadStore struct {
	repository.Repository
	failGetSession error
}

func (s *failingReadStore) GetSession(ctx context.Context, userID, sessionID string) (*domain.Session, error) {
	if s.failGetSession != nil {
		return nil, s.failGetSession
	}
	return s.Repository.GetSession(ctx, userID, sessionID)
}

// TestStoreFaultClassificationSurvivesTheServiceMapping runs each store sentinel
// through Send, where mapRepositoryError decides what the caller sees. The record
// must keep the store's own reason while the outward error stays what it was.
func TestStoreFaultClassificationSurvivesTheServiceMapping(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		storeErr    error
		wantReason  string
		wantOutward error
	}{
		{"missing session", repository.ErrNotFound, reasonNotFound, ErrNotFound},
		{"conflicting session", repository.ErrConflict, reasonConflict, ErrConflict},
		// The one the mapping used to hide: a store refusing an invalid call shares
		// an outward sentinel with a learner's bad input, but not a cause.
		{"invalid store call", repository.ErrInvalidArgument, reasonStoreInvalidArgument, ErrInvalidRequest},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := t.Context()
			inner := repository.NewInMemoryRepository()
			user, err := inner.CreateUser(ctx, &domain.User{Email: "map@example.com", Username: "map"})
			if err != nil {
				t.Fatal(err)
			}
			session, err := inner.CreateSession(ctx, &domain.Session{UserID: user.ID, Mode: domain.SessionModeChat})
			if err != nil {
				t.Fatal(err)
			}
			script := newProviderScript(t, scriptCompletion("不该到达模型。", usageBody(1, 1, 2)))
			direct, err := responder.NewModelStrategy(domain.AnswerStrategyDirectChat, script.client, "回答。")
			if err != nil {
				t.Fatal(err)
			}
			assistant, err := responder.NewStrategyResponder(direct)
			if err != nil {
				t.Fatal(err)
			}
			sink := &sinkRecorder{}
			service := NewMessageService(&failingReadStore{Repository: inner, failGetSession: testCase.storeErr},
				assistant, WithCollectionAccess(openAccess{}),
				WithTaskMetrics(modelcall.NewPublisher(true, sink.write)))

			_, outward := service.Send(ctx, user.ID, session.ID, "问题", domain.KnowledgeOptions{})
			if outward == nil {
				t.Fatal("a store that refused the read let the turn succeed")
			}
			if !errors.Is(outward, testCase.wantOutward) {
				t.Fatalf("outward error = %v, want the unchanged %v mapping", outward, testCase.wantOutward)
			}
			record := sink.one(t)
			if record.ReasonCode != testCase.wantReason {
				t.Fatalf("reason = %q, want the store's own code %q", record.ReasonCode, testCase.wantReason)
			}
			if record.FailureStage != stageValidate {
				t.Fatalf("stage = %q, want the ownership read that failed", record.FailureStage)
			}
			if record.Outcome != modelcall.OutcomeOther {
				t.Fatalf("outcome = %s, want other: a store refusal is none of the model outcomes",
					record.Outcome)
			}
			if record.Totals.Calls != 0 || script.dispatches() != 0 {
				t.Fatalf("calls=%d dispatches=%d, want no model work after the read failed",
					record.Totals.Calls, script.dispatches())
			}
		})
	}
}
