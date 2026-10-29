package devmodel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/model"
)

// The seam's promise is that what it writes can be stored. These tests check the shape
// against the same domain rules the workflow applies; internal/app drives the real
// prompts through the real service, which is where "valid" is actually decided.

const sampleGenerationPrompt = `主题：Redis 持久化
题数：3
题型只能是：short_answer、concept_comparison、code_analysis

[证据]
[1] 出自《Redis 手册》 · RDB 快照
RDB 把某一时刻的数据写成一份二进制文件，恢复时直接读回，代价是两次快照之间的写入可能丢失。

[2] 出自《Redis 手册》 · AOF 追加日志
AOF 把每条写命令追加到日志末尾，按策略回写磁盘，重启时重放日志，数据更安全但文件更大。

[3] 出自《Redis 手册》 · 配置
配置项 appendonly yes 打开 AOF；appendfsync everysec 每秒刷一次盘。

` + "```" + `redis.conf
appendonly yes
appendfsync everysec
` + "```" + `

[已出过的题]（只是曝光记录，不是证据）
- (short_answer) 旧题｜考查：旧目标

请输出 JSON。`

func TestGenerationAnswerHoldsThreeStorableQuestions(t *testing.T) {
	answer := completeGeneration(t, sampleGenerationPrompt, 1)
	if len(answer.Questions) != domain.LearningQuestionCount {
		t.Fatalf("%d questions, want %d", len(answer.Questions), domain.LearningQuestionCount)
	}
	shown := map[int]struct{}{1: {}, 2: {}, 3: {}}
	goals := map[string]struct{}{}
	types := map[domain.LearningQuestionType]struct{}{}
	for index, question := range answer.Questions {
		kind := domain.LearningQuestionType(question.Type)
		if !kind.IsValid() {
			t.Fatalf("question %d has type %q, outside the three the prompt allows", index+1, question.Type)
		}
		types[kind] = struct{}{}
		if len(question.Evidence) == 0 {
			t.Fatalf("question %d cites nothing, so grading would have no snapshot", index+1)
		}
		for _, ordinal := range question.Evidence {
			if _, exists := shown[ordinal]; !exists {
				t.Fatalf("question %d cites evidence %d, which the prompt never showed", index+1, ordinal)
			}
		}
		if _, repeat := goals[normalizeForTest(question.AssessmentGoal)]; repeat {
			t.Fatalf("questions %d and an earlier one measure the same thing: %q", index+1, question.AssessmentGoal)
		}
		goals[normalizeForTest(question.AssessmentGoal)] = struct{}{}

		// The same validator the workflow runs before storing a question.
		stored := domain.LearningQuestion{
			Ordinal: index + 1, Type: kind, Question: question.Question,
			AssessmentGoal: question.AssessmentGoal, ReferenceAnswer: question.ReferenceAnswer,
			Rubric: question.Rubric,
			Evidence: []domain.LearningEvidence{{
				Ordinal: question.Evidence[0], DocumentID: "doc-1", ChunkID: "chunk-1",
				DocumentVer: 1, StartLine: 1, EndLine: 3, ChunkOrder: 1,
				Snapshot: "RDB 把某一时刻的数据写成一份二进制文件。",
			}},
		}
		if kind == domain.LearningQuestionCodeAnalysis {
			if !strings.Contains(sampleGenerationPrompt, "```") {
				t.Fatalf("a code analysis question appeared without code in the prompt")
			}
			stored.Evidence[0].Snapshot = "appendonly yes" + "\n" + "```" + "redis.conf" + "\n" + "```"
		}
		if err := stored.ValidateContent(); err != nil {
			t.Fatalf("question %d would not be stored: %v", index+1, err)
		}
		// A learner reads this text. It has to say it came from a script.
		if !strings.Contains(question.ReferenceAnswer, "本地脚本参考答案") {
			t.Fatalf("reference answer reads like a model's judgement: %q", question.ReferenceAnswer)
		}
	}
	if len(types) < 2 {
		t.Fatalf("all three questions asked the same kind of thing: %+v", answer.Questions)
	}
}

