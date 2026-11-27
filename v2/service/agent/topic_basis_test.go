package agent

import (
	"context"
	"strings"
	"testing"

	"GopherAI/common/rag"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
)

// The answer basis of a turn that rests on the learner's own confirmed topic
// note.
//
// The question is the one the review used:
//
//	请根据我保存的分层摘要主题笔记，说出我用来区分现值和旧值的口诀。按笔记原样复述。
//
// What was wrong with it: the note reached the model, the model answered the
// question correctly from the note, and the knowledge retrieval returned four
// candidate passages the answer did not use. The document path then read "an
// answer with no document citation" as "an answer with no evidence at all",
// rewrote it from general knowledge, and labelled it "来自通用知识".
//
// The learner was told their own note was not the source of the sentence they
// had just read.
//
// These tests pin the corrected rule:
//
//  1. the answer is kept and attributed to the note,
//  2. the document retrieval is reported on its own fields,
//  3. a turn that did not use the note gets no topic basis,
//  4. a question that asked for a document keeps the document rules.

// The note the learner saved, and the question they asked about it.
const (
	topicBasisNoteKey  = "分层摘要"
	topicBasisNoteName = "分层摘要"
	topicBasisNoteText = "我用来区分现值和旧值的口诀是：现值看白板，旧值翻档案。"
	topicBasisQuestion = "请根据我保存的分层摘要主题笔记，说出我用来区分现值和旧值的口诀。按笔记原样复述。"
	// The answer the model wrote: the note's own words, as the learner asked
	// for them.
	topicBasisAnswer = "你用来区分现值和旧值的口诀是：现值看白板，旧值翻档案。"
)

// confirmedTopicWindowBuilder stands in for the window a verified consent
// produced.
//
// It carries the confirmed topic's notes the way the real builder does: the
// note is registered in the evidence registry under the confirmed topic's
// source, and the window carries the note's own wording so that the server
// can check the answer against it.
type confirmedTopicWindowBuilder struct {
	// topicKey is the confirmed topic. Empty means nothing was confirmed.
	topicKey string
	// note is the confirmed note's wording. Empty means the confirmed topic
	// has no note at all.
	note string
	// displayName is the name the learner knows the topic by.
	displayName string
}

func (b confirmedTopicWindowBuilder) Build(ctx context.Context, input RunContext) (ContextWindow, error) {
	window, err := MinimalContextBuilder{}.Build(ctx, input)
	if err != nil {
		return ContextWindow{}, err
	}
	if b.topicKey == "" {
		return window, nil
	}
	window.UsedTopicKeys = []string{b.topicKey}
	name := b.displayName
	if strings.TrimSpace(name) == "" {
		name = b.topicKey
	}
	window.UsedTopicNames = map[string]string{b.topicKey: name}
	if strings.TrimSpace(b.note) == "" {
		// A confirmed topic with no note under it. The receipt still says the
		// topic was confirmed, and there is nothing for an answer to rest on.
		return window, nil
	}
	ref := input.MemoryEvidence.Register(MemoryEvidenceEntry{
		Kind: MemoryEvidenceTopic, InternalID: "note-1", Version: 1,
		Source: confirmedTopicSourcePrefix + b.topicKey, Subject: name,
	})
	window.ConfirmedTopicNotes = []ConfirmedTopicNote{{
		Ref: ref, TopicKey: b.topicKey, Subject: name, Text: b.note,
	}}
	return window, nil
}

// topicBasisSearch is the knowledge search that returns candidates the answer
// does not use.
type topicBasisSearch struct{ calls int }

