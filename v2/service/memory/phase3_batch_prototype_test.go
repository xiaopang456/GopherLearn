package memory

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

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
	// List returns the pending drafts of the learner - what the session-end
	// display shows.
	List(ctx context.Context, userID int64) ([]*model.MemoryDraft, error)
	Create(ctx context.Context, candidate SessionMemoryCandidate) (*model.MemoryDraft, error)
	Update(ctx context.Context, draft *model.MemoryDraft, candidate SessionMemoryCandidate) (*model.MemoryDraft, error)
	Cancel(ctx context.Context, draft *model.MemoryDraft, reason string) error
}

// DraftDisplayStatus is what the session-end view shows for one draft.
type DraftDisplayStatus string

const (
	// DraftReady means the draft rests on judged evidence: it can be confirmed.
	DraftReady DraftDisplayStatus = "ready"
	// DraftVerifying means the last batch still holds a judgement for this
	// matter. The draft is shown with the old content, marked, and not
	// confirmable - the learner must not confirm a version a late utterance
	// may already have replaced.
	DraftVerifying DraftDisplayStatus = "verifying"
)

// displayedDraft is one row of the session-end display.
type displayedDraft struct {
	Draft        *model.MemoryDraft
	Status       DraftDisplayStatus
	StatusReason string
	Confirmable  bool
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
	// verifying marks the matters whose last batch still holds a judgement
	// (RetryPending). Their displayed drafts are marked and not confirmable.
	verifying map[string]string
	// Notices record the merges that kept a learner-edited draft.
	Notices []string
	// Displayed is what the session-end view shows, one row per pending
	// draft, with the verification status.
	Displayed []displayedDraft
	// tailPending says the session still has unprocessed messages. Until the
	// tail batch answers, every draft of this session may be affected, so the
	// display marks them all - there is no way to know which one the tail
	// will change.
	tailPending bool
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
	// A candidate whose judgement failed is not proposed; its matter is
	// marked so the session-end view can show the old draft as still being
	// verified instead of inviting a confirmation of a possibly stale value.
	for _, pending := range result.RetryPending {
		if p.verifying == nil {
			p.verifying = map[string]string{}
		}
		p.verifying[matterKey(pending.Scope, pending.TopicKey, pending.Subject)] = "relation judgement pending"
	}
	for _, candidate := range result.Proposals {
		// A judged candidate resolves the matter: whatever was being verified
		// has a fresh answer now.
		delete(p.verifying, matterKey(candidate.Scope, candidate.TopicKey, candidate.Subject))
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
	delete(p.verifying, key)
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
// Publish is the leave-time step: it publishes every draft that is ready,
// without waiting for the tail batch. If the session still has unprocessed
// messages, their effect is unknown, so every draft of this session is marked
// verifying and held back from confirmation until the tail settles.
func (p *batchExtractionPrototype) Publish(ctx context.Context, service *SessionFinalizeService) ([]displayedDraft, error) {
	if err := p.flushHeld(ctx); err != nil {
		return nil, err
	}
	pending, err := service.messages.LoadUnprocessedMessages(ctx, p.userID, p.sessionID, p.cursor)
	if err != nil {
		return nil, err
	}
	p.tailPending = len(pending) > 0
	return p.refreshDisplayed(ctx)
}

// RunTailBatch is the background step: it processes only the remaining
// messages, then applies whatever they changed - update in place, withdraw, or
// open confirmation - and refreshes the display. While it is running the
// published list stays readable, with the affected drafts still marked.
func (p *batchExtractionPrototype) RunTailBatch(ctx context.Context, service *SessionFinalizeService, proposer batchProposer) (FinalizeResult, error) {
	result, err := p.RunBatch(ctx, service, proposer)
	if err != nil {
		return result, err
	}
	if err := p.flushHeld(ctx); err != nil {
		return result, err
	}
	// Recompute the tail: a judgement that stayed pending keeps the watermark
	// held and the drafts marked; a completed tail opens confirmation.
	pending, loadErr := service.messages.LoadUnprocessedMessages(ctx, p.userID, p.sessionID, p.cursor)
	if loadErr != nil {
		return result, loadErr
	}
	p.tailPending = len(pending) > 0
	if _, refreshErr := p.refreshDisplayed(ctx); refreshErr != nil {
		return result, refreshErr
	}
	return result, nil
}

// Finalize is the two steps in order, for callers that do not publish early.
func (p *batchExtractionPrototype) Finalize(ctx context.Context, service *SessionFinalizeService, proposer batchProposer) (FinalizeResult, error) {
	if _, err := p.Publish(ctx, service); err != nil {
		return FinalizeResult{}, err
	}
	return p.RunTailBatch(ctx, service, proposer)
}

// flushHeld folds the held candidates into the drafts. Matters still under
// verification keep their old content; learner-edited drafts keep the
// learner's content and raise the notice.
func (p *batchExtractionPrototype) flushHeld(ctx context.Context) error {
	for i := range p.held {
		held := &p.held[i]
		candidate := held.Candidate
		key := matterKey(candidate.Scope, candidate.TopicKey, candidate.Subject)
		newHash := model.MemoryContentHash(candidate.Content)
		existing, findErr := p.drafts.FindByMatter(ctx, p.userID, candidate.Scope, candidate.TopicKey, candidate.Subject)
		if findErr != nil {
			return findErr
		}
		if _, pending := p.verifying[key]; pending {
			// The latest evidence for this matter is still being judged. Do
			// not create a fresh card, and do not overwrite the old one.
			continue
		}
		if existing == nil {
			if _, createErr := p.drafts.Create(ctx, candidate); createErr != nil {
				return createErr
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
			return updateErr
		}
		p.remember(key, newHash)
	}
	p.held = nil
	return nil
}

// refreshDisplayed rebuilds the session-end view: one row per pending draft,
// marked verifying while its matter is being judged or while the tail messages
// are still unprocessed.
func (p *batchExtractionPrototype) refreshDisplayed(ctx context.Context) ([]displayedDraft, error) {
	p.Displayed = p.Displayed[:0]
	drafts, listErr := p.drafts.List(ctx, p.userID)
	if listErr != nil {
		return nil, listErr
	}
	for _, draft := range drafts {
		key := matterKey(draft.Scope, draft.TopicKey, draft.Subject)
		row := displayedDraft{Draft: draft, Status: DraftReady, Confirmable: true}
		switch {
		case p.verifying[key] != "":
			row.Status = DraftVerifying
			row.StatusReason = p.verifying[key]
			row.Confirmable = false
		case p.tailPending:
			// The tail may affect any draft of this session and the batch has
			// not answered yet, so none of them is confirmable.
			row.Status = DraftVerifying
			row.StatusReason = "tail messages pending"
			row.Confirmable = false
		}
		p.Displayed = append(p.Displayed, row)
	}
	return p.Displayed, nil
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

func (f *fakeDraftStore) List(_ context.Context, _ int64) ([]*model.MemoryDraft, error) {
	result := make([]*model.MemoryDraft, 0, len(f.drafts))
	for _, draft := range f.drafts {
		result = append(result, draft)
	}
	return result, nil
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

// TestBatchPrototypeLastSentenceNewValueReplacesDisplayedDraft is the late
// change: the old version was already displayed mid-conversation, and only the
// last sentence states the new value. The session-end view shows one draft,
// the new value, and nothing of the old one.
func TestBatchPrototypeLastSentenceNewValueReplacesDisplayedDraft(t *testing.T) {
	store := newFakeDraftStore()
	prototype := &batchExtractionPrototype{userID: 7, sessionID: "S-late-new", drafts: store}
	first := &scriptedBatchProposer{responses: []batchProposal{{Proposals: []SessionRawProposal{
		wordCountProposal("每天背 22 个单词。", 3),
	}}}}
	if _, err := prototype.Finalize(context.Background(), batchPrototypeService(t), first); err != nil {
		t.Fatalf("finalize 1: %v", err)
	}
	if len(prototype.Displayed) != 1 || prototype.Displayed[0].Status != DraftReady || !prototype.Displayed[0].Confirmable {
		t.Fatalf("displayed=%+v, want one ready draft", prototype.Displayed)
	}

	late := newFinalizeService(t, fakeFinalizeBoards{}, fakeFinalizeMessages{messages: append(ext01Messages(),
		SessionMessage{ID: 12, IsUser: true, Content: "最后改成每天背 37 个。", CreatedAt: finalizeAt(70)})},
		nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{}, nil,
		&scriptedProposer{}, &scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "x"}}, 50)
	second := &scriptedBatchProposer{responses: []batchProposal{{Proposals: []SessionRawProposal{
		wordCountProposal("每天背 37 个单词。", 12),
	}}}}
	if _, err := prototype.Finalize(context.Background(), late, second); err != nil {
		t.Fatalf("finalize 2: %v", err)
	}
	if len(store.creates) != 1 || len(store.updates) != 1 {
		t.Fatalf("creates=%d updates=%d, want one card updated in place", len(store.creates), len(store.updates))
	}
	if len(prototype.Displayed) != 1 {
		t.Fatalf("displayed=%d, want one draft after the late change", len(prototype.Displayed))
	}
	row := prototype.Displayed[0]
	if row.Draft.Content != "每天背 37 个单词。" || row.Status != DraftReady || !row.Confirmable {
		t.Fatalf("displayed=%+v, want the new value ready for confirmation", row)
	}
}

// TestBatchPrototypeLastSentenceWithdrawalCancelsDisplayedDraft: the old
// version was displayed, and the last sentence takes it back. The session-end
// view has nothing left to confirm for that matter.
func TestBatchPrototypeLastSentenceWithdrawalCancelsDisplayedDraft(t *testing.T) {
	store := newFakeDraftStore()
	prototype := &batchExtractionPrototype{userID: 7, sessionID: "S-late-withdraw", drafts: store}
	first := &scriptedBatchProposer{responses: []batchProposal{{Proposals: []SessionRawProposal{
		wordCountProposal("每天背 22 个单词。", 3),
	}}}}
	if _, err := prototype.Finalize(context.Background(), batchPrototypeService(t), first); err != nil {
		t.Fatalf("finalize 1: %v", err)
	}
	late := newFinalizeService(t, fakeFinalizeBoards{}, fakeFinalizeMessages{messages: append(ext01Messages(),
		SessionMessage{ID: 12, IsUser: true, Content: "算了，刚才那条别记了。", CreatedAt: finalizeAt(71)})},
		nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{}, nil,
		&scriptedProposer{}, &scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "x"}}, 50)
	retract := &scriptedBatchProposer{responses: []batchProposal{{Withdrawals: []batchWithdrawal{{
		Scope: model.MemoryScopeGlobal, Subject: "背诵目标", EvidenceID: 12,
	}}}}}
	if _, err := prototype.Finalize(context.Background(), late, retract); err != nil {
		t.Fatalf("finalize 2: %v", err)
	}
	if len(store.cancels) != 1 {
		t.Fatalf("cancels=%d, want the displayed draft withdrawn", len(store.cancels))
	}
	if len(prototype.Displayed) != 0 {
		t.Fatalf("displayed=%+v, want nothing left to confirm", prototype.Displayed)
	}
}

// TestBatchPrototypeLastBatchVerifyingMarksDisplayedDraft: the old version was
// displayed, the last sentence states a new value, and the judgement for it is
// still pending. The view keeps the old content, marks the row verifying and
// blocks confirmation, so nobody confirms a version the late sentence may have
// replaced.
func TestBatchPrototypeLastBatchVerifyingMarksDisplayedDraft(t *testing.T) {
	store := newFakeDraftStore()
	prototype := &batchExtractionPrototype{userID: 7, sessionID: "S-late-verify", drafts: store}
	first := &scriptedBatchProposer{responses: []batchProposal{{Proposals: []SessionRawProposal{
		wordCountProposal("每天背 22 个单词。", 3),
	}}}}
	if _, err := prototype.Finalize(context.Background(), batchPrototypeService(t), first); err != nil {
		t.Fatalf("finalize 1: %v", err)
	}
	old := globalEntry("P-words", "每天背 22 个单词。")
	late := newFinalizeService(t, fakeFinalizeBoards{}, fakeFinalizeMessages{messages: append(ext01Messages(),
		SessionMessage{ID: 12, IsUser: true, Content: "最后改成每天背 37 个。", CreatedAt: finalizeAt(72)})},
		nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}},
		fakeFinalizeGlobalMind{entries: []model.LongTermMemory{old}}, nil,
		&scriptedProposer{}, &scriptedRelations{err: errors.New("judge unavailable")}, 50)
	second := &scriptedBatchProposer{responses: []batchProposal{{Proposals: []SessionRawProposal{
		wordCountProposal("每天背 37 个单词。", 12),
	}}}}
	result, err := prototype.Finalize(context.Background(), late, second)
	if err != nil {
		t.Fatalf("finalize 2: %v", err)
	}
	if len(result.RetryPending) != 1 || result.Cursor.CoveredMessageID != 11 {
		t.Fatalf("result=%+v, want the judgement pending with the watermark held", result)
	}
	if len(store.updates) != 0 {
		t.Fatalf("updates=%d, want the old content kept while verifying", len(store.updates))
	}
	if len(prototype.Displayed) != 1 {
		t.Fatalf("displayed=%+v, want the old draft shown with a status", prototype.Displayed)
	}
	row := prototype.Displayed[0]
	if row.Draft.Content != "每天背 22 个单词。" {
		t.Fatalf("displayed content=%q, want the old version", row.Draft.Content)
	}
	if row.Status != DraftVerifying || row.Confirmable {
		t.Fatalf("displayed=%+v, want verifying and not confirmable", row)
	}
	if !strings.Contains(row.StatusReason, "judgement pending") {
		t.Fatalf("status reason=%q", row.StatusReason)
	}
}

