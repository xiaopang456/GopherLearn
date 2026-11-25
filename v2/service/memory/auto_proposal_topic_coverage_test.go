package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	memorydao "GopherAI/dao/memory"
	"GopherAI/model"
)

// 本文件是「自动提议对主题笔记的过度去重」这一轮的开发样本与零付费回归。
//
// 背景：旧专题的 TOPIC-06 暴露了一个通用问题。旧笔记说 Gin 中间件按注册顺序
// 执行；新候选复述了这一点，同时补充了 c.Abort() 会跳过剩余 handler、却不
// 中断当前请求。真实关系判定看到了新增信息，仍返回 duplicate，整张草稿被跳过。
// 根因不是候选装配（旧笔记与候选全文都完整进入 TopicRelationClassifier），
// 而是判定规则本身：旧的 duplicate 定义是「同一件事、同一主题」，没有任何
// 「候选是否被旧笔记覆盖」的检查，于是「复述 + 新增知识点」被判成纯复述。
//
// 本文件的样本与冻结题集无关：技术主题、措辞、旧笔记都另起，为的是验证规则
// 能否迁移，而不是复现某一道题的答案。
//
// 两层验证，边界必须说清：
//   - 规则层（TestTopicCoverageRuleReplaysTheOldOverDedup）：把修复前后的两
//     条 duplicate 定义按字面写成两个纯函数，在样本声明的事实上跑一遍，证明
//     补充类样本在旧规则下确实被判成 duplicate、在新规则下不是。这是规则差异
//     的对照，不是模型结果。
//   - 程序层（TestAutoProposalTopicCoverageDevSamples）：用确定性替身驱动真实
//     的「提取 → 关系判定 → 草稿」链路，逐样本核对零付费验收的四条不变量。
//     替身只代替模型给出关系，不代替程序处理关系的那段代码。
//
// 判定器在真实模型上的表现不在本文件证明范围内，仍待评测窗口用新题实测。

// topicCoverageSample 是一个开发样本：某个主题下的一条旧笔记、一个关于同一
// 主题的候选，以及这条候选在覆盖规则下应当落到的关系。
type topicCoverageSample struct {
	name       string
	topicKey   string
	oldSubject string
	oldContent string
	// candidate 是新候选的正文。
	candidate string
	// sameMatter 说明候选与旧笔记说的是同一件事。
	sameMatter bool
	// conflictingValue 说明候选给同一件事换了一个当前值。
	conflictingValue bool
	// newPoints 是候选说到、旧笔记没有覆盖的知识点。空列表表示候选只是复述。
	newPoints []string
	// relation 是覆盖规则对这个样本的判定结果。
	relation TopicRelation
	// ruleContains 是替身用来匹配这条候选的片段，ruleTargetHint 是替身用来
	// 找到旧笔记的片段。
	ruleContains string
	ruleTarget   string
}