// TestConsecutiveRoundsRewriteEveryQuestion is the local half of the workflow's novelty
// rule: the server compares against stored history, and this is the property that makes
// that comparison able to pass at all.
func TestConsecutiveRoundsRewriteEveryQuestion(t *testing.T) {
	seen := map[string]struct{}{}
	kinds := map[string]int{}
	for round := 1; round <= 8; round++ {
		answer := completeGeneration(t, sampleGenerationPrompt, round)
		for _, question := range answer.Questions {
			key := normalizeForTest(question.Question)
			if _, repeat := seen[key]; repeat {
				t.Fatalf("round %d repeated a question verbatim: %q", round, question.Question)
			}
			seen[key] = struct{}{}
			kinds[question.Type]++
		}
	}
	if len(kinds) != 3 {
		t.Fatalf("eight rounds never produced all three question types: %+v", kinds)
	}
}

func TestCodeAnalysisNeedsCodeInTheEvidence(t *testing.T) {
	withoutCode := strings.ReplaceAll(sampleGenerationPrompt, "```", "")
	for round := 1; round <= 6; round++ {
		for _, question := range completeGeneration(t, withoutCode, round).Questions {
			if question.Type == string(domain.LearningQuestionCodeAnalysis) {
				t.Fatalf("round %d asked for code analysis over evidence holding no code", round)
			}
		}
	}
}

// TestNoCodeEvidenceNeverProducesCodeSnippetWording is the regression test for the defect
// this package shipped with: the downgrade replaced the type, the goal and the rubric while
// the question text kept coming from the code-reading template, so a learner was told to
// read a fragment that was never in their notes. The type check alone passed, which is why
// the wording is what is asserted here - in every field a learner or a grader reads.
func TestNoCodeEvidenceNeverProducesCodeSnippetWording(t *testing.T) {
	withoutCode := strings.ReplaceAll(sampleGenerationPrompt, "```", "")
	// "配置" is deliberately not on this list: notes can discuss a setting in prose, and
	// asking about it is lawful. These are the phrases that assert a fragment is present.
	claims := []string{"代码", "片段", "```", "配置项", "这段配置"}
	for round := 1; round <= 10; round++ {
		for index, question := range completeGeneration(t, withoutCode, round).Questions {
			if question.Type == string(domain.LearningQuestionCodeAnalysis) {
				t.Fatalf("round %d question %d is still code_analysis over codeless evidence", round, index+1)
			}
			readable := append([]string{question.Question, question.ReferenceAnswer, question.AssessmentGoal},
				question.Rubric...)
			for _, text := range readable {
				for _, claim := range claims {
					if strings.Contains(text, claim) {
						t.Fatalf("round %d question %d claims %q that the evidence cannot support:\n%q",
							round, index+1, claim, text)
					}
				}
			}
		}
	}

	// The other direction, so the assertion above is not vacuous: with a fenced block in
	// the evidence, the code-reading wording is reachable. If this never fired, "no
	// question mentions a fragment" would prove nothing about the rule.
	asked := 0
	for round := 1; round <= 10; round++ {
		for _, question := range completeGeneration(t, sampleGenerationPrompt, round).Questions {
			if question.Type != string(domain.LearningQuestionCodeAnalysis) {
				continue
			}
			asked++
			if !strings.Contains(question.Question, "代码片段") {
				t.Fatalf("a code_analysis question does not say what it asks the learner to read: %q",
					question.Question)
			}
			if !containsAny(question.Rubric, "配置项") {
				t.Fatalf("the rubric for a code question lost its own subject: %+v", question.Rubric)
			}
		}
	}
	if asked == 0 {
		t.Fatal("ten rounds over evidence with a code fence never asked a code question, so the check above tests nothing")
	}
}

func containsAny(items []string, phrase string) bool {
	for _, item := range items {
		if strings.Contains(item, phrase) {
			return true
		}
	}
	return false
}

