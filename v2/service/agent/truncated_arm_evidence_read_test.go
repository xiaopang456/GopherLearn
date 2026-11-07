package agent

import (
	"context"
	"slices"
	"strings"
	"testing"

	"GopherAI/common/memoryrecall"
	"github.com/cloudwego/eino/schema"
)

// The two arms of the layered-summary comparison compress the conversation
// differently: the applied arm puts the board and the summary segments in the
// window, the off arm puts the rolling summary there. Both compressions carry
// the ids of the messages they were compressed from, and those ids are the
// model's only way back to the learner's own words.
//
// Whether that read-back exists is the read mode's decision, and the mode -
// not the compression - decides it: the product does not widen a mode whose
// published measurements were taken before the widening. So the same window
// shows the rolling summary in every mode and authorises its ids in only some
// of them. The fact is pinned here because a measurement that reads an empty
// authorisation set as "the window carried nothing" would be reading the
// mode's rule as the window's content.

// The summary the off arm's window carries is authorised only where the mode
// reads evidence back, and it is in the window either way.
//
// The second half matters as much as the first: a mode that cannot read the
// summary back still gets the summary, so the compression is never the thing
// that is missing. Only the way back to the learner's own words is.
func TestTheSummaryTheWindowShowsIsAuthorisedOnlyWhereTheModeReadsEvidenceBack(t *testing.T) {
	snapshot := SummarySnapshot{
		Content:          summaryDocumentJSON(t, "把 RPC 拆包改成方案 B", map[string][]uint{"决定用长度字段": {11}}),
		CoveredMessageID: 40, Version: 3,
	}
	for _, test := range []struct {
		name       string
		mode       memoryrecall.ReadMode
		wantGrants []uint
	}{
		{"the shipped default authorises nothing", memoryrecall.ModeLegacyV2, nil},
		{"the source-priority v2 authorises the summary's ids", memoryrecall.ModeAgentMemorySourcePriorityV2, []uint{11}},
		{"the source-priority v3 authorises the summary's ids", memoryrecall.ModeAgentMemorySourcePriorityV3, []uint{11}},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := summaryReadContextConfig()
			config.MemoryReadMode = test.mode
			window := buildWindow(t, config, snapshot, summaryRun())

			if window.Budget.Summary == 0 {
				t.Fatal("the summary did not reach the window")
			}
			if got := grantedMessageIDs(window.SummaryHistoryGrants); !slices.Equal(got, test.wantGrants) {
				t.Fatalf("summary grants=%v want %v", got, test.wantGrants)
			}
			// The set the runner hands the read-back tool is the merged one, so
			// this is the set the tool's allow-list is built from.
			run := summaryRun()
			set := NewHistoryGrantSet(run, MergeHistoryMessageGrants(window.EpisodeHistoryGrants,
				window.SummaryHistoryGrants, window.SummarySegmentHistoryGrants))
			for _, id := range test.wantGrants {
				if !set.Covers(id, run.SessionID) {
					t.Fatalf("the summary's id %d is not readable", id)
				}
				// The reference space is what an answer's number is resolved
				// against, so an id that is readable but not referenceable
				// would still lose the answer that points at it.
				if !slices.Contains(set.References(), int(id)) {
					t.Fatalf("the summary's id %d is not a reference the answer may point at", id)
				}
			}
			if len(test.wantGrants) == 0 && set.Len() != 0 {
				t.Fatalf("the mode authorised %d messages", set.Len())
			}
		})
	}
}

// rollingSummaryWindowBuilder is the off arm's window: the compression in it
// is the rolling summary, and the ids that compression carries are the ones
// the model may point at and read back.
type rollingSummaryWindowBuilder struct {
	MinimalContextBuilder
	summary int
	grants  []uint
}

func (b rollingSummaryWindowBuilder) Build(ctx context.Context, input RunContext) (ContextWindow, error) {
	window, err := b.MinimalContextBuilder.Build(ctx, input)
	if err != nil {
		return ContextWindow{}, err
	}
	window.Budget.Summary = b.summary
	window.Budget.Assembled += b.summary
	for _, id := range b.grants {
		window.SummaryHistoryGrants = append(window.SummaryHistoryGrants,
			HistoryMessageGrant{MessageID: id, SourceKind: HistorySourceSummary,
				SourceSessionID: input.SessionID, SummaryVersion: 2, SummaryWatermark: 1000})
	}
	return window, nil
}

// A conversation reference is read the same way in both arms.
//
// The two arms hand the model different cards - the off arm's rolling summary
// and the applied arm's summary segments - but the citation check does not
// know which card it is looking at. A number in brackets is a reference in
// both, it resolves against the ids this turn may read in both, and a number
// that resolves to nothing is refused in both.
//
// The "message_ids=" form is the other half. The prompt asks for that form,
// and whether the id is written in brackets decides what the check sees: a
// bare id is not a citation at all and is never refused as one, while a
// bracketed id is a citation and has to resolve.
func TestAConversationReferenceIsReadTheSameWayInBothArms(t *testing.T) {
	const bracketed = "是「芦青」——这是之前定下的固定标题（message_ids=[63]）。"
	const bare = "是「芦青」——这是之前定下的固定标题（message_ids=63）。"
	for _, test := range []struct {
		name        string
		draft       string
		builder     ContextBuilder
		wantRefusal bool
	}{
		{
			name:    "the off arm's granted id resolves",
			draft:   bracketed,
			builder: rollingSummaryWindowBuilder{summary: 1104, grants: []uint{63}},
		},
		{
			name:    "the applied arm's granted id resolves",
			draft:   bracketed,
			builder: conversationWindowBuilder{summary: 1104, grants: []uint{63}},
		},
		{
			name:        "an id the off arm never granted is refused",
			draft:       bracketed,
			builder:     rollingSummaryWindowBuilder{summary: 1104},
			wantRefusal: true,
		},
		{
			name:        "an id the applied arm never granted is refused",
			draft:       bracketed,
			builder:     conversationWindowBuilder{summary: 1104},
			wantRefusal: true,
		},
		{
			name:    "a bare id is not a citation in the off arm",
			draft:   bare,
			builder: rollingSummaryWindowBuilder{summary: 1104},
		},
		{
			name:    "a bare id is not a citation in the applied arm",
			draft:   bare,
			builder: conversationWindowBuilder{summary: 1104},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
				return schema.AssistantMessage(test.draft, nil), nil
			}}
			runner, err := NewRunner(fake, StaticToolRegistry{}, newMemoryStepRepository(),
				test.builder, DefaultRunnerConfig())
			if err != nil {
				t.Fatal(err)
			}
			result, err := runner.Run(context.Background(), testRunContext(), DiscardEvents)
			if err != nil {
				t.Fatal(err)
			}
			if test.wantRefusal {
				if result.Answer != UnsupportedGroundingAnswer {
					t.Fatalf("answer = %q, want the refusal", result.Answer)
				}
				if result.GroundingRefusal == nil || result.GroundingRefusal.Rule != GroundingRuleInvalidCitation {
					t.Fatalf("refusal = %+v, want the invalid citation recorded", result.GroundingRefusal)
				}
				return
			}
			if result.GroundingRefusal != nil {
				t.Fatalf("refusal = %+v, want none", result.GroundingRefusal)
			}
			if !strings.Contains(result.Answer, "芦青") {
				t.Fatalf("answer = %q, want the grounded answer", result.Answer)
			}
		})
	}
}
