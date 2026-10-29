package learning

import (
	"GopherAI/common/rag"
	"GopherAI/model"
	"context"
	"errors"
	"strings"
	"testing"

	modelcomponent "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

type fakeQuestionModel struct {
	content string
	err     error
}

func (m fakeQuestionModel) Generate(context.Context, []*schema.Message, ...modelcomponent.Option) (*schema.Message, error) {
	if m.err != nil {
		return nil, m.err
	}
	return schema.AssistantMessage(m.content, nil), nil
}

type fakeEvidenceRetriever struct {
	results []rag.RetrievalResult
	err     error
}

type sequenceQuestionModel struct {
	responses []string
	calls     int
}

func (m *sequenceQuestionModel) Generate(context.Context, []*schema.Message, ...modelcomponent.Option) (*schema.Message, error) {
	index := m.calls
	m.calls++
	if index >= len(m.responses) {
		index = len(m.responses) - 1
	}
	return schema.AssistantMessage(m.responses[index], nil), nil
}

func (r fakeEvidenceRetriever) Retrieve(context.Context, int64, string, int) ([]rag.RetrievalResult, error) {
	return r.results, r.err
}

func testEvidence() []model.LearningEvidence {
	return []model.LearningEvidence{{
		EvidenceID: 1, DocumentID: "doc-1", DocumentName: "redis.md", DocVersion: 1,
		ChunkID: "chunk-1", TitlePath: []string{"Redis", "过期"}, StartLine: 3, EndLine: 9,
		ContentType: "prose", ContentHash: "hash", Snapshot: "Redis 使用惰性删除和定期删除。",
	}}
}

func TestModelQuestionGeneratorValidatesStructuredOutput(t *testing.T) {
	content := `{"questions":[
{"type":"short_answer","question":"Redis 有哪些过期删除策略？","assessment_goal":"识别过期删除策略","reference_answer":"惰性删除和定期删除","rubric":["说出惰性删除","说出定期删除"],"evidence_ids":[1]},
{"type":"concept_comparison","question":"比较惰性删除和定期删除。","assessment_goal":"比较两种删除时机","reference_answer":"触发时机和资源开销不同","rubric":["说明触发时机"],"evidence_ids":[1]},
{"type":"short_answer","question":"为什么两种策略需要配合？","assessment_goal":"分析组合策略的资源权衡","reference_answer":"平衡内存和CPU开销","rubric":["说明资源权衡"],"evidence_ids":[1]}
]}`
	generator, err := NewModelQuestionGenerator(fakeQuestionModel{content: content})
	if err != nil {
		t.Fatal(err)
	}
	request := QuestionGenerationRequest{Topic: "Redis", Evidence: testEvidence(), Count: 3}
	questions, err := generator.Generate(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(questions) != 3 || questions[0].Evidence[0].DocVersion != 1 {
		t.Fatalf("generated questions = %#v", questions)
	}

	bad, _ := NewModelQuestionGenerator(fakeQuestionModel{content: strings.Replace(content, `"evidence_ids":[1]`, `"evidence_ids":[99]`, 1)})
	if _, err := bad.Generate(context.Background(), request); !errors.Is(err, ErrInvalidQuestionOutput) {
		t.Fatalf("unknown evidence error = %v, want ErrInvalidQuestionOutput", err)
	}

	codeWithoutEvidence, _ := NewModelQuestionGenerator(fakeQuestionModel{content: strings.Replace(content, `"type":"short_answer"`, `"type":"code_analysis"`, 1)})
	if _, err := codeWithoutEvidence.Generate(context.Background(), request); !errors.Is(err, ErrInvalidQuestionOutput) {
		t.Fatalf("code without evidence error = %v, want ErrInvalidQuestionOutput", err)
	}
}

func TestRAGEvidenceProviderCreatesBoundedSnapshot(t *testing.T) {
	retriever := fakeEvidenceRetriever{results: []rag.RetrievalResult{{
		DocID: "doc-1", DocumentName: "rpc.md", DocVersion: 3, ChunkID: "chunk-9",
		TitlePath: []string{"协议"}, StartLine: 20, EndLine: 40, ChunkType: "code",
		RawContent: strings.Repeat("packet buffer code ", 1000),
	}}}
	provider, err := NewRAGEvidenceProvider(retriever)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := provider.Evidence(context.Background(), EvidenceRequest{UserID: 1, Topic: "RPC 粘包"})
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence) != 1 || evidence[0].ContentHash == "" || rag.EstimateTokens(evidence[0].Snapshot) > maxEvidenceSnapshotTokens {
		t.Fatalf("evidence = %#v", evidence)
	}

	emptyProvider, _ := NewRAGEvidenceProvider(fakeEvidenceRetriever{})
	if _, err := emptyProvider.Evidence(context.Background(), EvidenceRequest{UserID: 1, Topic: "missing"}); !errors.Is(err, ErrNoLearningEvidence) {
		t.Fatalf("empty evidence error = %v", err)
	}
	unavailableProvider, _ := NewRAGEvidenceProvider(fakeEvidenceRetriever{err: &rag.RetrievalUnavailableError{Reason: rag.RetrievalReasonLexical, Cause: errors.New("redis down")}})
	if _, err := unavailableProvider.Evidence(context.Background(), EvidenceRequest{UserID: 1, Topic: "missing"}); !errors.Is(err, rag.ErrRetrievalUnavailable) {
		t.Fatalf("unavailable evidence error = %v", err)
	}
}

func TestCoverageSelectorPrefersUnusedAndDiversifiesSections(t *testing.T) {
	results := []rag.RetrievalResult{
		learningResult("doc-a", "used-1", 1, "基础", "已经考过的基础内容"),
		learningResult("doc-a", "used-2", 2, "基础", "已经考过的另一段"),
		learningResult("doc-a", "fresh-1", 3, "基础", "新的基础边界"),
		learningResult("doc-a", "fresh-2", 4, "进阶", "新的进阶内容"),
		learningResult("doc-b", "fresh-3", 1, "故障", "新的故障处理"),
	}
	history := []model.LearningQuestion{{Evidence: []model.LearningEvidence{
		{DocumentID: "doc-a", DocVersion: 1, ChunkID: "used-1", ContentHash: "hash-used-1"},
		{DocumentID: "doc-a", DocVersion: 1, ChunkID: "used-2", ContentHash: "hash-used-2"},
	}}}
	selected := selectLearningEvidence(results, history, nil, 3)
	if len(selected) != 3 {
		t.Fatalf("selected %d results, want 3", len(selected))
	}
	for _, item := range selected {
		if strings.HasPrefix(item.ChunkID, "used-") {
			t.Fatalf("used evidence selected while three fresh chunks exist: %#v", selected)
		}
	}
	sections := map[string]bool{}
	for _, item := range selected {
		sections[strings.Join(item.TitlePath, ">")] = true
	}
	if len(sections) < 2 {
		t.Fatalf("selector did not diversify sections: %#v", selected)
	}
}

func TestCoverageSelectorReservesReliableWeakPoint(t *testing.T) {
	results := []rag.RetrievalResult{
		learningResult("doc-a", "fresh-1", 1, "基础", "全新的缓存基础内容"),
		learningResult("doc-a", "fresh-2", 3, "策略", "全新的过期策略内容"),
		learningResult("doc-b", "weak-used", 1, "异常", "缓存击穿与缓存穿透的区别和处理方式"),
	}
	history := []model.LearningQuestion{{Evidence: []model.LearningEvidence{{
		DocumentID: "doc-b", DocVersion: 1, ChunkID: "weak-used", ContentHash: "hash-weak-used",
	}}}}
	selected := selectLearningEvidence(results, history, []string{"缓存击穿与缓存穿透的区别"}, 2)
	if len(selected) != 2 || selected[0].ChunkID != "weak-used" {
		t.Fatalf("reliable weak-point evidence was not reserved: %#v", selected)
	}
}

func TestQuestionGeneratorRetriesCrossSessionParaphrase(t *testing.T) {
	evidence := testEvidence()
	history := []model.LearningQuestion{{
		Type: model.LearningQuestionShortAnswer, Question: "Redis 的惰性删除和定期删除分别在什么时候触发？",
		AssessmentGoal: "比较两种过期删除策略的触发时机", Evidence: evidence,
	}}
	duplicate := `{"questions":[
{"type":"short_answer","question":"惰性删除与定期删除各自在什么时机执行？","assessment_goal":"比较两种过期删除策略的触发时机","reference_answer":"访问键和周期扫描时","rubric":["两种时机"],"evidence_ids":[1]},
{"type":"concept_comparison","question":"两种策略在资源开销上有什么差异？","assessment_goal":"分析删除策略资源开销","reference_answer":"CPU和内存权衡","rubric":["资源权衡"],"evidence_ids":[1]},
{"type":"short_answer","question":"为什么需要组合使用两种策略？","assessment_goal":"解释组合删除策略","reference_answer":"平衡资源","rubric":["组合原因"],"evidence_ids":[1]}]}`
	unique := `{"questions":[
{"type":"short_answer","question":"如果只使用惰性删除，长期未访问的过期键会带来什么风险？","assessment_goal":"分析单独惰性删除的内存风险","reference_answer":"可能长期占用内存","rubric":["内存占用"],"evidence_ids":[1]},
{"type":"concept_comparison","question":"定期删除的扫描频率如何影响 CPU 与内存？","assessment_goal":"分析扫描频率的资源权衡","reference_answer":"频繁扫描耗CPU，过少扫描占内存","rubric":["双向权衡"],"evidence_ids":[1]},
{"type":"short_answer","question":"设计过期清理策略时为什么不能只追求立即释放？","assessment_goal":"解释过期清理的工程取舍","reference_answer":"需要平衡CPU与内存","rubric":["工程权衡"],"evidence_ids":[1]}]}`
	model := &sequenceQuestionModel{responses: []string{duplicate, unique}}
	generator, err := NewModelQuestionGenerator(model)
	if err != nil {
		t.Fatal(err)
	}
	questions, err := generator.Generate(context.Background(), QuestionGenerationRequest{
		Topic: "Redis", Evidence: evidence, History: history, Count: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if model.calls != 2 || len(questions) != 3 || questions[0].Question == "" {
		t.Fatalf("calls=%d questions=%#v", model.calls, questions)
	}
}

func TestQuestionGeneratorStopsAfterThreeDuplicateAttempts(t *testing.T) {
	evidence := testEvidence()
	history := []model.LearningQuestion{{
		Type: model.LearningQuestionShortAnswer, Question: "Redis 的惰性删除和定期删除分别在什么时候触发？",
		AssessmentGoal: "比较两种过期删除策略的触发时机", Evidence: evidence,
	}}
	duplicate := `{"questions":[
{"type":"short_answer","question":"惰性删除与定期删除各自在什么时机执行？","assessment_goal":"比较两种过期删除策略的触发时机","reference_answer":"访问键和周期扫描时","rubric":["两种时机"],"evidence_ids":[1]},
{"type":"concept_comparison","question":"两种策略在资源开销上有什么差异？","assessment_goal":"分析删除策略资源开销","reference_answer":"CPU和内存权衡","rubric":["资源权衡"],"evidence_ids":[1]},
{"type":"short_answer","question":"为什么需要组合使用两种策略？","assessment_goal":"解释组合删除策略","reference_answer":"平衡资源","rubric":["组合原因"],"evidence_ids":[1]}]}`
	questionModel := &sequenceQuestionModel{responses: []string{duplicate}}
	generator, err := NewModelQuestionGenerator(questionModel)
	if err != nil {
		t.Fatal(err)
	}
	_, err = generator.Generate(context.Background(), QuestionGenerationRequest{
		Topic: "Redis", Evidence: evidence, History: history, Count: 3,
	})
	if !errors.Is(err, ErrInvalidQuestionOutput) || questionModel.calls != maxQuestionAttempts {
		t.Fatalf("error=%v calls=%d, want duplicate failure after %d attempts", err, questionModel.calls, maxQuestionAttempts)
	}
}

func TestNoveltyAllowsSameTopicWithDifferentGoal(t *testing.T) {
	evidence := testEvidence()
	left := model.LearningQuestion{Type: model.LearningQuestionShortAnswer, Question: "Redis 有哪些过期删除策略？", AssessmentGoal: "列举删除策略", Evidence: evidence}
	right := model.LearningQuestion{Type: model.LearningQuestionConceptComparison, Question: "为什么定期删除不能扫描全部键？", AssessmentGoal: "分析全量扫描的性能边界", Evidence: evidence}
	if learningQuestionsDuplicate(left, right) {
		t.Fatal("different assessment goals in the same topic were treated as duplicates")
	}
}

func learningResult(documentID, chunkID string, chunkIndex int, section, content string) rag.RetrievalResult {
	return rag.RetrievalResult{
		DocID: documentID, DocumentName: documentID + ".md", DocVersion: 1,
		ChunkID: chunkID, ChunkIndex: chunkIndex, ChunkType: "prose",
		TitlePath: []string{section}, StartLine: chunkIndex*10 + 1, EndLine: chunkIndex*10 + 5,
		RawContent: content, ContentHash: "hash-" + chunkID,
	}
}
