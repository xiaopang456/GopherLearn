package memory

import (
	"context"
	"strings"
	"testing"

	"GopherAI/model"
)

// 第三阶段「对话中后台分批提取，收尾统一展示草稿」原型（评测树 test-only，
// 产品代码零改动；产品候选保持 5b9e6f7）。
//
// 机制：
//   - 每批只处理水位之后的已完成消息，少量必要上下文来自现有可见集（近期窗口
//     + 白板线索），沿用 SessionFinalizeService.Extract 的未处理读取与证据校验；
//   - 批产出的候选先进入本原型的 held 列表（会话内不展示）；
//   - 收尾时只补处理剩余消息，然后把 held 一次性交给草稿流程（现有待确认列表），
//     用户确认后才写入长期记忆；
//   - 改口：同一事项的新批次覆盖旧版（或经关系判定判为 replacement/duplicate
//     时覆盖），最终只出一张卡；
//   - 撤回：批次带回 withdraw 信号（评测侧由超集提议者解析 withdraw 工具；
//     本文件用脚本替身），移除 held 或取消已展示草稿；
//   - 用户已手动编辑：合并时以草稿当前内容哈希与「上次提议内容哈希」比对，不
//     一致即视为用户已改——保留用户的修改、不覆盖，并记录「有新证据」提示。
//
// 草稿存储用窄接口，评测侧适配真实 GovernanceService（ProposeDraft/EditDraft/
// ConfirmDraft-cancel 分别对应 create/update/cancel）。

// batchWithdrawal is one "take that back" signal from a batch. The eval-side
// proposer parses it from the model's withdraw tool; this prototype's tests
// script it.
type batchWithdrawal struct {
	Scope      model.MemoryScope
	TopicKey   string
	Subject    string
	EvidenceID uint
}

// batchProposal is what one batch produced.
type batchProposal struct {
	Proposals   []SessionRawProposal
	Withdrawals []batchWithdrawal
}

// batchProposer is the batch-level proposer seam. The eval-side wrapper parses
// the model's tool calls (remember_* plus a withdraw tool) into this shape; the
// adapter below lets the real extraction pipeline run unchanged.
type batchProposer interface {
	ProposeBatch(ctx context.Context, input *SessionExtractionInput) (batchProposal, error)
}

// batchProposerAdapter exposes one batchProposer as the pipeline's proposer and
// keeps the batch's withdrawals aside for the prototype.
type batchProposerAdapter struct {
	inner       batchProposer
	withdrawals []batchWithdrawal
}

func (a *batchProposerAdapter) Propose(ctx context.Context, input *SessionExtractionInput) ([]SessionRawProposal, error) {
	batch, err := a.inner.ProposeBatch(ctx, input)
	if err != nil {
		return nil, err
	}
	a.withdrawals = append([]batchWithdrawal(nil), batch.Withdrawals...)
	return batch.Proposals, nil
}

// heldCandidate is one candidate waiting for the session-end display.
type heldCandidate struct {
	Candidate SessionMemoryCandidate
}

// prototypeDraftStore is the narrow face of the draft flow the prototype
// merges into. The eval-side adapter maps it to the real governance service.
type prototypeDraftStore interface {
	FindByMatter(ctx context.Context, userID int64, scope model.MemoryScope, topicKey, subject string) (*model.MemoryDraft, error)
	Create(ctx context.Context, candidate SessionMemoryCandidate) (*model.MemoryDraft, error)
	Update(ctx context.Context, draft *model.MemoryDraft, candidate SessionMemoryCandidate) (*model.MemoryDraft, error)
	Cancel(ctx context.Context, draft *model.MemoryDraft, reason string) error
}

