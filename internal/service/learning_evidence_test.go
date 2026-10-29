package service

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/retrieval"
)

func TestLearningBudgetNormalizeRefusesZeroAsAMeaningForNothing(t *testing.T) {
	defaults := LearningBudget{}.Normalize()
	if defaults.MaxEvidenceItems != DefaultLearningEvidenceItems || defaults.TotalRunes != DefaultLearningEvidenceTotalRunes {
		t.Fatalf("defaults = %+v, want the documented budget", defaults)
	}
	// Zero means "not configured" everywhere in this project, never "generate nothing":
	// a caller that left a field out must not end up with a quiz that has no evidence.
	if got := (LearningBudget{MaxEvidenceItems: 0}).Normalize(); got.MaxEvidenceItems != DefaultLearningEvidenceItems {
		t.Fatalf("unconfigured item count = %d, want the default", got.MaxEvidenceItems)
	}
	// A pool narrower than the set it must fill would make "prefer unseen evidence" a
	// preference with nothing to prefer among.
	if got := (LearningBudget{CandidateLimit: 2, MaxEvidenceItems: 8}).Normalize(); got.CandidateLimit < got.MaxEvidenceItems {
		t.Fatalf("candidate pool %d is narrower than the %d items it must supply", got.CandidateLimit, got.MaxEvidenceItems)
	}
	// The prompt window cannot exceed the window that was read, and both stay bounded.
	if got := (LearningBudget{HistoryLimit: 5, PromptHistory: 50}).Normalize(); got.PromptHistory != 5 {
		t.Fatalf("prompt history %d exceeds the %d rows read", got.PromptHistory, got.HistoryLimit)
	}
	if got := (LearningBudget{TotalRunes: 10}).Normalize(); got.TotalRunes < DefaultLearningEvidenceItemRunes {
		t.Fatalf("total budget %d cannot even hold one item", got.TotalRunes)
	}
}

func TestSelectLearningEvidenceNeverRepeatsOrOverflows(t *testing.T) {
	chunks := learningChunks(12)
	for _, budget := range []LearningBudget{
		{},
		{MaxEvidenceItems: 3},
		{MaxEvidenceItems: 12},
		{MaxItemRunes: 130, TotalRunes: 500},
		{CandidateLimit: 4, MaxEvidenceItems: 4},
	} {
		evidence := SelectLearningEvidence(LearningEvidenceRequest{
			Candidates: chunks, Titles: map[string]string{"doc-a": "Redis 笔记", "doc-b": "Go 笔记"},
		}, budget)
		normalized := budget.Normalize()
		if len(evidence) > normalized.MaxEvidenceItems {
			t.Fatalf("budget %+v returned %d items, want at most %d", budget, len(evidence), normalized.MaxEvidenceItems)
		}
		seen := map[string]struct{}{}
		total := 0
		for index, item := range evidence {
			if item.Ordinal != index+1 {
				t.Fatalf("item %d carries ordinal %d, want the ordinals to run from 1 in returned order",
					index, item.Ordinal)
			}
			identity := item.Identity()
			if _, repeated := seen[identity]; repeated {
				t.Fatalf("budget %+v selected the same chunk twice: %q", budget, identity)
			}
			seen[identity] = struct{}{}
			total += len([]rune(item.Snapshot))
			if err := item.Validate(); err != nil {
				t.Fatalf("budget %+v produced unlawful evidence: %v", budget, err)
			}
		}
		if total > normalized.TotalRunes {
			t.Fatalf("budget %+v produced %d runes of snapshots, want at most %d",
				budget, total, normalized.TotalRunes)
		}
		// Selecting twice from the same input must give the same answer, because a quiz
		// that changes between two requests is a quiz nobody can debug.
		if again := SelectLearningEvidence(LearningEvidenceRequest{
			Candidates: chunks, Titles: map[string]string{"doc-a": "Redis 笔记", "doc-b": "Go 笔记"},
		}, budget); !reflect.DeepEqual(evidence, again) {
			t.Fatalf("budget %+v selected different evidence on the second call", budget)
		}
	}
}

