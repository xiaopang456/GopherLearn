package conversation

import (
	"context"
	"errors"
	"fmt"
	"testing"

	sessiondao "GopherAI/dao/session"
	"GopherAI/model"
	agent "GopherAI/service/agent"
)

// The offline regressions for the environment-failure rule.
//
// The rule is a causal one: the run is the environment's when the FIRST
// failure of the compaction happened at the summary MODEL request, that
// request timed out, and the window carried no summary.
//
// Two things this file pins, because both went wrong once:
//
//   - The STAGE decides, not the wording. A store read that ran out of time
//     is the database's timeout, and a document that would not parse is the
//     model's answer; neither may be turned into the upstream's by a
//     timeout wording in the same message. Only a timeout whose stage is
//     confirmed to be the model request is the upstream's, and a timeout
//     whose stage could not be read is 待核.
//   - A summary that reached the store is a write. The provider answers
//     without an error when the compaction was never triggered and when the
//     same snapshot is read again, so its answer proves nothing.
//
// These tests run offline: no model call, no network.

// scriptedSummaryGenerator stands in for the product's generator.
type scriptedSummaryGenerator struct {
	err error
}

func (g *scriptedSummaryGenerator) GenerateSummary(context.Context, string, []model.Message) (agent.SummaryGeneration, error) {
	if g.err != nil {
		return agent.SummaryGeneration{}, g.err
	}
	return agent.SummaryGeneration{Content: "a summary"}, nil
}

// stagedSummaryProvider behaves like the product's own provider for the one
// case that matters here: the first call spends a model call and fails, and
// every call after it is refused by the failure cooldown.
type stagedSummaryProvider struct {
	inner agent.SummaryGenerator
	calls int
}

func (p *stagedSummaryProvider) SessionSummary(ctx context.Context, _ agent.RunContext) (agent.SummarySnapshot, error) {
	p.calls++
	if p.calls == 1 {
		_, err := p.inner.GenerateSummary(ctx, "", nil)
		return agent.SummarySnapshot{}, err
	}
	return agent.SummarySnapshot{}, agent.ErrSummaryCoolingDown
}

// cooldownOnlyProvider never spends a model call: the failure that caused the
// cooldown happened before this replay, so the cause is not observed here.
type cooldownOnlyProvider struct {
	calls int
}

func (p *cooldownOnlyProvider) SessionSummary(context.Context, agent.RunContext) (agent.SummarySnapshot, error) {
	p.calls++
	return agent.SummarySnapshot{}, agent.ErrSummaryCoolingDown
}

// readingProvider answers with a snapshot and never writes. This is what a
// turn that did not need to compact looks like, and what reading the same
// stored snapshot again looks like.
type readingProvider struct {
	snapshot agent.SummarySnapshot
}

func (p readingProvider) SessionSummary(context.Context, agent.RunContext) (agent.SummarySnapshot, error) {
	return p.snapshot, nil
}

// committingProvider writes the summary to the store once and then reads it
// back.
type committingProvider struct {
	repo  agent.SummaryRepository
	wrote bool
}

func (p *committingProvider) SessionSummary(ctx context.Context, input agent.RunContext) (agent.SummarySnapshot, error) {
	if !p.wrote {
		p.wrote = true
		err := p.repo.SaveSummary(ctx, &model.SessionSummary{
			UserID: input.UserID, SessionID: input.SessionID,
			Content: "a summary", CoveredMessageID: 7,
		}, 0)
		if err != nil {
			return agent.SummarySnapshot{}, err
		}
	}
	return agent.SummarySnapshot{Content: "a summary", CoveredMessageID: 7}, nil
}

// fakeSummaryStore is the store behind the counting repository.
type fakeSummaryStore struct {
	saved []*model.SessionSummary
	fail  error
}

func (s *fakeSummaryStore) GetSummary(context.Context, int64, string) (*model.SessionSummary, error) {
	if len(s.saved) == 0 {
		return nil, sessiondao.ErrSummaryNotFound
	}
	return s.saved[len(s.saved)-1], nil
}

func (s *fakeSummaryStore) SaveSummary(_ context.Context, summary *model.SessionSummary, _ uint64) error {
	if s.fail != nil {
		return s.fail
	}
	s.saved = append(s.saved, summary)
	return nil
}