// batchExtractionPrototype runs the conversation's background batches and the
// session-end display.
type batchExtractionPrototype struct {
	userID    int64
	sessionID string
	cursor    uint
	held      []heldCandidate
	drafts    prototypeDraftStore
	relations GlobalRelationResolver
	// proposed remembers what the pipeline last proposed per matter, so a
	// draft whose current hash differs can be recognised as learner-edited
	// even after the held list was flushed.
	proposed map[string]string
	// Notices record the merges that kept a learner-edited draft.
	Notices []string
}

func matterKey(scope model.MemoryScope, topicKey, subject string) string {
	return strings.Join([]string{string(scope), strings.TrimSpace(topicKey), strings.TrimSpace(subject)}, "\x00")
}

func (p *batchExtractionPrototype) remember(matter, hash string) {
	if p.proposed == nil {
		p.proposed = map[string]string{}
	}
	p.proposed[matter] = hash
}

// RunBatch processes only the messages after the watermark and merges the
// batch into the held candidates. A batch whose relation judgement fails holds
// the watermark (Extract's own rule), so the same progress is retried.
func (p *batchExtractionPrototype) RunBatch(ctx context.Context, service *SessionFinalizeService, proposer batchProposer) (FinalizeResult, error) {
	adapter := &batchProposerAdapter{inner: proposer}
	// The prototype swaps the proposer per batch; the rest of the pipeline
	// (reading, validation, dedup, relation) is the product's own.
	service.proposer = adapter
	result, err := service.Extract(ctx, FinalizeRequest{
		UserID: p.userID,
		Cursor: FinalizeCursor{SessionID: p.sessionID, CoveredMessageID: p.cursor},
	})
	if err != nil {
		return result, err
	}
	for _, withdrawal := range adapter.withdrawals {
		p.withdraw(ctx, withdrawal)
	}
	for _, candidate := range result.Proposals {
		p.mergeHeld(ctx, candidate)
	}
	p.cursor = result.Cursor.CoveredMessageID
	return result, nil
}

// withdraw removes the matter from the held list, and cancels the displayed
// draft when there is one, so the retracted version never sits beside a new
// one.
func (p *batchExtractionPrototype) withdraw(ctx context.Context, withdrawal batchWithdrawal) {
	key := matterKey(withdrawal.Scope, withdrawal.TopicKey, withdrawal.Subject)
	kept := p.held[:0]
	for _, held := range p.held {
		if matterKey(held.Candidate.Scope, held.Candidate.TopicKey, held.Candidate.Subject) != key {
			kept = append(kept, held)
		}
	}
	p.held = kept
	if p.drafts == nil {
		return
	}
	draft, err := p.drafts.FindByMatter(ctx, p.userID, withdrawal.Scope, withdrawal.TopicKey, withdrawal.Subject)
	if err != nil || draft == nil {
		return
	}
	if p.drafts.Cancel(ctx, draft, "withdrawn by learner") == nil {
		delete(p.proposed, key)
	}
}

// mergeHeld folds one candidate into the held list: the same matter keeps one
// entry, the latest version wins; a resolver-confirmed replacement or
// duplicate also updates the held entry, so two versions never reach the user.
func (p *batchExtractionPrototype) mergeHeld(ctx context.Context, candidate SessionMemoryCandidate) {
	key := matterKey(candidate.Scope, candidate.TopicKey, candidate.Subject)
	for i := range p.held {
		existing := &p.held[i]
		if matterKey(existing.Candidate.Scope, existing.Candidate.TopicKey, existing.Candidate.Subject) == key {
			existing.Candidate = candidate
			return
		}
	}
	if len(p.held) > 0 && p.relations != nil {
		pseudo := make([]model.LongTermMemory, 0, len(p.held))
		for _, held := range p.held {
			pseudo = append(pseudo, model.LongTermMemory{
				ID: held.Candidate.ID, Scope: held.Candidate.Scope, TopicKey: held.Candidate.TopicKey,
				Subject: held.Candidate.Subject, Content: held.Candidate.Content,
				Status: model.LongTermMemoryActive,
			})
		}
		if relation, err := p.relations.Classify(ctx, candidate.DraftInput(), pseudo); err == nil {
			switch relation.Relation {
			case GlobalRelationReplacement, GlobalRelationDuplicate:
				for i := range p.held {
					if p.held[i].Candidate.ID == relation.TargetMemoryID {
						p.held[i].Candidate = candidate
						return
					}
				}
			}
		}
	}
	p.held = append(p.held, heldCandidate{Candidate: candidate})
}