// topicCoverageDevSamples 是本轮的开发样本：三个技术主题（Go context 取消、
// MySQL 联合索引、Redis 连接池配置），四个关系类别（纯同义复述、同义展开、
// 重合但新增独立知识点、明确更正）。措辞与旧笔记都与冻结题集不同。
var topicCoverageDevSamples = []topicCoverageSample{
	{
		name: "go-context-复述", topicKey: "go-context",
		oldSubject: "context 取消", oldContent: "父 context 被取消时，派生出来的子 context 会一起被取消。",
		candidate:     "context 的取消是级联的：父 context 一旦取消，由它派生的子 context 也会跟着取消。",
		sameMatter:    true,
		newPoints:     nil,
		relation:      TopicRelationDuplicate,
		ruleContains:  "取消是级联的",
		ruleTarget:    "派生出来的子 context",
	},
	{
		name: "go-context-同义展开", topicKey: "go-context",
		oldSubject: "context 取消", oldContent: "父 context 被取消时，派生出来的子 context 会一起被取消。",
		candidate:     "context 取消会向下级联传播：父级 context 取消之后，由它派生出来的子 context 全部变成 canceled 状态。",
		sameMatter:    true,
		newPoints:     nil,
		relation:      TopicRelationDuplicate,
		ruleContains:  "向下级联传播",
		ruleTarget:    "派生出来的子 context",
	},
	{
		name: "go-context-补充", topicKey: "go-context",
		oldSubject: "context 取消", oldContent: "父 context 被取消时，派生出来的子 context 会一起被取消。",
		candidate:     "父 context 被取消时子 context 会一起被取消；另外 cancel() 只是发出取消信号，本身并不阻塞调用方。",
		sameMatter:    true,
		newPoints:     []string{"cancel() 只是发出取消信号，本身并不阻塞调用方"},
		relation:      TopicRelationComplementary,
		ruleContains:  "并不阻塞调用方",
		ruleTarget:    "派生出来的子 context",
	},
	{
		name: "mysql-index-复述", topicKey: "mysql-index",
		oldSubject: "联合索引", oldContent: "联合索引 (a, b, c) 只能从最左一列开始匹配。",
		candidate:     "复合索引 (a, b, c) 的查找必须从最左侧的那一列开始。",
		sameMatter:    true,
		newPoints:     nil,
		relation:      TopicRelationDuplicate,
		ruleContains:  "复合索引",
		ruleTarget:    "只能从最左一列开始匹配",
	},
	{
		name: "mysql-index-同义展开", topicKey: "mysql-index",
		oldSubject: "联合索引", oldContent: "联合索引 (a, b, c) 只能从最左一列开始匹配。",
		candidate:     "联合索引 (a,b,c) 遵循最左前缀匹配：查询要从索引定义的第一列开始命中。",
		sameMatter:    true,
		newPoints:     nil,
		relation:      TopicRelationDuplicate,
		ruleContains:  "遵循最左前缀匹配",
		ruleTarget:    "只能从最左一列开始匹配",
	},
	{
		name: "mysql-index-补充", topicKey: "mysql-index",
		oldSubject: "联合索引", oldContent: "联合索引 (a, b, c) 只能从最左一列开始匹配。",
		candidate:     "联合索引 (a, b, c) 只能从最左一列开始匹配，但范围条件之后的列不能再用于索引下推。",
		sameMatter:    true,
		newPoints:     []string{"范围条件之后的列不能再用于索引下推"},
		relation:      TopicRelationComplementary,
		ruleContains:  "不能再用于索引下推",
		ruleTarget:    "只能从最左一列开始匹配",
	},
	{
		name: "redis-config-复述", topicKey: "redis-config",
		oldSubject: "连接池上限", oldContent: "线上 Redis 连接池的最大连接数固定为 16。",
		candidate:     "我们线上 Redis 的连接池上限是 16 个连接。",
		sameMatter:    true,
		newPoints:     nil,
		relation:      TopicRelationDuplicate,
		ruleContains:  "连接池上限是 16",
		ruleTarget:    "最大连接数固定为 16",
	},
	{
		name: "redis-config-更正", topicKey: "redis-config",
		oldSubject: "连接池上限", oldContent: "线上 Redis 连接池的最大连接数固定为 16。",
		candidate:        "线上 Redis 连接池的最大连接数调整为 64。",
		sameMatter:       true,
		conflictingValue: true,
		newPoints:        []string{"最大连接数调整为 64"},
		relation:         TopicRelationReplacement,
		ruleContains:     "调整为 64",
		ruleTarget:       "最大连接数固定为 16",
	},
}

// oldTopicDuplicateRule 是修复前的 duplicate 定义，按字面写成纯函数：
// 「候选表达的是同一件事、同一主题」就是重复，同义改写、不同类比都不让它
// 变得不同。它没有任何覆盖检查，所以「复述 + 新增知识点」会被判成重复。
func oldTopicDuplicateRule(sample topicCoverageSample) TopicRelation {
	switch {
	case sample.conflictingValue:
		return TopicRelationReplacement
	case sample.sameMatter:
		return TopicRelationDuplicate
	default:
		return TopicRelationIndependent
	}
}

// newTopicDuplicateRule 是修复后的定义：重复是「候选没有增加旧笔记未覆盖的
// 知识点」。旧笔记没覆盖、又值得单独保留的知识点存在时，候选不是重复。
func newTopicDuplicateRule(sample topicCoverageSample) TopicRelation {
	switch {
	case sample.conflictingValue:
		return TopicRelationReplacement
	case len(sample.newPoints) > 0:
		return TopicRelationComplementary
	case sample.sameMatter:
		return TopicRelationDuplicate
	default:
		return TopicRelationIndependent
	}
}