func TestClassifySummaryFailureReadsTheStageBeforeTheCause(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		stage summaryFailureStage
		want  summaryFailureCause
	}{
		{
			name:  "a timeout of the summary model request",
			err:   contextDeadlineError("generate rolling summary: failed to create chat completion: Post \"http://127.0.0.1:57655/v1/chat/completions\": context deadline exceeded"),
			stage: summaryStageModelRequest,
			want:  summaryCauseUpstreamTimeout,
		},
		{
			name:  "a timeout of the store, not of the model",
			err:   contextDeadlineError("publish rolling summary: context deadline exceeded"),
			stage: summaryStageStorage,
			want:  summaryCauseStorage,
		},
		{
			name:  "a store read that ran out of time",
			err:   errors.New("read rolling summary: context deadline exceeded"),
			stage: summaryStageStorage,
			want:  summaryCauseStorage,
		},
		{
			name:  "a document that would not parse and mentions a timeout",
			err:   errors.New("decode rolling summary: context deadline exceeded"),
			stage: summaryStageDocument,
			want:  summaryCauseFormatError,
		},
		{
			name:  "a document the product rejects and mentions a timeout",
			err:   errors.New("rolling summary cited non-user or unavailable message 12: context deadline exceeded"),
			stage: summaryStageDocument,
			want:  summaryCauseFormatError,
		},
		{
			name:  "an output that was too long and mentions a timeout",
			err:   errors.New("rolling summary exceeds output token limit: context deadline exceeded"),
			stage: summaryStageBudget,
			want:  summaryCauseOutputOverLimit,
		},
		{
			name:  "the summarizer's own input budget",
			err:   errors.New("oldest unsummarized message exceeds summary input budget"),
			stage: summaryStageBudget,
			want:  summaryCauseProductOther,
		},
		{
			name:  "a timeout whose stage could not be read",
			err:   errors.New("context deadline exceeded"),
			stage: summaryStageUnknown,
			want:  summaryCauseUnknown,
		},
		{
			name:  "a model call that failed for another reason",
			err:   errors.New("generate rolling summary: upstream refused the request"),
			stage: summaryStageModelRequest,
			want:  summaryCauseProductOther,
		},
		{
			name:  "the cooldown, which is the failure before it",
			err:   agent.ErrSummaryCoolingDown,
			stage: summaryStageUnknown,
			want:  summaryCauseNone,
		},
		{
			name:  "a skipped stage, which is not a failure at all",
			err:   agent.ErrSummaryStageSkipped,
			stage: summaryStageUnknown,
			want:  summaryCauseNone,
		},
	}
	for _, one := range cases {
		t.Run(one.name, func(t *testing.T) {
			got := classifySummaryFailure(one.err)
			if got.Cause != one.want {
				t.Fatalf("cause = %q, want %q (stage %q, reason %q, text %q)", got.Cause, one.want, got.Stage, got.Reason, got.Text)
			}
			if got.Stage != one.stage {
				t.Fatalf("stage = %q, want %q (cause %q)", got.Stage, one.stage, got.Cause)
			}
		})
	}
}

// A timeout whose stage could not be read is not the upstream's: the driver
// cannot prove the summary model request is where it happened.
func TestClassifySummaryFailureDoesNotClaimAnUnconfirmedTimeout(t *testing.T) {
	failure := classifySummaryFailure(errors.New("context deadline exceeded"))
	if failure.Cause == summaryCauseUpstreamTimeout {
		t.Fatalf("a timeout of an unknown stage was claimed as the model's: %+v", failure)
	}
	if failure.Cause != summaryCauseUnknown {
		t.Fatalf("cause = %q, want %q", failure.Cause, summaryCauseUnknown)
	}
}

// The product's own reason is reported as it is; the driver's cause is the
// driver's own reading and does not overwrite it.
func TestClassifySummaryFailureAgreesWithTheProductsReason(t *testing.T) {
	err := contextDeadlineError("generate rolling summary: context deadline exceeded")
	if reason := agent.SummaryFailureReasonOf(err); reason != agent.SummaryFailureTimeout {
		t.Fatalf("the product's reason = %q, want %q", reason, agent.SummaryFailureTimeout)
	}
	failure := classifySummaryFailure(err)
	if failure.Reason != agent.SummaryFailureTimeout {
		t.Fatalf("the driver read the reason as %q, want %q", failure.Reason, agent.SummaryFailureTimeout)
	}
}

