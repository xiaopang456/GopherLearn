package summary

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/responder"
)

// The input bound a refresh has to respect is not a bound on the new material alone, and
// it is not an approximation the planner keeps to itself. A summary is written by reading
// the previous summary, so that text and the framing around it are part of the request,
// and the planner and the sender price the same assembled request with one function.

func turnMaterial(count, runes int) []responder.Turn {
	material := make([]responder.Turn, 0, count*2)
	for index := 1; index <= count; index++ {
		body := strings.Repeat("记", runes)
		material = append(material,
			responder.Turn{Role: roleUser, Content: body},
			responder.Turn{Role: roleAssistant, Content: body},
		)
	}
	return material
}

func largeConversation(pairs, runes int) []domain.Message {
	messages := make([]domain.Message, 0, pairs*2)
	for index := 1; index <= pairs; index++ {
		body := strings.Repeat("记", runes)
		messages = append(messages,
			stored("q"+strconv.Itoa(index), roleUser, body+"问"),
			stored("a"+strconv.Itoa(index), roleAssistant, body+"答"),
		)
	}
	return messages
}

func TestRequestCostPricesBothMessagesAndTheFraming(t *testing.T) {
	empty := requestCost("", nil, nil, nil)
	if empty <= 0 {
		t.Fatalf("an empty summarizing request costs %d, want two messages plus their framing", empty)
	}
	withPrevious := requestCost(strings.Repeat("旧", 500), nil, nil, nil)
	if withPrevious <= empty {
		t.Fatalf("a previous summary cost nothing extra: %d vs %d", withPrevious, empty)
	}
	withMaterial := requestCost("", turnMaterial(1, 100), nil, nil)
	if withMaterial <= empty {
		t.Fatalf("one exchange cost nothing extra: %d vs %d", withMaterial, empty)
	}
	both := requestCost(strings.Repeat("旧", 500), turnMaterial(1, 100), nil, nil)
	if both <= withPrevious || both <= withMaterial {
		t.Fatalf("adding material to a previous summary was free: %d, %d, %d",
			withPrevious, withMaterial, both)
	}
}

func TestBuildCompressesLessWhenThePreviousSummaryIsLarge(t *testing.T) {
	// The schema-v2 generation policy is longer than the old prose prompt, so the
	// cap here prices the real frame: the test's claim is about the previous
	// summary's share of the bound, not about the prompt's.
	policy := Policy{TriggerMessages: 4, KeepRecentMessages: 2,
		MaxInputTokens: 4000, MaxInputRunes: 1_000_000, MaxInputMessages: 100}.Normalize()
	messages := largeConversation(6, 300)
	small := Build(messages, "", strings.Repeat("小", 10), policy, MigratedControls())
	large := Build(messages, "", strings.Repeat("大", 2000), policy, MigratedControls())
	if small.Skipped != "" || large.Skipped != "" {
		t.Fatalf("plans skipped: small=%q large=%q", small.Skipped, large.Skipped)
	}
	if small.CompressedMessages < 4 {
		t.Fatalf("the small-summary plan compressed %d messages, want several whole pairs",
			small.CompressedMessages)
	}
	if large.CompressedMessages >= small.CompressedMessages {
		t.Fatalf("a 2000-character previous summary still compressed %d messages, the same as a"+
			" 10-character one (%d): the previous text is not part of the input bound",
			large.CompressedMessages, small.CompressedMessages)
	}
	if large.CompressedMessages%2 != 0 {
		t.Fatalf("the trimmed set holds %d messages, which is not whole exchanges", large.CompressedMessages)
	}
}

func TestBuildNamesAnInputThatCannotHoldTheOldestExchange(t *testing.T) {
	policy := Policy{TriggerMessages: 4, KeepRecentMessages: 2,
		MaxInputTokens: 300, MaxInputRunes: 1_000_000, MaxInputMessages: 100}.Normalize()
	plan := Build(largeConversation(6, 300), "", "", policy, MigratedControls())
	if plan.Skipped != SkipInputTooLarge {
		t.Fatalf("plan = %+v, want the session told its oldest exchange alone will not fit", plan)
	}
}

// TestPlannerAndSenderAgreeOnTheBoundary is the reason a plan cannot be trusted on its
// own: the set the planner selects must be the set the sender is willing to send, priced
// once. Each fixture leaves a different amount of room for material - a long previous
// summary, none at all, a short one - and in every case the exchanges the summarizer
// actually sent are exactly the ones the plan chose, at the same measured cost.
func TestPlannerAndSenderAgreeOnTheBoundary(t *testing.T) {
	cases := []struct {
		name     string
		previous string
	}{
		{"a long previous summary", strings.Repeat("旧", 1200)},
		{"no previous summary", ""},
		{"a short previous summary", strings.Repeat("短", 30)},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			policy := Policy{TriggerMessages: 2, KeepRecentMessages: 2,
				MaxInputTokens: 3200, MaxInputRunes: 1_000_000, MaxInputMessages: 100}.Normalize()
			plan := Build(largeConversation(8, 120), "", testCase.previous, policy, MigratedControls())
			if plan.Skipped != "" {
				t.Fatalf("plan skipped: %q", plan.Skipped)
			}
			if plan.CompressedMessages < 2 {
				t.Fatalf("the plan compressed %d messages, want the fixture to leave room for whole pairs",
					plan.CompressedMessages)
			}
			planned := requestCost(testCase.previous, plan.Compress, plan.CompressIDs, plan.CompressSources)
			if planned > policy.MaxInputTokens {
				t.Fatalf("the planner chose a set costing %d against a cap of %d",
					planned, policy.MaxInputTokens)
			}
			chat := &recordingModel{replies: []string{summaryReply("复习 AOF 与 RDB 的取舍")}}
			summarizer, err := NewModelSummarizer(chat, policy)
			if err != nil {
				t.Fatal(err)
			}
			result, err := summarizer.Summarize(context.Background(), Request{
				Previous: testCase.previous, Turns: plan.Compress, IDs: plan.CompressIDs,
				Sources: plan.CompressSources, Controls: MigratedControls(),
			})
			if err != nil {
				t.Fatal(err)
			}
			sent := chat.requests[0]
			if got := strings.Count(sent.Messages[1].Content, "role=assistant_sources_only"); got != len(plan.Compress)/2 {
				t.Fatalf("the plan chose %d exchanges and the sender sent %d: they do not measure alike",
					len(plan.Compress)/2, got)
			}
			if result.InputTokens != planned {
				t.Fatalf("the receipt says %d and the ruler says %d", result.InputTokens, planned)
			}
			// One token over the cap must not be sendable: the boundary is the cap, not
			// the cap plus whatever a looser estimate let through.
			if planned+1 > policy.MaxInputTokens && result.InputTokens <= policy.MaxInputTokens {
				t.Fatal("a request priced exactly at the cap was reported as fitting twice over")
			}
		})
	}
}

