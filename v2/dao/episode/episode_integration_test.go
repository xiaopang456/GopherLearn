package episode

import (
	"GopherAI/internal/testdb"
	"GopherAI/model"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	mysqldriver "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func openEpisodeTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), TranslateError: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.Session{}, &model.Message{}, &model.EpisodeMemory{}, &model.EpisodeProjectionJob{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func createEpisodeOwner(t *testing.T, db *gorm.DB, prefix string) (model.User, model.Session) {
	t.Helper()
	suffix := uuid.NewString()
	user := model.User{Username: fmt.Sprintf("%s-%s", prefix, suffix[:12]), Password: "secret"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	session := model.Session{
		ID: uuid.NewString(), UserID: user.ID, UserName: user.Username, Mode: model.SessionModeAgent, Title: "episode test",
	}
	if err := db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Unscoped().Where("user_id = ?", user.ID).Delete(&model.EpisodeProjectionJob{})
		db.Unscoped().Where("user_id = ?", user.ID).Delete(&model.EpisodeMemory{})
		db.Unscoped().Where("session_id = ?", session.ID).Delete(&model.Message{})
		db.Unscoped().Delete(&session)
		db.Unscoped().Delete(&user)
	})
	return user, session
}

func TestListProjectionGapsOnlyReturnsCompletedAgentConversation(t *testing.T) {
	db := openEpisodeTestDB(t)
	owner, session := createEpisodeOwner(t, db, "episode-gap")
	repository := NewRepository(db)
	ctx := context.Background()

	messageUserName := "gap-" + uuid.NewString()[:12]
	userMessage := model.Message{SessionID: session.ID, UserName: messageUserName, Content: "问题", IsUser: true}
	if err := db.Create(&userMessage).Error; err != nil {
		t.Fatal(err)
	}
	if gaps, err := repository.ListProjectionGaps(ctx, 100); err != nil {
		t.Fatal(err)
	} else {
		for _, gap := range gaps {
			if gap.UserID == owner.ID && gap.SessionID == session.ID {
				t.Fatalf("user-only session appeared as gap: %v", gaps)
			}
		}
	}
	assistantMessage := model.Message{SessionID: session.ID, UserName: messageUserName, Content: "回答", IsUser: false}
	if err := db.Create(&assistantMessage).Error; err != nil {
		t.Fatal(err)
	}
	gaps, err := repository.ListProjectionGaps(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, gap := range gaps {
		if gap.UserID == owner.ID && gap.SessionID == session.ID && gap.LatestMessageID == assistantMessage.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing projection gap in %v", gaps)
	}

	episode := episodeFixture(owner.ID, session.ID, assistantMessage.ID, 0)
	if err := repository.Save(ctx, episode, 0); err != nil {
		t.Fatal(err)
	}
	gaps, err = repository.ListProjectionGaps(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, gap := range gaps {
		if gap.UserID == owner.ID && gap.SessionID == session.ID {
			t.Fatalf("up-to-date session remained a gap: %v", gaps)
		}
	}
}

func episodeFixture(userID int64, sessionID string, messageID uint, version uint64) *model.EpisodeMemory {
	return &model.EpisodeMemory{
		ID: uuid.NewString(), UserID: userID, SessionID: sessionID, Title: "RPC framing",
		Document: model.EpisodeDocument{
			Synopsis: "讨论 RPC 拆包边界",
			Topics:   []model.EpisodeTopic{{Key: "rpc-framing", Label: "RPC 粘包与拆包"}},
			Decisions: []model.EpisodeEvidence{{
				Text: "先用长度字段完成拆包", EvidenceMessageIDs: []uint{messageID},
			}},
			SourceHandles: []string{fmt.Sprintf("session:%s", sessionID), fmt.Sprintf("message:%d", messageID)},
		},
		SearchText: "RPC framing 长度字段 拆包", TopicKeys: model.StringList{"rpc-framing"},
		SourceLastMessageID: messageID, LastMessageAt: time.Now().UTC().Truncate(time.Second), Version: version,
		Status: model.EpisodeStatusActive, IndexStatus: model.EpisodeIndexPending,
	}
}

func TestEpisodeRepositoryOwnershipOptimisticUpdateAndSessionLifecycle(t *testing.T) {
	db := openEpisodeTestDB(t)
	owner, session := createEpisodeOwner(t, db, "episode-owner")
	other, _ := createEpisodeOwner(t, db, "episode-other")
	repository := NewRepository(db)
	ctx := context.Background()

	foreign := episodeFixture(other.ID, session.ID, 10, 1)
	if err := repository.Save(ctx, foreign, 0); !errors.Is(err, ErrEpisodeNotFound) {
		t.Fatalf("cross-user episode create error = %v, want ErrEpisodeNotFound", err)
	}

	created := episodeFixture(owner.ID, session.ID, 10, 0)
	if err := repository.Save(ctx, created, 0); err != nil {
		t.Fatal(err)
	}
	if created.Version != 1 {
		t.Fatalf("created version = %d, want 1", created.Version)
	}
	reloaded, err := repository.GetBySession(ctx, owner.ID, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ID != created.ID || reloaded.Document.Decisions[0].EvidenceMessageIDs[0] != 10 {
		t.Fatalf("unexpected reloaded episode: %#v", reloaded)
	}
	if _, err := repository.GetByID(ctx, other.ID, created.ID); !errors.Is(err, ErrEpisodeNotFound) {
		t.Fatalf("cross-user episode read error = %v", err)
	}
	duplicate := episodeFixture(owner.ID, session.ID, 11, 0)
	if err := repository.Save(ctx, duplicate, 0); !errors.Is(err, ErrEpisodeConflict) {
		t.Fatalf("duplicate episode create error = %v", err)
	}

	updated := episodeFixture(owner.ID, session.ID, 20, 0)
	updated.ID = created.ID
	updated.Document.Decisions[0].Text = "长度字段方案已确认"
	updated.SearchText = "RPC framing 长度字段 方案确认"
	if err := repository.Save(ctx, updated, 1); err != nil {
		t.Fatal(err)
	}
	if updated.Version != 2 {
		t.Fatalf("updated version = %d, want 2", updated.Version)
	}
	older := episodeFixture(owner.ID, session.ID, 15, 0)
	older.ID = created.ID
	if err := repository.Save(ctx, older, 2); !errors.Is(err, ErrEpisodeConflict) {
		t.Fatalf("non-monotonic update error = %v", err)
	}
	stale := episodeFixture(owner.ID, session.ID, 30, 0)
	stale.ID = created.ID
	if err := repository.Save(ctx, stale, 1); !errors.Is(err, ErrEpisodeConflict) {
		t.Fatalf("stale version update error = %v", err)
	}
	manualSession := model.Session{ID: uuid.NewString(), UserID: owner.ID, UserName: owner.Username, Mode: model.SessionModeAgent, Title: "manual cleanup"}
	if err := db.Create(&manualSession).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Unscoped().Delete(&manualSession) })
	manualEpisode := episodeFixture(owner.ID, manualSession.ID, 5, 0)
	if err := repository.Save(ctx, manualEpisode, 0); err != nil {
		t.Fatal(err)
	}
	manualJob, err := repository.UpsertDesired(ctx, owner.ID, manualSession.ID, 5, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.DeleteBySession(ctx, owner.ID, manualSession.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.GetByID(ctx, owner.ID, manualEpisode.ID); !errors.Is(err, ErrEpisodeNotFound) {
		t.Fatalf("explicit cleanup left episode: %v", err)
	}
	if _, err := repository.GetJob(ctx, owner.ID, manualJob.ID); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("explicit cleanup left job: %v", err)
	}
	var manualSessionCount int64
	if err := db.Model(&model.Session{}).Where("id = ? AND user_id = ?", manualSession.ID, owner.ID).Count(&manualSessionCount).Error; err != nil || manualSessionCount != 1 {
		t.Fatalf("derived cleanup changed source session: count=%d err=%v", manualSessionCount, err)
	}

	job, err := repository.UpsertDesired(ctx, owner.ID, session.ID, 20, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.UpsertDesired(ctx, other.ID, session.ID, 21, time.Now().UTC()); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("cross-user job upsert error = %v", err)
	}
	if err := repository.DeleteBySession(ctx, other.ID, session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.GetJob(ctx, owner.ID, job.ID); err != nil {
		t.Fatalf("foreign delete removed owner job: %v", err)
	}
	if err := db.Delete(&session).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := repository.GetBySession(ctx, owner.ID, session.ID); !errors.Is(err, ErrEpisodeNotFound) {
		t.Fatalf("soft-deleted session exposed episode: %v", err)
	}
	if _, err := repository.GetJob(ctx, owner.ID, job.ID); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("soft-deleted session exposed job: %v", err)
	}
	if due, err := repository.ListDue(ctx, 200, 3, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	} else {
		for _, listed := range due {
			if listed.ID == job.ID {
				t.Fatalf("soft-deleted session job remained due: %#v", listed)
			}
		}
	}
	if err := db.Unscoped().Delete(&session).Error; err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]any{"episode": &model.EpisodeMemory{}, "job": &model.EpisodeProjectionJob{}} {
		var count int64
		if err := db.Model(target).Where("user_id = ? AND session_id = ?", owner.ID, session.ID).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("hard-deleted session left %s rows = %d", name, count)
		}
	}
}

