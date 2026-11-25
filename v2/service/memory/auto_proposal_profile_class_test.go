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

// 本文件是「自动替换草稿继承旧条目的画像类别」这一轮的零付费回归。
//
// 场景：B2 v1.1 首轮 42/48，四条替换题都选对了旧记录，失分全部在新草稿用了
// 模型提取的类别（constraint），而旧条目属于 background。替换的是同一件事，
// 它的归类不该在替换时顺手改掉——旧条目才是学习者自己给这件事定的归类。
//
// 规则：
//   - 自动提议的全局画像被明确判为 replacement、且目标仍是该用户的生效全局
//     条目时，新草稿取目标的 ProfileClass；
//   - 旧条目类别为空时沿用候选自己的类别；
//   - 继承需要明确的 replacement：complementary 也会带旧条目 id、也有
//     ConflictIDs，只有冲突目标不足以说明这是替换；
//   - independent 与 complementary 草稿保留自己的类别；
//   - 范围限定在自动提议：手工添加与显式写入走各自原有逻辑。
//
// 断言一直验到确认写入：确认前旧值仍生效、无正式写入、无修订、掌握度不动；
// 用户确认替换后新记录保留继承的类别，旧记录才失效。

// scriptedGlobalClassifier answers a scripted sequence of judgements. The
// finalize extraction judges a candidate first and the draft entry re-judges
// the same candidate, so the first answer is the extraction's judgement and
// the second is the draft entry's.
type scriptedGlobalClassifier struct {
	rules []scriptedGlobalRule
	calls int
}

type scriptedGlobalRule struct {
	relation   GlobalRelation
	targetHint string
}

func (c *scriptedGlobalClassifier) Classify(_ context.Context, _ ExplicitMemoryInput, existing []model.LongTermMemory) (GlobalRelationResult, error) {
	index := c.calls
	c.calls++
	if index >= len(c.rules) {
		return GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "脚本默认独立"}, nil
	}
	rule := c.rules[index]
	result := GlobalRelationResult{Relation: rule.relation, Reason: "脚本判定"}
	if rule.relation == GlobalRelationIndependent {
		return result, nil
	}
	hint := strings.ToLower(rule.targetHint)
	for _, memory := range existing {
		if hint == "" || strings.Contains(strings.ToLower(memory.Subject+"\n"+memory.Content), hint) {
			result.TargetMemoryID = memory.ID
			break
		}
	}
	if result.TargetMemoryID == "" {
		return GlobalRelationResult{}, fmt.Errorf("脚本判定没有找到目标 %q", rule.targetHint)
	}
	return result, nil
}