func TestSelectLearningEvidenceSpreadsAcrossDocumentsAndSections(t *testing.T) {
	// Three documents, each with its own sections and non-adjacent chunk positions.
	var candidates []retrieval.Result
	for document := range 3 {
		for section := range 4 {
			candidates = append(candidates, retrieval.Result{Chunk: domain.DocumentChunk{
				ID: fmt.Sprintf("chk-%d-%d", document, section), DocumentID: fmt.Sprintf("doc-%d", document),
				CollectionID: "col-1", Version: 1, Order: section * 2,
				Section: fmt.Sprintf("文档 %d 的第 %d 节", document, section),
				Text:    strings.Repeat("持久化与淘汰策略的取舍说明", 20) + fmt.Sprintf(" %d-%d", document, section),
			}})
		}
	}
	evidence := SelectLearningEvidence(LearningEvidenceRequest{Candidates: candidates},
		LearningBudget{MaxEvidenceItems: 8})
	if len(evidence) != 8 {
		t.Fatalf("selected %d items from a pool that can supply 8, want 8", len(evidence))
	}
	perDocument := map[string]int{}
	for _, item := range evidence {
		perDocument[item.DocumentID]++
	}
	for document, count := range perDocument {
		if count > DefaultLearningMaxItemsPerDocument {
			t.Fatalf("%s supplied %d of %d items, want at most %d",
				document, count, len(evidence), DefaultLearningMaxItemsPerDocument)
		}
	}
	for index := 1; index < len(evidence); index++ {
		previous, current := evidence[index-1], evidence[index]
		if previous.DocumentID == current.DocumentID && previous.ChunkOrder+1 == current.ChunkOrder {
			t.Fatalf("adjacent chunks %d and %d of one document were both selected",
				previous.ChunkOrder, current.ChunkOrder)
		}
	}
}

func TestSelectLearningEvidencePrefersUnseenAndReservesWeakPoints(t *testing.T) {
	// The pool's own order has the first chunk ahead, and the learner's history says the
	// first chunk already backed a question. Freshness has to beat the retriever's order.
	var candidates []retrieval.Result
	for index := range 6 {
		candidates = append(candidates, retrieval.Result{Chunk: domain.DocumentChunk{
			ID: fmt.Sprintf("chk-%d", index), DocumentID: "doc-a", CollectionID: "col-1", Version: 1,
			Order:   index * 2,
			Section: fmt.Sprintf("章节 %d", index),
			Text:    "Redis 的持久化与内存淘汰策略。" + strings.Repeat("补充说明", 40) + fmt.Sprintf(" %d", index),
		}})
	}
	seen := func(ordinal int, identity string) domain.LearningEvidence {
		return domain.LearningEvidence{Ordinal: ordinal, DocumentID: "doc-a", ChunkID: identity, DocumentVer: 1,
			StartLine: 1, EndLine: 4, Snapshot: "旧内容"}
	}
	history := []domain.LearningQuestion{{
		Ordinal: 1, Type: domain.LearningQuestionShortAnswer, Question: "旧题？", AssessmentGoal: "旧目标",
		ReferenceAnswer: "旧答案", Rubric: []string{"点"},
		Evidence: []domain.LearningEvidence{seen(1, "chk-0"), seen(2, "chk-1")},
	}}

	fresh := SelectLearningEvidence(LearningEvidenceRequest{Candidates: candidates, History: history,
		Titles: map[string]string{"doc-a": "A"}}, LearningBudget{MaxEvidenceItems: 3})
	for _, item := range fresh {
		if item.ChunkID == "chk-0" || item.ChunkID == "chk-1" {
			t.Fatalf("unseen evidence was available but %q was chosen anyway", item.ChunkID)
		}
	}

	// A weak point that clearly appears in a chunk is claimed even when it breaks the
	// spread: re-testing what the learner failed outranks variety. The affinity text is
	// deliberately the weak point itself so the coverage measure has something to match.
	candidates[4].Chunk.Text = "一致性哈希的虚拟节点数目决定了负载迁移的粒度。" +
		strings.Repeat("虚拟节点的数目与负载迁移的粒度相关。", 12)
	reserved := SelectLearningEvidence(LearningEvidenceRequest{Candidates: candidates,
		Titles: map[string]string{"doc-a": "A"}, WeakPoints: []string{"虚拟节点的数目"}},
		LearningBudget{MaxEvidenceItems: 6})
	var hit bool
	for _, item := range reserved {
		if item.ChunkID == "chk-4" {
			hit = true
		}
	}
	if !hit {
		t.Fatalf("the chunk carrying the learner's weak point was not selected: %+v", reserved)
	}

	// A gap from fewer than three valid attempts is not reliable, and the caller's gate -
	// not the selector - decides that. Fed nothing, the selector simply ranks by freshness
	// and order, which is what a first quiz needs.
	plain := SelectLearningEvidence(LearningEvidenceRequest{Candidates: candidates,
		Titles: map[string]string{"doc-a": "A"}}, LearningBudget{MaxEvidenceItems: 2})
	if len(plain) != 2 {
		t.Fatalf("selected %d items, want 2", len(plain))
	}
}