// TestTopicCoverageRuleReplaysTheOldOverDedup 是规则层的对照：补充类样本在
// 旧定义下确实被判成重复（这正是 TOPIC-06 的形状），在新定义下不是；纯复述
// 与同义展开在两条定义下都是重复；明确更正在两条定义下都是替换。
func TestTopicCoverageRuleReplaysTheOldOverDedup(t *testing.T) {
	flipped := 0
	for _, sample := range topicCoverageDevSamples {
		old := oldTopicDuplicateRule(sample)
		now := newTopicDuplicateRule(sample)
		if old != now {
			flipped++
			if sample.relation != now {
				t.Fatalf("%s: new rule=%s, want %s", sample.name, now, sample.relation)
			}
			// 只有补充类样本允许翻转：旧定义把它当成重复而丢掉整张草稿。
			if old != TopicRelationDuplicate || now != TopicRelationComplementary {
				t.Fatalf("%s: unexpected flip %s -> %s", sample.name, old, now)
			}
			continue
		}
		if old != sample.relation {
			t.Fatalf("%s: rule=%s, want %s", sample.name, old, sample.relation)
		}
	}
	if flipped == 0 {
		t.Fatal("no sample flipped: the old and the new rule are indistinguishable")
	}
}

// seedTopicNote 落一条生效的主题笔记：它是候选的比较对象，也是「旧条目继续
// 有效」这条不变量的观察对象。
func seedTopicNote(t *testing.T, acc *finalizeAcceptance, topicKey, subject, content string) *model.LongTermMemory {
	t.Helper()
	written, err := acc.repo.CreateUserMemory(context.Background(), memorydao.ExplicitWriteParams{
		Memory: model.LongTermMemory{
			UserID: acc.userID, Scope: model.MemoryScopeTopic, TopicKey: topicKey,
			Subject: subject, Content: content, StructuredJSON: `{}`,
			Durability: model.MemoryDurabilityStable, SourceRefs: model.StringList{"user_api:seed-topic-coverage"},
			Confidence:  1, Importance: 8, Status: model.LongTermMemoryActive, UserConfirmed: true,
		},
		RequestKey: fmt.Sprintf("seed-topic:%d:%s:%s", acc.userID, topicKey, subject),
		Reason:     "开发样本种子",
		Now:        time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &written.Memory
}

// TestAutoProposalTopicCoverageDevSamples 是程序层的零付费验收：每个开发样本
// 走一遍真实的「提取 → 关系判定 → 草稿」链路，核对四条不变量。
//
//   - 纯复述与同义展开：判定为 duplicate，触发 semantic_duplicate，零草稿；
//   - 有效补充：形成待确认草稿，正文保留新增知识点，旧条目继续有效；
//   - 明确更正：形成替换草稿并钉住正确的旧条目；
//   - 三类都不越权：确认前无正式写入、无替换、无修订，掌握度逐字段一致。
func TestAutoProposalTopicCoverageDevSamples(t *testing.T) {
	for _, sample := range topicCoverageDevSamples {
		sample := sample
		t.Run(sample.name, func(t *testing.T) {
			acc := newFinalizeAcceptance(t)
			ctx := context.Background()
			sessionID := topicCoverageSessionID(sample.name)

			old := seedTopicNote(t, acc, sample.topicKey, sample.oldSubject, sample.oldContent)
			// 掌握度：提取全程不得触碰这一行。
			if err := acc.db.Create(&model.TopicProgress{
				UserID: acc.userID, TopicKey: sample.topicKey, Topic: sample.oldSubject,
				MasteryScore: 72, Confidence: 0.6, ValidAttempts: 4, Level: model.TopicProgressDeveloping,
				CreatedAt: time.Now(), UpdatedAt: time.Now(),
			}).Error; err != nil {
				t.Fatal(err)
			}
			masteryBefore := topicCoverageMastery(t, acc, sample.topicKey)
			memoriesBefore := countRows(t, acc, &model.LongTermMemory{})
			revisionsBefore := countRows(t, acc, &model.MemoryRevision{})

			stored := acc.seedSession(t, ctx, sessionID, nil, []SessionMessage{
				{IsUser: true, Content: "我把这个再确认一下：" + sample.candidate, CreatedAt: time.Now().Add(-time.Minute)},
			})
			// 替身给出关系：它代替模型，不代替程序处理关系的代码。
			acc.topic.inner = NewDeterministicTopicRelationResolver([]DeterministicTopicRule{{
				Contains: sample.ruleContains, Relation: sample.relation, TargetHint: sample.ruleTarget,
			}})
			acc.proposer.proposals = []SessionRawProposal{{
				Scope: model.MemoryScopeTopic, Kind: FinalizeKindUnderstanding,
				TopicKey: sample.topicKey, TopicUnique: true,
				Subject: "我的理解", Content: sample.candidate,
				Durability: model.MemoryDurabilityStable,
				EvidenceIDs: []uint{stored[0].ID}, StatementEvidenceID: stored[0].ID,
			}}

			outcome, err := acc.runner.RunSession(ctx, acc.userID, sessionID)
			if err != nil {
				t.Fatalf("run session: %v", err)
			}

			// 主题判定的比较集只有该主题下的生效条目：别的主题的笔记既不
			// 参与判定，也不会被当成冲突目标。
			if len(acc.topic.saw) != 1 {
				t.Fatalf("topic judgements=%d, want one", len(acc.topic.saw))
			}
			saw := acc.topic.saw[0]
			if len(saw) != 1 || saw[0].ID != old.ID {
				t.Fatalf("judgement range=%+v, want only the topic's own active note %s", saw, old.ID)
			}

			switch sample.relation {
			case TopicRelationDuplicate:
				// 纯复述：跳过，零草稿，旧条目继续有效。
				if len(outcome.Proposals) != 0 || len(outcome.Persisted) != 0 {
					t.Fatalf("proposals=%d persisted=%v, want the restatement skipped", len(outcome.Proposals), outcome.Persisted)
				}
				if len(outcome.Skipped) != 1 || outcome.Skipped[0].Reason != FinalizeSkipSemanticDuplicate {
					t.Fatalf("skipped=%+v, want semantic_duplicate", outcome.Skipped)
				}
				if drafts := acc.pendingDrafts(ctx); len(drafts) != 0 {
					t.Fatalf("drafts=%d, want no card for a pure restatement", len(drafts))
				}
			case TopicRelationComplementary, TopicRelationReplacement:
				// 有效补充与明确更正：形成待确认草稿。
				if len(outcome.Proposals) != 1 || len(outcome.Persisted) != 1 {
					t.Fatalf("proposals=%d persisted=%v skipped=%+v, want one card", len(outcome.Proposals), outcome.Persisted, outcome.Skipped)
				}
				drafts := acc.pendingDrafts(ctx)
				if len(drafts) != 1 {
					t.Fatalf("drafts=%d, want one pending card", len(drafts))
				}
				draft := drafts[0]
				if draft.Status != model.MemoryDraftPending {
					t.Fatalf("draft status=%s, want pending", draft.Status)
				}
				// 正文保留新增知识点。
				for _, point := range sample.newPoints {
					if !strings.Contains(draft.Content, point) {
						t.Fatalf("draft body=%q, want the added point %q kept", draft.Content, point)
					}
				}
				if !strings.Contains(draft.Content, sample.candidate) {
					t.Fatalf("draft body=%q, want the candidate body kept", draft.Content)
				}
				// 草稿钉住正确的旧条目。
				if !stringListContains(draft.ConflictIDs, old.ID) {
					t.Fatalf("conflict ids=%v, want the old note %s pinned", draft.ConflictIDs, old.ID)
				}
				if sample.relation == TopicRelationReplacement && outcome.Proposals[0].Relation.Relation != GlobalRelationReplacement {
					t.Fatalf("relation=%+v, want replacement", outcome.Proposals[0].Relation)
				}
				if sample.relation == TopicRelationReplacement && outcome.Proposals[0].Relation.TargetMemoryID != old.ID {
					t.Fatalf("target=%q, want the old note %s", outcome.Proposals[0].Relation.TargetMemoryID, old.ID)
				}
			default:
				t.Fatalf("sample %s has an unsupported relation %s", sample.name, sample.relation)
			}

			// 不变量：确认前无正式写入、无替换、无修订，旧条目继续有效。
			if got := countRows(t, acc, &model.LongTermMemory{}); got != memoriesBefore {
				t.Fatalf("memories=%d, want %d (no formal write before confirmation)", got, memoriesBefore)
			}
			if got := countRows(t, acc, &model.MemoryRevision{}); got != revisionsBefore {
				t.Fatalf("revisions=%d, want %d (no revision before confirmation)", got, revisionsBefore)
			}
			if got := seedProfileStatus(t, acc, old.ID); got != model.LongTermMemoryActive {
				t.Fatalf("old note status=%s, want active (the old entry keeps standing)", got)
			}
			// 掌握度逐字段一致。
			after := topicCoverageMastery(t, acc, sample.topicKey)
			if after != masteryBefore {
				t.Fatalf("mastery=%+v, want %+v (untouched)", after, masteryBefore)
			}
		})
	}
}

// TestTopicCoverageJudgeSeesTheWholeCandidateAndNote pins the judgement
// boundary the fix depends on: the judge receives the candidate's whole body
// and the whole of the topic's active note, so the coverage comparison it has
// to make is answerable from what it is given. The added point is in the
// payload, not only in the model's head.
func TestTopicCoverageJudgeSeesTheWholeCandidateAndNote(t *testing.T) {
	chat := &memoryChat{response: toolMessage(topicConflictTool,
		`{"relation":"complementary","target_memory_id":"note-1","uncovered_points":["cancel() 只是发出取消信号"],"reason":"旧笔记没有覆盖这一条"}`)}
	classifier, _ := NewTopicRelationClassifier(chat)
	incoming := ExplicitMemoryInput{
		Scope: model.MemoryScopeTopic, TopicKey: "go-context", Subject: "context 取消",
		Content: "父 context 被取消时子 context 会一起被取消；另外 cancel() 只是发出取消信号，本身并不阻塞调用方。",
	}
	existing := []model.LongTermMemory{{
		ID: "note-1", Scope: model.MemoryScopeTopic, TopicKey: "go-context", Subject: "context 取消",
		Content: "父 context 被取消时，派生出来的子 context 会一起被取消。", Status: model.LongTermMemoryActive,
	}}
	if _, err := classifier.Classify(context.Background(), incoming, existing); err != nil {
		t.Fatalf("classify: %v", err)
	}
	if len(chat.messages) != 2 {
		t.Fatalf("messages=%d, want system plus user", len(chat.messages))
	}
	payload := chat.messages[1].Content
	for _, fragment := range []string{
		"cancel() 只是发出取消信号",       // 候选新增的知识点
		"父 context 被取消时，派生出来的子 context 会一起被取消。", // 旧笔记全文
		"note-1",
	} {
		if !strings.Contains(payload, fragment) {
			t.Fatalf("judge payload=%q, want it to carry %q", payload, fragment)
		}
	}
}

// topicCoverageMasterySnapshot 是掌握度行的可比快照：逐字段读出来，比对时
// 不依赖结构体整体可比较。
type topicCoverageMasterySnapshot struct {
	TopicKey      string
	Topic         string
	MasteryScore  int
	Confidence    float64
	ValidAttempts int
	Level         model.TopicProgressLevel
	WeakPoints    string
	LastAttemptAt string
}

// topicCoverageMastery 读出掌握度行的每一个字段，用来逐字段比对。
func topicCoverageMastery(t *testing.T, acc *finalizeAcceptance, topicKey string) topicCoverageMasterySnapshot {
	t.Helper()
	var progress model.TopicProgress
	if err := acc.db.Where("user_id = ? AND topic_key = ?", acc.userID, topicKey).First(&progress).Error; err != nil {
		t.Fatal(err)
	}
	lastAttempt := ""
	if progress.LastAttemptAt != nil {
		lastAttempt = progress.LastAttemptAt.UTC().Format(time.RFC3339Nano)
	}
	return topicCoverageMasterySnapshot{
		TopicKey: progress.TopicKey, Topic: progress.Topic,
		MasteryScore: progress.MasteryScore, Confidence: progress.Confidence,
		ValidAttempts: progress.ValidAttempts, Level: progress.Level,
		WeakPoints: strings.Join(progress.WeakPoints, ","), LastAttemptAt: lastAttempt,
	}
}

// topicCoverageSessionID 给每个样本一个稳定的会话 id：夹具按会话 id 清理，
// 同一个 id 重复跑不会撞上上一次的残留。
func topicCoverageSessionID(name string) string {
	return "fin-acc-tcov-" + strings.ToLower(strings.ReplaceAll(name, " ", "-"))
}
