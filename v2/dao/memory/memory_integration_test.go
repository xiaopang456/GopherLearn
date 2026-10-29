package memory

import (
	"GopherAI/internal/testdb"
	"GopherAI/model"
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	mysqldriver "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestRepositoryIdempotencyLeaseRecoveryAndUserIsolation(t *testing.T) {
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), TranslateError: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.MemoryExtractionJob{}, &model.MemoryCandidate{}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	repository := NewRepository(db)
	userID := uniqueMemoryTestUserID()
	job := &model.MemoryExtractionJob{
		ID: uuid.NewString(), UserID: userID, EventType: model.MemoryEventAgentRun, SourceID: "run-" + uuid.NewString(),
		SourceSessionID: uuid.NewString(), SourceRunID: uuid.NewString(), Status: model.MemoryJobPending, AvailableAt: now,
	}
	t.Cleanup(func() {
		db.Unscoped().Where("user_id IN ?", []int64{userID, userID + 1}).Delete(&model.MemoryCandidate{})
		db.Unscoped().Where("user_id IN ?", []int64{userID, userID + 1}).Delete(&model.MemoryExtractionJob{})
	})
	created, err := repository.Enqueue(ctx, job)
	if err != nil || !created {
		t.Fatalf("first enqueue created=%v err=%v", created, err)
	}
	duplicate := *job
	duplicate.ID = uuid.NewString()
	created, err = repository.Enqueue(ctx, &duplicate)
	if err != nil || created {
		t.Fatalf("duplicate enqueue created=%v err=%v", created, err)
	}
	otherUser := *job
	otherUser.ID = uuid.NewString()
	otherUser.UserID++
	created, err = repository.Enqueue(ctx, &otherUser)
	if err != nil || !created {
		t.Fatalf("same source for another user created=%v err=%v", created, err)
	}

	claimed, err := repository.Claim(ctx, job.ID, now.Add(time.Millisecond), time.Minute, 3)
	if err != nil || claimed.UserID != job.UserID || claimed.Attempts != 1 {
		t.Fatalf("claim=%#v err=%v", claimed, err)
	}
	if _, err := repository.Claim(ctx, job.ID, now.Add(time.Millisecond), time.Minute, 3); !errors.Is(err, ErrJobConflict) {
		t.Fatalf("duplicate claim err=%v", err)
	}
	// Simulate process interruption after a lease was acquired. Startup recovery
	// must make the durable job claimable again without an in-memory goroutine.
	if err := db.Model(&model.MemoryExtractionJob{}).Where("id = ?", job.ID).Update("lease_ends_at", now.Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	if count, err := repository.RecoverExpired(ctx, now, 3); err != nil || count != 1 {
		t.Fatalf("recover count=%d err=%v", count, err)
	}
	reclaimed, err := repository.Claim(ctx, job.ID, now.Add(time.Millisecond), time.Minute, 3)
	if err != nil || reclaimed.Attempts != 2 {
		t.Fatalf("reclaim=%#v err=%v", reclaimed, err)
	}
	candidate := &model.MemoryCandidate{
		ID: uuid.NewString(), JobID: reclaimed.ID, UserID: reclaimed.UserID, Scope: model.MemoryScopeTopic, TopicKey: "docker", Durability: model.MemoryDurabilityStable,
		Subject: "deployment", ProposedContent: "GopherAI runs in Docker", ContentHash: model.MemoryContentHash("GopherAI runs in Docker"), StructuredJSON: `{}`,
		Tags: model.StringList{"docker", "gopherai", "deployment"}, SourceRefs: model.StringList{"agent_run:" + reclaimed.SourceRunID},
		Confidence: .9, Importance: 7, Sensitivity: model.MemorySensitivitySafe, Status: model.MemoryCandidatePending,
	}
	if err := repository.Complete(ctx, reclaimed, candidate, model.MemoryJobCompleted, "", now); err != nil {
		t.Fatal(err)
	}
	var visible, foreign int64
	db.Model(&model.MemoryCandidate{}).Where("job_id = ? AND user_id = ?", job.ID, job.UserID).Count(&visible)
	db.Model(&model.MemoryCandidate{}).Where("job_id = ? AND user_id = ?", job.ID, job.UserID+1).Count(&foreign)
	if visible != 1 || foreign != 0 {
		t.Fatalf("candidate visibility owner=%d foreign=%d", visible, foreign)
	}
	if err := repository.Complete(ctx, reclaimed, candidate, model.MemoryJobCompleted, "", now); !errors.Is(err, ErrJobConflict) && !errors.Is(err, gorm.ErrDuplicatedKey) {
		t.Fatalf("duplicate completion err=%v", err)
	}
}