func TestGradingAnswerIsAContractLegalGrade(t *testing.T) {
	chat := NewLearning()
	seen := map[int]struct{}{}
	for index, question := range completeGeneration(t, sampleGenerationPrompt, 1).Questions {
		payload := gradingPayload(t, question.Question, "学习者的回答。")
		content := complete(t, chat, payload)
		decoded := decodeStrict[gradePayload](t, content)
		if decoded.Correctness < 0 || decoded.Correctness > domain.MaxLearningCorrectness {
			t.Fatalf("correctness %d outside 0..%d", decoded.Correctness, domain.MaxLearningCorrectness)
		}
		if decoded.Completeness < 0 || decoded.Completeness > domain.MaxLearningCompleteness {
			t.Fatalf("completeness %d outside 0..%d", decoded.Completeness, domain.MaxLearningCompleteness)
		}
		if decoded.Clarity < 0 || decoded.Clarity > domain.MaxLearningClarity {
			t.Fatalf("clarity %d outside 0..%d", decoded.Clarity, domain.MaxLearningClarity)
		}
		if decoded.Confidence < 0.5 || decoded.Confidence > 1 {
			t.Fatalf("confidence %v would leave the attempt uncounted", decoded.Confidence)
		}
		if strings.TrimSpace(decoded.Feedback) == "" {
			t.Fatal("a grade with no feedback is refused by the domain validator")
		}
		if len(decoded.MissingPoints) == 0 {
			t.Fatalf("question %d scored with no gap named, so the weak-point projection would never move", index+1)
		}
		for _, point := range decoded.MissingPoints {
			if strings.TrimSpace(point) == "" {
				t.Fatal("an empty missing point is refused by the domain validator")
			}
		}
		if strings.Contains(content, "reference_answer") || strings.Contains(content, question.ReferenceAnswer) {
			t.Fatalf("the grade echoed the answer key back into the conversation: %s", content)
		}
		seen[decoded.Correctness] = struct{}{}
	}
	// The fixed script chooses by question, so a real quiz exercises the recency-weighted
	// mean with more than one number rather than restating one score three times.
	if len(seen) < 2 {
		t.Fatalf("three questions all got the same verdict, so mastery could not move: %+v", seen)
	}
}

func TestSameQuestionAlwaysGetsTheSameVerdict(t *testing.T) {
	first := complete(t, NewLearning(), gradingPayload(t, "稳定的一题？", "答案甲"))
	second := complete(t, NewLearning(), gradingPayload(t, "稳定的一题？", "答案乙完全不同"))
	if first != second {
		t.Fatalf("the same question scored %s then %s; a restart would move a learner's record", first, second)
	}
}