func TestDecodeLearningGradeRefusesEverythingTheContractForbids(t *testing.T) {
	cases := []struct {
		name  string
		reply string
		want  error
	}{
		{"a total the model computed itself",
			`{"correctness":60,"completeness":25,"clarity":15,"total":100,"feedback":"好。","missing_points":[],"confidence":1}`,
			ErrLearningInvalidGrade},
		{"a mastery verdict the model claimed",
			`{"correctness":50,"completeness":20,"clarity":10,"feedback":"好。","missing_points":[],"confidence":1,"mastered":true}`,
			ErrLearningInvalidGrade},
		{"a dimension above its ceiling",
			`{"correctness":61,"completeness":20,"clarity":10,"feedback":"好。","missing_points":[],"confidence":1}`,
			ErrLearningInvalidGrade},
		{"a negative dimension",
			`{"correctness":-1,"completeness":20,"clarity":10,"feedback":"好。","missing_points":[],"confidence":1}`,
			ErrLearningInvalidGrade},
		{"confidence outside nought to one",
			`{"correctness":50,"completeness":20,"clarity":10,"feedback":"好。","missing_points":[],"confidence":1.4}`,
			ErrLearningInvalidGrade},
		{"no feedback",
			`{"correctness":50,"completeness":20,"clarity":10,"feedback":"  ","missing_points":[],"confidence":1}`,
			ErrLearningInvalidGrade},
		{"too many missing points",
			`{"correctness":50,"completeness":20,"clarity":10,"feedback":"好。","missing_points":` +
				`["一","二","三","四","五","六","七","八","九"],"confidence":1}`, ErrLearningInvalidGrade},
		{"markdown around the object with a trailing value",
			"```json\n{\"correctness\":50,\"completeness\":20,\"clarity\":10,\"feedback\":\"好\"," +
				"\"missing_points\":[],\"confidence\":1}\n```\n再补一句", ErrLearningInvalidGrade},
		{"not json at all", `评分：七十分`, ErrLearningInvalidGrade},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := decodeLearningGrade(testCase.reply); !errors.Is(err, testCase.want) {
				t.Fatalf("reply %q = %v, want %v", testCase.reply, err, testCase.want)
			}
		})
	}
	// A lawful response is accepted and its total is the server's arithmetic, not the
	// model's: the dimensions are all that a grade is made of.
	grade, err := decodeLearningGrade(`{"correctness":45,"completeness":20,"clarity":12,` +
		`"feedback":"两个目标都答到了，但没说清取舍。","missing_points":["恢复速度与写入开销的取舍"],"confidence":0.8}`)
	if err != nil {
		t.Fatal(err)
	}
	if grade.Total() != 77 {
		t.Fatalf("total = %d, want 77 from 45+20+12", grade.Total())
	}
	if len(grade.MissingPoints) != 1 {
		t.Fatalf("missing points = %q", grade.MissingPoints)
	}
}

