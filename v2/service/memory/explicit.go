package memory

import (
	memorydao "GopherAI/dao/memory"
	"GopherAI/model"
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

var ErrInvalidExplicitWrite = errors.New("invalid explicit memory write")
var ErrGlobalMemoryCapacity = errors.New("global memory capacity reached")

// Skip reasons reported by ProposeDraft when no draft is created.
const (
	// SkipDuplicateExisting: an active memory already carries the same content
	// (byte-identical) or was judged the same durable meaning, so the draft is
	// skipped and the existing entry is kept. The learner never sees this on
	// the confirmation list.
	SkipDuplicateExisting = "duplicate_existing"
	// SkipIgnoredSource: the same source session already proposed this content
	// and the learner ignored it. Suppression is scoped to the source, so a
	// re-expression from a different session proposes normally.
	SkipIgnoredSource = "ignored_source"
)

type ExplicitMemoryInput struct {
	Scope          model.MemoryScope
	Subject        string
	Content        string
	Tags           []string
	TopicKey       string
	Durability     model.MemoryDurability
	ValidUntil     *time.Time
	IdempotencyKey string
	// 自动候选来源契约（批次 1 字段契约）：全局画像类别、用户证据原话、被采纳
	// 的助手原话、支撑当前值的原话时间。随候选进草稿，确认后随正式记忆保留。
	// 请记住与手动路径留空；批次 2 的提取端负责填充。
	ProfileClass      model.MemoryProfileClass
	EvidenceMessages  []string
	AssistantMessages []string
	StatementTime     *time.Time
	SourceSession     string
}

type CreateRequest struct {
	UserID         int64
	Input          ExplicitMemoryInput
	Decision       string
	TargetMemoryID string
	Reason         string
}

type CreateResult struct {
	Memory    *model.LongTermMemory
	Revision  *model.MemoryRevision
	Conflicts []model.LongTermMemory
	Replay    bool
}

func (s *GovernanceService) Create(ctx context.Context, request CreateRequest) (CreateResult, error) {
	if s == nil || s.explicit == nil {
		return CreateResult{}, ErrDreamNotConfigured
	}
	input, err := normalizeExplicitInput(request.UserID, request.Input, s.now(), false)
	if err != nil || strings.TrimSpace(request.Reason) == "" || utf8.RuneCountInString(request.Reason) > 500 {
		return CreateResult{}, ErrInvalidExplicitWrite
	}
	if replay, err := s.explicit.GetExplicitWrite(ctx, request.UserID, input.IdempotencyKey); err != nil {
		return CreateResult{}, err
	} else if replay != nil {
		return CreateResult{Memory: &replay.Memory, Revision: &replay.Revision, Replay: true}, nil
	}
	decision := strings.ToLower(strings.TrimSpace(request.Decision))
	if decision != "" && decision != "add" && decision != "replace" {
		return CreateResult{}, ErrInvalidExplicitWrite
	}
	conflicts, snapshotHash, snapshotSize, _, conflictErr := s.explicitConflicts(ctx, request.UserID, input)
	if conflictErr != nil {
		return CreateResult{}, conflictErr
	}
	if len(conflicts) > 0 && decision == "" {
		return CreateResult{Conflicts: conflicts}, nil
	}
	if input.Scope == model.MemoryScopeGlobal && decision == "add" && snapshotSize >= GlobalMemoryHardLimit {
		return CreateResult{}, ErrGlobalMemoryCapacity
	}
	target := ""
	if decision == "replace" {
		target = strings.TrimSpace(request.TargetMemoryID)
		if !containsMemoryID(conflicts, target) {
			return CreateResult{}, memorydao.ErrMemoryConflict
		}
	}
	memory := model.LongTermMemory{
		UserID: request.UserID, Scope: input.Scope, Subject: input.Subject, Content: input.Content, StructuredJSON: `{}`,
		Tags: model.StringList(input.Tags), TopicKey: input.TopicKey, Durability: input.Durability, ValidUntil: input.ValidUntil,
		SourceRefs: model.StringList{"user_api:" + input.IdempotencyKey}, Confidence: 1, Importance: 8,
		// 来源字段是统一写入口的一部分：自动候选经草稿带全量，手动 UI 不传即为
		// 零值（兼容不变），API 直写的调用方可以选择性携带。
		ProfileClass: input.ProfileClass, EvidenceMessages: model.StringList(input.EvidenceMessages),
		AssistantMessages: model.StringList(input.AssistantMessages), StatementTime: input.StatementTime,
		SourceSession: input.SourceSession,
	}
	written, err := s.explicit.CreateUserMemory(ctx, memorydao.ExplicitWriteParams{
		Memory: memory, TargetMemoryID: target, RequestKey: input.IdempotencyKey, Reason: strings.TrimSpace(request.Reason), Now: s.now(), GlobalSnapshotHash: snapshotHash,
	})
	if err != nil {
		return CreateResult{}, err
	}
	s.syncExplicitWrite(ctx, &written.Memory, target)
	return CreateResult{Memory: &written.Memory, Revision: &written.Revision, Replay: written.Replay}, nil
}

type DraftRequest struct {
	UserID    int64
	SessionID string
	RunID     string
	Input     ExplicitMemoryInput
}

// ProposeDraft captures one explicit-write candidate as a pending draft. It
// returns (draft, created, skipReason, error); skipReason is non-empty only
// when no draft was created on purpose:
//   - a pending draft with the same content inside one scope class coalesces
//     into the existing card (created=false, no skip reason);
//   - an ignored draft from the same source session suppresses the retry
//     (SkipIgnoredSource) - a re-expression from a different session is a new
//     candidate and proposes normally;
//   - an active memory with the same content hash, or a global semantic
//     judgement of duplicate, keeps the existing entry and skips the proposal
//     (SkipDuplicateExisting) - the confirmation list never shows a duplicate
//     and the learner never has to dismiss one.
func (s *GovernanceService) ProposeDraft(ctx context.Context, request DraftRequest) (*model.MemoryDraft, bool, string, error) {
	if s == nil || s.explicit == nil {
		return nil, false, "", ErrDreamNotConfigured
	}
	input, err := normalizeExplicitInput(request.UserID, request.Input, s.now(), true)
	if err != nil || strings.TrimSpace(request.SessionID) == "" || strings.TrimSpace(request.RunID) == "" {
		return nil, false, "", ErrInvalidExplicitWrite
	}
	contentHash := model.MemoryContentHash(input.Content)
	// Cheap database checks run before the conflict query so a retry against
	// an existing card never pays for a classifier call.
	if pending, err := s.explicit.FindDraftByContent(ctx, request.UserID, input.Scope, input.TopicKey, contentHash, "", model.MemoryDraftPending); err != nil {
		return nil, false, "", err
	} else if pending != nil {
		return pending, false, "", nil
	}
	if ignored, err := s.explicit.FindDraftByContent(ctx, request.UserID, input.Scope, input.TopicKey, contentHash, request.SessionID, model.MemoryDraftCancelled); err != nil {
		return nil, false, "", err
	} else if ignored != nil {
		return nil, false, SkipIgnoredSource, nil
	}
	conflicts, _, _, semanticDuplicate, err := s.explicitConflicts(ctx, request.UserID, input)
	if err != nil {
		return nil, false, "", err
	}
	duplicateExisting := semanticDuplicate
	if !duplicateExisting {
		for _, conflict := range conflicts {
			if conflict.ContentHash == contentHash {
				duplicateExisting = true
				break
			}
		}
	}
	if duplicateExisting {
		return nil, false, SkipDuplicateExisting, nil
	}
	conflictIDs := make(model.StringList, 0, len(conflicts))
	conflictHashes := make(model.StringList, 0, len(conflicts))
	for _, conflict := range conflicts {
		conflictIDs = append(conflictIDs, conflict.ID)
		conflictHashes = append(conflictHashes, conflict.ContentHash)
	}
	now := s.now()
	draft := &model.MemoryDraft{
		ID: uuid.NewString(), UserID: request.UserID, SessionID: strings.TrimSpace(request.SessionID), RunID: strings.TrimSpace(request.RunID),
		IdempotencyKey: input.IdempotencyKey, Scope: input.Scope, Subject: input.Subject, Content: input.Content,
		ContentHash: contentHash, Tags: model.StringList(input.Tags), TopicKey: input.TopicKey,
		Durability: input.Durability, ValidUntil: input.ValidUntil, ConflictIDs: conflictIDs, ConflictHashes: conflictHashes,
		Status: model.MemoryDraftPending, CreatedAt: now, UpdatedAt: now,
		ProfileClass: input.ProfileClass, EvidenceMessages: model.StringList(input.EvidenceMessages), AssistantMessages: model.StringList(input.AssistantMessages),
		StatementTime: input.StatementTime,
	}
	createdDraft, created, err := s.explicit.UpsertDraft(ctx, draft)
	return createdDraft, created, "", err
}

func (s *GovernanceService) explicitConflicts(ctx context.Context, userID int64, input ExplicitMemoryInput) ([]model.LongTermMemory, string, int, bool, error) {
	if input.Scope != model.MemoryScopeGlobal {
		conflicts, err := s.explicit.FindExplicitConflicts(ctx, userID, input.Scope, input.TopicKey, model.MemoryContentHash(input.Content), input.Tags, 10)
		return conflicts, "", 0, false, err
	}
	if s.globalSource == nil || s.globalRelations == nil {
		return nil, "", 0, false, ErrGlobalRelationUnavailable
	}
	// Byte-identical content is authoritative and does not need a provider call.
	exact, err := s.explicit.FindExplicitConflicts(ctx, userID, input.Scope, "", model.MemoryContentHash(input.Content), nil, 10)
	if err != nil {
		return nil, "", 0, false, err
	}
	existing, err := s.globalSource.ListAllActive(ctx, userID, model.MemoryScopeGlobal, 0, GlobalMemoryHardLimit+1)
	if err != nil {
		return nil, "", 0, false, fmt.Errorf("load global memory conflict snapshot: %w", err)
	}
	if len(existing) > GlobalMemoryHardLimit {
		return nil, "", len(existing), false, ErrGlobalMemoryCapacity
	}
	snapshotHash := memorydao.GlobalSnapshotFingerprint(existing)
	if len(exact) > 0 {
		return exact, snapshotHash, len(existing), false, nil
	}
	relation, err := s.globalRelations.Classify(ctx, input, existing)
	if err != nil {
		return nil, "", len(existing), false, fmt.Errorf("classify global memory conflict: %w", err)
	}
	if relation.Relation == GlobalRelationIndependent {
		if len(existing) == GlobalMemoryHardLimit {
			return nil, "", len(existing), false, ErrGlobalMemoryCapacity
		}
		return nil, snapshotHash, len(existing), false, nil
	}
	for _, memory := range existing {
		if memory.ID == relation.TargetMemoryID {
			return []model.LongTermMemory{memory}, snapshotHash, len(existing), relation.Relation == GlobalRelationDuplicate, nil
		}
	}
	return nil, "", len(existing), false, ErrInvalidGlobalRelation
}

func (s *GovernanceService) PendingDrafts(ctx context.Context, userID int64) ([]model.MemoryDraft, error) {
	if s == nil || s.explicit == nil {
		return nil, ErrDreamNotConfigured
	}
	return s.explicit.ListDrafts(ctx, userID, model.MemoryDraftPending, 100)
}

// ActiveMemoriesByIDs exposes the conflict-target lookup the memory page uses
// to hydrate the details a draft card shows beside its replace decision.
func (s *GovernanceService) ActiveMemoriesByIDs(ctx context.Context, userID int64, ids []string) ([]model.LongTermMemory, error) {
	if s == nil || s.explicit == nil {
		return nil, ErrDreamNotConfigured
	}
	return s.explicit.FindActiveByIDs(ctx, userID, ids)
}

// MemoriesBySession is the user-scoped trace-back path of the provenance
// contract: given the source session a confirmed memory came from, return the
// memories it produced. Ownership is enforced by the user id, so one learner
// can never read another learner's session trail.
func (s *GovernanceService) MemoriesBySession(ctx context.Context, userID int64, sessionID string) ([]model.LongTermMemory, error) {
	if s == nil || s.explicit == nil {
		return nil, ErrDreamNotConfigured
	}
	return s.explicit.FindBySourceSession(ctx, userID, sessionID)
}

// EditDraftRequest carries the learner's changes to a pending draft. Pointer
// fields are optional: nil leaves the stored value untouched.
type EditDraftRequest struct {
	UserID     int64
	DraftID    string
	Subject    *string
	Content    *string
	Tags       []string
	TopicKey   *string
	Durability *model.MemoryDurability
	ValidUntil *time.Time
}

type EditDraftResult struct {
	Draft *model.MemoryDraft
	// Closed reports that the re-check judged the final content under the
	// final topic identical to an active memory, so the draft was closed by
	// the system (CancelReasonSemanticDuplicate) and the existing entry stays
	// effective. The learner never has to abandon such a draft.
	Closed bool
	// Targets are the conflict rows the re-check produced - the judged target
	// when closed, the replace candidates otherwise.
	Targets []model.LongTermMemory
}

// EditDraft applies the learner's changes to a pending draft and re-checks
// conflicts with the final content under the final topic. The semantic
// judgement runs once per explicit save: a duplicate closes the draft without
// asking the learner to abandon anything, a replacement or complementary
// detail goes to the normal conflict decision, and an independent detail is a
// plain update of the pending card. Nothing here writes official memory; the
// confirm path remains the safety net that re-verifies the pinned conflict
// hashes against the live rows.
func (s *GovernanceService) EditDraft(ctx context.Context, request EditDraftRequest) (EditDraftResult, error) {
	if s == nil || s.explicit == nil {
		return EditDraftResult{}, ErrDreamNotConfigured
	}
	if request.UserID <= 0 || strings.TrimSpace(request.DraftID) == "" {
		return EditDraftResult{}, ErrInvalidExplicitWrite
	}
	stored, err := s.explicit.GetDraft(ctx, request.UserID, strings.TrimSpace(request.DraftID))
	if err != nil {
		return EditDraftResult{}, err
	}
	if stored.Status != model.MemoryDraftPending {
		return EditDraftResult{}, memorydao.ErrDraftNotPending
	}
	merged := ExplicitMemoryInput{
		Scope: stored.Scope, Subject: stored.Subject, Content: stored.Content,
		Tags: []string(stored.Tags), TopicKey: stored.TopicKey,
		Durability: stored.Durability, ValidUntil: stored.ValidUntil, IdempotencyKey: stored.IdempotencyKey,
		// 来源字段只随候选产生：编辑保留候选上的原值，批次 2 提取端负责填充。
		ProfileClass: stored.ProfileClass, EvidenceMessages: stored.EvidenceMessages, AssistantMessages: stored.AssistantMessages,
		StatementTime: stored.StatementTime,
	}
	if request.Subject != nil {
		merged.Subject = *request.Subject
	}
	if request.Content != nil {
		merged.Content = *request.Content
	}
	if request.Tags != nil {
		merged.Tags = request.Tags
	}
	if request.TopicKey != nil {
		merged.TopicKey = *request.TopicKey
	}
	if request.Durability != nil {
		merged.Durability = *request.Durability
	}
	if request.ValidUntil != nil {
		merged.ValidUntil = request.ValidUntil
	}
	allowPendingTopic := stored.Scope == model.MemoryScopeTopic && strings.TrimSpace(merged.TopicKey) == ""
	input, err := normalizeExplicitInput(request.UserID, merged, s.now(), allowPendingTopic)
	if err != nil {
		return EditDraftResult{}, err
	}
	conflicts, _, _, semanticDuplicate, err := s.explicitConflicts(ctx, request.UserID, input)
	if err != nil {
		return EditDraftResult{}, err
	}
	targets := append([]model.LongTermMemory(nil), conflicts...)
	closeAsDuplicate := semanticDuplicate
	for _, conflict := range conflicts {
		if conflict.ContentHash == model.MemoryContentHash(input.Content) {
			closeAsDuplicate = true
			break
		}
	}
	if !closeAsDuplicate && input.Scope == model.MemoryScopeTopic && s.topicRelations != nil {
		sameTopic := make([]model.LongTermMemory, 0, len(targets))
		for _, row := range targets {
			if row.TopicKey == input.TopicKey {
				sameTopic = append(sameTopic, row)
			}
		}
		if len(sameTopic) > 0 {
			relation, judgeErr := s.topicRelations.Classify(ctx, input, sameTopic)
			if judgeErr != nil {
				return EditDraftResult{}, fmt.Errorf("judge topic memory relation: %w", judgeErr)
			}
			if judgeValidateErr := validateTopicRelationResult(relation, sameTopic); judgeValidateErr != nil {
				return EditDraftResult{}, judgeValidateErr
			}
			switch relation.Relation {
			case TopicRelationDuplicate:
				closeAsDuplicate = true
			case TopicRelationIndependent:
				targets = nil
			default:
				targets = targetRows(sameTopic, relation.TargetMemoryID)
			}
		}
	}
	conflictIDs := make(model.StringList, 0, len(targets))
	conflictHashes := make(model.StringList, 0, len(targets))
	for _, target := range targets {
		conflictIDs = append(conflictIDs, target.ID)
		conflictHashes = append(conflictHashes, target.ContentHash)
	}
	closeFlag := closeAsDuplicate
	updated, err := s.explicit.ApplyDraftEdit(ctx, memorydao.DraftEditParams{
		UserID: request.UserID, DraftID: strings.TrimSpace(request.DraftID),
		Subject: input.Subject, Content: input.Content, ContentHash: model.MemoryContentHash(input.Content),
		Tags: model.StringList(input.Tags), TopicKey: input.TopicKey,
		Durability: input.Durability, ValidUntil: input.ValidUntil,
		ConflictIDs: conflictIDs, ConflictHashes: conflictHashes,
		Close: &closeFlag, CancelReason: model.CancelReasonSemanticDuplicate, Now: s.now(),
	})
	if err != nil {
		return EditDraftResult{}, err
	}
	return EditDraftResult{Draft: updated, Closed: closeAsDuplicate, Targets: targets}, nil
}

func targetRows(rows []model.LongTermMemory, targetID string) []model.LongTermMemory {
	for _, row := range rows {
		if row.ID == targetID {
			return []model.LongTermMemory{row}
		}
	}
	return nil
}

type ConfirmDraftRequest struct {
	UserID         int64
	DraftID        string
	Action         string
	TargetMemoryID string
	Reason         string
	// ExpectedTargetHash carries the version the user re-confirmed against on
	// the replace path: set after the user looked at the latest content the
	// previous attempt returned as modified. Empty falls back to the hash
	// pinned on the draft when it captured the conflict.
	ExpectedTargetHash string
}

type ConfirmDraftResult struct {
	Draft    model.MemoryDraft
	Memory   *model.LongTermMemory
	Revision *model.MemoryRevision
	Replay   bool
}

func (s *GovernanceService) ConfirmDraft(ctx context.Context, request ConfirmDraftRequest) (ConfirmDraftResult, error) {
	if s == nil || s.explicit == nil {
		return ConfirmDraftResult{}, ErrDreamNotConfigured
	}
	action := strings.ToLower(strings.TrimSpace(request.Action))
	if request.UserID <= 0 || strings.TrimSpace(request.DraftID) == "" || (action != "add" && action != "replace" && action != "cancel") ||
		(action != "cancel" && strings.TrimSpace(request.Reason) == "") || utf8.RuneCountInString(request.Reason) > 500 {
		return ConfirmDraftResult{}, ErrInvalidExplicitWrite
	}
	snapshotHash := ""
	if action != "cancel" {
		stored, err := s.explicit.GetDraft(ctx, request.UserID, strings.TrimSpace(request.DraftID))
		if err != nil {
			return ConfirmDraftResult{}, err
		}
		// Replay note: an already-confirmed draft must fall through, so a
		// duplicate submission returns the same result instead of an error
		// (resolveDraftTx replays it). Only pending drafts run the checks
		// below; cancelled drafts replay as a no-op.
		if stored.Status == model.MemoryDraftPending {
			// An attribution-pending draft cannot be written. The learner
			// assigns the topic through the edit endpoint; the confirm API
			// never picks a topic for them.
			if stored.Scope == model.MemoryScopeTopic && strings.TrimSpace(stored.TopicKey) == "" {
				return ConfirmDraftResult{}, memorydao.ErrDraftAttributionRequired
			}
		}
		// Fast path for the replace version check: answer with the latest
		// content without opening the write transaction. The authoritative
		// check runs inside the transaction after the target row is locked.
		targetID := strings.TrimSpace(request.TargetMemoryID)
		if action == "replace" && targetID != "" {
			expectedHash := strings.TrimSpace(request.ExpectedTargetHash)
			if expectedHash == "" {
				expectedHash = memorydao.PinnedConflictHash(*stored, targetID)
			}
			if expectedHash != "" {
				if current, currentErr := s.explicit.GetActiveByID(ctx, request.UserID, targetID); currentErr == nil && current.ContentHash != expectedHash {
					return ConfirmDraftResult{}, memorydao.ErrMemoryTargetModified
				}
			}
		}
		if stored.Status == model.MemoryDraftPending && stored.Scope == model.MemoryScopeGlobal {
			input, normalizeErr := normalizeExplicitInput(request.UserID, ExplicitMemoryInput{
				Scope: stored.Scope, Subject: stored.Subject, Content: stored.Content, Tags: stored.Tags, TopicKey: stored.TopicKey,
				Durability: stored.Durability, ValidUntil: stored.ValidUntil, IdempotencyKey: stored.IdempotencyKey,
				ProfileClass: stored.ProfileClass, EvidenceMessages: stored.EvidenceMessages, AssistantMessages: stored.AssistantMessages,
			}, s.now(), false)
			if normalizeErr != nil {
				return ConfirmDraftResult{}, normalizeErr
			}
			currentConflicts, currentSnapshot, currentSize, _, conflictErr := s.explicitConflicts(ctx, request.UserID, input)
			if conflictErr != nil {
				return ConfirmDraftResult{}, conflictErr
			}
			if action == "replace" && !containsMemoryID(currentConflicts, strings.TrimSpace(request.TargetMemoryID)) {
				return ConfirmDraftResult{}, memorydao.ErrMemoryConflict
			}
			if action == "add" && hasNewConflict(currentConflicts, stored.ConflictIDs) {
				return ConfirmDraftResult{}, memorydao.ErrMemoryConflict
			}
			if action == "add" && currentSize >= GlobalMemoryHardLimit {
				return ConfirmDraftResult{}, ErrGlobalMemoryCapacity
			}
			snapshotHash = currentSnapshot
		}
		if stored.Status == model.MemoryDraftPending && stored.Scope == model.MemoryScopeTopic && action == "add" {
			// Content-level guard mirroring the global check: a byte-identical
			// note that appeared after the proposal blocks a blind add. Same
			// topic rows without matching content are the normal replace
			// targets and stay with the user's decision.
			rows, rowsErr := s.explicit.FindExplicitConflicts(ctx, request.UserID, model.MemoryScopeTopic, stored.TopicKey, model.MemoryContentHash(stored.Content), nil, 10)
			if rowsErr != nil {
				return ConfirmDraftResult{}, rowsErr
			}
			sameContent := make([]model.LongTermMemory, 0, len(rows))
			storedHash := model.MemoryContentHash(stored.Content)
			for _, row := range rows {
				if row.ContentHash == storedHash {
					sameContent = append(sameContent, row)
				}
			}
			if hasNewConflict(sameContent, stored.ConflictIDs) {
				return ConfirmDraftResult{}, memorydao.ErrMemoryConflict
			}
		}
	}
	draft, write, err := s.explicit.ResolveDraft(ctx, memorydao.ResolveDraftParams{
		UserID: request.UserID, DraftID: strings.TrimSpace(request.DraftID), Action: action,
		TargetMemoryID: strings.TrimSpace(request.TargetMemoryID), Reason: strings.TrimSpace(request.Reason), Now: s.now(),
		GlobalSnapshotHash: snapshotHash, ExpectedTargetHash: strings.TrimSpace(request.ExpectedTargetHash),
	})
	if err != nil {
		return ConfirmDraftResult{}, err
	}
	result := ConfirmDraftResult{Draft: *draft}
	if write != nil {
		result.Memory, result.Revision, result.Replay = &write.Memory, &write.Revision, write.Replay
		target := ""
		if action == "replace" {
			target = request.TargetMemoryID
		}
		s.syncExplicitWrite(ctx, result.Memory, target)
	}
	return result, nil
}

func hasNewConflict(current []model.LongTermMemory, approved model.StringList) bool {
	for _, memory := range current {
		if !stringListContains(approved, memory.ID) {
			return true
		}
	}
	return false
}

func stringListContains(values model.StringList, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func (s *GovernanceService) Archive(ctx context.Context, userID int64, memoryID, reason string) (*model.LongTermMemory, *model.MemoryRevision, error) {
	if s == nil || s.explicit == nil {
		return nil, nil, ErrDreamNotConfigured
	}
	if userID <= 0 || strings.TrimSpace(memoryID) == "" || strings.TrimSpace(reason) == "" || utf8.RuneCountInString(reason) > 500 {
		return nil, nil, ErrInvalidExplicitWrite
	}
	memory, revision, err := s.explicit.ArchiveMemory(ctx, userID, strings.TrimSpace(memoryID), strings.TrimSpace(reason), s.now())
	if err == nil && s.indexSync != nil {
		if syncErr := s.indexSync.Forget(ctx, userID, []string{memoryID}); syncErr != nil {
			// MySQL is authoritative; stale derived keys are still rejected on reload.
			log.Printf("memory index sync after archive %s: %v", memoryID, syncErr)
		}
	}
	return memory, revision, err
}

func (s *GovernanceService) syncExplicitWrite(ctx context.Context, memory *model.LongTermMemory, retiredID string) {
	if s.indexSync == nil || memory == nil {
		return
	}
	if retiredID != "" {
		_ = s.indexSync.Forget(ctx, memory.UserID, []string{retiredID})
	}
	_ = s.indexSync.ApplyDreamResult(ctx, memory)
}

// trimProvenanceRefs normalizes one provenance list: trim each reference,
// drop empties, keep order.
func trimProvenanceRefs(refs []string) []string {
	trimmed := make([]string, 0, len(refs))
	for _, ref := range refs {
		if value := strings.TrimSpace(ref); value != "" {
			trimmed = append(trimmed, value)
		}
	}
	return trimmed
}

func normalizeExplicitInput(userID int64, input ExplicitMemoryInput, now time.Time, allowPendingTopic bool) (ExplicitMemoryInput, error) {
	input.Scope = model.MemoryScope(strings.ToLower(strings.TrimSpace(string(input.Scope))))
	input.Subject = strings.TrimSpace(input.Subject)
	input.Content = strings.TrimSpace(input.Content)
	input.TopicKey = model.NormalizeLearningTopicKey(input.TopicKey)
	input.Durability = model.MemoryDurability(strings.ToLower(strings.TrimSpace(string(input.Durability))))
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	if userID <= 0 || input.Subject == "" || input.Content == "" || input.IdempotencyKey == "" || utf8.RuneCountInString(input.IdempotencyKey) > 128 || containsSensitive(input.Subject+"\n"+input.Content) {
		return ExplicitMemoryInput{}, ErrInvalidExplicitWrite
	}
	tags, err := normalizeOptionalTags(input.Tags)
	if err != nil {
		return ExplicitMemoryInput{}, err
	}
	if len(tags) == 0 {
		// Tags no longer drive production recall, but the explicit-write
		// conflict query still uses the fixed vocabulary to catch global
		// paraphrases. Derive them when API/model callers omit optional tags;
		// otherwise only byte-identical ContentHash values can conflict.
		tags = GenerateQueryTags(input.Subject+"\n"+input.Content, input.TopicKey)
	}
	input.Tags = tags
	// Provenance fields: trim and keep as given; the model validation on the
	// probe enforces the class vocabulary, the scope restriction and the list
	// bounds, so a malformed candidate is rejected before it becomes a draft.
	input.ProfileClass = model.MemoryProfileClass(strings.TrimSpace(string(input.ProfileClass)))
	input.EvidenceMessages = trimProvenanceRefs(input.EvidenceMessages)
	input.AssistantMessages = trimProvenanceRefs(input.AssistantMessages)
	if input.ValidUntil != nil {
		local := input.ValidUntil.In(time.Local)
		input.ValidUntil = &local
	}
	probeTopicKey := input.TopicKey
	if allowPendingTopic && input.Scope == model.MemoryScopeTopic && strings.TrimSpace(probeTopicKey) == "" {
		// The learner has not assigned the topic yet; the draft waits for it.
		// Only the shape check needs the placeholder - it still verifies the
		// durability/expiry consistency for the pending note.
		probeTopicKey = "pending-attribution"
	}
	probe := model.LongTermMemory{
		ID: "validation", UserID: userID, Scope: input.Scope, Subject: input.Subject, Content: input.Content,
		ContentHash: model.MemoryContentHash(input.Content), StructuredJSON: `{}`, Tags: model.StringList(tags), TopicKey: probeTopicKey,
		Durability: input.Durability, ValidUntil: input.ValidUntil, SourceRefs: model.StringList{"validation"}, Confidence: 1, Importance: 8,
		Status: model.LongTermMemoryActive, UserConfirmed: true, CreatedAt: now,
		ProfileClass: input.ProfileClass, EvidenceMessages: model.StringList(input.EvidenceMessages), AssistantMessages: model.StringList(input.AssistantMessages),
	}
	if err := probe.Validate(); err != nil || (input.ValidUntil != nil && !input.ValidUntil.After(now)) {
		return ExplicitMemoryInput{}, ErrInvalidExplicitWrite
	}
	return input, nil
}

func normalizeOptionalTags(tags []string) ([]string, error) {
	if len(tags) > 5 {
		return nil, ErrInvalidExplicitWrite
	}
	seen := make(map[string]struct{}, len(tags))
	result := make([]string, 0, len(tags))
	for _, raw := range tags {
		tag := strings.ToLower(strings.TrimSpace(raw))
		if tag == "" || !model.ValidMemoryTag(tag) {
			return nil, ErrInvalidExplicitWrite
		}
		if _, exists := seen[tag]; exists {
			return nil, ErrInvalidExplicitWrite
		}
		seen[tag] = struct{}{}
		result = append(result, tag)
	}
	sort.Strings(result)
	return result, nil
}

func containsMemoryID(memories []model.LongTermMemory, id string) bool {
	for _, memory := range memories {
		if memory.ID == id {
			return true
		}
	}
	return false
}