// autoProposalRunnerWithGlobalJudge builds a finalize runner and a governance
// service that both judge with the given global classifier. The two share one
// classifier in production; here a scripted one makes the extraction's
// judgement and the draft entry's re-judgement differ.
func autoProposalRunnerWithGlobalJudge(t *testing.T, acc *finalizeAcceptance, judge GlobalRelationResolver) (*FinalizeRunner, *GovernanceService) {
	t.Helper()
	governance, err := NewExplicitGovernanceService(acc.repo, nil, DefaultDreamConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := governance.UseGlobalRelations(acc.repo, judge); err != nil {
		t.Fatal(err)
	}
	catalogue, err := NewTopicCatalogue(nil, MemoryTopicAdapter{Repository: acc.repo}, 100)
	if err != nil {
		t.Fatal(err)
	}
	service, err := BuildSessionFinalizeService(acc.session, acc.messages, acc.repo, catalogue, acc.proposer, judge, acc.topic)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewFinalizeRunner(service, governance, acc.repo)
	if err != nil {
		t.Fatal(err)
	}
	return runner, governance
}

// profileClassFixture seeds the learner state one automatic replacement needs:
// the old entry that is about to be replaced, a mastery row that must not
// move, and the candidate the extraction proposes.
type profileClassFixture struct {
	acc        *finalizeAcceptance
	old        *model.LongTermMemory
	sessionID  string
	masteryKey string
}

func newProfileClassFixture(t *testing.T, oldClass, candidateClass model.MemoryProfileClass, sessionID string) *profileClassFixture {
	t.Helper()
	acc := newFinalizeAcceptance(t)
	old := seedProfileEntry(t, acc, "Go 版本", "当前项目使用 Go 1.22。", oldClass)
	if err := acc.db.Create(&model.TopicProgress{
		UserID: acc.userID, TopicKey: "go-basics", Topic: "Go 基础", MasteryScore: 80,
		Confidence: 0.8, ValidAttempts: 6, Level: model.TopicProgressDeveloping,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	return &profileClassFixture{acc: acc, old: old, sessionID: sessionID, masteryKey: "go-basics"}
}

// propose seeds the session and the candidate, then runs the extraction.
func (f *profileClassFixture) propose(t *testing.T, runner *FinalizeRunner, candidateClass model.MemoryProfileClass) {
	t.Helper()
	ctx := context.Background()
	stored := f.acc.seedSession(t, ctx, f.sessionID, nil, baseFinalizeMessages())
	f.acc.proposer.proposals = []SessionRawProposal{{
		Scope: model.MemoryScopeGlobal, Class: candidateClass, Subject: "Go 版本",
		Durability: model.MemoryDurabilityStable, Content: "当前项目使用 Go 1.23。",
		EvidenceIDs: []uint{stored[2].ID}, StatementEvidenceID: stored[2].ID,
	}}
	if _, err := runner.RunSession(ctx, f.acc.userID, f.sessionID); err != nil {
		t.Fatalf("run session: %v", err)
	}
}

// TestAutoProposalReplacementInheritsTheOldProfileClass is the class rule
// itself: an automatic replacement takes the class of the entry it replaces,
// across several class combinations. The old entry is the learner's own
// filing of the same matter, so the replacement must not re-file it.
func TestAutoProposalReplacementInheritsTheOldProfileClass(t *testing.T) {
	cases := []struct {
		name      string
		session   string
		oldClass  model.MemoryProfileClass
		candidate model.MemoryProfileClass
		want      model.MemoryProfileClass
	}{
		{"旧 background 新 constraint", "fin-acc-class-bg", model.ProfileClassBackground, model.ProfileClassConstraint, model.ProfileClassBackground},
		{"旧 constraint 新 goal", "fin-acc-class-ct", model.ProfileClassConstraint, model.ProfileClassGoal, model.ProfileClassConstraint},
		{"旧 goal 新 background", "fin-acc-class-go", model.ProfileClassGoal, model.ProfileClassBackground, model.ProfileClassGoal},
		{"旧 teaching_preference 新 constraint", "fin-acc-class-tp", model.ProfileClassTeachingPreference, model.ProfileClassConstraint, model.ProfileClassTeachingPreference},
		{"旧类别为空时沿用候选类别", "fin-acc-class-none", "", model.ProfileClassConstraint, model.ProfileClassConstraint},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			fixture := newProfileClassFixture(t, tc.oldClass, tc.candidate, tc.session)
			acc := fixture.acc
			ctx := context.Background()
			// 判定：同一事项换了当前值 → 替换，目标指向旧条目。
			acc.global.inner = &deterministicGlobalClassifier{rules: []stubGlobalRule{
				{contains: "1.23", relation: "replacement", targetHint: "1.22"},
			}}
			memoriesBefore := countRows(t, acc, &model.LongTermMemory{})
			revisionsBefore := countRows(t, acc, &model.MemoryRevision{})
			masteryBefore := profileClassMastery(t, acc, fixture.masteryKey)

			fixture.propose(t, acc.runner, tc.candidate)

			drafts := acc.pendingDrafts(ctx)
			if len(drafts) != 1 {
				t.Fatalf("drafts=%d, want one pending card", len(drafts))
			}
			draft := drafts[0]
			// 替换草稿的类别来自最终目标。
			if draft.ProfileClass != tc.want {
				t.Fatalf("draft class=%q, want %q (the class of the entry it replaces)", draft.ProfileClass, tc.want)
			}
			// 目标 id 与版本哈希正确：草稿钉住的是旧条目当时的正文哈希。
			if !stringListContains(draft.ConflictIDs, fixture.old.ID) {
				t.Fatalf("conflict ids=%v, want the old entry %s", draft.ConflictIDs, fixture.old.ID)
			}
			if pinned := memorydao.PinnedConflictHash(draft, fixture.old.ID); pinned != fixture.old.ContentHash {
				t.Fatalf("pinned hash=%q, want the old entry's %q", pinned, fixture.old.ContentHash)
			}
			// 确认前：旧值仍生效，无正式写入、无修订、掌握度不动。
			if got := seedProfileStatus(t, acc, fixture.old.ID); got != model.LongTermMemoryActive {
				t.Fatalf("old entry status=%s, want active before confirmation", got)
			}
			if got := countRows(t, acc, &model.LongTermMemory{}); got != memoriesBefore {
				t.Fatalf("memories=%d, want %d (no formal write before confirmation)", got, memoriesBefore)
			}
			if got := countRows(t, acc, &model.MemoryRevision{}); got != revisionsBefore {
				t.Fatalf("revisions=%d, want %d (no revision before confirmation)", got, revisionsBefore)
			}
			if after := profileClassMastery(t, acc, fixture.masteryKey); after != masteryBefore {
				t.Fatalf("mastery=%+v, want %+v (untouched)", after, masteryBefore)
			}

			// 用户确认替换：新记录保留继承的类别，旧记录才失效。
			confirmed, err := acc.governance.ConfirmDraft(ctx, ConfirmDraftRequest{
				UserID: acc.userID, DraftID: draft.ID, Action: "replace",
				TargetMemoryID: fixture.old.ID, Reason: "验收确认替换",
			})
			if err != nil {
				t.Fatalf("confirm replace: %v", err)
			}
			if confirmed.Memory == nil {
				t.Fatal("confirm returned no memory")
			}
			if confirmed.Memory.ProfileClass != tc.want {
				t.Fatalf("confirmed class=%q, want the inherited %q", confirmed.Memory.ProfileClass, tc.want)
			}
			if got := seedProfileStatus(t, acc, fixture.old.ID); got != model.LongTermMemorySuperseded {
				t.Fatalf("old entry status=%s, want superseded after the replacement", got)
			}
			if confirmed.Revision.ID == "" {
				t.Fatal("the replacement left no revision")
			}
			if after := profileClassMastery(t, acc, fixture.masteryKey); after != masteryBefore {
				t.Fatalf("mastery=%+v, want %+v (untouched by the replacement)", after, masteryBefore)
			}
		})
	}
}

// TestAutoProposalNonReplacementDraftsKeepTheirOwnClass is the other half of
// the rule. Only an explicit replacement re-files the candidate:
//
//   - complementary also carries the old entry id and ConflictIDs, so a
//     conflict target alone must not re-file anything;
//   - independent has no target at all;
//   - when the extraction judged a replacement but the draft entry re-judged
//     the same candidate a complementary detail, the draft entry's judgement
//     is the authoritative one;
//   - a proposal that is not the automatic one keeps the class its caller gave.
func TestAutoProposalNonReplacementDraftsKeepTheirOwnClass(t *testing.T) {
	cases := []struct {
		name     string
		session  string
		relation string
		wantPin  bool
	}{
		{"补充判定", "fin-acc-class-comp", "complementary", true},
		{"独立判定", "fin-acc-class-indep", "independent", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			fixture := newProfileClassFixture(t, model.ProfileClassBackground, model.ProfileClassConstraint, tc.session)
			acc := fixture.acc
			ctx := context.Background()
			acc.global.inner = &deterministicGlobalClassifier{rules: []stubGlobalRule{
				{contains: "1.23", relation: tc.relation, targetHint: "1.22"},
			}}
			fixture.propose(t, acc.runner, model.ProfileClassConstraint)

			drafts := acc.pendingDrafts(ctx)
			if len(drafts) != 1 {
				t.Fatalf("drafts=%d, want one pending card", len(drafts))
			}
			if drafts[0].ProfileClass != model.ProfileClassConstraint {
				t.Fatalf("draft class=%q, want the candidate's own %q", drafts[0].ProfileClass, model.ProfileClassConstraint)
			}
			if tc.wantPin && !stringListContains(drafts[0].ConflictIDs, fixture.old.ID) {
				t.Fatalf("conflict ids=%v, want the old entry pinned as a comparison target", drafts[0].ConflictIDs)
			}
			if !tc.wantPin && len(drafts[0].ConflictIDs) != 0 {
				t.Fatalf("conflict ids=%v, want none for an independent candidate", drafts[0].ConflictIDs)
			}
		})
	}
}