func TestLearningNoveltyRulesSeparateRewritesFromDistinctQuestions(t *testing.T) {
	chunk := func(ids ...string) []domain.LearningEvidence {
		items := make([]domain.LearningEvidence, 0, len(ids))
		for index, id := range ids {
			items = append(items, domain.LearningEvidence{Ordinal: index + 1, DocumentID: "doc-a",
				ChunkID: id, DocumentVer: 1, StartLine: 1, EndLine: 3, Snapshot: "同一份笔记"})
		}
		return items
	}
	base := domain.LearningQuestion{
		Type:           domain.LearningQuestionShortAnswer,
		Question:       "请说明 Redis 中 RDB 与 AOF 两种持久化方式各自的取舍。",
		AssessmentGoal: "区分 RDB 与 AOF 在恢复速度与数据完整性上的取舍",
		Evidence:       chunk("chk-1", "chk-2"),
	}
	cases := []struct {
		name   string
		other  domain.LearningQuestion
		want   bool
		reason string
	}{
		{
			name: "the identical question", other: base, want: true,
			reason: "an exact repeat needs no second signal to be recognised",
		},
		{
			name: "a reworded question over the same evidence",
			other: domain.LearningQuestion{Type: base.Type,
				Question:       "请说明 Redis 中 RDB 与 AOF 这两种持久化方式各自有什么取舍？",
				AssessmentGoal: "区分 RDB 与 AOF 在恢复速度与数据完整性上的取舍",
				Evidence:       chunk("chk-1", "chk-2")},
			want: true, reason: "near-identical wording plus the same evidence is one question",
		},
		{
			name: "the same sentence over different evidence",
			other: domain.LearningQuestion{Type: base.Type, Question: base.Question,
				AssessmentGoal: base.AssessmentGoal, Evidence: chunk("chk-9")},
			want:   true,
			reason: "identical text is a repeat whatever it cites - the text rule alone catches it",
		},
		{
			name: "a different question on one shared chunk",
			other: domain.LearningQuestion{Type: domain.LearningQuestionConceptComparison,
				Question:       "请比较 Redis 的主从复制与哨兵机制在故障转移上的差别。",
				AssessmentGoal: "说明主从复制与哨兵各自的职责边界",
				Evidence:       chunk("chk-1", "chk-5")},
			want:   false,
			reason: "one overlapping chunk must not condemn every question that touches the same page",
		},
		{
			name: "the same goal phrased alike over different chunks",
			other: domain.LearningQuestion{Type: base.Type,
				Question:       "请解释为什么 RDB 恢复更快而 AOF 丢得更少。",
				AssessmentGoal: "区分 RDB 与 AOF 在恢复速度与数据完整性上的取舍。",
				Evidence:       chunk("chk-3", "chk-4")},
			want:   false,
			reason: "shared wording of a goal with disjoint evidence is a genuinely new question",
		},
		{
			name:  "an empty question text",
			other: domain.LearningQuestion{Type: base.Type, Question: "   ", AssessmentGoal: base.AssessmentGoal},
			want:  false, reason: "nothing to compare is never a repeat",
		},
		{
			// The wording rule is the only one that can decide this pair. The goals are about
			// different things, so neither goal-based rule applies even though the texts
			// resemble each other, and one of two citations is shared, which is well above the
			// evidence floor the wording rule needs but far from the "same material" the
			// goal-based rules demand. Without a row like this the table is over-determined:
			// its "reworded question" row has identical goals, so it stays a duplicate no
			// matter where the wording threshold is set - which is how the loosened threshold
			// in this rule first survived a mutation run.
			name: "near-identical wording over half-shared evidence and an unrelated goal",
			other: domain.LearningQuestion{Type: base.Type,
				Question:       "请说明 Redis 中 RDB 与 AOF 两种持久化方式各自的取舍是什么？",
				AssessmentGoal: "列举副本集重新搭建时成员的优先顺序",
				Evidence:       chunk("chk-1", "chk-8")},
			want:   true,
			reason: "the question is the same one re-asked; only the wording rule can see that",
		},
		{
			name: "the same half-shared evidence under clearly different wording",
			other: domain.LearningQuestion{Type: base.Type,
				Question:       "哨兵进程如何在主节点失联后选出新的主节点？",
				AssessmentGoal: "列举副本集重新搭建时成员的优先顺序",
				Evidence:       chunk("chk-1", "chk-8")},
			want:   false,
			reason: "overlapping citations alone must not condemn an unrelated question",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := learningQuestionsDuplicate(base, testCase.other); got != testCase.want {
				t.Fatalf("duplicate = %v, want %v: %s", got, testCase.want, testCase.reason)
			}
			if got := learningQuestionsDuplicate(testCase.other, base); got != testCase.want {
				t.Fatalf("the reverse comparison = %v, want %v: the rule must be symmetric", got, testCase.want)
			}
		})
	}
}

