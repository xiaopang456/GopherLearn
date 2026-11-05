package session

import (
	"GopherAI/internal/testdb"
	"GopherAI/model"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	mysqldriver "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// layerFixture is one user, one session and a repository over a real database.
type layerFixture struct {
	db        *gorm.DB
	repo      *Repository
	user      model.User
	other     model.User
	session   model.Session
	otherSess model.Session
}

func newLayerFixture(t *testing.T) *layerFixture {
	t.Helper()
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), TranslateError: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.Session{}, &model.Message{},
		&model.SessionSummary{}, &model.SessionWhiteboard{},
		&model.SessionSummarySegment{}, &model.SessionSummarySegmentJob{},
		&model.EpisodeMemory{}, &model.EpisodeProjectionJob{},
		&model.MemoryExtractionJob{}, &model.MemoryCandidate{}, &model.MemoryDraft{},
		&model.MemoryDreamRun{}, &model.AgentRun{}, &model.AgentStep{}); err != nil {
		t.Fatal(err)
	}
	suffix := uuid.NewString()
	fixture := &layerFixture{db: db, repo: NewRepository(db)}
	fixture.user = model.User{Username: fmt.Sprintf("layer-%s", suffix), Password: "secret"}
	fixture.other = model.User{Username: fmt.Sprintf("layer-other-%s", suffix), Password: "secret"}
	if err := db.Create(&fixture.user).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&fixture.other).Error; err != nil {
		t.Fatal(err)
	}
	fixture.session = model.Session{ID: uuid.NewString(), UserID: fixture.user.ID, UserName: fixture.user.Username, Mode: model.SessionModeAgent, Title: "layer"}
	fixture.otherSess = model.Session{ID: uuid.NewString(), UserID: fixture.other.ID, UserName: fixture.other.Username, Mode: model.SessionModeAgent, Title: "other"}
	if err := db.Create(&fixture.session).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&fixture.otherSess).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, table := range []any{&model.SessionWhiteboard{}, &model.SessionSummarySegment{}, &model.SessionSummarySegmentJob{}} {
			db.Unscoped().Where("session_id IN ?", []string{fixture.session.ID, fixture.otherSess.ID}).Delete(table)
		}
		db.Unscoped().Where("id IN ?", []string{fixture.session.ID, fixture.otherSess.ID}).Delete(&model.Session{})
		db.Unscoped().Delete(&fixture.other)
		db.Unscoped().Delete(&fixture.user)
	})
	return fixture
}