func (k *topicBasisSearch) build(t *testing.T) tool.InvokableTool {
	t.Helper()
	built, err := toolutils.InferTool(knowledgeSearchToolName, "search the learner's documents",
		func(context.Context, *struct{}) (*structuredEvidenceOutput, error) {
			k.calls++
			return &structuredEvidenceOutput{Summary: "evidence", Results: groundedResults()}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return built
}

// runTopicBasisTurn drives one turn and returns what the server recorded.
func runTopicBasisTurn(t *testing.T, question string, builder ContextBuilder, policy rag.KnowledgePolicy,
	script func(int) *schema.Message) (*scriptedChatModel, RunnerResult) {
	t.Helper()
	fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		return script(call), nil
	}}
	runner, err := NewRunner(fake, StaticToolRegistry{Registered: []tool.BaseTool{}}, newMemoryStepRepository(),
		builder, userConfirmedRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	input := testRunContext()
	input.Question = question
	input.KnowledgePolicy = policy
	input.MemoryEvidence = NewMemoryEvidenceRegistry()
	result, err := runner.Run(context.Background(), input, DiscardEvents)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	return fake, result
}

// 1. Both switches on, the document retrieval returns candidates, and the
// answer uses the confirmed note.
//
// The answer is kept, the note is credited, and the document retrieval is
// reported on its own fields rather than swallowing the whole basis.
func TestConfirmedTopicAnswerKeepsItsBasisWhenTheDocumentSearchReturnsCandidates(t *testing.T) {
	search := &topicBasisSearch{}
	fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		switch call {
		case 1:
			return knowledgeCall("k1"), nil
		default:
			return schema.AssistantMessage(topicBasisAnswer, nil), nil
		}
	}}
	runner, err := NewRunner(fake, StaticToolRegistry{Registered: []tool.BaseTool{search.build(t)}},
		newMemoryStepRepository(), confirmedTopicWindowBuilder{
			topicKey: topicBasisNoteKey, note: topicBasisNoteText, displayName: topicBasisNoteName,
		}, userConfirmedRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	input := testRunContext()
	input.Question = topicBasisQuestion
	input.KnowledgePolicy = rag.KnowledgePreferred
	input.MemoryEvidence = NewMemoryEvidenceRegistry()
	result, err := runner.Run(context.Background(), input, DiscardEvents)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}

	// The answer stands, and it is the note's answer.
	if !strings.Contains(result.Answer, "现值看白板，旧值翻档案") {
		t.Fatalf("the answer lost the note's own words: %q", result.Answer)
	}
	if strings.Contains(result.Answer, noEvidenceFallbackBanner) {
		t.Fatalf("the answer was rewritten from general knowledge: %q", result.Answer)
	}
	// The basis is the topic note, and it names the subject.
	if result.AnswerBasis != rag.AnswerBasisTopicMemory {
		t.Fatalf("basis = %s, want %s (answer %q)", result.AnswerBasis, rag.AnswerBasisTopicMemory, result.Answer)
	}
	if len(result.AnswerBasisTopics) != 1 || result.AnswerBasisTopics[0] != topicBasisNoteName {
		t.Fatalf("answerBasisTopics = %v, want [%s]", result.AnswerBasisTopics, topicBasisNoteName)
	}
	// The document side is reported on its own fields: the retrieval ran, and
	// the answer did not come from it.
	if result.KnowledgeStatus != rag.KnowledgeStatusNotUsed {
		t.Fatalf("knowledgeStatus = %s, want %s", result.KnowledgeStatus, rag.KnowledgeStatusNotUsed)
	}
	if result.KnowledgeReason != rag.KnowledgeReasonNotCited {
		t.Fatalf("knowledgeReason = %s, want %s", result.KnowledgeReason, rag.KnowledgeReasonNotCited)
	}
	// The receipt is a separate fact and still says what reached the context.
	if len(result.UsedTopicKeys) != 1 || result.UsedTopicKeys[0] != topicBasisNoteKey {
		t.Fatalf("usedTopicKeys = %v, want [%s]", result.UsedTopicKeys, topicBasisNoteKey)
	}
}

// 2. Two ways of not resting on the note, and both get no topic basis.
//
// "本次不使用" means no topic was confirmed at all; a confirmed topic with no
// note under it means there was nothing for the answer to rest on. Neither is
// an answer from the topic memory, and neither may claim to be.
func TestNoConfirmedNoteMeansNoTopicBasis(t *testing.T) {
	for name, builder := range map[string]ContextBuilder{
		"nothing confirmed":              confirmedTopicWindowBuilder{},
		"confirmed topic without a note": confirmedTopicWindowBuilder{topicKey: topicBasisNoteKey},
	} {
		t.Run(name, func(t *testing.T) {
			search := &topicBasisSearch{}
			fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
				switch call {
				case 1:
					return knowledgeCall("k1"), nil
				default:
					// The answer happens to contain the note's words even
					// though the turn had no note to read.
					return schema.AssistantMessage(topicBasisAnswer, nil), nil
				}
			}}
			runner, err := NewRunner(fake, StaticToolRegistry{Registered: []tool.BaseTool{search.build(t)}},
				newMemoryStepRepository(), builder, userConfirmedRunnerConfig())
			if err != nil {
				t.Fatal(err)
			}
			input := testRunContext()
			input.Question = topicBasisQuestion
			input.KnowledgePolicy = rag.KnowledgePreferred
			input.MemoryEvidence = NewMemoryEvidenceRegistry()
			result, err := runner.Run(context.Background(), input, DiscardEvents)
			if err != nil {
				t.Fatalf("run failed: %v", err)
			}
			if result.AnswerBasis == rag.AnswerBasisTopicMemory {
				t.Fatalf("basis = %s, want no topic basis", result.AnswerBasis)
			}
			if len(result.AnswerBasisTopics) != 0 {
				t.Fatalf("answerBasisTopics = %v, want none", result.AnswerBasisTopics)
			}
		})
	}
}