// Finalize processes the remaining messages once, then displays every held
// candidate through the draft flow in one pass. Merging respects learner edits:
// a draft whose content differs from what the pipeline last proposed is kept,
// and a notice records that new evidence arrived.
func (p *batchExtractionPrototype) Finalize(ctx context.Context, service *SessionFinalizeService, proposer batchProposer) (FinalizeResult, error) {
	result, err := p.RunBatch(ctx, service, proposer)
	if err != nil {
		return result, err
	}
	for i := range p.held {
		held := &p.held[i]
		candidate := held.Candidate
		key := matterKey(candidate.Scope, candidate.TopicKey, candidate.Subject)
		newHash := model.MemoryContentHash(candidate.Content)
		existing, findErr := p.drafts.FindByMatter(ctx, p.userID, candidate.Scope, candidate.TopicKey, candidate.Subject)
		if findErr != nil {
			return result, findErr
		}
		if existing == nil {
			if _, createErr := p.drafts.Create(ctx, candidate); createErr != nil {
				return result, createErr
			}
			p.remember(key, newHash)
			continue
		}
		if last, known := p.proposed[key]; known && existing.ContentHash != last {
			// The learner edited this draft after it was shown. Their words
			// win; the new evidence is surfaced as a notice instead of an
			// overwrite.
			p.Notices = append(p.Notices,
				"draft "+existing.ID+" was edited by the learner; new evidence kept out")
			continue
		}
		if _, updateErr := p.drafts.Update(ctx, existing, candidate); updateErr != nil {
			return result, updateErr
		}
		p.remember(key, newHash)
	}
	p.held = nil
	return result, nil
}

// ---- 零付费测试（脚本替身 + 内存草稿存储） ----

type scriptedBatchProposer struct {
	responses []batchProposal
	calls     int
	inputs    []*SessionExtractionInput
}

func (s *scriptedBatchProposer) ProposeBatch(_ context.Context, input *SessionExtractionInput) (batchProposal, error) {
	s.calls++
	s.inputs = append(s.inputs, input)
	if s.calls-1 < len(s.responses) {
		return s.responses[s.calls-1], nil
	}
	return batchProposal{}, nil
}

type fakeDraftStore struct {
	drafts  map[string]*model.MemoryDraft
	creates []SessionMemoryCandidate
	updates []SessionMemoryCandidate
	cancels []string
	nextID  int
}

func newFakeDraftStore() *fakeDraftStore {
	return &fakeDraftStore{drafts: map[string]*model.MemoryDraft{}}
}

func (f *fakeDraftStore) FindByMatter(_ context.Context, _ int64, scope model.MemoryScope, topicKey, subject string) (*model.MemoryDraft, error) {
	return f.drafts[matterKey(scope, topicKey, subject)], nil
}

func (f *fakeDraftStore) Create(_ context.Context, candidate SessionMemoryCandidate) (*model.MemoryDraft, error) {
	f.nextID++
	draft := &model.MemoryDraft{
		ID: "draft-" + string(rune('0'+f.nextID)), UserID: candidate.UserID,
		Scope: candidate.Scope, TopicKey: candidate.TopicKey, Subject: candidate.Subject,
		Content: candidate.Content, ContentHash: model.MemoryContentHash(candidate.Content),
		Status: model.MemoryDraftPending,
	}
	f.drafts[matterKey(candidate.Scope, candidate.TopicKey, candidate.Subject)] = draft
	f.creates = append(f.creates, candidate)
	return draft, nil
}