func TestWhiteboardOptimisticUpdateAndOwnership(t *testing.T) {
	fixture := newLayerFixture(t)
	ctx := context.Background()

	// Another user's board cannot be created against this session.
	foreign := &model.SessionWhiteboard{UserID: fixture.other.ID, SessionID: fixture.session.ID, Content: "{}", CoveredMessageID: 1}
	if err := fixture.repo.SaveWhiteboard(ctx, foreign, 0); !errors.Is(err, ErrWhiteboardNotFound) {
		t.Fatalf("cross-user create error=%v", err)
	}

	first := &model.SessionWhiteboard{UserID: fixture.user.ID, SessionID: fixture.session.ID, Content: `{"schema_version":1}`, CoveredMessageID: 10, TokenEstimate: 4}
	if err := fixture.repo.SaveWhiteboard(ctx, first, 0); err != nil {
		t.Fatal(err)
	}
	if first.Version != 1 {
		t.Fatalf("initial version=%d", first.Version)
	}

	// A second create for the same session loses: the board is one row.
	stale := &model.SessionWhiteboard{UserID: fixture.user.ID, SessionID: fixture.session.ID, Content: "{}", CoveredMessageID: 11, TokenEstimate: 1}
	if err := fixture.repo.SaveWhiteboard(ctx, stale, 0); !errors.Is(err, ErrWhiteboardConflict) {
		t.Fatalf("duplicate create error=%v", err)
	}

	// A write built on the current version succeeds and moves the version on.
	next := &model.SessionWhiteboard{UserID: fixture.user.ID, SessionID: fixture.session.ID, Content: `{"schema_version":1,"goal":{"text":"g"}}`, CoveredMessageID: 20, TokenEstimate: 6}
	if err := fixture.repo.SaveWhiteboard(ctx, next, 1); err != nil {
		t.Fatal(err)
	}
	if next.Version != 2 {
		t.Fatalf("version after update=%d", next.Version)
	}

	// A write built on the old version loses.
	behind := &model.SessionWhiteboard{UserID: fixture.user.ID, SessionID: fixture.session.ID, Content: "{}", CoveredMessageID: 30, TokenEstimate: 1}
	if err := fixture.repo.SaveWhiteboard(ctx, behind, 1); !errors.Is(err, ErrWhiteboardConflict) {
		t.Fatalf("stale version error=%v", err)
	}

	// A write that does not move the coverage forward loses as well: a board
	// that goes backwards would un-say something already said.
	backwards := &model.SessionWhiteboard{UserID: fixture.user.ID, SessionID: fixture.session.ID, Content: "{}", CoveredMessageID: 15, TokenEstimate: 1}
	if err := fixture.repo.SaveWhiteboard(ctx, backwards, 2); !errors.Is(err, ErrWhiteboardConflict) {
		t.Fatalf("backwards coverage error=%v", err)
	}

	// The read path is owner-scoped.
	if _, err := fixture.repo.GetWhiteboard(ctx, fixture.other.ID, fixture.session.ID); !errors.Is(err, ErrWhiteboardNotFound) {
		t.Fatalf("cross-user read error=%v", err)
	}
	stored, err := fixture.repo.GetWhiteboard(ctx, fixture.user.ID, fixture.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.CoveredMessageID != 20 || stored.Version != 2 {
		t.Fatalf("stored board=%+v", stored)
	}
}

func TestSummarySegmentSequenceIsAssignedAndUnique(t *testing.T) {
	fixture := newLayerFixture(t)
	ctx := context.Background()
	now := time.Now()

	seq, err := fixture.repo.NextSummarySegmentSeq(ctx, fixture.user.ID, fixture.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if seq != 1 {
		t.Fatalf("first seq=%d", seq)
	}

	segment := &model.SessionSummarySegment{
		UserID: fixture.user.ID, SessionID: fixture.session.ID, Seq: seq,
		StartMessageID: 1, EndMessageID: 10, Topic: "first",
		StartAt: now, EndAt: now,
		Content: `{"schema_version":1,"topic":"first"}`,
	}
	if err := fixture.repo.SaveSummarySegment(ctx, segment); err != nil {
		t.Fatal(err)
	}

	// The same sequence number cannot be taken twice.
	duplicate := &model.SessionSummarySegment{
		UserID: fixture.user.ID, SessionID: fixture.session.ID, Seq: seq,
		StartMessageID: 1, EndMessageID: 10, Topic: "again",
		StartAt: now, EndAt: now,
		Content: `{"schema_version":1,"topic":"again"}`,
	}
	if err := fixture.repo.SaveSummarySegment(ctx, duplicate); !errors.Is(err, ErrSummarySegmentConflict) {
		t.Fatalf("duplicate segment error=%v", err)
	}

	next, err := fixture.repo.NextSummarySegmentSeq(ctx, fixture.user.ID, fixture.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if next != 2 {
		t.Fatalf("second seq=%d", next)
	}

	// A segment of another user's session cannot be written here.
	foreign := &model.SessionSummarySegment{
		UserID: fixture.other.ID, SessionID: fixture.session.ID, Seq: 3,
		StartMessageID: 1, EndMessageID: 10, StartAt: now, EndAt: now,
		Content: `{"schema_version":1}`,
	}
	if err := fixture.repo.SaveSummarySegment(ctx, foreign); !errors.Is(err, ErrSummarySegmentNotFound) {
		t.Fatalf("cross-user segment error=%v", err)
	}

	segments, err := fixture.repo.ListSummarySegments(ctx, fixture.user.ID, fixture.session.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 1 || segments[0].Seq != 1 {
		t.Fatalf("segments=%+v", segments)
	}
}

func TestSummarySegmentJobIsExclusivePerSession(t *testing.T) {
	fixture := newLayerFixture(t)
	ctx := context.Background()
	now := time.Now()

	job := &model.SessionSummarySegmentJob{
		UserID: fixture.user.ID, SessionID: fixture.session.ID,
		DesiredMessageID: 20, StartMessageID: 1,
		Status: model.SessionSummarySegmentJobPending, AvailableAt: now,
	}
	if err := fixture.repo.UpsertSummarySegmentJob(ctx, job); err != nil {
		t.Fatal(err)
	}

	// A second request for the same session updates the row rather than adding
	// another one: one session has one segment job.
	second := &model.SessionSummarySegmentJob{
		UserID: fixture.user.ID, SessionID: fixture.session.ID,
		DesiredMessageID: 30, StartMessageID: 1,
		Status: model.SessionSummarySegmentJobPending, AvailableAt: now,
	}
	if err := fixture.repo.UpsertSummarySegmentJob(ctx, second); err != nil {
		t.Fatal(err)
	}

	due, err := fixture.repo.ListDueSummarySegmentJobs(ctx, 10, 3, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, candidate := range due {
		if candidate.SessionID == fixture.session.ID {
			count++
			if candidate.DesiredMessageID != 30 {
				t.Fatalf("desired message id=%d", candidate.DesiredMessageID)
			}
		}
	}
	if count != 1 {
		t.Fatalf("the session had %d due jobs", count)
	}

	// The job is claimable once, by one worker.
	stored, err := fixture.repo.GetSummarySegmentJob(ctx, fixture.user.ID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The claim happens a moment after the job became available. MySQL stores
	// a timestamp with millisecond precision, so claiming at the exact instant
	// the job was queued would sit on the rounding boundary.
	claimAt := now.Add(time.Second)
	claimed, err := fixture.repo.ClaimSummarySegmentJob(ctx, fixture.user.ID, stored.ID, claimAt, time.Minute, 3)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Status != model.SessionSummarySegmentJobProcessing || claimed.Attempts != 1 {
		t.Fatalf("claimed=%+v", claimed)
	}
	if _, err := fixture.repo.ClaimSummarySegmentJob(ctx, fixture.user.ID, stored.ID, claimAt, time.Minute, 3); !errors.Is(err, ErrSummarySegmentJobConflict) {
		t.Fatalf("second claim error=%v", err)
	}

	// Only the lease holder may finish it.
	notHolder := &model.SessionSummarySegmentJob{ID: stored.ID, UserID: fixture.user.ID, LeaseToken: "someone-else"}
	if err := fixture.repo.CompleteSummarySegmentJob(ctx, notHolder, 20, claimAt); !errors.Is(err, ErrSummarySegmentJobConflict) {
		t.Fatalf("completion by a non-holder error=%v", err)
	}
	if err := fixture.repo.CompleteSummarySegmentJob(ctx, claimed, 30, claimAt); err != nil {
		t.Fatal(err)
	}
}

func TestSummarySegmentJobRetryKeepsTheSameStretch(t *testing.T) {
	fixture := newLayerFixture(t)
	ctx := context.Background()
	now := time.Now()

	job := &model.SessionSummarySegmentJob{
		UserID: fixture.user.ID, SessionID: fixture.session.ID,
		DesiredMessageID: 20, StartMessageID: 5,
		Status: model.SessionSummarySegmentJobPending, AvailableAt: now,
	}
	if err := fixture.repo.UpsertSummarySegmentJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	claimed, err := fixture.repo.ClaimSummarySegmentJob(ctx, fixture.user.ID, job.ID, now, time.Minute, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.repo.RetrySummarySegmentJob(ctx, claimed, "model_refused", now.Add(15*time.Second), 3); err != nil {
		t.Fatal(err)
	}

	// The retry covers the same stretch: a failed segment does not accumulate
	// the way a repeatedly rewritten summary did.
	requeued, err := fixture.repo.GetSummarySegmentJob(ctx, fixture.user.ID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if requeued.StartMessageID != 5 || requeued.DesiredMessageID != 20 {
		t.Fatalf("retried job covers %d..%d", requeued.StartMessageID, requeued.DesiredMessageID)
	}
	if requeued.Status != model.SessionSummarySegmentJobPending {
		t.Fatalf("retried job status=%q", requeued.Status)
	}
	if requeued.ErrorCode != "model_refused" {
		t.Fatalf("retried job error code=%q", requeued.ErrorCode)
	}
}

func TestRecoverExpiredSummarySegmentJobsRequeuesTheInterrupted(t *testing.T) {
	fixture := newLayerFixture(t)
	ctx := context.Background()
	now := time.Now()

	job := &model.SessionSummarySegmentJob{
		UserID: fixture.user.ID, SessionID: fixture.session.ID,
		DesiredMessageID: 20, StartMessageID: 1,
		Status: model.SessionSummarySegmentJobPending, AvailableAt: now,
	}
	if err := fixture.repo.UpsertSummarySegmentJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	claimed, err := fixture.repo.ClaimSummarySegmentJob(ctx, fixture.user.ID, job.ID, now, time.Minute, 3)
	if err != nil {
		t.Fatal(err)
	}

	// The worker dies: the lease expires and nothing finishes the job.
	affected, err := fixture.repo.RecoverExpiredSummarySegmentJobs(ctx, now.Add(2*time.Minute), 3)
	if err != nil {
		t.Fatal(err)
	}
	if affected != 1 {
		t.Fatalf("recovered=%d", affected)
	}
	recovered, err := fixture.repo.GetSummarySegmentJob(ctx, fixture.user.ID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != model.SessionSummarySegmentJobPending {
		t.Fatalf("recovered job status=%q", recovered.Status)
	}
	if recovered.LeaseToken != "" {
		t.Fatalf("recovered job still holds a lease")
	}
	// The interrupted attempt did not consume the retry budget twice.
	if recovered.Attempts != 1 {
		t.Fatalf("recovered job attempts=%d", recovered.Attempts)
	}
	_ = claimed
}

func TestDeletingASessionRemovesItsBoardSegmentsAndJobs(t *testing.T) {
	fixture := newLayerFixture(t)
	ctx := context.Background()
	now := time.Now()

	board := &model.SessionWhiteboard{UserID: fixture.user.ID, SessionID: fixture.session.ID, Content: `{"schema_version":1}`, CoveredMessageID: 10}
	if err := fixture.repo.SaveWhiteboard(ctx, board, 0); err != nil {
		t.Fatal(err)
	}
	segment := &model.SessionSummarySegment{
		UserID: fixture.user.ID, SessionID: fixture.session.ID, Seq: 1,
		StartMessageID: 1, EndMessageID: 10, StartAt: now, EndAt: now,
		Content: `{"schema_version":1}`,
	}
	if err := fixture.repo.SaveSummarySegment(ctx, segment); err != nil {
		t.Fatal(err)
	}
	job := &model.SessionSummarySegmentJob{
		UserID: fixture.user.ID, SessionID: fixture.session.ID,
		DesiredMessageID: 20, StartMessageID: 1,
		Status: model.SessionSummarySegmentJobPending, AvailableAt: now,
	}
	if err := fixture.repo.UpsertSummarySegmentJob(ctx, job); err != nil {
		t.Fatal(err)
	}

	if _, err := fixture.repo.DeleteOwned(ctx, fixture.user.ID, fixture.session.ID); err != nil {
		t.Fatal(err)
	}

	for _, table := range []any{&model.SessionWhiteboard{}, &model.SessionSummarySegment{}, &model.SessionSummarySegmentJob{}} {
		var count int64
		if err := fixture.db.Unscoped().Model(table).Where("session_id = ?", fixture.session.ID).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%T kept %d rows after the session was deleted", table, count)
		}
	}
}

// The batch of a job is frozen when the batch starts. A target that moves
// forward while the attempt is retried must not widen the batch: the retry
// has to face the same turns, or it is not the same attempt again.
func TestTheFrozenBatchSurvivesATargetThatMovesForward(t *testing.T) {
	fixture := newLayerFixture(t)
	ctx := context.Background()
	now := time.Now()

	job := &model.SessionSummarySegmentJob{
		UserID: fixture.user.ID, SessionID: fixture.session.ID,
		DesiredMessageID: 20, StartMessageID: 5,
		Status: model.SessionSummarySegmentJobPending, AvailableAt: now,
	}
	if err := fixture.repo.UpsertSummarySegmentJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	claimed, err := fixture.repo.ClaimSummarySegmentJob(ctx, fixture.user.ID, job.ID, now, time.Minute, 3)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.BatchMessageID != 20 {
		t.Fatalf("the frozen batch ends at %d, want 20", claimed.BatchMessageID)
	}
	if err := fixture.repo.RetrySummarySegmentJob(ctx, claimed, "model_refused", now.Add(time.Second), 3); err != nil {
		t.Fatal(err)
	}

	// The target moves forward while the attempt is being retried.
	later := &model.SessionSummarySegmentJob{
		UserID: fixture.user.ID, SessionID: fixture.session.ID,
		DesiredMessageID: 40, StartMessageID: 5,
		Status: model.SessionSummarySegmentJobPending, AvailableAt: now,
	}
	if err := fixture.repo.UpsertSummarySegmentJob(ctx, later); err != nil {
		t.Fatal(err)
	}
	retried, err := fixture.repo.ClaimSummarySegmentJob(ctx, fixture.user.ID, job.ID, now.Add(2*time.Second), time.Minute, 3)
	if err != nil {
		t.Fatal(err)
	}
	if retried.BatchMessageID != 20 {
		t.Fatalf("the frozen batch ends at %d, want 20", retried.BatchMessageID)
	}
	if retried.DesiredMessageID != 40 {
		t.Fatalf("the target is %d, want 40", retried.DesiredMessageID)
	}
}

// A segment that covers fewer turns than the batch holds leaves the rest of
// the turns owed. The job stays open for them and starts after what was
// covered, so the turns are not summarized twice.
func TestTheJobContinuesAfterASegmentThatCoveredOnlyPartOfTheBatch(t *testing.T) {
	fixture := newLayerFixture(t)
	ctx := context.Background()
	now := time.Now()

	job := &model.SessionSummarySegmentJob{
		UserID: fixture.user.ID, SessionID: fixture.session.ID,
		DesiredMessageID: 100, StartMessageID: 1,
		Status: model.SessionSummarySegmentJobPending, AvailableAt: now,
	}
	if err := fixture.repo.UpsertSummarySegmentJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	claimed, err := fixture.repo.ClaimSummarySegmentJob(ctx, fixture.user.ID, job.ID, now, time.Minute, 3)
	if err != nil {
		t.Fatal(err)
	}
	// The segment covered 40 of the 100 turns the batch holds.
	if err := fixture.repo.CompleteSummarySegmentJob(ctx, claimed, 40, now); err != nil {
		t.Fatal(err)
	}
	continued, err := fixture.repo.GetSummarySegmentJob(ctx, fixture.user.ID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if continued.Status != model.SessionSummarySegmentJobPending {
		t.Fatalf("the job status is %q, want pending", continued.Status)
	}
	if continued.StartMessageID != 41 {
		t.Fatalf("the job starts at %d, want 41", continued.StartMessageID)
	}
	if continued.BatchMessageID != 100 {
		t.Fatalf("the batch ends at %d, want 100", continued.BatchMessageID)
	}
}

// A session that caught up gets new turns. The job that was completed has to
// come back to life for them, with its attempts over and its start where the
// last segment stopped.
func TestACompletedJobComesBackToLifeForNewTurns(t *testing.T) {
	fixture := newLayerFixture(t)
	ctx := context.Background()
	now := time.Now()

	job := &model.SessionSummarySegmentJob{
		UserID: fixture.user.ID, SessionID: fixture.session.ID,
		DesiredMessageID: 20, StartMessageID: 1,
		Status: model.SessionSummarySegmentJobPending, AvailableAt: now,
	}
	if err := fixture.repo.UpsertSummarySegmentJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	claimed, err := fixture.repo.ClaimSummarySegmentJob(ctx, fixture.user.ID, job.ID, now, time.Minute, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.repo.CompleteSummarySegmentJob(ctx, claimed, 20, now); err != nil {
		t.Fatal(err)
	}
	finished, err := fixture.repo.GetSummarySegmentJob(ctx, fixture.user.ID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.Status != model.SessionSummarySegmentJobCompleted {
		t.Fatalf("the job status is %q, want completed", finished.Status)
	}
	if finished.StartMessageID != 21 {
		t.Fatalf("the job starts at %d, want the turn after the coverage 21", finished.StartMessageID)
	}

	// New turns arrive. The job comes back to life for them.
	later := &model.SessionSummarySegmentJob{
		UserID: fixture.user.ID, SessionID: fixture.session.ID,
		DesiredMessageID: 40, StartMessageID: 1,
		Status: model.SessionSummarySegmentJobPending, AvailableAt: now,
	}
	if err := fixture.repo.UpsertSummarySegmentJob(ctx, later); err != nil {
		t.Fatal(err)
	}
	revived, err := fixture.repo.GetSummarySegmentJob(ctx, fixture.user.ID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if revived.Status != model.SessionSummarySegmentJobPending {
		t.Fatalf("the revived job status is %q, want pending", revived.Status)
	}
	if revived.DesiredMessageID != 40 {
		t.Fatalf("the revived job targets %d, want 40", revived.DesiredMessageID)
	}
	if revived.StartMessageID != 21 {
		t.Fatalf("the revived job starts at %d, want 21", revived.StartMessageID)
	}
	if revived.Attempts != 0 {
		t.Fatalf("the revived job has %d attempts, want 0", revived.Attempts)
	}
}

// One session has one generation at a time. A takeover refuses while another
// holder has the lease, and it takes the session when nobody holds it.
func TestTheTakeoverWaitsForTheHolderToLetTheSessionGo(t *testing.T) {
	fixture := newLayerFixture(t)
	ctx := context.Background()
	now := time.Now()

	job := &model.SessionSummarySegmentJob{
		UserID: fixture.user.ID, SessionID: fixture.session.ID,
		DesiredMessageID: 20, StartMessageID: 1,
		Status: model.SessionSummarySegmentJobPending, AvailableAt: now,
	}
	if err := fixture.repo.UpsertSummarySegmentJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	claimed, err := fixture.repo.ClaimSummarySegmentJob(ctx, fixture.user.ID, job.ID, now, time.Minute, 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.repo.TakeOverSummarySegmentJob(ctx, fixture.user.ID, fixture.session.ID, 30, 1, now, time.Minute); !errors.Is(err, ErrSummarySegmentJobConflict) {
		t.Fatalf("takeover during a live lease error=%v", err)
	}

	// The holder is gone: the takeover takes the session and moves the start
	// to the first turn no segment covers.
	taken, err := fixture.repo.TakeOverSummarySegmentJob(ctx, fixture.user.ID, fixture.session.ID, 30, 11, now.Add(2*time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if taken.Status != model.SessionSummarySegmentJobProcessing {
		t.Fatalf("the taken job status is %q", taken.Status)
	}
	if taken.StartMessageID != 11 || taken.DesiredMessageID != 30 {
		t.Fatalf("the taken job covers %d..%d", taken.StartMessageID, taken.DesiredMessageID)
	}
	if taken.LeaseToken == "" || taken.LeaseToken == claimed.LeaseToken {
		t.Fatalf("the taken job holds the lease %q", taken.LeaseToken)
	}
}