// The failure's text is kept short: a reader has to be able to tell two
// failures apart, not to read the whole request back.
func TestClassifySummaryFailureKeepsTheTextShort(t *testing.T) {
	err := fmt.Errorf("generate rolling summary: %s", fmt.Sprintf("%0*d", maxFailureText*2, 0))
	failure := classifySummaryFailure(err)
	if len(failure.Text) > maxFailureText {
		t.Fatalf("the failure text is %d bytes, want at most %d", len(failure.Text), maxFailureText)
	}
}

func TestClassifySummaryOutcomeTimeoutWithNoSummaryIsTheEnvironment(t *testing.T) {
	replay := rollingReplay{
		cooldowns:    1,
		hasFailure:   true,
		firstFailure: summaryFailure{Reason: agent.SummaryFailureTimeout, Stage: summaryStageModelRequest, Cause: summaryCauseUpstreamTimeout, Text: "context deadline exceeded"},
	}
	outcome := classifySummaryOutcome(LayeredArmTruncated, replay, 0)
	if outcome.EnvironmentFailure == "" {
		t.Fatalf("a timeout of the model request that left no summary behind is not marked as an environment failure: %+v", outcome)
	}
	if outcome.WriteFailure != "" {
		t.Fatalf("an upstream timeout is not a product write failure: %+v", outcome)
	}
	if outcome.Unknown != "" {
		t.Fatalf("the cause is known, so the run is not 待核: %+v", outcome)
	}
}

// The timeout is the environment's even when no cooldown ever followed it:
// the cooldown is what the provider does after a failure, and a rule that
// required one to have happened would miss every failure that was the last
// of its kind.
func TestClassifySummaryOutcomeTimeoutWithoutACooldownIsStillTheEnvironment(t *testing.T) {
	replay := rollingReplay{
		cooldowns:    0,
		hasFailure:   true,
		firstFailure: summaryFailure{Reason: agent.SummaryFailureTimeout, Stage: summaryStageModelRequest, Cause: summaryCauseUpstreamTimeout, Text: "context deadline exceeded"},
	}
	outcome := classifySummaryOutcome(LayeredArmTruncated, replay, 0)
	if outcome.EnvironmentFailure == "" {
		t.Fatalf("a timeout with no cooldown behind it was not marked: %+v", outcome)
	}
}

// A product-side failure that the cooldown followed is still the product's.
// Washing it into the environment's column would take the product's own
// failures off its sheet.
func TestClassifySummaryOutcomeProductFailureAfterCooldownIsNotTheEnvironment(t *testing.T) {
	cases := []struct {
		name  string
		cause summaryFailureCause
		stage summaryFailureStage
	}{
		{name: "an output that was too long", cause: summaryCauseOutputOverLimit, stage: summaryStageBudget},
		{name: "a body that would not parse", cause: summaryCauseFormatError, stage: summaryStageDocument},
		{name: "a storage failure", cause: summaryCauseStorage, stage: summaryStageStorage},
		{name: "the summarizer's own budget", cause: summaryCauseProductOther, stage: summaryStageBudget},
	}
	for _, one := range cases {
		t.Run(one.name, func(t *testing.T) {
			replay := rollingReplay{
				cooldowns:    7,
				hasFailure:   true,
				firstFailure: summaryFailure{Reason: agent.SummaryFailureLength, Stage: one.stage, Cause: one.cause, Text: "the failure"},
			}
			outcome := classifySummaryOutcome(LayeredArmTruncated, replay, 0)
			if outcome.EnvironmentFailure != "" {
				t.Fatalf("a product-side failure was marked as the environment's: %+v", outcome)
			}
			if outcome.WriteFailure == "" {
				t.Fatalf("a product-side failure was not reported as one: %+v", outcome)
			}
			if outcome.Unknown != "" {
				t.Fatalf("the cause is known, so the run is not 待核: %+v", outcome)
			}
		})
	}
}

// A timeout whose stage could not be read is 待核: it is neither the
// environment's nor the product's, and it is not a reason to exclude the run
// from the statistics.
func TestClassifySummaryOutcomeUnconfirmedTimeoutIsPendingVerification(t *testing.T) {
	replay := rollingReplay{
		hasFailure:   true,
		firstFailure: summaryFailure{Reason: agent.SummaryFailureTimeout, Stage: summaryStageUnknown, Cause: summaryCauseUnknown, Text: "context deadline exceeded"},
	}
	outcome := classifySummaryOutcome(LayeredArmTruncated, replay, 0)
	if outcome.EnvironmentFailure != "" {
		t.Fatalf("a timeout of an unknown stage was marked as an environment failure: %+v", outcome)
	}
	if outcome.WriteFailure != "" {
		t.Fatalf("a timeout of an unknown stage was marked as a product failure: %+v", outcome)
	}
	if outcome.Unknown == "" {
		t.Fatalf("a timeout of an unknown stage was not marked 待核: %+v", outcome)
	}
}