func TestEpisodeRecallReloadEnforcesOwnerSessionStatusAndTime(t *testing.T) {
	db := openEpisodeTestDB(t)
	owner, current := createEpisodeOwner(t, db, "eprecall")
	repository := NewRepository(db)
	ctx := context.Background()
	oldSession := model.Session{ID: uuid.NewString(), UserID: owner.ID, UserName: owner.Username, Mode: model.SessionModeAgent, Title: "old"}
	if err := db.Create(&oldSession).Error; err != nil {
		t.Fatal(err)
	}
	active := episodeFixture(owner.ID, oldSession.ID, 11, 0)
	active.LastMessageAt = time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	if err := repository.Save(ctx, active, 0); err != nil {
		t.Fatal(err)
	}
	if err := repository.UpdateIndexStatus(ctx, owner.ID, active.ID, active.Version, model.EpisodeIndexIndexed); err != nil {
		t.Fatal(err)
	}
	currentEpisode := episodeFixture(owner.ID, current.ID, 12, 0)
	if err := repository.Save(ctx, currentEpisode, 0); err != nil {
		t.Fatal(err)
	}
	if err := repository.UpdateIndexStatus(ctx, owner.ID, currentEpisode.ID, currentEpisode.Version, model.EpisodeIndexIndexed); err != nil {
		t.Fatal(err)
	}
	failedSession := oldSession
	failedSession.ID = uuid.NewString()
	if err := db.Create(&failedSession).Error; err != nil {
		t.Fatal(err)
	}
	failed := episodeFixture(owner.ID, failedSession.ID, 13, 0)
	failed.LastMessageAt = time.Now().Add(-3 * time.Hour).UTC().Truncate(time.Second)
	if err := repository.Save(ctx, failed, 0); err != nil {
		t.Fatal(err)
	}
	if err := repository.UpdateIndexStatus(ctx, owner.ID, failed.ID, failed.Version, model.EpisodeIndexFailed); err != nil {
		t.Fatal(err)
	}
	rows, err := repository.LoadCandidates(ctx, owner.ID, current.ID, []string{currentEpisode.ID, failed.ID, active.ID})
	if err != nil || len(rows) != 1 || rows[0].ID != active.ID {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	timed, err := repository.ListByTime(ctx, owner.ID, current.ID, time.Now().Add(-2*time.Hour), time.Now(), 5)
	if err != nil || len(timed) < 1 || timed[0].ID != active.ID {
		t.Fatalf("timed=%+v err=%v", timed, err)
	}
}

func TestEpisodeRecallRejectsDeletedOrChangedProvenanceTransitively(t *testing.T) {
	db := openEpisodeTestDB(t)
	owner, current := createEpisodeOwner(t, db, "ep-provenance")
	repository := NewRepository(db)
	ctx := context.Background()
	makeSession := func(title string) model.Session {
		session := model.Session{ID: uuid.NewString(), UserID: owner.ID, UserName: owner.Username, Mode: model.SessionModeAgent, Title: title}
		if err := db.Create(&session).Error; err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Unscoped().Delete(&session) })
		return session
	}
	sourceSession, derivedSession, transitiveSession, independentSession := makeSession("source"), makeSession("derived"), makeSession("transitive"), makeSession("independent")

	source := episodeFixture(owner.ID, sourceSession.ID, 1, 0)
	source.LastMessageAt = time.Now().Add(-3 * time.Hour)
	if err := repository.Save(ctx, source, 0); err != nil {
		t.Fatal(err)
	}
	derived := episodeFixture(owner.ID, derivedSession.ID, 2, 0)
	derived.LastMessageAt = time.Now().Add(-2 * time.Hour)
	derived.SourceEpisodes = model.EpisodeSourceRefs{{EpisodeID: source.ID, Version: source.Version}}
	if err := repository.Save(ctx, derived, 0); err != nil {
		t.Fatal(err)
	}
	transitive := episodeFixture(owner.ID, transitiveSession.ID, 3, 0)
	transitive.LastMessageAt = time.Now().Add(-time.Hour)
	transitive.SourceEpisodes = model.EpisodeSourceRefs{
		{EpisodeID: source.ID, Version: source.Version},
		{EpisodeID: derived.ID, Version: derived.Version},
	}
	if err := repository.Save(ctx, transitive, 0); err != nil {
		t.Fatal(err)
	}
	independent := episodeFixture(owner.ID, independentSession.ID, 4, 0)
	independent.LastMessageAt = time.Now().Add(-4 * time.Hour)
	if err := repository.Save(ctx, independent, 0); err != nil {
		t.Fatal(err)
	}
	for _, episode := range []*model.EpisodeMemory{source, derived, transitive, independent} {
		if err := repository.UpdateIndexStatus(ctx, owner.ID, episode.ID, episode.Version, model.EpisodeIndexIndexed); err != nil {
			t.Fatal(err)
		}
	}

	ids := []string{derived.ID, transitive.ID}
	rows, err := repository.LoadCandidates(ctx, owner.ID, current.ID, ids)
	if err != nil || len(rows) != 2 {
		t.Fatalf("valid provenance rows=%+v err=%v", rows, err)
	}
	otherOwner, otherSession := createEpisodeOwner(t, db, "ep-provenance-other")
	foreignSource := episodeFixture(otherOwner.ID, otherSession.ID, 5, 0)
	if err := repository.Save(ctx, foreignSource, 0); err != nil {
		t.Fatal(err)
	}
	if err := repository.UpdateIndexStatus(ctx, otherOwner.ID, foreignSource.ID, foreignSource.Version, model.EpisodeIndexIndexed); err != nil {
		t.Fatal(err)
	}
	foreignDerivedSession := makeSession("foreign-derived")
	foreignDerived := episodeFixture(owner.ID, foreignDerivedSession.ID, 6, 0)
	foreignDerived.SourceEpisodes = model.EpisodeSourceRefs{{EpisodeID: foreignSource.ID, Version: foreignSource.Version}}
	if err := repository.Save(ctx, foreignDerived, 0); err != nil {
		t.Fatal(err)
	}
	if err := repository.UpdateIndexStatus(ctx, owner.ID, foreignDerived.ID, foreignDerived.Version, model.EpisodeIndexIndexed); err != nil {
		t.Fatal(err)
	}
	rows, err = repository.LoadCandidates(ctx, owner.ID, current.ID, []string{foreignDerived.ID})
	if err != nil || len(rows) != 0 {
		t.Fatalf("cross-user provenance must fail closed: rows=%+v err=%v", rows, err)
	}
	if err := db.Model(&model.EpisodeMemory{}).Where("id = ? AND user_id = ?", source.ID, owner.ID).Update("version", source.Version+1).Error; err != nil {
		t.Fatal(err)
	}
	rows, err = repository.LoadCandidates(ctx, owner.ID, current.ID, ids)
	if err != nil || len(rows) != 0 {
		t.Fatalf("changed ancestor must invalidate closure: rows=%+v err=%v", rows, err)
	}
	if err := db.Model(&model.EpisodeMemory{}).Where("id = ? AND user_id = ?", source.ID, owner.ID).Update("version", source.Version).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.EpisodeMemory{}).Where("id = ? AND user_id = ?", source.ID, owner.ID).Update("index_status", model.EpisodeIndexFailed).Error; err != nil {
		t.Fatal(err)
	}
	rows, err = repository.LoadCandidates(ctx, owner.ID, current.ID, ids)
	if err != nil || len(rows) != 0 {
		t.Fatalf("failed ancestor index must invalidate closure: rows=%+v err=%v", rows, err)
	}
	if err := db.Model(&model.EpisodeMemory{}).Where("id = ? AND user_id = ?", source.ID, owner.ID).Update("index_status", model.EpisodeIndexIndexed).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.EpisodeMemory{}).Where("id = ? AND user_id = ?", source.ID, owner.ID).Update("status", model.EpisodeStatusArchived).Error; err != nil {
		t.Fatal(err)
	}
	rows, err = repository.LoadCandidates(ctx, owner.ID, current.ID, ids)
	if err != nil || len(rows) != 0 {
		t.Fatalf("archived ancestor must invalidate closure: rows=%+v err=%v", rows, err)
	}
	if err := db.Model(&model.EpisodeMemory{}).Where("id = ? AND user_id = ?", source.ID, owner.ID).Update("status", model.EpisodeStatusActive).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Delete(&sourceSession).Error; err != nil {
		t.Fatal(err)
	}
	rows, err = repository.LoadCandidates(ctx, owner.ID, current.ID, ids)
	if err != nil || len(rows) != 0 {
		t.Fatalf("deleted ancestor session must invalidate closure: rows=%+v err=%v", rows, err)
	}
	timed, err := repository.ListByTime(ctx, owner.ID, current.ID, time.Time{}, time.Now(), 1)
	if err != nil || len(timed) != 1 || timed[0].ID != independent.ID {
		t.Fatalf("last-session must skip invalid newer lineage: rows=%+v err=%v", timed, err)
	}
}