func (f *fakeDraftStore) Update(_ context.Context, draft *model.MemoryDraft, candidate SessionMemoryCandidate) (*model.MemoryDraft, error) {
	draft.Content = candidate.Content
	draft.ContentHash = model.MemoryContentHash(candidate.Content)
	f.updates = append(f.updates, candidate)
	return draft, nil
}

func (f *fakeDraftStore) Cancel(_ context.Context, draft *model.MemoryDraft, _ string) error {
	draft.Status = model.MemoryDraftCancelled
	delete(f.drafts, matterKey(draft.Scope, draft.TopicKey, draft.Subject))
	f.cancels = append(f.cancels, draft.ID)
	return nil
}

func batchPrototypeService(t *testing.T) *SessionFinalizeService {
	t.Helper()
	return newFinalizeService(t, fakeFinalizeBoards{}, fakeFinalizeMessages{messages: ext01Messages()},
		nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{}, nil,
		&scriptedProposer{}, &scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "x"}}, 50)
}

func wordCountProposal(content string, evidence uint) SessionRawProposal {
	return SessionRawProposal{
		Scope: model.MemoryScopeGlobal, Class: model.ProfileClassGoal, Subject: "背诵目标",
		Durability: model.MemoryDurabilityStable, Content: content,
		EvidenceIDs: []uint{evidence}, StatementEvidenceID: evidence,
	}
}

// TestBatchPrototypeChangedMindKeepsOneDraft: two batches stating the same
// matter with a new value end at the session with exactly one draft - the
// latest - never two versions side by side.
func TestBatchPrototypeChangedMindKeepsOneDraft(t *testing.T) {
	service := batchPrototypeService(t)
	store := newFakeDraftStore()
	prototype := &batchExtractionPrototype{userID: 7, sessionID: "S-batch-change", drafts: store}

	first := &scriptedBatchProposer{responses: []batchProposal{{Proposals: []SessionRawProposal{
		wordCountProposal("每天背 22 个单词。", 3),
	}}}}
	if _, err := prototype.RunBatch(context.Background(), service, first); err != nil {
		t.Fatalf("batch 1: %v", err)
	}

	messages := append(ext01Messages(), SessionMessage{ID: 12, IsUser: true,
		Content: "后来改成每天背 37 个单词。", CreatedAt: finalizeAt(60)})
	secondService := newFinalizeService(t, fakeFinalizeBoards{}, fakeFinalizeMessages{messages: messages},
		nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{}, nil,
		&scriptedProposer{}, &scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "x"}}, 50)
	second := &scriptedBatchProposer{responses: []batchProposal{{Proposals: []SessionRawProposal{
		wordCountProposal("每天背 37 个单词。", 12),
	}}}}
	if _, err := prototype.RunBatch(context.Background(), secondService, second); err != nil {
		t.Fatalf("batch 2: %v", err)
	}

	if _, err := prototype.Finalize(context.Background(), secondService, &scriptedBatchProposer{}); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if len(store.creates) != 1 {
		t.Fatalf("creates=%d, want exactly one draft", len(store.creates))
	}
	if store.creates[0].Content != "每天背 37 个单词。" {
		t.Fatalf("the displayed draft=%q, want the latest value only", store.creates[0].Content)
	}
	if strings.Contains(store.creates[0].Content, "22") {
		t.Fatalf("the old version reached the pending list")
	}
}