// TestAutoProposalDraftEntryRejudgementWinsOverTheExtraction is the
// authority rule: the extraction's first judgement is not the last word. When
// the draft entry re-judges the same candidate a complementary detail, the
// draft keeps its own class even though the extraction called it a
// replacement.
func TestAutoProposalDraftEntryRejudgementWinsOverTheExtraction(t *testing.T) {
	fixture := newProfileClassFixture(t, model.ProfileClassBackground, model.ProfileClassConstraint, "fin-acc-class-rejudge")
	acc := fixture.acc
	ctx := context.Background()
	judge := &scriptedGlobalClassifier{rules: []scriptedGlobalRule{
		{relation: GlobalRelationReplacement, targetHint: "1.22"},   // 提取初判
		{relation: GlobalRelationComplementary, targetHint: "1.22"}, // 草稿入口复判
	}}
	runner, _ := autoProposalRunnerWithGlobalJudge(t, acc, judge)
	fixture.propose(t, runner, model.ProfileClassConstraint)

	drafts := acc.pendingDrafts(ctx)
	if len(drafts) != 1 {
		t.Fatalf("drafts=%d, want one pending card", len(drafts))
	}
	if drafts[0].ProfileClass != model.ProfileClassConstraint {
		t.Fatalf("draft class=%q, want the candidate's own %q", drafts[0].ProfileClass, model.ProfileClassConstraint)
	}
	if !stringListContains(drafts[0].ConflictIDs, fixture.old.ID) {
		t.Fatalf("conflict ids=%v, want the old entry pinned", drafts[0].ConflictIDs)
	}
	if judge.calls < 2 {
		t.Fatalf("judge calls=%d, want the extraction's judgement and the draft entry's re-judgement", judge.calls)
	}
}

