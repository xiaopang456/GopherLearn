package conversation

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"GopherAI/model"
	agent "GopherAI/service/agent"
)

// The offline regressions for the environment-failure rule.
//
// The rule is a causal one: the run is the environment's when the FIRST
// failure of the compaction was the upstream's, and the window carried no
// summary. A cooldown alone says nothing, because the same cooldown follows
// a timeout, an output that was too long, a body that would not parse and a
// storage failure. These tests pin each of those apart, offline, without a
// model call.

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

func TestClassifySummaryFailureReadsTheCause(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want summaryFailureCause
	}{
		{
			name: "an upstream timeout whose chain survived",
			err:  contextDeadlineError("Post \"http://127.0.0.1:57655/v1/chat/completions\": context deadline exceeded"),
			want: summaryCauseUpstreamTimeout,
		},
		{
			name: "an upstream timeout whose chain was flattened into a string",
			err: errors.New("rolling summary replay: generate rolling summary: failed to create chat completion: " +
				"Post \"http://127.0.0.1:57655/v1/chat/completions\": context deadline exceeded"),
			want: summaryCauseUpstreamTimeout,
		},
		{
			name: "an output that was too long",
			err:  errors.New("rolling summary exceeds output token limit"),
			want: summaryCauseOutputOverLimit,
		},
		{
			name: "a body that would not parse",
			err:  errors.New("decode rolling summary: unexpected end of JSON input"),
			want: summaryCauseFormatError,
		},
		{
			name: "the summarizer's own input budget",
			err:  errors.New("oldest unsummarized message exceeds summary input budget"),
			want: summaryCauseProductOther,
		},
		{
			name: "a storage failure",
			err:  errors.New("save rolling summary: sql: database is closed"),
			want: summaryCauseStorage,
		},
		{
			name: "the cooldown, which is the failure before it",
			err:  agent.ErrSummaryCoolingDown,
			want: summaryCauseNone,
		},
		{
			name: "a skipped stage, which is not a failure at all",
			err:  agent.ErrSummaryStageSkipped,
			want: summaryCauseNone,
		},
	}
	for _, one := range cases {
		t.Run(one.name, func(t *testing.T) {
			got := classifySummaryFailure(one.err)
			if got.Cause != one.want {
				t.Fatalf("cause = %q, want %q (reason %q, text %q)", got.Cause, one.want, got.Reason, got.Text)
			}
		})
	}
}

// A storage failure and a parse failure are both the product's, and both must
// stay the product's: neither is the upstream's timeout. They are also
// different from each other, and the driver has to say which is which.
func TestClassifySummaryFailureKeepsProductFailuresApartFromTimeouts(t *testing.T) {
	for _, text := range []string{
		"save rolling summary: sql: database is closed",
		"decode rolling summary: unexpected end of JSON input",
		"rolling summary exceeds output token limit",
	} {
		got := classifySummaryFailure(errors.New(text))
		if got.Cause == summaryCauseUpstreamTimeout {
			t.Fatalf("%q was read as an upstream timeout: %+v", text, got)
		}
	}
	storage := classifySummaryFailure(errors.New("save rolling summary: sql: database is closed"))
	format := classifySummaryFailure(errors.New("decode rolling summary: unexpected end of JSON input"))
	if storage.Cause == format.Cause {
		t.Fatalf("a storage failure and a parse failure were read as the same cause: %+v and %+v", storage, format)
	}
}

func TestClassifySummaryOutcomeTimeoutWithNoSummaryIsTheEnvironment(t *testing.T) {
	replay := rollingReplay{
		cooldowns:    1,
		hasFailure:   true,
		firstFailure: summaryFailure{Reason: agent.SummaryFailureTimeout, Cause: summaryCauseUpstreamTimeout, Text: "context deadline exceeded"},
	}
	outcome := classifySummaryOutcome(LayeredArmTruncated, replay, 0)
	if outcome.EnvironmentFailure == "" {
		t.Fatalf("a timeout that left no summary behind is not marked as an environment failure: %+v", outcome)
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
		firstFailure: summaryFailure{Reason: agent.SummaryFailureTimeout, Cause: summaryCauseUpstreamTimeout, Text: "context deadline exceeded"},
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
	}{
		{name: "an output that was too long", cause: summaryCauseOutputOverLimit},
		{name: "a body that would not parse", cause: summaryCauseFormatError},
		{name: "a storage failure", cause: summaryCauseProductOther},
	}
	for _, one := range cases {
		t.Run(one.name, func(t *testing.T) {
			replay := rollingReplay{
				cooldowns:    7,
				hasFailure:   true,
				firstFailure: summaryFailure{Reason: agent.SummaryFailureLength, Cause: one.cause, Text: "the failure"},
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

// A timeout that left a usable summary behind does not void the answer: the
// arm still answered from a compression, and the write phase's failure is a
// fact to report.
func TestClassifySummaryOutcomeTimeoutWithASummaryKeepsTheResult(t *testing.T) {
	replay := rollingReplay{
		cooldowns:    1,
		hasFailure:   true,
		firstFailure: summaryFailure{Reason: agent.SummaryFailureTimeout, Cause: summaryCauseUpstreamTimeout, Text: "context deadline exceeded"},
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
		firstFailure: summaryFailure{Reason: agent.SummaryFailureTimeout, Cause: summaryCauseUpstreamTimeout, Text: "context deadline exceeded"},
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
		err: contextDeadlineError("Post \"http://127.0.0.1:57655/v1/chat/completions\": context deadline exceeded"),
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
	if replay.cooldowns != 2 {
		t.Fatalf("cooldowns = %d, want 2", replay.cooldowns)
	}
	// The call count is not a success count: one model call was made and it
	// produced nothing.
	if replay.compactions != 1 {
		t.Fatalf("compactions = %d, want 1", replay.compactions)
	}
	if replay.writes != 0 {
		t.Fatalf("writes = %d, want 0", replay.writes)
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

// A successful compaction is a write, not only a call.
func TestReplayRollingSummariesCountsWritesSeparatelyFromCalls(t *testing.T) {
	provider := &writeOnlyProvider{}
	counting := &countingSummaryGenerator{inner: &scriptedSummaryGenerator{}}
	replay := rollingReplay{provider: provider}
	replayRollingSummaries(context.Background(), provider, agent.RunContext{UserID: 7001, SessionID: "s"}, 2, &replay, counting)

	if replay.writes != 2 {
		t.Fatalf("writes = %d, want 2", replay.writes)
	}
	if len(replay.degraded) != 0 {
		t.Fatalf("a successful replay reported degradations: %v", replay.degraded)
	}
}

// writeOnlyProvider succeeds without spending a model call.
type writeOnlyProvider struct{ calls int }

func (p *writeOnlyProvider) SessionSummary(context.Context, agent.RunContext) (agent.SummarySnapshot, error) {
	p.calls++
	return agent.SummarySnapshot{Content: "a summary"}, nil
}

// The product's own reason for a timeout is read from the error chain, and
// the driver must not disagree with it.
func TestClassifySummaryFailureAgreesWithTheProductsReason(t *testing.T) {
	err := contextDeadlineError("context deadline exceeded")
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