// TestBatchPrototypeWithdrawalUpdatesDraft: a retraction removes the held
// candidate; a retraction after the draft was displayed cancels that draft.
func TestBatchPrototypeWithdrawalUpdatesDraft(t *testing.T) {
	service := batchPrototypeService(t)
	store := newFakeDraftStore()
	prototype := &batchExtractionPrototype{userID: 7, sessionID: "S-batch-withdraw", drafts: store}

	first := &scriptedBatchProposer{responses: []batchProposal{{Proposals: []SessionRawProposal{
		wordCountProposal("每天背 22 个单词。", 3),
	}}}}
	if _, err := prototype.RunBatch(context.Background(), service, first); err != nil {
		t.Fatalf("batch 1: %v", err)
	}
	withdrawal := batchWithdrawal{Scope: model.MemoryScopeGlobal, Subject: "背诵目标", EvidenceID: 12}
	retract := &scriptedBatchProposer{responses: []batchProposal{{Withdrawals: []batchWithdrawal{withdrawal}}}}
	// 撤回是一条新消息带来的：批次才有可处理的内容，提议者才会被调用。
	retractService := newFinalizeService(t, fakeFinalizeBoards{}, fakeFinalizeMessages{messages: append(ext01Messages(),
		SessionMessage{ID: 12, IsUser: true, Content: "刚才那条不算，别记了。", CreatedAt: finalizeAt(60)})},
		nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{}, nil,
		&scriptedProposer{}, &scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "x"}}, 50)
	if _, err := prototype.RunBatch(context.Background(), retractService, retract); err != nil {
		t.Fatalf("batch 2: %v", err)
	}
	if _, err := prototype.Finalize(context.Background(), retractService, &scriptedBatchProposer{}); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if len(store.creates) != 0 {
		t.Fatalf("creates=%d, want the retracted matter never displayed", len(store.creates))
	}

	// 已展示后的撤回：取消既有草稿。
	again := &scriptedBatchProposer{responses: []batchProposal{{Proposals: []SessionRawProposal{
		wordCountProposal("每天背 22 个单词。", 13),
	}}}}
	after := newFinalizeService(t, fakeFinalizeBoards{}, fakeFinalizeMessages{messages: append(ext01Messages(),
		SessionMessage{ID: 12, IsUser: true, Content: "继续。", CreatedAt: finalizeAt(61)},
		SessionMessage{ID: 13, IsUser: true, Content: "还是按 22 个来。", CreatedAt: finalizeAt(62)})},
		nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{}, nil,
		&scriptedProposer{}, &scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "x"}}, 50)
	if _, err := prototype.RunBatch(context.Background(), after, again); err != nil {
		t.Fatalf("batch 3: %v", err)
	}
	if _, err := prototype.Finalize(context.Background(), after, &scriptedBatchProposer{}); err != nil {
		t.Fatalf("finalize 2: %v", err)
	}
	if len(store.creates) != 1 {
		t.Fatalf("creates=%d, want one displayed draft", len(store.creates))
	}
	// 已展示后再撤回：撤回也是一条新消息。
	afterWithdraw := newFinalizeService(t, fakeFinalizeBoards{}, fakeFinalizeMessages{messages: append(ext01Messages(),
		SessionMessage{ID: 12, IsUser: true, Content: "继续。", CreatedAt: finalizeAt(61)},
		SessionMessage{ID: 13, IsUser: true, Content: "还是按 22 个来。", CreatedAt: finalizeAt(62)},
		SessionMessage{ID: 14, IsUser: true, Content: "还是别记了。", CreatedAt: finalizeAt(63)})},
		nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{}, nil,
		&scriptedProposer{}, &scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "x"}}, 50)
	if _, err := prototype.RunBatch(context.Background(), afterWithdraw, &scriptedBatchProposer{
		responses: []batchProposal{{Withdrawals: []batchWithdrawal{withdrawal}}},
	}); err != nil {
		t.Fatalf("withdraw after display: %v", err)
	}
	if len(store.cancels) != 1 {
		t.Fatalf("cancels=%d, want the displayed draft cancelled", len(store.cancels))
	}
}