// 3. A question that asks for a document keeps the document rules.
//
// "请引用《03-layered-summary.md》里白板定义的原文" names a document, so the
// turn is answered from the documents or not at all - and a missing citation is
// handled by the document rules rather than by the note.
func TestAQuestionThatAsksForADocumentKeepsTheDocumentRules(t *testing.T) {
	question := "请引用《03-layered-summary.md》里白板定义的原文"
	search := &topicBasisSearch{}
	fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		switch call {
		case 1:
			return knowledgeCall("k1"), nil
		default:
			// No document citation: the document rules own this draft.
			return schema.AssistantMessage(topicBasisAnswer, nil), nil
		}
	}}
	runner, err := NewRunner(fake, StaticToolRegistry{Registered: []tool.BaseTool{search.build(t)}},
		newMemoryStepRepository(), confirmedTopicWindowBuilder{
			topicKey: topicBasisNoteKey, note: topicBasisNoteText, displayName: topicBasisNoteName,
		}, userConfirmedRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	input := testRunContext()
	input.Question = question
	input.KnowledgePolicy = rag.KnowledgePreferred
	input.MemoryEvidence = NewMemoryEvidenceRegistry()
	result, err := runner.Run(context.Background(), input, DiscardEvents)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if result.AnswerBasis == rag.AnswerBasisTopicMemory {
		t.Fatalf("basis = %s: a question that named a document may not be answered from the note",
			result.AnswerBasis)
	}
}

// The same question, answered with a document citation that resolves, is a
// document answer and says so.
func TestAQuestionThatAsksForADocumentWithAValidCitationIsADocumentAnswer(t *testing.T) {
	question := "请引用《03-layered-summary.md》里白板定义的原文"
	search := &topicBasisSearch{}
	fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		switch call {
		case 1:
			return knowledgeCall("k1"), nil
		default:
			return schema.AssistantMessage("白板定义在文档里是这样写的 [1]。", nil), nil
		}
	}}
	runner, err := NewRunner(fake, StaticToolRegistry{Registered: []tool.BaseTool{search.build(t)}},
		newMemoryStepRepository(), confirmedTopicWindowBuilder{
			topicKey: topicBasisNoteKey, note: topicBasisNoteText, displayName: topicBasisNoteName,
		}, userConfirmedRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	input := testRunContext()
	input.Question = question
	input.KnowledgePolicy = rag.KnowledgePreferred
	input.MemoryEvidence = NewMemoryEvidenceRegistry()
	result, err := runner.Run(context.Background(), input, DiscardEvents)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if result.AnswerBasis != rag.AnswerBasisPersonalKnowledge {
		t.Fatalf("basis = %s, want %s", result.AnswerBasis, rag.AnswerBasisPersonalKnowledge)
	}
	if result.KnowledgeStatus != rag.KnowledgeStatusGrounded {
		t.Fatalf("knowledgeStatus = %s, want %s", result.KnowledgeStatus, rag.KnowledgeStatusGrounded)
	}
}

// The receipt alone is not an attribution.
//
// A topic the learner confirmed says the notes reached the model's context.
// It does not say the answer rests on them, and an answer that says something
// else must not be credited to the note.
func TestAConfirmedTopicIsNotAnAttributionByItself(t *testing.T) {
	attribution := attributeToTopicNotes(nil, []ConfirmedTopicNote{{
		Ref: "S1", TopicKey: topicBasisNoteKey, Text: topicBasisNoteText,
	}}, "Redis 的 AOF 有三种策略，和你的笔记无关。", map[string]string{topicBasisNoteKey: topicBasisNoteName})
	if !attribution.Empty() {
		t.Fatalf("attribution = %v, want none: the answer does not rest on the note", attribution.Keys)
	}
}

// The note's own words are the attribution.
//
// A learner who asked for a mnemonic "as written" gets it as written, and the
// server can see that without guessing.
func TestTheNotesOwnWordsAreAnAttribution(t *testing.T) {
	attribution := attributeToTopicNotes(nil, []ConfirmedTopicNote{{
		Ref: "S1", TopicKey: topicBasisNoteKey, Text: topicBasisNoteText,
	}}, topicBasisAnswer, map[string]string{topicBasisNoteKey: topicBasisNoteName})
	if attribution.Empty() {
		t.Fatal("the answer repeats the note's own words and was not attributed to it")
	}
	if len(attribution.Names) != 1 || attribution.Names[0] != topicBasisNoteName {
		t.Fatalf("names = %v, want [%s]", attribution.Names, topicBasisNoteName)
	}
}

// A note short enough to be a title cannot carry the words signal.
//
// "缓存" appearing in an answer is not an answer built on a note called
// "缓存". Such a note can still be attributed through its reference, which is
// the model's own statement about what it used.
func TestAShortNoteIsNotAttributedByItsWords(t *testing.T) {
	attribution := attributeToTopicNotes(nil, []ConfirmedTopicNote{{
		Ref: "S1", TopicKey: "缓存", Text: "缓存",
	}}, "Redis 的缓存要记得设置过期时间，否则会一直占着内存。", map[string]string{"缓存": "缓存"})
	if !attribution.Empty() {
		t.Fatalf("attribution = %v, want none: a title is not a statement", attribution.Keys)
	}
}

// A question that asks for the learner's own words back is owned by the
// history rule, and a confirmed note is not a way around it.
func TestAHistoryQuestionIsNotAnsweredFromTheNote(t *testing.T) {
	question := "请逐字贴出我上次说的口诀"
	if !requiresExactHistoryEvidence(question) {
		t.Fatalf("%q must stay a history question", question)
	}
	if questionDemandsDocument(question) {
		t.Fatalf("%q must not be read as a document question", question)
	}
}
