package agent

import (
	"context"
	"strings"
	"testing"

	"GopherAI/common/rag"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// The knowledge citation check and the conversation evidence the product
// keeps in front of the model meet in one place: a draft that carries
// "[1]".
//
// "[1]" is the document citation syntax - the prompt teaches it - so the
// check reads it as a claim about the learner's notes. When the turn showed
// documents, the claim is checkable and a number that resolves to nothing is
// refused.
//
// When the turn never consulted the notes, the same number can point at the
// conversation instead: the whiteboard, the summary segments, the rolling
// summary and the read-back tool all carry message ids, and the model may
// cite those ids. But only those ids. A number that resolves to nothing is
// a pointer at nothing whether a summary is in the window or not, and an
// unrelated summary does not launder an invented reference.
//
// The tests below are the sides of that rule. None passes alone: the first
// fails if the escape is missing, the second fails if it is too wide.

// conversationWindowBuilder is a window that carries compressed conversation
// evidence and the message ids that evidence cited.
type conversationWindowBuilder struct {
	MinimalContextBuilder
	summary int
	grants  []uint
}

func (b conversationWindowBuilder) Build(ctx context.Context, input RunContext) (ContextWindow, error) {
	window, err := b.MinimalContextBuilder.Build(ctx, input)
	if err != nil {
		return ContextWindow{}, err
	}
	window.Budget.Summary = b.summary
	window.Budget.Assembled += b.summary
	for _, id := range b.grants {
		window.SummarySegmentHistoryGrants = append(window.SummarySegmentHistoryGrants,
			HistoryMessageGrant{MessageID: id, SourceKind: HistorySourceSummarySegment,
				SourceSessionID: input.SessionID, SegmentSeq: 1, SegmentEndMessageID: id})
	}
	return window, nil
}

// segmentWindowBuilder is a window that carried summary segments, with the
// numbers those segments were labelled with.
type segmentWindowBuilder struct {
	MinimalContextBuilder
	summary     int
	segmentSeqs []int
}

func (b segmentWindowBuilder) Build(ctx context.Context, input RunContext) (ContextWindow, error) {
	window, err := b.MinimalContextBuilder.Build(ctx, input)
	if err != nil {
		return ContextWindow{}, err
	}
	window.Budget.Summary = b.summary
	window.Budget.Assembled += b.summary
	for seq, id := range b.segmentSeqs {
		window.SummarySegmentHistoryGrants = append(window.SummarySegmentHistoryGrants,
			HistoryMessageGrant{MessageID: uint(100 + id), SourceKind: HistorySourceSummarySegment,
				SourceSessionID: input.SessionID, SegmentSeq: seq + 1, SegmentEndMessageID: uint(100 + id)})
	}
	return window, nil
}

// An answer that rests on the conversation, and points at a message the
// conversation evidence carried, must not lose itself to a citation check
// that cannot mean anything here.
func TestAnAnswerFromConversationEvidenceIsNotRefusedByTheCitationCheck(t *testing.T) {
	fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		return schema.AssistantMessage("你最初定的是每次读 15 页 [1]。", nil), nil
	}}
	runner, err := NewRunner(fake, StaticToolRegistry{}, newMemoryStepRepository(),
		conversationWindowBuilder{summary: 1104, grants: []uint{1}}, DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), testRunContext(), DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Answer, "15 页") {
		t.Fatalf("answer = %q, want the grounded answer", result.Answer)
	}
	if result.AnswerBasis != rag.AnswerBasisConversation {
		t.Fatalf("basis = %s, want the conversation", result.AnswerBasis)
	}
	if result.KnowledgeStatus != rag.KnowledgeStatusNotRequired || result.KnowledgeReason != rag.KnowledgeReasonNone {
		t.Fatalf("status/reason = %s/%s, want the turn to be unencumbered", result.KnowledgeStatus, result.KnowledgeReason)
	}
	if result.GroundingRefusal != nil {
		t.Fatalf("refusal = %+v, want none", result.GroundingRefusal)
	}
}

// The conversation evidence labels its segments, and a model that answers
// from those segments may point at them by the number it was shown. That
// number is a pointer at the conversation too, and it resolves.
func TestAnAnswerCitingASegmentNumberIsNotRefused(t *testing.T) {
	fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		return schema.AssistantMessage("你最初定的是每周 2 次 [1]，后来改成了每周 6 次 [2]。", nil), nil
	}}
	runner, err := NewRunner(fake, StaticToolRegistry{}, newMemoryStepRepository(),
		segmentWindowBuilder{summary: 1104, segmentSeqs: []int{1, 2}}, DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), testRunContext(), DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Answer, "2 次") {
		t.Fatalf("answer = %q, want the grounded answer", result.Answer)
	}
	if result.GroundingRefusal != nil {
		t.Fatalf("refusal = %+v, want none", result.GroundingRefusal)
	}
}

// A number that points at nothing is refused, whether or not the window
// carried a summary and whether or not a real reference sits next to it.
func TestAnInventedReferenceIsRefusedEvenWithAnUnrelatedSummary(t *testing.T) {
	draft := "根据你上传的笔记，排序复杂度是 O(1) [99]。"
	fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		return schema.AssistantMessage(draft, nil), nil
	}}
	runner, err := NewRunner(fake, StaticToolRegistry{}, newMemoryStepRepository(),
		conversationWindowBuilder{summary: 1}, DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), testRunContext(), DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != UnsupportedGroundingAnswer {
		t.Fatalf("answer = %q, want the refusal", result.Answer)
	}
	if result.KnowledgeReason != rag.KnowledgeReasonInvalidCitation {
		t.Fatalf("reason = %s, want the invented reference to be named", result.KnowledgeReason)
	}
	if result.GroundingRefusal == nil || result.GroundingRefusal.Rule != GroundingRuleInvalidCitation {
		t.Fatalf("refusal = %+v, want the invalid citation recorded", result.GroundingRefusal)
	}
}