func TestUnrecognizedPromptsFailRatherThanCrossWires(t *testing.T) {
	chat := NewLearning()
	cases := map[string]string{
		"a chat turn":                     "请用一句话解释 RDB。",
		"an evidence block with no topic": "[证据]\n[1] 出自《手册》 x\n",
		"a grading payload with no key":   `{"question":"","student_answer":"甲"}`,
		"a grading payload with an extra": `{"question":"题？","student_answer":"甲","total":100}`,
		"two JSON values":                 `{"question":"题？","student_answer":"甲"}{"question":"再一题？","student_answer":"乙"}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := chat.Complete(context.Background(), model.Request{Messages: []model.Message{
				{Role: model.RoleSystem, Content: "你是 GopherLearn 的某个角色"},
				{Role: model.RoleUser, Content: content},
			}})
			if !errors.Is(err, ErrUnrecognizedRequest) {
				t.Fatalf("error = %v, want the loud refusal", err)
			}
		})
	}
	// None of those answers may be billed as a generation or a grading.
	if generations, gradings := chat.Calls(); generations != 0 || gradings != 0 {
		t.Fatalf("refused requests counted as %d generations and %d gradings", generations, gradings)
	}
}

func TestCanceledContextIsAnsweredWithTheCancelation(t *testing.T) {
	chat := NewLearning()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := chat.Complete(ctx, generationRequest(sampleGenerationPrompt)); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want the caller's cancellation to be respected", err)
	}
}

func TestStreamProducesExactlyOneDeltaWithTheSameContent(t *testing.T) {
	chat := NewLearning()
	var deltas []string
	streamed, err := chat.Stream(context.Background(), generationRequest(sampleGenerationPrompt),
		func(delta string) error {
			deltas = append(deltas, delta)
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(deltas) != 1 {
		t.Fatalf("%d deltas, want the whole scripted answer in one", len(deltas))
	}
	if deltas[0] != streamed.Content {
		t.Fatalf("the stream emitted %q and returned %q", deltas[0], streamed.Content)
	}
	if strings.Contains(streamed.Content, "[DONE]") {
		t.Fatalf("the completion marker leaked into answer text: %q", streamed.Content)
	}
	if streamed.FinishReason != model.FinishStop {
		t.Fatalf("finish reason = %q", streamed.FinishReason)
	}

	// A client that goes away mid-write gets the write's error, not a half answer.
	aborted := errors.New("connection closed")
	if _, err := chat.Stream(context.Background(), generationRequest(sampleGenerationPrompt),
		func(string) error { return aborted }); !errors.Is(err, aborted) {
		t.Fatalf("error = %v, want the handler's failure to surface", err)
	}
}

func TestLearningModelCannotBeWiredIntoAnAgentLoop(t *testing.T) {
	// The agent harness asks for model.ToolChatModel and refuses anything else. This
	// assertion is the reason a local learning flag cannot turn a chat turn into a
	// scripted answer even if someone wired it there by mistake.
	if _, capable := any(NewLearning()).(model.ToolChatModel); capable {
		t.Fatal("the scripted learning model advertises tool calling")
	}
	var _ model.ChatModel = NewLearning()
}

func TestLearningModelNameIsTheOnlyValueServed(t *testing.T) {
	if LearningModelName != "deterministic" {
		t.Fatalf("name = %q; app's configuration gate and the docs name this literal", LearningModelName)
	}
}

// --- helpers ---

func generationRequest(content string) model.Request {
	return model.Request{Messages: []model.Message{
		{Role: model.RoleSystem, Content: "出题系统提示"},
		{Role: model.RoleUser, Content: content},
	}}
}

func complete(t *testing.T, chat *Learning, user string) string {
	t.Helper()
	completion, err := chat.Complete(context.Background(), generationRequest(user))
	if err != nil {
		t.Fatal(err)
	}
	if completion.Content == "" {
		t.Fatal("an empty completion would be refused by the caller as a provider fault")
	}
	return completion.Content
}

func completeGeneration(t *testing.T, user string, round int) generationAnswer {
	t.Helper()
	if round < 1 {
		t.Fatal("rounds start at one")
	}
	chat := NewLearning()
	// The script keys off its own call count, so reaching a given round means making that
	// many generation calls.
	var answer generationAnswer
	for current := 1; current <= round; current++ {
		content := complete(t, chat, user)
		if err := json.Unmarshal([]byte(content), &answer); err != nil {
			t.Fatalf("round %d answered unparseable text: %v\n%s", current, err, content)
		}
	}
	if generations, gradings := chat.Calls(); generations != round || gradings != 0 {
		t.Fatalf("after %d rounds: %d generations, %d gradings", round, generations, gradings)
	}
	return answer
}

// decodeStrict mirrors the workflow's decoder: an unagreed key is a misunderstanding, and
// a test that ignored one would pass while the request failed in production.
func decodeStrict[T any](t *testing.T, content string) T {
	t.Helper()
	var value T
	decoder := json.NewDecoder(bytes.NewBufferString(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("decode %q: %v", content, err)
	}
	if err := decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		t.Fatalf("trailing content after the object: %v", err)
	}
	return value
}

func gradingPayload(t *testing.T, question, answer string) string {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"question":         question,
		"reference_answer": "参考答案",
		"rubric":           []string{"要点一"},
		"evidence":         []string{"[1] 证据"},
		"student_answer":   answer,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func normalizeForTest(value string) string {
	var builder strings.Builder
	for _, symbol := range strings.ToLower(value) {
		if symbol >= 'a' && symbol <= 'z' || symbol >= '0' && symbol <= '9' || symbol > 0x2E80 {
			builder.WriteRune(symbol)
		}
	}
	return builder.String()
}