// A timeout that left a usable summary behind does not void the answer: the
// arm still answered from a compression, and the write phase's failure is a
// fact to report.
func TestClassifySummaryOutcomeTimeoutWithASummaryKeepsTheResult(t *testing.T) {
	replay := rollingReplay{
		cooldowns:    1,
		hasFailure:   true,
		firstFailure: summaryFailure{Reason: agent.SummaryFailureTimeout, Stage: summaryStageModelRequest, Cause: summaryCauseUpstreamTimeout, Text: "context deadline exceeded"},
	}
	outcome := classifySummaryOutcome(LayeredArmTruncated, replay, 128)
	if outcome.EnvironmentFailure != "" {
		t.Fatalf("a timeout that left a summary behind voided the answer: %+v", outcome)
	}
	if outcome.WriteFailure == "" {
		t.Fatalf("the write phase's failure was not reported: %+v", outcome)
	}
}

// A cooldown whose failure behind it was not observed is neither the
// environment's nor the product's: the honest reading is 待核, and it is not
// a reason to exclude the run from the statistics.
func TestClassifySummaryOutcomeCooldownWithoutARootCauseIsPendingVerification(t *testing.T) {
	replay := rollingReplay{cooldowns: 5}
	outcome := classifySummaryOutcome(LayeredArmTruncated, replay, 0)
	if outcome.EnvironmentFailure != "" {
		t.Fatalf("a cooldown without a root cause was marked as an environment failure: %+v", outcome)
	}
	if outcome.WriteFailure != "" {
		t.Fatalf("a cooldown without a root cause was marked as a product failure: %+v", outcome)
	}
	if outcome.Unknown == "" {
		t.Fatalf("a cooldown without a root cause was not marked 待核: %+v", outcome)
	}
}

// The layered arm runs no rolling summary, so none of this applies to it.
func TestClassifySummaryOutcomeLeavesTheLayeredArmAlone(t *testing.T) {
	replay := rollingReplay{
		cooldowns:    3,
		hasFailure:   true,
		firstFailure: summaryFailure{Reason: agent.SummaryFailureTimeout, Stage: summaryStageModelRequest, Cause: summaryCauseUpstreamTimeout, Text: "context deadline exceeded"},
	}
	outcome := classifySummaryOutcome(LayeredArmOn, replay, 0)
	if outcome != (summaryOutcome{}) {
		t.Fatalf("the layered arm was judged by the rolling summary's rule: %+v", outcome)
	}
}

// The replay records the FIRST real failure, because that is the one that
// caused the cooldown, and it does not let a later cooldown overwrite it.
func TestReplayRollingSummariesKeepsTheFirstRealFailure(t *testing.T) {
	generator := &scriptedSummaryGenerator{
		err: contextDeadlineError("generate rolling summary: failed to create chat completion: Post \"http://127.0.0.1:57655/v1/chat/completions\": context deadline exceeded"),
	}
	counting := &countingSummaryGenerator{inner: generator}
	provider := &stagedSummaryProvider{inner: counting}
	replay := rollingReplay{provider: provider}
	replayRollingSummaries(context.Background(), provider, agent.RunContext{UserID: 7001, SessionID: "s"}, 3, &replay, counting)

	if !replay.hasFailure {
		t.Fatalf("the first failure was not recorded: %+v", replay)
	}
	if replay.firstFailure.Cause != summaryCauseUpstreamTimeout {
		t.Fatalf("cause = %q, want %q", replay.firstFailure.Cause, summaryCauseUpstreamTimeout)
	}
	if replay.firstFailure.Stage != summaryStageModelRequest {
		t.Fatalf("stage = %q, want %q", replay.firstFailure.Stage, summaryStageModelRequest)
	}
	if replay.cooldowns != 2 {
		t.Fatalf("cooldowns = %d, want 2", replay.cooldowns)
	}
	// The call count is not a success count: one model call was made and it
	// produced nothing.
	if replay.compactions != 1 {
		t.Fatalf("compactions = %d, want 1", replay.compactions)
	}
	outcome := classifySummaryOutcome(LayeredArmTruncated, replay, 0)
	if outcome.EnvironmentFailure == "" {
		t.Fatalf("the run was not marked as an environment failure: %+v", outcome)
	}
}

