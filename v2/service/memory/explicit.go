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

type ExplicitMemoryInput struct {
	Scope          model.MemoryScope
	Subject        string
	Content        string
	Tags           []string
	TopicKey       string
	Durability     model.MemoryDurability
	ValidUntil     *time.Time
	IdempotencyKey string
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
	input, err := normalizeExplicitInput(request.UserID, request.Input, s.now())
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
	conflicts, snapshotHash, snapshotSize, err := s.explicitConflicts(ctx, request.UserID, input)
	if err != nil {
		return CreateResult{}, err
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

func (s *GovernanceService) ProposeDraft(ctx context.Context, request DraftRequest) (*model.MemoryDraft, bool, error) {
	if s == nil || s.explicit == nil {
		return nil, false, ErrDreamNotConfigured
	}
	input, err := normalizeExplicitInput(request.UserID, request.Input, s.now())
	if err != nil || strings.TrimSpace(request.SessionID) == "" || strings.TrimSpace(request.RunID) == "" {
		return nil, false, ErrInvalidExplicitWrite
	}
	conflicts, _, _, err := s.explicitConflicts(ctx, request.UserID, input)
	if err != nil {
		return nil, false, err
	}
	conflictIDs := make(model.StringList, 0, len(conflicts))
	for _, conflict := range conflicts {
		conflictIDs = append(conflictIDs, conflict.ID)
	}
	now := s.now()
	draft := &model.MemoryDraft{
		ID: uuid.NewString(), UserID: request.UserID, SessionID: strings.TrimSpace(request.SessionID), RunID: strings.TrimSpace(request.RunID),
		IdempotencyKey: input.IdempotencyKey, Scope: input.Scope, Subject: input.Subject, Content: input.Content,
		ContentHash: model.MemoryContentHash(input.Content), Tags: model.StringList(input.Tags), TopicKey: input.TopicKey,
		Durability: input.Durability, ValidUntil: input.ValidUntil, ConflictIDs: conflictIDs, Status: model.MemoryDraftPending, CreatedAt: now, UpdatedAt: now,
	}
	return s.explicit.UpsertDraft(ctx, draft)
}

func (s *GovernanceService) explicitConflicts(ctx context.Context, userID int64, input ExplicitMemoryInput) ([]model.LongTermMemory, string, int, error) {
	if input.Scope != model.MemoryScopeGlobal {
		conflicts, err := s.explicit.FindExplicitConflicts(ctx, userID, input.Scope, input.TopicKey, model.MemoryContentHash(input.Content), input.Tags, 10)
		return conflicts, "", 0, err
	}
	if s.globalSource == nil || s.globalRelations == nil {
		return nil, "", 0, ErrGlobalRelationUnavailable
	}
	// Byte-identical content is authoritative and does not need a provider call.
	exact, err := s.explicit.FindExplicitConflicts(ctx, userID, input.Scope, "", model.MemoryContentHash(input.Content), nil, 10)
	if err != nil {
		return nil, "", 0, err
	}
	existing, err := s.globalSource.ListAllActive(ctx, userID, model.MemoryScopeGlobal, 0, GlobalMemoryHardLimit+1)
	if err != nil {
		return nil, "", 0, fmt.Errorf("load global memory conflict snapshot: %w", err)
	}
	if len(existing) > GlobalMemoryHardLimit {
		return nil, "", len(existing), ErrGlobalMemoryCapacity
	}
	snapshotHash := memorydao.GlobalSnapshotFingerprint(existing)
	if len(exact) > 0 {
		return exact, snapshotHash, len(existing), nil
	}
	relation, err := s.globalRelations.Classify(ctx, input, existing)
	if err != nil {
		return nil, "", len(existing), fmt.Errorf("classify global memory conflict: %w", err)
	}
	if relation.Relation == GlobalRelationIndependent {
		if len(existing) == GlobalMemoryHardLimit {
			return nil, "", len(existing), ErrGlobalMemoryCapacity
		}
		return nil, snapshotHash, len(existing), nil
	}
	for _, memory := range existing {
		if memory.ID == relation.TargetMemoryID {
			return []model.LongTermMemory{memory}, snapshotHash, len(existing), nil
		}
	}
	return nil, "", len(existing), ErrInvalidGlobalRelation
}

func (s *GovernanceService) PendingDrafts(ctx context.Context, userID int64) ([]model.MemoryDraft, error) {
	if s == nil || s.explicit == nil {
		return nil, ErrDreamNotConfigured
	}
	return s.explicit.ListDrafts(ctx, userID, model.MemoryDraftPending, 100)
}

type ConfirmDraftRequest struct {
	UserID         int64
	DraftID        string
	Action         string
	TargetMemoryID string
	Reason         string
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
		if stored.Status == model.MemoryDraftPending && stored.Scope == model.MemoryScopeGlobal {
			input, normalizeErr := normalizeExplicitInput(request.UserID, ExplicitMemoryInput{
				Scope: stored.Scope, Subject: stored.Subject, Content: stored.Content, Tags: stored.Tags, TopicKey: stored.TopicKey,
				Durability: stored.Durability, ValidUntil: stored.ValidUntil, IdempotencyKey: stored.IdempotencyKey,
			}, s.now())
			if normalizeErr != nil {
				return ConfirmDraftResult{}, normalizeErr
			}
			currentConflicts, currentSnapshot, currentSize, conflictErr := s.explicitConflicts(ctx, request.UserID, input)
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
	}
	draft, write, err := s.explicit.ResolveDraft(ctx, memorydao.ResolveDraftParams{
		UserID: request.UserID, DraftID: strings.TrimSpace(request.DraftID), Action: action,
		TargetMemoryID: strings.TrimSpace(request.TargetMemoryID), Reason: strings.TrimSpace(request.Reason), Now: s.now(), GlobalSnapshotHash: snapshotHash,
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

func normalizeExplicitInput(userID int64, input ExplicitMemoryInput, now time.Time) (ExplicitMemoryInput, error) {
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
	if input.ValidUntil != nil {
		local := input.ValidUntil.In(time.Local)
		input.ValidUntil = &local
	}
	probe := model.LongTermMemory{
		ID: "validation", UserID: userID, Scope: input.Scope, Subject: input.Subject, Content: input.Content,
		ContentHash: model.MemoryContentHash(input.Content), StructuredJSON: `{}`, Tags: model.StringList(tags), TopicKey: input.TopicKey,
		Durability: input.Durability, ValidUntil: input.ValidUntil, SourceRefs: model.StringList{"validation"}, Confidence: 1, Importance: 8,
		Status: model.LongTermMemoryActive, UserConfirmed: true, CreatedAt: now,
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