// gatedBatchProposer blocks the tail batch's model response until the test
// releases it, so the published list can be inspected while the judgement is
// still in flight.
type gatedBatchProposer struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	result  batchProposal
}

func (g *gatedBatchProposer) ProposeBatch(ctx context.Context, _ *SessionExtractionInput) (batchProposal, error) {
	g.once.Do(func() { close(g.started) })
	select {
	case <-g.release:
		return g.result, nil
	case <-ctx.Done():
		return batchProposal{}, ctx.Err()
	}
}

// TestBatchPrototypePublishBeforeTailBatch is the deterministic split test:
// leaving publishes the ready drafts first; the tail batch then runs in the
// background against a blocked model. While it is blocked the list is already
// readable and the old value is not confirmable; releasing it applies the
// late change - or the late withdrawal - in place.
func TestBatchPrototypePublishBeforeTailBatch(t *testing.T) {
	store := newFakeDraftStore()
	prototype := &batchExtractionPrototype{userID: 7, sessionID: "S-publish-tail", drafts: store}
	service := batchPrototypeService(t)

	// 对话中途：旧值已进 held 并在离开时发布为可确认草稿。
	first := &scriptedBatchProposer{responses: []batchProposal{{Proposals: []SessionRawProposal{
		wordCountProposal("每天背 22 个单词。", 3),
	}}}}
	if _, err := prototype.RunBatch(context.Background(), service, first); err != nil {
		t.Fatalf("batch 1: %v", err)
	}
	if _, err := prototype.Publish(context.Background(), service); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if len(prototype.Displayed) != 1 || prototype.Displayed[0].Status != DraftReady || !prototype.Displayed[0].Confirmable {
		t.Fatalf("displayed=%+v, want the old draft ready before the tail exists", prototype.Displayed)
	}

	// 尾部新值 37：离开时发布——还不知道影响哪一条，先标本会话草稿。
	messages := append(ext01Messages(), SessionMessage{ID: 12, IsUser: true,
		Content: "最后改成每天背 37 个。", CreatedAt: finalizeAt(80)})
	tailService := newFinalizeService(t, fakeFinalizeBoards{}, fakeFinalizeMessages{messages: messages},
		nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{}, nil,
		&scriptedProposer{}, &scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "x"}}, 50)
	if _, err := prototype.Publish(context.Background(), tailService); err != nil {
		t.Fatalf("publish with tail: %v", err)
	}
	if len(prototype.Displayed) != 1 {
		t.Fatalf("displayed=%d, want the published list readable", len(prototype.Displayed))
	}
	row := prototype.Displayed[0]
	if row.Status != DraftVerifying || row.Confirmable || row.Draft.Content != "每天背 22 个单词。" {
		t.Fatalf("displayed=%+v, want the old value readable but not confirmable", row)
	}
	if !strings.Contains(row.StatusReason, "tail messages pending") {
		t.Fatalf("status reason=%q", row.StatusReason)
	}

	// 后台核对：卡住模型响应，列表仍可读、旧值仍不可确认。
	gate := &gatedBatchProposer{started: make(chan struct{}), release: make(chan struct{}),
		result: batchProposal{Proposals: []SessionRawProposal{wordCountProposal("每天背 37 个单词。", 12)}}}
	done := make(chan error, 1)
	go func() {
		_, err := prototype.RunTailBatch(context.Background(), tailService, gate)
		done <- err
	}()
	select {
	case <-gate.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the tail batch never reached the model")
	}
	if len(prototype.Displayed) != 1 || prototype.Displayed[0].Status != DraftVerifying ||
		prototype.Displayed[0].Confirmable || prototype.Displayed[0].Draft.Content != "每天背 22 个单词。" {
		t.Fatalf("displayed=%+v while blocked, want the old value still readable and not confirmable", prototype.Displayed)
	}
	close(gate.release)
	if err := <-done; err != nil {
		t.Fatalf("tail batch: %v", err)
	}
	// 放行后：改口生效、原位更新、开放确认。
	if len(prototype.Displayed) != 1 || prototype.Displayed[0].Status != DraftReady || !prototype.Displayed[0].Confirmable {
		t.Fatalf("displayed=%+v after release, want the new value ready", prototype.Displayed)
	}
	if prototype.Displayed[0].Draft.Content != "每天背 37 个单词。" {
		t.Fatalf("draft=%q after release, want the late value in place", prototype.Displayed[0].Draft.Content)
	}

	// 撤回对照：再追加一句撤回，同样的门控——放行后草稿取消、列表清空。
	retractMessages := append(messages, SessionMessage{ID: 13, IsUser: true,
		Content: "算了，刚才那条别记了。", CreatedAt: finalizeAt(81)})
	retractService := newFinalizeService(t, fakeFinalizeBoards{}, fakeFinalizeMessages{messages: retractMessages},
		nil, &fakeFinalizeFingerprints{known: map[string]struct{}{}}, fakeFinalizeGlobalMind{}, nil,
		&scriptedProposer{}, &scriptedRelations{result: GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "x"}}, 50)
	if _, err := prototype.Publish(context.Background(), retractService); err != nil {
		t.Fatalf("publish before withdrawal: %v", err)
	}
	if len(prototype.Displayed) != 1 || prototype.Displayed[0].Confirmable {
		t.Fatalf("displayed=%+v, want the draft readable but not confirmable before withdrawal", prototype.Displayed)
	}
	withdrawGate := &gatedBatchProposer{started: make(chan struct{}), release: make(chan struct{}),
		result: batchProposal{Withdrawals: []batchWithdrawal{{Scope: model.MemoryScopeGlobal, Subject: "背诵目标", EvidenceID: 13}}}}
	withdrawDone := make(chan error, 1)
	go func() {
		_, err := prototype.RunTailBatch(context.Background(), retractService, withdrawGate)
		withdrawDone <- err
	}()
	select {
	case <-withdrawGate.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the withdrawal tail batch never reached the model")
	}
	if len(prototype.Displayed) != 1 || prototype.Displayed[0].Confirmable {
		t.Fatalf("displayed=%+v while the withdrawal is blocked, want it still not confirmable", prototype.Displayed)
	}
	close(withdrawGate.release)
	if err := <-withdrawDone; err != nil {
		t.Fatalf("withdrawal tail batch: %v", err)
	}
	if len(store.cancels) != 1 {
		t.Fatalf("cancels=%d, want the withdrawn draft cancelled", len(store.cancels))
	}
	if len(prototype.Displayed) != 0 {
		t.Fatalf("displayed=%+v after withdrawal, want nothing left to confirm", prototype.Displayed)
	}
}
