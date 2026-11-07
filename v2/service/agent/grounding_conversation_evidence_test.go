package agent

import (
	"context"
	"strings"
	"testing"

	"GopherAI/common/rag"
	modelcomponent "github.com/cloudwego/eino/components/model"
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
// refused. When the turn never consulted the notes at all, the same number
// is not a claim about the notes: the turn's evidence was the learner's own
// recorded past, and the number points at that.
//
// The two tests below are the two halves of that rule. Neither passes alone:
// the first fails if the escape is missing, the second fails if the escape is
// too wide.

// conversationWindowBuilder is a window that carries one block of compressed
// conversation evidence, which is what the rolling summary, the whiteboard
// and the summary segments are.
type conversationWindowBuilder struct {
	MinimalContextBuilder
	summary int
}

func (b conversationWindowBuilder) Build(ctx context.Context, input RunContext) (ContextWindow, error) {
	window, err := b.MinimalContextBuilder.Build(ctx, input)
	if err != nil {
		return ContextWindow{}, err
	}
	window.Budget.Summary = b.summary
	window.Budget.Assembled += b.summary
	return window, nil
}

// A turn whose evidence was the learner's own recorded past must not lose its
// answer to a citation check that cannot mean anything here.
func TestAnAnswerFromConversationEvidenceIsNotRefusedByTheCitationCheck(t *testing.T) {
	fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		return schema.AssistantMessage("你最初定的是每次读 15 页 [1]。", nil), nil
	}}
	runner, err := NewRunner(fake, StaticToolRegistry{}, newMemoryStepRepository(),
		conversationWindowBuilder{summary: 1104}, DefaultRunnerConfig())
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
		conversationWindowBuilder{summary: 1104}, DefaultRunnerConfig())
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

var _ modelcomponent.ToolCallingChatModel = (*scriptedChatModel)(nil)