// TestAutoProposalClassInheritanceIsScopedToAutomaticProposals keeps the
// rule inside the automatic proposal path. A proposal that arrives from
// anywhere else - the explicit write, the agent's "please remember" tool -
// carries a class its caller chose, and the draft entry does not re-file it.
func TestAutoProposalClassInheritanceIsScopedToAutomaticProposals(t *testing.T) {
	acc := newFinalizeAcceptance(t)
	ctx := context.Background()
	old := seedProfileEntry(t, acc, "Go 版本", "当前项目使用 Go 1.22。", model.ProfileClassBackground)
	acc.global.inner = &deterministicGlobalClassifier{rules: []stubGlobalRule{
		{contains: "1.23", relation: "replacement", targetHint: "1.22"},
	}}
	draft, created, skip, err := acc.governance.ProposeDraft(ctx, DraftRequest{
		UserID: acc.userID, SessionID: "fin-acc-class-manual", RunID: "agent-manual",
		Input: ExplicitMemoryInput{
			Scope: model.MemoryScopeGlobal, ProfileClass: model.ProfileClassConstraint, Subject: "Go 版本",
			Content: "当前项目使用 Go 1.23。", Durability: model.MemoryDurabilityStable,
			IdempotencyKey: "manual-class-check",
		},
	})
	if err != nil || draft == nil || !created || skip != "" {
		t.Fatalf("draft=%v created=%v skip=%q err=%v", draft, created, skip, err)
	}
	if draft.ProfileClass != model.ProfileClassConstraint {
		t.Fatalf("draft class=%q, want the caller's own %q", draft.ProfileClass, model.ProfileClassConstraint)
	}
	if !stringListContains(draft.ConflictIDs, old.ID) {
		t.Fatalf("conflict ids=%v, want the old entry pinned", draft.ConflictIDs)
	}
}

// profileClassMastery reads the mastery row field by field into a comparable
// snapshot.
type profileClassMasterySnapshot struct {
	TopicKey      string
	Topic         string
	MasteryScore  int
	Confidence    float64
	ValidAttempts int
	Level         model.TopicProgressLevel
	WeakPoints    string
	LastAttemptAt string
}

func profileClassMastery(t *testing.T, acc *finalizeAcceptance, topicKey string) profileClassMasterySnapshot {
	t.Helper()
	var progress model.TopicProgress
	if err := acc.db.Where("user_id = ? AND topic_key = ?", acc.userID, topicKey).First(&progress).Error; err != nil {
		t.Fatal(err)
	}
	lastAttempt := ""
	if progress.LastAttemptAt != nil {
		lastAttempt = progress.LastAttemptAt.UTC().Format(time.RFC3339Nano)
	}
	return profileClassMasterySnapshot{
		TopicKey: progress.TopicKey, Topic: progress.Topic,
		MasteryScore: progress.MasteryScore, Confidence: progress.Confidence,
		ValidAttempts: progress.ValidAttempts, Level: progress.Level,
		WeakPoints: strings.Join(progress.WeakPoints, ","), LastAttemptAt: lastAttempt,
	}
}
