package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
)

// Unconfirmed cross-session evidence must not become the answer.
//
// The acceptance runs kept producing this shape: a pointer question ("上次…")
// offered several conversations, the model opened one (or one the search never
// offered), read a stretch, and answered about it. The answer is usable and
// about the wrong conversation. The product must use the location results it
// already has: one confirmed target keeps the direct path, ambiguous or
// unconfirmed evidence asks the learner.

type evidenceControlBrowseQuery struct {
	Session string `json:"session"`
	Query   string `json:"query"`
}

type evidenceControlBrowseOutput struct {
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Summary string `json:"summary"`
}

func mustEvidenceControlCardTool(t *testing.T, candidates []handoffCandidate) tool.InvokableTool {
	t.Helper()
	built, err := toolutils.InferTool("search_session_cards", "find earlier conversations",
		func(context.Context, *handoffCardQuery) (*handoffCardOutput, error) {
			return &handoffCardOutput{Status: "ok", Reason: "none", Candidates: candidates}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return built
}

func mustEvidenceControlBrowseTool(t *testing.T, sessions *[]string) tool.InvokableTool {
	t.Helper()
	built, err := toolutils.InferTool("browse_summary_segments", "browse one conversation's summary",
		func(_ context.Context, input *evidenceControlBrowseQuery) (*evidenceControlBrowseOutput, error) {
			*sessions = append(*sessions, input.Session)
			return &evidenceControlBrowseOutput{Status: "ok", Reason: "none", Summary: "evidence from " + input.Session}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return built
}

func evidenceControlCardCall(id int) *schema.Message {
	return schema.AssistantMessage("", []schema.ToolCall{{ID: "call-card", Type: "function",
		Function: schema.FunctionCall{Name: "search_session_cards", Arguments: `{"query":"练习清单"}`}}})
}

func evidenceControlBrowseCall(id int, session string) *schema.Message {
	return schema.AssistantMessage("", []schema.ToolCall{{ID: id2(id), Type: "function",
		Function: schema.FunctionCall{Name: "browse_summary_segments",
			Arguments: `{"session":"` + session + `","query":"清单"}`}}})
}

func id2(id int) string {
	if id == 1 {
		return "call-browse-1"
	}
	return "call-browse-2"
}

func candidatesOfSessions(sessions ...string) []handoffCandidate {
	out := make([]handoffCandidate, 0, len(sessions))
	for index, session := range sessions {
		out = append(out, handoffCandidate{
			SessionID: session, Title: "Evaluation conversation",
			Topics: []string{"练习清单"}, LastTalkedAt: "2026-05-0" + string(rune('1'+index)) + "T00:00:00Z",
		})
	}
	return out
}

func TestEvidenceFromTwoSimilarConversationsBecomesAClarification(t *testing.T) {
	var sessions []string
	chat := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		switch call {
		case 1:
			return evidenceControlCardCall(1), nil
		case 2:
			return evidenceControlBrowseCall(1, "p2c-30"), nil
		case 3:
			return evidenceControlBrowseCall(2, "p2c-29"), nil
		default:
			return schema.AssistantMessage("清单是把现象、提问、验收三段对齐排的。", nil), nil
		}
	}}
	runner, err := NewRunner(chat, StaticToolRegistry{Registered: []tool.BaseTool{
		mustEvidenceControlCardTool(t, candidatesOfSessions("p2c-30", "p2c-29")),
		mustEvidenceControlBrowseTool(t, &sessions),
	}}, newMemoryStepRepository(), MinimalContextBuilder{}, DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	input := testRunContext()
	input.Question = "上次那份练习清单，你说是把哪两样东西对齐着排的？"
	result, err := runner.Run(context.Background(), input, DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if result.Clarification == nil {
		t.Fatalf("two similar conversations were answered anyway: %q", result.Answer)
	}
	if result.Answer != WaitingChoiceAnswer {
		t.Fatalf("the answer is %q, want the waiting sentence", result.Answer)
	}
	if len(result.Clarification.Candidates) != 2 {
		t.Fatalf("the card offers %d candidates", len(result.Clarification.Candidates))
	}
	if !result.Clarification.AllowHistoryBrowse || !result.Clarification.AllowFreeText {
		t.Fatal("the card does not offer the learner's own list and words")
	}
}

func TestEvidenceFromAConversationTheSearchNeverOfferedBecomesAClarification(t *testing.T) {
	var sessions []string
	chat := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		switch call {
		case 1:
			return evidenceControlCardCall(1), nil
		case 2:
			return evidenceControlBrowseCall(1, "p2c-13"), nil
		default:
			return schema.AssistantMessage("讨论过，但没有定下来。", nil), nil
		}
	}}
	runner, err := NewRunner(chat, StaticToolRegistry{Registered: []tool.BaseTool{
		mustEvidenceControlCardTool(t, candidatesOfSessions("p2c-25")),
		mustEvidenceControlBrowseTool(t, &sessions),
	}}, newMemoryStepRepository(), MinimalContextBuilder{}, DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	input := testRunContext()
	input.Question = "上次我们讨论过数据库的事务隔离级别吗？定下来用哪一档了吗？"
	result, err := runner.Run(context.Background(), input, DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if result.Clarification == nil {
		t.Fatalf("a conversation outside the candidates was answered anyway: %q", result.Answer)
	}
	if len(result.Clarification.Candidates) != 1 || result.Clarification.Candidates[0].SessionID != "p2c-25" {
		t.Fatalf("the card offers %+v", result.Clarification.Candidates)
	}
}

func TestASingleConfirmedTargetStillAnswersDirectly(t *testing.T) {
	var sessions []string
	chat := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		switch call {
		case 1:
			return evidenceControlCardCall(1), nil
		case 2:
			return evidenceControlBrowseCall(1, "p2c-01"), nil
		default:
			return schema.AssistantMessage("先看语句本身和执行计划。", nil), nil
		}
	}}
	runner, err := NewRunner(chat, StaticToolRegistry{Registered: []tool.BaseTool{
		mustEvidenceControlCardTool(t, candidatesOfSessions("p2c-01")),
		mustEvidenceControlBrowseTool(t, &sessions),
	}}, newMemoryStepRepository(), MinimalContextBuilder{}, DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	input := testRunContext()
	input.Question = "上次说到数据库这边加索引，你当时建议先看哪两样再动手？"
	result, err := runner.Run(context.Background(), input, DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if result.Clarification != nil {
		t.Fatalf("a single confirmed target was turned into a clarification: %+v", result.Clarification)
	}
	if result.Answer != "先看语句本身和执行计划。" {
		t.Fatalf("answer=%q", result.Answer)
	}
}

func TestALockedTurnIsNotReaskedWhenItUsesTheLockedConversation(t *testing.T) {
	var sessions []string
	var executions atomic.Int32
	chat := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		switch call {
		case 1:
			return evidenceControlBrowseCall(1, "p2c-01"), nil
		default:
			return schema.AssistantMessage("先看语句本身和执行计划。", nil), nil
		}
	}}
	runner, err := NewRunner(chat, StaticToolRegistry{Registered: []tool.BaseTool{
		mustEvidenceControlBrowseTool(t, &sessions),
		mustEvidenceControlCardTool(t, candidatesOfSessions("p2c-01", "p2c-02")),
	}}, newMemoryStepRepository(),
		lockedWindowBuilder{locked: &LockedSession{SessionID: "p2c-01"}}, DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	input := testRunContext()
	input.Question = "上次说到数据库这边加索引，你当时建议先看哪两样再动手？"
	result, err := runner.Run(context.Background(), input, DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if result.Clarification != nil {
		t.Fatalf("a locked turn was re-asked: %+v", result.Clarification)
	}
	if result.Answer != "先看语句本身和执行计划。" {
		t.Fatalf("answer=%q", result.Answer)
	}
	_ = executions.Load()
}

// An absence question cannot be answered "no" before the target is confirmed.
//
// "上次我们聊过限流和网关配置吗？" is a yes/no question about ONE conversation.
// The card search offered four conversations; without the learner saying
// which one, "我没找到记录" is a conclusion about a conversation nobody
// identified. The turn asks instead.
func TestAnAbsenceQuestionWithoutAConfirmedTargetAsksFirst(t *testing.T) {
	chat := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return evidenceControlCardCall(1), nil
		}
		return schema.AssistantMessage("我在以往的会话里没找到聊过限流和网关的记录，所以应该是没讨论过。", nil), nil
	}}
	runner, err := NewRunner(chat, StaticToolRegistry{Registered: []tool.BaseTool{
		mustEvidenceControlCardTool(t, candidatesOfSessions("p2c-09", "p2c-08", "p2c-07", "p2c-06")),
		mustEvidenceControlBrowseTool(t, &[]string{}),
	}}, newMemoryStepRepository(), MinimalContextBuilder{}, DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	input := testRunContext()
	input.Question = "上次我们聊过限流和网关配置吗？数据库那边怎么配索引优化定下来了吗？"
	result, err := runner.Run(context.Background(), input, DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if result.Clarification == nil {
		t.Fatalf("an absence question was answered before the target was confirmed: %q", result.Answer)
	}
	if result.Answer != WaitingChoiceAnswer {
		t.Fatalf("the answer is %q", result.Answer)
	}
}

// Once the target is unique AND its record was read, an absence question is
// answered directly.
func TestAnAbsenceQuestionWithAConfirmedTargetAnswersDirectly(t *testing.T) {
	var sessions []string
	chat := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		switch call {
		case 1:
			return evidenceControlCardCall(1), nil
		case 2:
			return evidenceControlBrowseCall(1, "p2c-25"), nil
		default:
			return schema.AssistantMessage("上次那次会话里没有讨论过事务隔离级别。", nil), nil
		}
	}}
	runner, err := NewRunner(chat, StaticToolRegistry{Registered: []tool.BaseTool{
		mustEvidenceControlCardTool(t, candidatesOfSessions("p2c-25")),
		mustEvidenceControlBrowseTool(t, &sessions),
	}}, newMemoryStepRepository(), MinimalContextBuilder{}, DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	input := testRunContext()
	input.Question = "上次我们讨论过数据库的事务隔离级别吗？定下来用哪一档了吗？"
	result, err := runner.Run(context.Background(), input, DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if result.Clarification != nil {
		t.Fatalf("a confirmed target with its record read was turned into a clarification: %+v", result.Clarification)
	}
	if result.Answer != "上次那次会话里没有讨论过事务隔离级别。" {
		t.Fatalf("answer=%q", result.Answer)
	}
}

// The locked block tells the model how to answer when the record is thin.
func TestTheLockedBlockSaysToNameWhatIsMissing(t *testing.T) {
	block := renderLockedSessionBlock(&LockedSession{SessionID: "p2c-25", Title: "Evaluation conversation",
		Evidence: "record"}, "上次我们讨论过数据库的事务隔离级别吗？")
	for _, phrase := range []string{"say what is missing", "do not claim the conversation never discussed it"} {
		if !strings.Contains(block, phrase) {
			t.Fatalf("the locked block does not say %q: %s", phrase, block)
		}
	}
}

// Pointer words inside the PRESENT conversation are not a history question.
//
// "你之前说的这个方案可行吗？" continues the current chat. The clarification
// rules belong to the cross-session recall path: a turn that never searched
// history must answer normally, even when it offers the history tools and the
// question contains "之前" and "吗".
func TestACurrentConversationFollowUpWithPointerWordsIsAnsweredDirectly(t *testing.T) {
	const answer = "可行，按现在这个方案走。"
	for _, test := range []struct {
		name     string
		registry ToolRegistry
	}{
		{name: "no tools registered", registry: StaticToolRegistry{}},
		{name: "history tools offered but not used", registry: StaticToolRegistry{Registered: []tool.BaseTool{
			mustEvidenceControlCardTool(t, candidatesOfSessions("p2c-10", "p2c-11")),
			mustEvidenceControlBrowseTool(t, &[]string{}),
		}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			chat := &scriptedChatModel{respond: func(context.Context, int) (*schema.Message, error) {
				return schema.AssistantMessage(answer, nil), nil
			}}
			runner, err := NewRunner(chat, test.registry, newMemoryStepRepository(),
				MinimalContextBuilder{}, DefaultRunnerConfig())
			if err != nil {
				t.Fatal(err)
			}
			input := testRunContext()
			input.Question = "你之前说的这个方案可行吗？"
			result, err := runner.Run(context.Background(), input, DiscardEvents)
			if err != nil {
				t.Fatal(err)
			}
			if result.Clarification != nil {
				t.Fatalf("a current-conversation follow-up was turned into a history question: %+v", result.Clarification)
			}
			if result.Answer != answer {
				t.Fatalf("answer=%q", result.Answer)
			}
		})
	}
}

// The top candidate is the target; the model opened a similar candidate from
// the same list and answered from it. The target is still ambiguous, so the
// turn asks with a formal card.
func TestReadingASimilarCandidateInsteadOfTheTopOneAsks(t *testing.T) {
	for _, test := range []struct {
		name       string
		candidates []string
		read       string
	}{
		{name: "p2-art-02-r2", candidates: []string{"p2c-03", "p2c-02"}, read: "p2c-02"},
		{name: "p2-acc-02-r1", candidates: []string{"p2c-31", "p2c-30"}, read: "p2c-30"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var browseSessions []string
			chat := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
				switch call {
				case 1:
					return evidenceControlCardCall(1), nil
				case 2:
					return evidenceControlBrowseCall(1, test.read), nil
				default:
					return schema.AssistantMessage("第二道练的是 Redis 持久化方式。", nil), nil
				}
			}}
			runner, err := NewRunner(chat, StaticToolRegistry{Registered: []tool.BaseTool{
				mustEvidenceControlCardTool(t, candidatesOfSessions(test.candidates...)),
				mustEvidenceControlBrowseTool(t, &browseSessions),
			}}, newMemoryStepRepository(), MinimalContextBuilder{}, DefaultRunnerConfig())
			if err != nil {
				t.Fatal(err)
			}
			input := testRunContext()
			input.Question = "上次那份练习单子，第二道练的是什么？"
			result, err := runner.Run(context.Background(), input, DiscardEvents)
			if err != nil {
				t.Fatal(err)
			}
			if result.Clarification == nil {
				t.Fatalf("a similar conversation was answered directly: %q", result.Answer)
			}
			if len(result.Clarification.Candidates) != 2 {
				t.Fatalf("the card offers %d candidates", len(result.Clarification.Candidates))
			}
		})
	}
}

// Reading the top candidate is the confirmed target: answer directly.
func TestReadingTheTopCandidateStaysADirectAnswer(t *testing.T) {
	var browseSessions []string
	chat := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		switch call {
		case 1:
			return evidenceControlCardCall(1), nil
		case 2:
			return evidenceControlBrowseCall(1, "p2c-03"), nil
		default:
			return schema.AssistantMessage("第二道练的是执行计划。", nil), nil
		}
	}}
	runner, err := NewRunner(chat, StaticToolRegistry{Registered: []tool.BaseTool{
		mustEvidenceControlCardTool(t, candidatesOfSessions("p2c-03", "p2c-02")),
		mustEvidenceControlBrowseTool(t, &browseSessions),
	}}, newMemoryStepRepository(), MinimalContextBuilder{}, DefaultRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	input := testRunContext()
	input.Question = "上次那份练习单子，第二道练的是什么？"
	result, err := runner.Run(context.Background(), input, DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if result.Clarification != nil {
		t.Fatalf("the top candidate was turned into a card: %+v", result.Clarification)
	}
	if result.Answer != "第二道练的是执行计划。" {
		t.Fatalf("answer=%q", result.Answer)
	}
}