// TestBatchPrototypeUserEditPreservedWithNotice: a learner-edited draft is
// never overwritten by later evidence; the merge keeps the edit and raises a
// notice instead.
func TestBatchPrototypeUserEditPreservedWithNotice(t *testing.T) {
	store := newFakeDraftStore()
	prototype := &batchExtractionPrototype{userID: 7, sessionID: "S-batch-edit", drafts: store}
	service := batchPrototypeService(t)
	first := &scriptedBatchProposer{responses: []batchProposal{{Proposals: []SessionRawProposal{
		wordCountProposal("每天背 22 个单词。", 3),
	}}}}
	if _, err := prototype.Finalize(context.Background(), service, first); err != nil {
		t.Fatalf("finalize 1: %v", err)
	}
	if len(store.creates) != 1 {
		t.Fatalf("creates=%d", len(store.creates))
	}
	// 学习者手动编辑：草稿内容与哈希都变了。
	draft := store.drafts[matterKey(model.MemoryScopeGlobal, "", "背诵目标")]
	draft.Content = "每天背 30 个单词（我自己改的）。"
	draft.ContentHash = model.MemoryContentHash(draft.Content)

	// 新证据到达：同一事项的新值。
	messages := append(ext01Messages(), SessionMessage{ID: 12, IsUser: true,
		Content: "对了，每天改成背 37 个。", CreatedAt: finalizeAt(62)})
	service2 := newFinalizeService(t, fakeFinalizeBoards{}, fakeFinalizeMessages{messages: messages},
		nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{}, nil,
		&scriptedProposer{}, &scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "x"}}, 50)
	second := &scriptedBatchProposer{responses: []batchProposal{{Proposals: []SessionRawProposal{
		wordCountProposal("每天背 37 个单词。", 12),
	}}}}
	if _, err := prototype.Finalize(context.Background(), service2, second); err != nil {
		t.Fatalf("finalize 2: %v", err)
	}
	if len(store.updates) != 0 {
		t.Fatalf("updates=%d, want the learner edit preserved", len(store.updates))
	}
	kept := store.drafts[matterKey(model.MemoryScopeGlobal, "", "背诵目标")]
	if kept.Content != "每天背 30 个单词（我自己改的）。" {
		t.Fatalf("draft=%q, want the learner's edit", kept.Content)
	}
	if len(prototype.Notices) != 1 || !strings.Contains(prototype.Notices[0], "edited by the learner") {
		t.Fatalf("notices=%v, want the new-evidence notice", prototype.Notices)
	}
}

// TestBatchPrototypeFinalizeProcessesRemainderOnly: each batch covers only the
// messages after the watermark; finalize continues from the batch cursor and
// processes the remainder exactly once.
func TestBatchPrototypeFinalizeProcessesRemainderOnly(t *testing.T) {
	service := batchPrototypeService(t)
	prototype := &batchExtractionPrototype{userID: 7, sessionID: "S-batch-remainder", drafts: newFakeDraftStore()}
	first := &scriptedBatchProposer{responses: []batchProposal{{}}}
	result1, err := prototype.RunBatch(context.Background(), service, first)
	if err != nil {
		t.Fatalf("batch 1: %v", err)
	}
	if result1.Cursor.CoveredMessageID != 11 {
		t.Fatalf("batch 1 covered=%d, want the whole seeded conversation", result1.Cursor.CoveredMessageID)
	}
	// 会话继续：新消息在收尾时才补处理。
	messages := append(ext01Messages(), SessionMessage{ID: 12, IsUser: true, Content: "还有一点。", CreatedAt: finalizeAt(63)})
	service2 := newFinalizeService(t, fakeFinalizeBoards{}, fakeFinalizeMessages{messages: messages},
		nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{}, nil,
		&scriptedProposer{}, &scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "x"}}, 50)
	final := &scriptedBatchProposer{}
	result2, err := prototype.Finalize(context.Background(), service2, final)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if result2.Cursor.CoveredMessageID != 12 {
		t.Fatalf("finalize covered=%d, want only the remainder up to 12", result2.Cursor.CoveredMessageID)
	}
	if final.calls != 1 || len(final.inputs) != 1 {
		t.Fatalf("finalize proposer calls=%d, want exactly one batch over the remainder", final.calls)
	}
}