func TestRecallUsesUserStatusTypeTagsConfidenceAndMarkUsedIsolation(t *testing.T) {
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), TranslateError: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.LongTermMemory{}, &model.MemoryRevision{}); err != nil {
		t.Fatal(err)
	}
	userID := uniqueMemoryTestUserID()
	foreignUserID := userID + 1
	active := recallFixture(uuid.NewString(), userID, model.MemoryScopeTopic, model.LongTermMemoryActive, []string{"docker", "gopherai", "project"}, .9)
	foreign := recallFixture(uuid.NewString(), foreignUserID, model.MemoryScopeTopic, model.LongTermMemoryActive, []string{"docker", "gopherai", "project"}, .99)
	archived := recallFixture(uuid.NewString(), userID, model.MemoryScopeTopic, model.LongTermMemoryArchived, []string{"docker", "gopherai", "project"}, .99)
	low := recallFixture(uuid.NewString(), userID, model.MemoryScopeTopic, model.LongTermMemoryActive, []string{"docker", "gopherai", "project"}, .3)
	expired := recallFixture(uuid.NewString(), userID, model.MemoryScopeTopic, model.LongTermMemoryActive, []string{"docker"}, .99)
	past := time.Now().UTC().Add(-time.Hour)
	expired.Durability, expired.ValidUntil = model.MemoryDurabilityTimeBound, &past
	globalNoTags := recallFixture(uuid.NewString(), userID, model.MemoryScopeGlobal, model.LongTermMemoryActive, nil, .9)
	globalNoTags.TopicKey = ""
	// A pre-AGENT-018 row: same user, same tag, high confidence. It must stay
	// stored and stay out of every result, which is what "keep but freeze"
	// means in practice.
	retired := recallFixture(uuid.NewString(), userID, model.MemoryScopeTopic, model.LongTermMemoryActive, []string{"docker", "gopherai", "project"}, .99)
	retired.Scope = model.MemoryScopeLegacyProjectFact
	retired.TopicKey = ""
	fixtures := []model.LongTermMemory{active, foreign, archived, low, retired, expired, globalNoTags}
	if err := db.Create(&fixtures).Error; err != nil {
		t.Fatal(err)
	}
	ids := []string{active.ID, foreign.ID, archived.ID, low.ID, retired.ID, expired.ID, globalNoTags.ID}
	t.Cleanup(func() {
		db.Unscoped().Where("user_id IN ?", []int64{userID, foreignUserID}).Delete(&model.MemoryRevision{})
		db.Unscoped().Where("id IN ?", ids).Delete(&model.LongTermMemory{})
	})
	repository := NewRepository(db)
	memories, err := repository.Recall(context.Background(), RecallQuery{
		UserID: userID, Scopes: []model.MemoryScope{model.MemoryScopeTopic, model.MemoryScopeGlobal},
		Tags: []string{"docker", "project"}, MinConfidence: .6, Limit: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(memories) != 1 || memories[0].ID != active.ID {
		t.Fatalf("recalled=%#v", memories)
	}
	var frozen model.LongTermMemory
	if err := db.Where("id = ?", retired.ID).First(&frozen).Error; err != nil || frozen.Content == "" {
		t.Fatalf("a retired-type row must be preserved rather than deleted: %+v err=%v", frozen, err)
	}
	comparable, err := repository.ListComparable(context.Background(), userID, model.MemoryScopeGlobal, nil, globalNoTags.ContentHash, 10)
	if err != nil || len(comparable) != 1 || comparable[0].ID != globalNoTags.ID {
		t.Fatalf("no-tag comparable=%+v err=%v", comparable, err)
	}
	if count, err := repository.ArchiveExpired(context.Background(), time.Now().UTC(), 100); err != nil || count < 1 {
		t.Fatalf("archive expired count=%d err=%v", count, err)
	}
	var archivedExpired model.LongTermMemory
	if err := db.First(&archivedExpired, "id = ?", expired.ID).Error; err != nil || archivedExpired.Status != model.LongTermMemoryArchived {
		t.Fatalf("expired status=%s err=%v", archivedExpired.Status, err)
	}
	var expiryRevision model.MemoryRevision
	if err := db.Where("memory_id = ? AND actor = ?", expired.ID, model.MemoryRevisionSystem).First(&expiryRevision).Error; err != nil || expiryRevision.BeforeSnapshot == "" || expiryRevision.AfterSnapshot == "" {
		t.Fatalf("expiry revision=%+v err=%v", expiryRevision, err)
	}
	usedAt := time.Now().UTC().Truncate(time.Millisecond)
	if err := repository.MarkUsed(context.Background(), userID, []string{active.ID, foreign.ID}, usedAt); err != nil {
		t.Fatal(err)
	}
	var owner, other model.LongTermMemory
	if err := db.Where("id = ?", active.ID).First(&owner).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Where("id = ?", foreign.ID).First(&other).Error; err != nil {
		t.Fatal(err)
	}
	if owner.UsageCount != 1 || owner.LastUsedAt == nil || other.UsageCount != 0 || other.LastUsedAt != nil {
		t.Fatalf("owner=%+v foreign=%+v", owner, other)
	}
}

func recallFixture(id string, userID int64, memoryType model.MemoryScope, status model.LongTermMemoryStatus, tags []string, confidence float64) model.LongTermMemory {
	memory := model.LongTermMemory{
		ID: id, UserID: userID, Scope: memoryType, Subject: id, Content: id + " content", ContentHash: model.MemoryContentHash(id + " content"), StructuredJSON: `{}`,
		Tags: model.StringList(tags), SourceRefs: model.StringList{"source:" + id}, Confidence: confidence, Importance: 7, Status: status,
		Durability: model.MemoryDurabilityStable,
	}
	// AGENT-018 shape: a topic note is bound to a topic, a profile never is.
	switch memoryType {
	case model.MemoryScopeTopic:
		memory.TopicKey = "docker"
	}
	return memory
}

func TestGovernanceAppliesFiveActionsIdempotentlyAndSupportsUserControl(t *testing.T) {
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), TranslateError: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.MemoryExtractionJob{}, &model.MemoryCandidate{}, &model.LongTermMemory{}, &model.MemoryDreamRun{}, &model.MemoryRevision{}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	userID := uniqueMemoryTestUserID()
	repository := NewRepository(db)
	t.Cleanup(func() {
		db.Unscoped().Where("user_id = ?", userID).Delete(&model.MemoryRevision{})
		db.Unscoped().Where("user_id = ?", userID).Delete(&model.MemoryDreamRun{})
		db.Unscoped().Where("user_id = ?", userID).Delete(&model.LongTermMemory{})
		db.Unscoped().Where("user_id = ?", userID).Delete(&model.MemoryCandidate{})
		db.Unscoped().Where("user_id = ?", userID).Delete(&model.MemoryExtractionJob{})
	})

	newCandidate := func(content string) *model.MemoryCandidate {
		jobID := uuid.NewString()
		completed := now
		job := model.MemoryExtractionJob{
			ID: jobID, UserID: userID, EventType: model.MemoryEventAgentRun, SourceID: "run-" + uuid.NewString(),
			Status: model.MemoryJobCompleted, AvailableAt: now, CompletedAt: &completed,
		}
		if err := db.Create(&job).Error; err != nil {
			t.Fatal(err)
		}
		candidate := &model.MemoryCandidate{
			ID: uuid.NewString(), JobID: jobID, UserID: userID, Scope: model.MemoryScopeTopic, TopicKey: "docker", Durability: model.MemoryDurabilityStable,
			Subject: "deployment", ProposedContent: content, ContentHash: model.MemoryContentHash(content), StructuredJSON: `{}`,
			Tags: model.StringList{"docker", "gopherai", "deployment"}, SourceRefs: model.StringList{"agent_run:" + job.SourceID},
			Confidence: .9, Importance: 8, Sensitivity: model.MemorySensitivitySafe, Status: model.MemoryCandidatePending,
		}
		if err := db.Create(candidate).Error; err != nil {
			t.Fatal(err)
		}
		return candidate
	}
	resultMemory := func(candidate *model.MemoryCandidate) *model.LongTermMemory {
		origin := candidate.ID
		return &model.LongTermMemory{
			ID: uuid.NewString(), UserID: userID, OriginCandidateID: &origin, Scope: candidate.Scope,
			Subject: candidate.Subject, Content: candidate.ProposedContent, ContentHash: candidate.ContentHash, OriginContentHash: candidate.ContentHash, StructuredJSON: candidate.StructuredJSON,
			Tags: append(model.StringList(nil), candidate.Tags...), TopicKey: "deployment", Durability: candidate.Durability, ValidUntil: candidate.ValidUntil, SourceRefs: append(model.StringList(nil), candidate.SourceRefs...),
			Confidence: candidate.Confidence, Importance: candidate.Importance, Status: model.LongTermMemoryActive,
		}
	}
	apply := func(candidate *model.MemoryCandidate, action model.MemoryDreamAction, target *model.LongTermMemory, result *model.LongTermMemory) *DreamApplyResult {
		claim, err := repository.ClaimCandidate(ctx, userID, candidate.ID, now)
		if err != nil {
			t.Fatal(err)
		}
		params := ApplyDreamParams{UserID: userID, CandidateID: candidate.ID, DreamRunID: claim.Run.ID, Action: action, ResultMemory: result, Reason: "integration decision", Now: now}
		if target != nil {
			params.TargetMemoryID, params.ExpectedTargetHash = target.ID, target.ContentHash
		}
		applied, err := repository.ApplyDream(ctx, params)
		if err != nil {
			t.Fatalf("apply %s: %v", action, err)
		}
		return applied
	}

	concurrentCandidate := newCandidate("candidate claimed by one dream only")
	type claimResult struct {
		claim *DreamClaim
		err   error
	}
	claims := make(chan claimResult, 2)
	start := make(chan struct{})
	var wait sync.WaitGroup
	for i := 0; i < 2; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			claim, claimErr := repository.ClaimCandidate(ctx, userID, concurrentCandidate.ID, now)
			claims <- claimResult{claim: claim, err: claimErr}
		}()
	}
	close(start)
	wait.Wait()
	close(claims)
	var winner *DreamClaim
	successes, busy := 0, 0
	for result := range claims {
		if result.err == nil {
			winner = result.claim
			successes++
		} else if errors.Is(result.err, ErrDreamBusy) {
			busy++
		} else {
			t.Fatalf("unexpected concurrent claim error: %v", result.err)
		}
	}
	if successes != 1 || busy != 1 || winner == nil {
		t.Fatalf("concurrent claim successes=%d busy=%d winner=%+v", successes, busy, winner)
	}
	if err := repository.FailDream(ctx, userID, concurrentCandidate.ID, winner.Run.ID, "test_retry", now); err != nil {
		t.Fatal(err)
	}

	addCandidate := newCandidate("GopherAI runs in Docker")
	added := apply(addCandidate, model.MemoryDreamAdd, nil, resultMemory(addCandidate))
	if added.Memory == nil || added.Run.Action != model.MemoryDreamAdd {
		t.Fatalf("add result=%+v", added)
	}
	replay, err := repository.ClaimCandidate(ctx, userID, addCandidate.ID, now.Add(time.Second))
	if err != nil || !replay.Replay || replay.Run.ID != added.Run.ID {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	if _, err := repository.ClaimCandidate(ctx, userID+1, addCandidate.ID, now); !errors.Is(err, ErrCandidateNotFound) {
		t.Fatalf("cross-user claim err=%v", err)
	}

	mergeCandidate := newCandidate("GopherAI runs in Docker Compose")
	mergedMemory := *added.Memory
	mergedMemory.Content = mergeCandidate.ProposedContent
	mergedMemory.ContentHash = mergeCandidate.ContentHash
	mergedMemory.SourceRefs = append(mergedMemory.SourceRefs, mergeCandidate.SourceRefs...)
	merged := apply(mergeCandidate, model.MemoryDreamMerge, added.Memory, &mergedMemory)
	if merged.Memory == nil || merged.Memory.ID != added.Memory.ID || merged.Memory.Content != mergeCandidate.ProposedContent {
		t.Fatalf("merge result=%+v", merged)
	}

	supersedeCandidate := newCandidate("GopherAI production uses Kubernetes")
	replacement := resultMemory(supersedeCandidate)
	superseded := apply(supersedeCandidate, model.MemoryDreamSupersede, merged.Memory, replacement)
	var old model.LongTermMemory
	if err := db.Where("id = ?", merged.Memory.ID).First(&old).Error; err != nil || old.Status != model.LongTermMemorySuperseded {
		t.Fatalf("old=%+v err=%v", old, err)
	}
	recalledAfterSupersede, err := repository.Recall(ctx, RecallQuery{
		UserID: userID, Scopes: []model.MemoryScope{model.MemoryScopeTopic}, Tags: []string{"deployment"}, MinConfidence: 0, Limit: 10,
	})
	if err != nil || len(recalledAfterSupersede) != 1 || recalledAfterSupersede[0].ID != superseded.Memory.ID {
		t.Fatalf("superseded recall=%+v err=%v", recalledAfterSupersede, err)
	}

	archiveCandidate := newCandidate("deployment fact was retracted")
	archived := apply(archiveCandidate, model.MemoryDreamArchive, superseded.Memory, nil)
	if archived.Memory == nil || archived.Memory.Status != model.LongTermMemoryArchived {
		t.Fatalf("archive result=%+v", archived)
	}

	rejectCandidate := newCandidate("temporary deployment thought")
	rejected := apply(rejectCandidate, model.MemoryDreamReject, nil, nil)
	if rejected.Run.Action != model.MemoryDreamReject {
		t.Fatalf("reject result=%+v", rejected)
	}
	var rejectedRow model.MemoryCandidate
	if err := db.Where("id = ?", rejectCandidate.ID).First(&rejectedRow).Error; err != nil || rejectedRow.Status != model.MemoryCandidateRejected {
		t.Fatalf("rejected row=%+v err=%v", rejectedRow, err)
	}

	editable := recallFixture(uuid.NewString(), userID, model.MemoryScopeTopic, model.LongTermMemoryActive, []string{"docker", "gopherai", "project"}, .8)
	if err := db.Create(&editable).Error; err != nil {
		t.Fatal(err)
	}
	corrected, correction, err := repository.CorrectMemory(ctx, CorrectMemoryParams{
		UserID: userID, MemoryID: editable.ID, Subject: "confirmed deployment", Content: "GopherAI is deployed with Docker Compose",
		Tags: []string{"docker", "gopherai", "deployment"}, Reason: "user correction", Now: now.Add(time.Second),
	})
	if err != nil || !corrected.UserConfirmed || correction.Action != model.MemoryUserCorrect || corrected.ContentHash != model.MemoryContentHash(corrected.Content) {
		t.Fatalf("corrected=%+v revision=%+v err=%v", corrected, correction, err)
	}
	foreignList, err := repository.ListMemories(ctx, MemoryListQuery{UserID: userID + 1, Limit: 20})
	if err != nil || len(foreignList) != 0 {
		t.Fatalf("cross-user list=%+v err=%v", foreignList, err)
	}
	if _, _, err := repository.CorrectMemory(ctx, CorrectMemoryParams{
		UserID: userID + 1, MemoryID: editable.ID, Subject: "foreign", Content: "foreign", Tags: []string{"foreign"}, Reason: "cross-user", Now: now,
	}); !errors.Is(err, ErrMemoryNotFound) {
		t.Fatalf("cross-user correction err=%v", err)
	}
	if _, err := repository.DeleteMemory(ctx, userID+1, editable.ID, "cross-user", now); !errors.Is(err, ErrMemoryNotFound) {
		t.Fatalf("cross-user deletion err=%v", err)
	}
	// AGENT-018: rows written under a retired type are kept but frozen. They
	// cannot be corrected, because a correction would have to re-validate them
	// under the new shape rules, which they cannot satisfy.
	retired := recallFixture(uuid.NewString(), userID, model.MemoryScopeTopic, model.LongTermMemoryActive, []string{"go", "learning"}, .8)
	retired.Scope = model.MemoryScopeLegacyLearningState
	retired.TopicKey = ""
	if err := db.Create(&retired).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := repository.CorrectMemory(ctx, CorrectMemoryParams{
		UserID: userID, MemoryID: retired.ID, Subject: "fake mastery", Content: "mastered", Tags: []string{"go", "learning"}, Reason: "direct edit", Now: now,
	}); !errors.Is(err, ErrDerivedMemory) {
		t.Fatalf("retired type correction err=%v", err)
	}
	// It must also be invisible to recall, without having been deleted.
	stillStored, err := repository.Recall(ctx, RecallQuery{
		UserID: userID, Scopes: []model.MemoryScope{model.MemoryScopeGlobal, model.MemoryScopeTopic},
		Tags: []string{"go", "learning"}, MinConfidence: 0, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range stillStored {
		if item.ID == retired.ID {
			t.Fatal("a retired-type memory re-entered recall")
		}
	}
	var preserved model.LongTermMemory
	if err := db.Where("id = ?", retired.ID).First(&preserved).Error; err != nil || preserved.Content == "" {
		t.Fatalf("retired memory content must be preserved, not deleted: %+v err=%v", preserved, err)
	}
	duplicatePending := newCandidate(corrected.Content)
	deletion, err := repository.DeleteMemory(ctx, userID, editable.ID, "user deleted memory", now.Add(2*time.Second))
	if err != nil || deletion.Action != model.MemoryUserDelete || deletion.BeforeSnapshot != "" {
		t.Fatalf("deletion=%+v err=%v", deletion, err)
	}
	var tombstone model.LongTermMemory
	if err := db.Where("id = ?", editable.ID).First(&tombstone).Error; err != nil || tombstone.Status != model.LongTermMemoryDeleted || tombstone.Content != "" || tombstone.TombstoneHash == "" {
		t.Fatalf("tombstone=%+v err=%v", tombstone, err)
	}
	var suppressed model.MemoryCandidate
	if err := db.Where("id = ?", duplicatePending.ID).First(&suppressed).Error; err != nil || suppressed.Status != model.MemoryCandidateRejected || suppressed.RejectionReason != "deleted_tombstone" {
		t.Fatalf("suppressed=%+v err=%v", suppressed, err)
	}
	secondDelete, err := repository.DeleteMemory(ctx, userID, editable.ID, "repeat", now.Add(3*time.Second))
	if err != nil || secondDelete.ID != deletion.ID {
		t.Fatalf("idempotent deletion=%+v err=%v", secondDelete, err)
	}

	var dreamRevisions int64
	if err := db.Model(&model.MemoryRevision{}).Where("user_id = ? AND actor = ?", userID, model.MemoryRevisionDream).Count(&dreamRevisions).Error; err != nil || dreamRevisions != 5 {
		t.Fatalf("dream revisions=%d err=%v", dreamRevisions, err)
	}
}

func uniqueMemoryTestUserID() int64 {
	id := uuid.New()
	return int64(binary.BigEndian.Uint64(id[:8])&((1<<62)-1)) + 1
}

func TestEXP002AWorkerAttemptsExhaustToTerminalFailure(t *testing.T) {
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), TranslateError: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.MemoryExtractionJob{}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	userID := uniqueMemoryTestUserID()
	repository := NewRepository(db)
	t.Cleanup(func() { db.Unscoped().Where("user_id = ?", userID).Delete(&model.MemoryExtractionJob{}) })

	job := model.MemoryExtractionJob{ID: uuid.NewString(), UserID: userID, EventType: model.MemoryEventAgentRun, SourceID: "attempts", Status: model.MemoryJobPending, Attempts: 2, AvailableAt: now}
	if err := db.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	claimed, err := repository.Claim(ctx, job.ID, now, time.Minute, 3)
	if err != nil || claimed.Attempts != 3 || claimed.Status != model.MemoryJobProcessing {
		t.Fatalf("claim=%+v err=%v", claimed, err)
	}
	if err := repository.Retry(ctx, claimed, "injected_dependency", now.Add(time.Second), 3); err != nil {
		t.Fatal(err)
	}
	var exhausted model.MemoryExtractionJob
	if err := db.Where("id = ? AND user_id = ?", job.ID, userID).First(&exhausted).Error; err != nil {
		t.Fatal(err)
	}
	if exhausted.Status != model.MemoryJobFailed || exhausted.ErrorCode != "injected_dependency" || exhausted.CompletedAt == nil || exhausted.LeaseToken != "" {
		t.Fatalf("exhausted=%+v", exhausted)
	}

	leaseEnd := now.Add(-time.Second)
	interrupted := model.MemoryExtractionJob{ID: uuid.NewString(), UserID: userID, EventType: model.MemoryEventAgentRun, SourceID: "interrupted", Status: model.MemoryJobProcessing, Attempts: 3, AvailableAt: now, LeaseToken: "expired", LeaseEndsAt: &leaseEnd}
	if err := db.Create(&interrupted).Error; err != nil {
		t.Fatal(err)
	}
	affected, err := repository.RecoverExpired(ctx, now, 3)
	if err != nil || affected < 1 {
		t.Fatalf("affected=%d err=%v", affected, err)
	}
	if err := db.Where("id = ? AND user_id = ?", interrupted.ID, userID).First(&interrupted).Error; err != nil {
		t.Fatal(err)
	}
	if interrupted.Status != model.MemoryJobFailed || interrupted.ErrorCode != "worker_interrupted" || interrupted.LeaseToken != "" {
		t.Fatalf("interrupted=%+v", interrupted)
	}
}

func TestEXP002ADreamClaimCrashRecoversAfterLease(t *testing.T) {
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), TranslateError: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.MemoryExtractionJob{}, &model.MemoryCandidate{}, &model.MemoryDreamRun{}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	userID := uniqueMemoryTestUserID()
	t.Cleanup(func() {
		db.Unscoped().Where("user_id = ?", userID).Delete(&model.MemoryDreamRun{})
		db.Unscoped().Where("user_id = ?", userID).Delete(&model.MemoryCandidate{})
		db.Unscoped().Where("user_id = ?", userID).Delete(&model.MemoryExtractionJob{})
	})
	completed := now
	job := model.MemoryExtractionJob{ID: uuid.NewString(), UserID: userID, EventType: model.MemoryEventExplicitRemember, SourceID: "dream-crash", Status: model.MemoryJobCompleted, AvailableAt: now, CompletedAt: &completed}
	if err := db.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	content := "durable fact claimed before process crash"
	candidate := model.MemoryCandidate{ID: uuid.NewString(), JobID: job.ID, UserID: userID, Scope: model.MemoryScopeTopic, TopicKey: "docker", Durability: model.MemoryDurabilityStable, Subject: "crash boundary", ProposedContent: content, ContentHash: model.MemoryContentHash(content), StructuredJSON: `{}`, Tags: model.StringList{"project"}, SourceRefs: model.StringList{"explicit_remember:dream-crash"}, Confidence: 1, Importance: 8, Sensitivity: model.MemorySensitivitySafe, Status: model.MemoryCandidatePending}
	if err := db.Create(&candidate).Error; err != nil {
		t.Fatal(err)
	}
	firstRepository := NewRepository(db)
	claim, err := firstRepository.ClaimCandidate(ctx, userID, candidate.ID, now)
	if err != nil || claim.Run.Status != model.MemoryDreamRunProcessing {
		t.Fatalf("claim=%+v err=%v", claim, err)
	}

	// A new repository represents a restarted process. Before the lease expires
	// the claim remains busy; after expiry recovery fails the old run and makes
	// the candidate claimable again.
	restartedRepository := NewRepository(db)
	if _, err := restartedRepository.ClaimCandidate(ctx, userID, candidate.ID, now.Add(time.Minute)); !errors.Is(err, ErrDreamBusy) {
		t.Fatalf("second claim err=%v", err)
	}
	recovered, err := restartedRepository.RecoverExpiredDreams(ctx, userID, now.Add(time.Hour), 2*time.Minute)
	if err != nil || recovered != 1 {
		t.Fatalf("recovered=%d err=%v", recovered, err)
	}
	second, err := restartedRepository.ClaimCandidate(ctx, userID, candidate.ID, now.Add(time.Hour))
	if err != nil || second.Run.ID == claim.Run.ID || second.Run.Status != model.MemoryDreamRunProcessing {
		t.Fatalf("reclaimed=%+v err=%v", second, err)
	}
	var persistedCandidate model.MemoryCandidate
	var persistedRun model.MemoryDreamRun
	if err := db.Where("id = ? AND user_id = ?", candidate.ID, userID).First(&persistedCandidate).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Where("id = ? AND user_id = ?", claim.Run.ID, userID).First(&persistedRun).Error; err != nil {
		t.Fatal(err)
	}
	if persistedCandidate.Status != model.MemoryCandidateProcessing || persistedCandidate.DreamRunID != second.Run.ID || persistedRun.Status != model.MemoryDreamRunFailed || persistedRun.ErrorCode != "dream_lease_expired" {
		t.Fatalf("candidate=%s run=%s", persistedCandidate.Status, persistedRun.Status)
	}
}