// A replay that only ever met the cooldown never saw the failure behind it.
func TestReplayRollingSummariesWithOnlyCooldownsHasNoCause(t *testing.T) {
	provider := &cooldownOnlyProvider{}
	replay := rollingReplay{provider: provider}
	replayRollingSummaries(context.Background(), provider, agent.RunContext{UserID: 7001, SessionID: "s"}, 4, &replay, nil)

	if replay.hasFailure {
		t.Fatalf("a cooldown was read as a failure: %+v", replay)
	}
	if replay.cooldowns != 4 {
		t.Fatalf("cooldowns = %d, want 4", replay.cooldowns)
	}
	outcome := classifySummaryOutcome(LayeredArmTruncated, replay, 0)
	if outcome.EnvironmentFailure != "" {
		t.Fatalf("a cooldown without a root cause was marked as an environment failure: %+v", outcome)
	}
	if outcome.Unknown == "" {
		t.Fatalf("a cooldown without a root cause was not marked 待核: %+v", outcome)
	}
}

// A summary is a write only when it reached the store.
//
// The provider answers without an error when the compaction was never
// triggered, when the stored summary was read, and when the same snapshot is
// read again. Counting those would say the arm held a compression it never
// held.
func TestSummaryWritesCountOnlyRealCommits(t *testing.T) {
	t.Run("a turn that never needed to compact", func(t *testing.T) {
		store := &fakeSummaryStore{}
		counting := &countingSummaryRepository{inner: store}
		provider := readingProvider{snapshot: agent.SummarySnapshot{Content: "the stored summary", CoveredMessageID: 3}}
		replay := rollingReplay{provider: provider}
		replayRollingSummaries(context.Background(), provider, agent.RunContext{UserID: 7001, SessionID: "s"}, 5, &replay, nil)

		if got := counting.writes.Load(); got != 0 {
			t.Fatalf("writes = %d, want 0: no compaction reached the store", got)
		}
		if len(store.saved) != 0 {
			t.Fatalf("the store holds %d summaries, want 0", len(store.saved))
		}
	})

	t.Run("the same snapshot read again", func(t *testing.T) {
		store := &fakeSummaryStore{saved: []*model.SessionSummary{{Content: "the stored summary", CoveredMessageID: 3}}}
		counting := &countingSummaryRepository{inner: store}
		provider := readingProvider{snapshot: agent.SummarySnapshot{Content: "the stored summary", CoveredMessageID: 3}}
		replay := rollingReplay{provider: provider}
		replayRollingSummaries(context.Background(), provider, agent.RunContext{UserID: 7001, SessionID: "s"}, 4, &replay, nil)

		if got := counting.writes.Load(); got != 0 {
			t.Fatalf("writes = %d, want 0: reading a snapshot is not a write", got)
		}
		if len(store.saved) != 1 {
			t.Fatalf("the store holds %d summaries, want 1 (the one that was already there)", len(store.saved))
		}
	})

	t.Run("a compaction that really reached the store", func(t *testing.T) {
		store := &fakeSummaryStore{}
		counting := &countingSummaryRepository{inner: store}
		provider := &committingProvider{repo: counting}
		replay := rollingReplay{provider: provider}
		replayRollingSummaries(context.Background(), provider, agent.RunContext{UserID: 7001, SessionID: "s"}, 3, &replay, nil)

		if got := counting.writes.Load(); got != 1 {
			t.Fatalf("writes = %d, want 1: one summary reached the store", got)
		}
		if len(store.saved) != 1 {
			t.Fatalf("the store holds %d summaries, want 1", len(store.saved))
		}
	})
}

// A save that failed is not a write.
func TestSummaryWritesDoNotCountAFailedSave(t *testing.T) {
	store := &fakeSummaryStore{fail: errors.New("sql: database is closed")}
	counting := &countingSummaryRepository{inner: store}
	provider := &committingProvider{repo: counting}
	replay := rollingReplay{provider: provider}
	replayRollingSummaries(context.Background(), provider, agent.RunContext{UserID: 7001, SessionID: "s"}, 2, &replay, nil)

	if got := counting.writes.Load(); got != 0 {
		t.Fatalf("writes = %d, want 0: the summary never reached the store", got)
	}
}