// A payload the trim cannot rescue is refused rather than sent: the bound exists to stop
// one refresh from growing without limit as summaries stack up, and a bound that gives way
// exactly when it is needed is not a bound.
func TestSummarizerRefusesAPayloadThatCannotFit(t *testing.T) {
	policy := Policy{MaxInputTokens: 900, MaxInputMessages: 100, MaxInputRunes: 1_000_000}.Normalize()
	chat := &recordingModel{replies: []string{summaryReply("不该发出")}}
	summarizer, err := NewModelSummarizer(chat, policy)
	if err != nil {
		t.Fatal(err)
	}
	_, err = summarizer.Summarize(context.Background(), Request{
		Previous: strings.Repeat("旧", 2000),
		Turns:    turnMaterial(1, 10),
		Controls: MigratedControls(),
	})
	if err == nil || !strings.Contains(err.Error(), ErrInputBudgetExceeded.Error()) {
		t.Fatalf("err = %v, want the input budget named", err)
	}
	if len(chat.requests) != 0 {
		t.Fatal("an over-cap summarizing request was sent to the model anyway")
	}
}

// The same oversized previous summary must still be accepted by a legacy refresh: the
// token bound was never part of that strategy's contract, and adopting it for chat would
// change when a chat turn can fail to summarize.
func TestLegacyControlsLeaveTheTokenBoundAlone(t *testing.T) {
	policy := Policy{MaxInputTokens: 900, MaxInputMessages: 100, MaxInputRunes: 1_000_000}.Normalize()
	chat := &recordingModel{replies: []string{summaryReply("旧摘要延续")}}
	summarizer, err := NewModelSummarizer(chat, policy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := summarizer.Summarize(context.Background(), Request{
		Previous: strings.Repeat("旧", 2000),
		Turns:    turnMaterial(1, 10),
	}); err != nil {
		t.Fatalf("a legacy refresh was refused by a bound it never had: %v", err)
	}
	if len(chat.requests) != 1 {
		t.Fatalf("%d calls, want the legacy path to send what it always sent", len(chat.requests))
	}
}

func TestLegacyControlsNeverTriggerOnTokens(t *testing.T) {
	policy := Policy{TriggerMessages: 12, KeepRecentMessages: 8, TriggerTokens: 100}.Normalize()
	// Ten messages is below the count and far past the token figure: the only honest
	// reading of a legacy turn is "not due".
	messages := largeConversation(5, 900)
	legacy := Build(messages, "", "", policy, LegacyControls())
	if legacy.Skipped != SkipBelowThreshold {
		t.Fatalf("legacy plan = %+v, want below the message threshold", legacy)
	}
	if legacy.UncoveredTokens < policy.TriggerTokens {
		t.Fatalf("the fixture is not over the token figure at all: %d", legacy.UncoveredTokens)
	}
	migrated := Build(messages, "", "", policy, MigratedControls())
	if migrated.Skipped != "" || migrated.TriggeredBy != "tokens" {
		t.Fatalf("migrated plan = %+v, want the token threshold to have made it due", migrated)
	}
}

func TestPolicyStageBudgetNeverExceedsItsOwnTimeout(t *testing.T) {
	policy := Policy{Timeout: 2 * time.Second, StageBudget: time.Minute}.Normalize()
	if policy.StageBudget > policy.Timeout {
		t.Fatalf("stage budget %v outlives the %v call timeout it must fit inside",
			policy.StageBudget, policy.Timeout)
	}
	normalized := (Policy{}).Normalize()
	if normalized.MaxInputTokens != DefaultMaxInputTokens ||
		normalized.TriggerTokens != DefaultTriggerTokens ||
		normalized.FailureCooldown != DefaultFailureCooldown ||
		normalized.StageBudget != DefaultSummaryStageBudget ||
		normalized.MaxDeadlineShare != DefaultMaxDeadlineShare {
		t.Fatalf("an unset policy lost the documented defaults: %+v", normalized)
	}
	if legacy := LegacyControls(); legacy.TokenTrigger || legacy.TokenInputCap ||
		legacy.StageDeadline || legacy.FailureCooldown {
		t.Fatal("the legacy control set is not empty, so a forgotten argument would migrate behaviour")
	}
}