// The same draft is refused whether or not the window carried a summary.
//
// This is the case that had to change: a summary in the window used to be
// enough to let an invented reference through, so the same sentence was
// refused with an empty window and delivered with a summary in it. The
// window's budget says nothing about what a number points at, and the
// number has to be checked against what the turn showed either way.
func TestTheSameDraftIsRefusedWhetherOrNotTheWindowCarriedASummary(t *testing.T) {
	draft := "根据你上传的笔记，排序复杂度是 O(1) [99]。"
	fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		return schema.AssistantMessage(draft, nil), nil
	}}
	cases := []struct {
		name    string
		builder ContextBuilder
	}{
		{name: "no summary in the window", builder: MinimalContextBuilder{}},
		{name: "an unrelated summary in the window", builder: conversationWindowBuilder{summary: 1}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			runner, err := NewRunner(fake, StaticToolRegistry{}, newMemoryStepRepository(),
				testCase.builder, DefaultRunnerConfig())
			if err != nil {
				t.Fatal(err)
			}
			result, err := runner.Run(context.Background(), testRunContext(), DiscardEvents)
			if err != nil {
				t.Fatal(err)
			}
			if result.Answer != UnsupportedGroundingAnswer {
				t.Fatalf("answer = %q, want the refusal", result.Answer)
			}
			if result.KnowledgeReason != rag.KnowledgeReasonInvalidCitation {
				t.Fatalf("reason = %s, want the invented reference to be named", result.KnowledgeReason)
			}
			if result.GroundingRefusal == nil || result.GroundingRefusal.Rule != GroundingRuleInvalidCitation {
				t.Fatalf("refusal = %+v, want the invalid citation recorded", result.GroundingRefusal)
			}
			if len(result.GroundingRefusal.Citations) != 1 || result.GroundingRefusal.Citations[0] != 99 {
				t.Fatalf("citations = %v, want [99]", result.GroundingRefusal.Citations)
			}
		})
	}
}

// One reference that resolves and one that does not is still a refusal: a
// draft may not carry a pointer at nothing, and one real id does not excuse
// the invented one next to it.
func TestOneUnresolvedReferenceRefusesTheWholeAnswer(t *testing.T) {
	fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		return schema.AssistantMessage("你最初定的是每次读 15 页 [1]，后来改成了 30 页 [99]。", nil), nil
	}}
	runner, err := NewRunner(fake, StaticToolRegistry{}, newMemoryStepRepository(),
		conversationWindowBuilder{summary: 1104, grants: []uint{1}}, DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), testRunContext(), DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != UnsupportedGroundingAnswer {
		t.Fatalf("answer = %q, want the refusal", result.Answer)
	}
}

// A turn with no conversation evidence at all keeps the check's teeth: a
// number that resolves to nothing is an invented citation, and it is refused
// as one - with the draft recorded, so the refusal can be read afterwards.
func TestAnInventedCitationIsStillRefusedWithoutConversationEvidence(t *testing.T) {
	draft := "根据你的笔记，你最初定的是每次读 15 页 [1]。"
	fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		return schema.AssistantMessage(draft, nil), nil
	}}
	runner, err := NewRunner(fake, StaticToolRegistry{}, newMemoryStepRepository(),
		MinimalContextBuilder{}, DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), testRunContext(), DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != UnsupportedGroundingAnswer {
		t.Fatalf("answer = %q, want the refusal", result.Answer)
	}
	if result.KnowledgeReason != rag.KnowledgeReasonInvalidCitation {
		t.Fatalf("reason = %s, want the invented citation to be named", result.KnowledgeReason)
	}
	if result.GroundingRefusal == nil {
		t.Fatal("the refusal recorded nothing")
	}
	if result.GroundingRefusal.Rule != GroundingRuleInvalidCitation {
		t.Fatalf("rule = %q, want %q", result.GroundingRefusal.Rule, GroundingRuleInvalidCitation)
	}
	if result.GroundingRefusal.Draft != draft {
		t.Fatalf("draft = %q, want the refused draft", result.GroundingRefusal.Draft)
	}
	if len(result.GroundingRefusal.Citations) != 1 || result.GroundingRefusal.Citations[0] != 1 {
		t.Fatalf("citations = %v, want [1]", result.GroundingRefusal.Citations)
	}
}

// The check keeps its teeth where it has always had them: a turn that showed
// documents has to cite them, and a number that resolves to nothing is
// refused even though the turn also carried conversation evidence.
func TestAnInventedCitationIsStillRefusedWhenDocumentsWereShown(t *testing.T) {
	draft := "根据笔记，最初是每次读 15 页 [7]。"
	fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		return schema.AssistantMessage(draft, nil), nil
	}}
	knowledgeTool := mustNamedEvidenceTool(t, knowledgeSearchToolName, nil)
	registry := &proberRegistry{StaticToolRegistry: StaticToolRegistry{Registered: []tool.BaseTool{knowledgeTool}},
		outcome: RelevanceOutcome{Status: RelevanceBelowThreshold, Threshold: DefaultRelevanceThreshold}}
	runner, err := NewRunner(fake, registry, newMemoryStepRepository(),
		conversationWindowBuilder{summary: 1104, grants: []uint{7}}, DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), testRunContext(), DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != UnsupportedGroundingAnswer {
		t.Fatalf("answer = %q, want the refusal", result.Answer)
	}
	if result.KnowledgeReason != rag.KnowledgeReasonInvalidCitation {
		t.Fatalf("reason = %s, want the invented citation to be named", result.KnowledgeReason)
	}
}