func TestDecodeGeneratedQuestionsRefusesUnlawfulBatches(t *testing.T) {
	input := learningGenerationInput{
		Topic: "Redis 持久化",
		Evidence: []domain.LearningEvidence{
			{Ordinal: 1, DocumentID: "doc-a", ChunkID: "chk-1", DocumentVer: 1, StartLine: 1, EndLine: 3,
				Snapshot: "RDB 与 AOF 的取舍。"},
			{Ordinal: 2, DocumentID: "doc-a", ChunkID: "chk-2", DocumentVer: 1, StartLine: 4, EndLine: 8,
				Snapshot: "重写与追加。"},
			{Ordinal: 3, DocumentID: "doc-a", ChunkID: "chk-3", DocumentVer: 1, StartLine: 9, EndLine: 12,
				Snapshot: "```go\nappendonly yes\n```"},
		},
	}
	lawful := learningQuestions([]int{1}, []int{2}, []int{3})
	if _, err := decodeGeneratedQuestions(lawful, input); err != nil {
		t.Fatalf("a lawful batch was refused: %v", err)
	}

	// Unknown fields, a wrong count, a citation to a number that was never shown, a
	// duplicated citation, an unlawful type and a code question over prose are each a
	// refusal. Every one is checked with the whole batch visible, because a set of three
	// questions is one unit: storing the two that passed would leave a quiz whose missing
	// item cannot be told apart from one that has not been reached yet.
	cases := []struct {
		name  string
		reply string
	}{
		{"an extra field", strings.Replace(lawful, `"type":"short_answer"`, `"type":"short_answer","points":10`, 1)},
		{"a missing question", `{"questions":[]}`},
		{"a citation outside the set", strings.Replace(lawful, `"evidence":[3]`, `"evidence":[7]`, 1)},
		{"an unknown type", strings.Replace(lawful, `"type":"short_answer"`, `"type":"essay"`, 1)},
		{"a blank rubric", strings.Replace(lawful, `"rubric":["指出两种机制","说出一种取舍"]`, `"rubric":["  "]`, 1)},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := decodeGeneratedQuestions(testCase.reply, input); err == nil {
				t.Fatalf("reply %q was accepted", testCase.reply)
			}
		})
	}

	// A code question is lawful exactly when one cited snapshot really carries code.
	code := learningGeneratorReply(
		questionSpec("short_answer", "第一题？", "目标一", 1),
		questionSpec("short_answer", "第二题？", "目标二", 2),
		questionSpec("code_analysis", "第三题：这段配置在边界上会怎样？", "目标三", 3),
	)
	decoded, err := decodeGeneratedQuestions(code, input)
	if err != nil {
		t.Fatalf("a code question over code evidence was refused: %v", err)
	}
	if decoded[2].Type != domain.LearningQuestionCodeAnalysis {
		t.Fatalf("third question type = %q", decoded[2].Type)
	}
	// The same question, cited against a set whose third chunk no longer carries code.
	// Losing the code is what must refuse the question - not losing the citation, which
	// would fail for the wrong reason and leave the real rule untested.
	withoutEvidence := append([]domain.LearningEvidence(nil), input.Evidence...)
	withoutEvidence[2].Snapshot = "appendonly yes 这一行开启追加日志。"
	noCode := input
	noCode.Evidence = withoutEvidence
	if _, err := decodeGeneratedQuestions(code, noCode); err == nil {
		t.Fatal("a code question was accepted with no code in the evidence")
	}

	// A batch whose three questions state the same goal measures one thing three times.
	sameGoal := learningGeneratorReply(
		questionSpec("short_answer", "第一题：说明快照的作用？", "同一个目标", 1),
		questionSpec("short_answer", "第二题：说明追加日志的作用？", "同一个目标", 2),
		questionSpec("short_answer", "第三题：说明两者如何配合？", "同一个目标", 3),
	)
	if _, err := decodeGeneratedQuestions(sameGoal, input); err == nil {
		t.Fatal("three questions with one assessment goal were accepted")
	}
}