func TestProjectionJobConcurrentClaimRequeueRetryAndRecovery(t *testing.T) {
	db := openEpisodeTestDB(t)
	owner, session := createEpisodeOwner(t, db, "episode-job")
	repository := NewRepository(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	job, err := repository.UpsertDesired(ctx, owner.ID, session.ID, 10, now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := repository.UpsertDesired(ctx, owner.ID, session.ID, 10, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !unchanged.AvailableAt.Equal(job.AvailableAt) {
		t.Fatalf("idempotent upsert moved available_at from %v to %v", job.AvailableAt, unchanged.AvailableAt)
	}
	lower, err := repository.UpsertDesired(ctx, owner.ID, session.ID, 9, now.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if lower.DesiredMessageID != 10 || !lower.AvailableAt.Equal(job.AvailableAt) {
		t.Fatalf("lower desired target changed job: %#v", lower)
	}

	type claimResult struct {
		job *model.EpisodeProjectionJob
		err error
	}
	start := make(chan struct{})
	results := make(chan claimResult, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			claimed, claimErr := NewRepository(db).Claim(ctx, owner.ID, job.ID, now, time.Minute, 3)
			results <- claimResult{job: claimed, err: claimErr}
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	var winner *model.EpisodeProjectionJob
	conflicts := 0
	for result := range results {
		switch {
		case result.err == nil:
			winner = result.job
		case errors.Is(result.err, ErrJobConflict):
			conflicts++
		default:
			t.Fatalf("unexpected concurrent claim error: %v", result.err)
		}
	}
	if winner == nil || conflicts != 1 || winner.ClaimedMessageID != 10 || winner.Attempts != 1 {
		t.Fatalf("claim winner=%#v conflicts=%d", winner, conflicts)
	}

	quietUntil := now.Add(10 * time.Minute)
	processing, err := repository.UpsertDesired(ctx, owner.ID, session.ID, 20, quietUntil)
	if err != nil {
		t.Fatal(err)
	}
	if processing.Status != model.EpisodeProjectionProcessing || processing.ClaimedMessageID != 10 || processing.DesiredMessageID != 20 {
		t.Fatalf("new desired target stole live claim: %#v", processing)
	}
	requeued, err := repository.CompleteOrRequeue(ctx, winner, 10, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if requeued.Status != model.EpisodeProjectionPending || requeued.Attempts != 0 || requeued.DesiredMessageID != 20 || !requeued.AvailableAt.Equal(quietUntil) {
		t.Fatalf("completed old target did not requeue new target: %#v", requeued)
	}
	if _, err := repository.Claim(ctx, owner.ID, job.ID, now.Add(time.Minute), time.Minute, 3); !errors.Is(err, ErrJobConflict) {
		t.Fatalf("claim before quiet window error = %v", err)
	}

	second, err := repository.Claim(ctx, owner.ID, job.ID, quietUntil, time.Minute, 3)
	if err != nil {
		t.Fatal(err)
	}
	if second.ClaimedMessageID != 20 || second.Attempts != 1 {
		t.Fatalf("second claim = %#v", second)
	}
	if _, err := repository.CompleteOrRequeue(ctx, winner, 10, quietUntil); !errors.Is(err, ErrJobConflict) {
		t.Fatalf("old worker completion error = %v", err)
	}
	retried, err := repository.Retry(ctx, second, "injected_dependency", quietUntil.Add(time.Minute), 2)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Status != model.EpisodeProjectionPending || retried.Attempts != 1 {
		t.Fatalf("retry state = %#v", retried)
	}
	third, err := repository.Claim(ctx, owner.ID, job.ID, quietUntil.Add(time.Minute), time.Minute, 2)
	if err != nil {
		t.Fatal(err)
	}
	if third.Attempts != 2 {
		t.Fatalf("third attempts = %d, want 2", third.Attempts)
	}
	if count, err := repository.recoverExpired(ctx, quietUntil.Add(3*time.Minute), 2, owner.ID); err != nil || count != 1 {
		t.Fatalf("recover exhausted count=%d err=%v", count, err)
	}
	failed, err := repository.GetJob(ctx, owner.ID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != model.EpisodeProjectionFailed || failed.CompletedAt == nil || failed.LeaseToken != "" {
		t.Fatalf("exhausted job = %#v", failed)
	}

	revived, err := repository.UpsertDesired(ctx, owner.ID, session.ID, 30, quietUntil.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if revived.Status != model.EpisodeProjectionPending || revived.Attempts != 0 || revived.DesiredMessageID != 30 {
		t.Fatalf("new target did not revive failed job: %#v", revived)
	}
	fourth, err := repository.Claim(ctx, owner.ID, job.ID, revived.AvailableAt, time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	latestAt := revived.AvailableAt.Add(10 * time.Minute)
	if _, err := repository.UpsertDesired(ctx, owner.ID, session.ID, 40, latestAt); err != nil {
		t.Fatal(err)
	}
	if count, err := repository.recoverExpired(ctx, fourth.LeaseEndsAt.Add(time.Second), 1, owner.ID); err != nil || count != 1 {
		t.Fatalf("recover with newer target count=%d err=%v", count, err)
	}
	newer, err := repository.GetJob(ctx, owner.ID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if newer.Status != model.EpisodeProjectionPending || newer.Attempts != 0 || newer.DesiredMessageID != 40 || !newer.AvailableAt.Equal(latestAt) {
		t.Fatalf("new target inherited expired attempt: %#v", newer)
	}
	if _, err := repository.CompleteOrRequeue(ctx, fourth, 30, latestAt); !errors.Is(err, ErrJobConflict) {
		t.Fatalf("expired old worker completion error = %v", err)
	}
	finalClaim, err := repository.Claim(ctx, owner.ID, job.ID, latestAt, time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	completed, err := repository.CompleteOrRequeue(ctx, finalClaim, 40, latestAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != model.EpisodeProjectionCompleted || completed.CompletedAt == nil || completed.DesiredMessageID != 40 || completed.ClaimedMessageID != 0 {
		t.Fatalf("final completed job = %#v", completed)
	}
}

func TestProjectionJobConcurrentSessionReleasePreservesLatestDesiredTarget(t *testing.T) {
	db := openEpisodeTestDB(t)
	owner, firstSession := createEpisodeOwner(t, db, "episode-release-order")
	sessions := []model.Session{firstSession}
	for index := 1; index < 8; index++ {
		session := model.Session{
			ID: uuid.NewString(), UserID: owner.ID, UserName: owner.Username,
			Mode: model.SessionModeAgent, Title: fmt.Sprintf("release-%d", index),
		}
		if err := db.Create(&session).Error; err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, session)
	}
	t.Cleanup(func() {
		for _, session := range sessions[1:] {
			db.Unscoped().Where("session_id = ?", session.ID).Delete(&model.EpisodeProjectionJob{})
			db.Unscoped().Where("id = ?", session.ID).Delete(&model.Session{})
		}
	})

	repository := NewRepository(db)
	ctx := context.Background()
	base := time.Now().UTC()
	for _, session := range sessions {
		if _, err := repository.UpsertDesired(ctx, owner.ID, session.ID, 1, base.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}

	const rounds = 30
	start := make(chan struct{})
	errs := make(chan error, len(sessions))
	var wait sync.WaitGroup
	for _, session := range sessions {
		session := session
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			local := NewRepository(db)
			for round := 1; round <= rounds; round++ {
				messageID := uint(round + 1)
				now := base.Add(time.Duration(round) * time.Millisecond)
				if _, err := local.UpsertDesired(ctx, owner.ID, session.ID, messageID, now.Add(time.Hour)); err != nil {
					errs <- fmt.Errorf("upsert session %s round %d: %w", session.ID, round, err)
					return
				}
				if _, err := local.ReleaseOtherSessions(ctx, owner.ID, session.ID, now); err != nil {
					errs <- fmt.Errorf("release session %s round %d: %w", session.ID, round, err)
					return
				}
			}
		}()
	}
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		return
	}

	// A final deterministic pass makes every session somebody else's target;
	// concurrent releases above may legitimately finish with their own last
	// upsert still inside the quiet window.
	releaseCutoff := base.Add(time.Minute).Truncate(time.Millisecond)
	for _, session := range sessions {
		if _, err := repository.ReleaseOtherSessions(ctx, owner.ID, session.ID, releaseCutoff); err != nil {
			t.Fatal(err)
		}
	}
	var jobs []model.EpisodeProjectionJob
	if err := db.Where("user_id = ?", owner.ID).Find(&jobs).Error; err != nil {
		t.Fatal(err)
	}
	if len(jobs) != len(sessions) {
		t.Fatalf("jobs=%d want=%d", len(jobs), len(sessions))
	}
	for _, job := range jobs {
		if job.DesiredMessageID != rounds+1 || job.Status != model.EpisodeProjectionPending {
			t.Fatalf("job lost latest target: %+v", job)
		}
		if job.AvailableAt.After(releaseCutoff) {
			t.Fatalf("job was not released from quiet window: %+v", job)
		}
	}
}

// BUG-AGENT-104. RecoverExpired used to be three multi-row UPDATEs whose WHERE
// clause led with status, so InnoDB entered through idx_episode_jobs_status and
// then took the primary key — the opposite order to CompleteOrRequeue's point
// update. A load run caught the two deadlocking, and the rolled-back side was
// the completion, which left its job in processing holding a lease.
//
// This drives both against real MySQL at the same time and asserts InnoDB's own
// deadlock counter does not move. It needs a real server: the lock order is a
// property of the engine, and nothing in a fake can show it.
func TestRecoverExpiredDoesNotDeadlockWithCompletion(t *testing.T) {
	db := openEpisodeTestDB(t)
	repository := NewRepository(db)
	ctx := context.Background()

	const (
		rounds   = 6
		perRound = 40
	)
	before := innodbDeadlocks(t, db)

	for round := 1; round <= rounds; round++ {
		owner, session := createEpisodeOwner(t, db, fmt.Sprintf("episode-lockorder-%d", round))

		// Every job is claimed with a lease that is already in the past, so the
		// recovery pass and the completions are contending for the same rows.
		claims := make([]*model.EpisodeProjectionJob, 0, perRound)
		for index := 0; index < perRound; index++ {
			jobSession := session
			if index > 0 {
				jobSession = createEpisodeSession(t, db, owner)
			}
			if _, err := repository.UpsertDesired(ctx, owner.ID, jobSession.ID, uint(10+index), time.Now().Add(-time.Minute)); err != nil {
				t.Fatal(err)
			}
			job, err := getJobBySession(db, owner.ID, jobSession.ID)
			if err != nil {
				t.Fatal(err)
			}
			// A one millisecond lease: claimed legitimately, and already expired
			// by the time the recovery pass looks at it. That is the state a
			// worker leaves behind when its completion is rolled back.
			claimed, err := repository.Claim(ctx, owner.ID, job.ID, time.Now(), time.Millisecond, 3)
			if err != nil {
				t.Fatalf("claim: %v", err)
			}
			claims = append(claims, claimed)
		}

		var group sync.WaitGroup
		start := make(chan struct{})
		errs := make(chan error, perRound+8)

		for _, claimed := range claims {
			group.Add(1)
			go func(claimed *model.EpisodeProjectionJob) {
				defer group.Done()
				<-start
				if _, err := repository.CompleteOrRequeue(ctx, claimed, claimed.ClaimedMessageID, time.Now()); err != nil &&
					!errors.Is(err, ErrJobConflict) && !errors.Is(err, ErrJobNotFound) {
					errs <- fmt.Errorf("complete: %w", err)
				}
			}(claimed)
		}
		// Several recovery passes, unscoped, so the scan overlaps the completions
		// rather than happening to run before or after them. Unscoped is what
		// production runs, and it is the wider scan that makes the index range
		// long enough for the old lock order to matter.
		for pass := 0; pass < 6; pass++ {
			group.Add(1)
			go func() {
				defer group.Done()
				<-start
				if _, err := repository.RecoverExpired(ctx, time.Now(), 3); err != nil {
					errs <- fmt.Errorf("recover: %w", err)
				}
			}()
		}
		close(start)
		group.Wait()
		close(errs)
		for err := range errs {
			t.Errorf("round %d: %v", round, err)
		}

		// Nothing may be left mid-flight: a job still processing after both
		// sides finished is the stranding this fix exists to prevent.
		var stuck int64
		if err := db.Model(&model.EpisodeProjectionJob{}).
			Where("user_id = ? AND status = ?", owner.ID, model.EpisodeProjectionProcessing).
			Count(&stuck).Error; err != nil {
			t.Fatal(err)
		}
		if stuck != 0 {
			t.Errorf("round %d left %d job(s) in processing", round, stuck)
		}
	}

	if delta := innodbDeadlocks(t, db) - before; delta != 0 {
		t.Errorf("InnoDB reported %d deadlock(s) across %d rounds; the lock order is still inverted",
			delta, rounds)
	}
}

// An expired processing job must be reclaimed by a pass that is not a restart.
func TestRecoverExpiredReclaimsAStrandedJob(t *testing.T) {
	db := openEpisodeTestDB(t)
	owner, session := createEpisodeOwner(t, db, "episode-stranded")
	repository := NewRepository(db)
	ctx := context.Background()

	if _, err := repository.UpsertDesired(ctx, owner.ID, session.ID, 10, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	job, err := getJobBySession(db, owner.ID, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A claim whose lease has already elapsed: what a rolled-back completion
	// leaves behind, with nobody holding the token.
	if _, err := repository.Claim(ctx, owner.ID, job.ID, time.Now(), time.Millisecond, 3); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)

	recovered, err := repository.recoverExpired(ctx, time.Now(), 3, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered != 1 {
		t.Fatalf("recovered = %d, want 1", recovered)
	}
	reclaimed, err := getJobBySession(db, owner.ID, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed.Status != model.EpisodeProjectionPending {
		t.Errorf("status = %q, want pending", reclaimed.Status)
	}
	if reclaimed.LeaseToken != "" || reclaimed.LeaseEndsAt != nil {
		t.Errorf("a reclaimed job still holds a lease: token=%q ends=%v",
			reclaimed.LeaseToken, reclaimed.LeaseEndsAt)
	}
	if reclaimed.ErrorCode != "worker_interrupted" {
		t.Errorf("error code = %q, want worker_interrupted", reclaimed.ErrorCode)
	}
	// A job whose lease is still valid belongs to a live worker and must be
	// left alone, or recovery would steal work from the worker doing it.
	if _, err := repository.Claim(ctx, owner.ID, job.ID, time.Now(), time.Hour, 3); err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if recovered, err := repository.recoverExpired(ctx, time.Now(), 3, owner.ID); err != nil || recovered != 0 {
		t.Errorf("recovered = %d err = %v; a live lease was reclaimed", recovered, err)
	}
}

func innodbDeadlocks(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var count int64
	if err := db.Raw("SELECT COALESCE(SUM(count),0) FROM information_schema.innodb_metrics WHERE name = 'lock_deadlocks'").
		Row().Scan(&count); err != nil {
		t.Fatalf("read InnoDB deadlock counter: %v", err)
	}
	return count
}

func createEpisodeSession(t *testing.T, db *gorm.DB, owner model.User) model.Session {
	t.Helper()
	session := model.Session{
		ID: uuid.NewString(), UserID: owner.ID, UserName: owner.Username,
		Mode: model.SessionModeAgent, Title: "episode lock order",
	}
	if err := db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Unscoped().Where("session_id = ?", session.ID).Delete(&model.EpisodeProjectionJob{})
		db.Unscoped().Where("id = ?", session.ID).Delete(&model.Session{})
	})
	return session
}