// questionSpec is a one-line question spec builder for the decode cases above. The
// evidence numbers are chosen by the caller because the code-question rule turns on
// which chunk a question cites, not on what it asks.
func questionSpec(kind, question, goal string, evidence ...int) learningQuestionSpec {
	return learningQuestionSpec{Type: kind, Question: question, Goal: goal,
		Answer: "参考答案", Rubric: []string{"评分点一", "评分点二"}, Evidence: evidence}
}

func TestLearningTextMeasurementsAreDeterministicAndBounded(t *testing.T) {
	if got := normalizeLearningText("  REdis 的-RDB ！\n"); got != "redis的rdb" {
		t.Fatalf("normalized = %q", got)
	}
	if learningDice("", "x") != 0 || learningDice("x", "") != 0 {
		t.Fatal("an empty string has no similarity to anything, including itself")
	}
	if learningDice("同一句话", "同一句话") != 1 {
		t.Fatal("a string is fully similar to itself")
	}
	// Coverage is deliberately asymmetric: a long text can fully contain a short phrase
	// while the reverse is not true, which is what makes it the right measure for
	// "does this chunk speak to this gap".
	long := strings.Repeat("虚拟节点的数目决定了负载迁移的粒度", 4)
	if learningCoverage(long, "虚拟节点的数目") != 1 {
		t.Fatal("a contained phrase must read as fully covered")
	}
	if learningCoverage("虚拟节点的数目", long) >= 1 {
		t.Fatal("a phrase cannot cover a text several times its length")
	}
	if learningMaxCoverage("anything", nil) != 0 {
		t.Fatal("no phrases means no affinity")
	}
	if clipped := clipLearningRunes("一二三四五", 3); clipped != "一二三…" {
		t.Fatalf("clip = %q, want the cut to be marked", clipped)
	}
	if clipLearningRunes("一二", 5) != "一二" {
		t.Fatal("text within the limit must come back untouched")
	}
	if clipLearningRunes("一二三四五", 0) != "" {
		t.Fatal("a zero budget holds nothing, and must not hold a whole string")
	}
	// A single rune still has a comparable signature; returning no bigrams would make
	// every short string equally unlike everything.
	if len(learningBigrams("字")) != 1 {
		t.Fatalf("one rune yielded %d pairs, want one", len(learningBigrams("字")))
	}
}
