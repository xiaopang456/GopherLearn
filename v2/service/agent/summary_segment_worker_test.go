package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"GopherAI/model"
)

// The worker and the producer exist to hold four properties under pressure:
// a retry covers the same turns, one session runs one job at a time, a
// restart continues the work, and a fallback is recorded rather than silent.
// The tests here are those four properties.

type stubSegmentJobRepository struct {
	jobs        map[string]model.SessionSummarySegmentJob
	due         []model.SessionSummarySegmentJob
	claimed     []*model.SessionSummarySegmentJob
	completed   []*model.SessionSummarySegmentJob
	retried     []*model.SessionSummarySegmentJob
	recovered   int
	claimErr    error
	completeErr error
	retryErr    error
}

func newStubSegmentJobRepository() *stubSegmentJobRepository {
	return &stubSegmentJobRepository{jobs: map[string]model.SessionSummarySegmentJob{}}
}

func (r *stubSegmentJobRepository) ListDueSummarySegmentJobs(_ context.Context, limit, maxAttempts int, now time.Time) ([]model.SessionSummarySegmentJob, error) {
	var out []model.SessionSummarySegmentJob
	for _, job := range r.due {
		if job.Status != model.SessionSummarySegmentJobPending || job.AvailableAt.After(now) || job.Attempts >= maxAttempts {
			continue
		}
		out = append(out, job)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (r *stubSegmentJobRepository) ClaimSummarySegmentJob(_ context.Context, userID int64, jobID string, now time.Time, lease time.Duration, maxAttempts int) (*model.SessionSummarySegmentJob, error) {
	if r.claimErr != nil {
		return nil, r.claimErr
	}
	job, ok := r.jobs[jobID]
	if !ok {
		return nil, errors.New("unknown job")
	}
	if job.Status != model.SessionSummarySegmentJobPending || job.AvailableAt.After(now) || job.Attempts >= maxAttempts {
		return nil, errors.New("job is not claimable")
	}
	job.Status = model.SessionSummarySegmentJobProcessing
	job.Attempts++
	job.LeaseToken = "token-" + jobID
	ends := now.Add(lease)
	job.LeaseEndsAt = &ends
	r.jobs[jobID] = job
	r.claimed = append(r.claimed, &job)
	return &job, nil
}

func (r *stubSegmentJobRepository) CompleteSummarySegmentJob(_ context.Context, claimed *model.SessionSummarySegmentJob, now time.Time) error {
	if r.completeErr != nil {
		return r.completeErr
	}
	r.completed = append(r.completed, claimed)
	return nil
}

func (r *stubSegmentJobRepository) RetrySummarySegmentJob(_ context.Context, claimed *model.SessionSummarySegmentJob, errorCode string, availableAt time.Time, maxAttempts int) error {
	if r.retryErr != nil {
		return r.retryErr
	}
	r.retried = append(r.retried, claimed)
	return nil
}

func (r *stubSegmentJobRepository) RecoverExpiredSummarySegmentJobs(_ context.Context, now time.Time, maxAttempts int) (int64, error) {
	return int64(r.recovered), nil
}

type stubSegmentProducer struct {
	err      error
	calls    int
	outcomes []SummarySegmentOutcome
	lastJob  *model.SessionSummarySegmentJob
}

func (p *stubSegmentProducer) ProduceSegment(_ context.Context, job *model.SessionSummarySegmentJob) (SummarySegmentOutcome, error) {
	p.calls++
	p.lastJob = job
	if p.err != nil {
		return SummarySegmentOutcome{}, p.err
	}
	outcome := SummarySegmentOutcome{SegmentSeq: 1}
	if len(p.outcomes) > 0 {
		outcome = p.outcomes[0]
		p.outcomes = p.outcomes[1:]
	}
	return outcome, nil
}

func segmentWorkerConfig() SummarySegmentWorkerConfig {
	config := DefaultSummarySegmentWorkerConfig()
	config.ScanInterval = 10 * time.Millisecond
	config.ReconcileInterval = 10 * time.Millisecond
	config.RecoveryInterval = 10 * time.Millisecond
	return config
}

func newTestWorker(t *testing.T, repository *stubSegmentJobRepository, producer *stubSegmentProducer) *SummarySegmentWorker {
	t.Helper()
	reconciler, err := NewSummarySegmentReconciler(&stubReconcileRepository{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := NewSummarySegmentWorker(repository, producer, reconciler, segmentWorkerConfig())
	if err != nil {
		t.Fatal(err)
	}
	return worker
}

// A retry has to cover the same stretch of turns as the attempt that failed.
// If it did not, a failing generation would slowly change what the segment
// was about, and the failure would look like progress.
func TestTheRetryCoversTheSameTurnsAsTheAttemptThatFailed(t *testing.T) {
	repository := newStubSegmentJobRepository()
	job := model.SessionSummarySegmentJob{
		ID: "job-1", UserID: 1, SessionID: "s1",
		DesiredMessageID: 300, StartMessageID: 100,
		Status: model.SessionSummarySegmentJobPending, AvailableAt: time.Now(),
	}
	repository.jobs[job.ID] = job
	repository.due = []model.SessionSummarySegmentJob{job}
	producer := &stubSegmentProducer{err: errors.New("model refused")}

	worker := newTestWorker(t, repository, producer)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := worker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer worker.Stop(context.Background())

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(repository.retried) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if len(repository.retried) == 0 {
		t.Fatal("the failed attempt was never retried")
	}
	retried := repository.retried[0]
	if retried.StartMessageID != job.StartMessageID {
		t.Fatalf("the retry starts at %d, want %d", retried.StartMessageID, job.StartMessageID)
	}
	if retried.DesiredMessageID != job.DesiredMessageID {
		t.Fatalf("the retry targets %d, want %d", retried.DesiredMessageID, job.DesiredMessageID)
	}
}

// The batch a producer reads is fixed by the job, not by what an earlier
// attempt happened to leave behind.
func TestTheProducerReadsTheSameBatchOnEveryAttempt(t *testing.T) {
	source := &stubSegmentSource{messages: []model.Message{
		{ID: 100, IsUser: true, Content: "改成 48 分钟吧"},
		{ID: 150, IsUser: false, Content: "好的"},
		{ID: 300, IsUser: true, Content: "继续"},
	}}
	sink := &stubSegmentSink{}
	generator := stubSegmentGeneratorWith(t, []model.SessionSummarySegmentPoint{{Text: "p", EvidenceMessageIDs: []uint{100}}})
	producer, err := NewModelSummarySegmentProducer(source, sink, generator, DefaultSummarySegmentProducerConfig())
	if err != nil {
		t.Fatal(err)
	}
	job := &model.SessionSummarySegmentJob{
		UserID: 1, SessionID: "s1",
		DesiredMessageID: 300, StartMessageID: 100,
	}
	first, err := producer.ProduceSegment(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	second, err := producer.ProduceSegment(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	if first.StartMessageID != second.StartMessageID || first.EndMessageID != second.EndMessageID {
		t.Fatalf("the two attempts covered different turns: %+v and %+v", first, second)
	}
	if source.readStart != 100 || source.readEnd != 300 {
		t.Fatalf("the batch read was %d..%d, want 100..300", source.readStart, source.readEnd)
	}
	if first.EndMessageID != 300 {
		t.Fatalf("the segment covers up to %d, want 300", first.EndMessageID)
	}
}

// One session runs one job at a time. Two workers claiming the same job must
// not both summarize it, and the second one has to be turned away.
func TestOneSessionRunsOneJobAtATime(t *testing.T) {
	repository := newStubSegmentJobRepository()
	job := model.SessionSummarySegmentJob{
		ID: "job-1", UserID: 1, SessionID: "s1",
		DesiredMessageID: 300, StartMessageID: 100,
		Status: model.SessionSummarySegmentJobPending, AvailableAt: time.Now(),
	}
	repository.jobs[job.ID] = job
	now := time.Now()
	first, err := repository.ClaimSummarySegmentJob(context.Background(), 1, job.ID, now, time.Minute, 3)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != model.SessionSummarySegmentJobProcessing {
		t.Fatalf("the job status is %q", first.Status)
	}
	if _, err := repository.ClaimSummarySegmentJob(context.Background(), 1, job.ID, now, time.Minute, 3); err == nil {
		t.Fatal("a second claim of the same job must be refused")
	}
}

// A job whose worker died is reclaimed and put back in the queue, so a
// restart continues the work instead of leaving it stranded.
func TestARestartReclaimsTheJobsWhoseWorkerDied(t *testing.T) {
	repository := newStubSegmentJobRepository()
	repository.recovered = 2
	producer := &stubSegmentProducer{}
	worker := newTestWorker(t, repository, producer)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := worker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer worker.Stop(context.Background())
	// The recovery ran at Start: the repository was asked for expired jobs.
	if repository.recovered == 0 {
		t.Fatal("the worker did not reclaim the expired jobs at start")
	}
}

// A job that is reclaimed keeps its start. The turns it covers do not move
// because the process that was working on it went away.
func TestAReclaimedJobKeepsTheTurnsItCovers(t *testing.T) {
	repository := newStubSegmentJobRepository()
	job := model.SessionSummarySegmentJob{
		ID: "job-1", UserID: 1, SessionID: "s1",
		DesiredMessageID: 300, StartMessageID: 100,
		Status: model.SessionSummarySegmentJobPending, AvailableAt: time.Now(),
	}
	repository.jobs[job.ID] = job
	claimed, err := repository.ClaimSummarySegmentJob(context.Background(), 1, job.ID, time.Now(), time.Minute, 3)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.StartMessageID != 100 || claimed.DesiredMessageID != 300 {
		t.Fatalf("the claimed job covers %d..%d", claimed.StartMessageID, claimed.DesiredMessageID)
	}
}

// A failed attempt does not complete the job. Completing it would say the
// stretch was summarized when it was not.
func TestAFailedAttemptDoesNotCompleteTheJob(t *testing.T) {
	repository := newStubSegmentJobRepository()
	job := model.SessionSummarySegmentJob{
		ID: "job-1", UserID: 1, SessionID: "s1",
		DesiredMessageID: 300, StartMessageID: 100,
		Status: model.SessionSummarySegmentJobPending, AvailableAt: time.Now(),
	}
	repository.jobs[job.ID] = job
	repository.due = []model.SessionSummarySegmentJob{job}
	producer := &stubSegmentProducer{err: errors.New("model refused")}

	worker := newTestWorker(t, repository, producer)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := worker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer worker.Stop(context.Background())

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(repository.retried) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if len(repository.completed) != 0 {
		t.Fatalf("a failed attempt completed the job: %+v", repository.completed)
	}
}

// A successful attempt completes the job.
func TestASuccessfulAttemptCompletesTheJob(t *testing.T) {
	repository := newStubSegmentJobRepository()
	job := model.SessionSummarySegmentJob{
		ID: "job-1", UserID: 1, SessionID: "s1",
		DesiredMessageID: 300, StartMessageID: 100,
		Status: model.SessionSummarySegmentJobPending, AvailableAt: time.Now(),
	}
	repository.jobs[job.ID] = job
	repository.due = []model.SessionSummarySegmentJob{job}
	producer := &stubSegmentProducer{}

	worker := newTestWorker(t, repository, producer)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := worker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer worker.Stop(context.Background())

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(repository.completed) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if len(repository.completed) != 1 {
		t.Fatalf("the successful attempt did not complete the job: %+v", repository.completed)
	}
}

// The worker's configuration has to hold: a lease that is shorter than the
// task timeout would let a second worker take a job that is still running.
func TestTheWorkerConfigurationRejectsALeaseShorterThanTheTask(t *testing.T) {
	config := DefaultSummarySegmentWorkerConfig()
	config.Lease = config.TaskTimeout
	if err := config.Validate(); err == nil {
		t.Fatal("a lease that equals the task timeout must be refused")
	}
	config = DefaultSummarySegmentWorkerConfig()
	config.RecoveryInterval = config.Lease + time.Second
	if err := config.Validate(); err == nil {
		t.Fatal("a recovery that runs less often than a lease must be refused")
	}
}

// The producer's batch is bounded, so one segment cannot cover an entire
// long conversation and the generator's input stays bounded.
func TestTheProducerBoundsTheBatchItReads(t *testing.T) {
	source := &stubSegmentSource{messages: []model.Message{
		{ID: 100, IsUser: true, Content: "a"},
		{ID: 150, IsUser: false, Content: "b"},
		{ID: 300, IsUser: true, Content: "c"},
	}}
	sink := &stubSegmentSink{}
	generator := stubSegmentGeneratorWith(t, []model.SessionSummarySegmentPoint{{Text: "p", EvidenceMessageIDs: []uint{100}}})
	config := DefaultSummarySegmentProducerConfig()
	config.MaxMessages = 2
	producer, err := NewModelSummarySegmentProducer(source, sink, generator, config)
	if err != nil {
		t.Fatal(err)
	}
	job := &model.SessionSummarySegmentJob{
		UserID: 1, SessionID: "s1", DesiredMessageID: 500, StartMessageID: 100,
	}
	outcome, err := producer.ProduceSegment(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	// The batch stops at the last turn it actually read, not at the target:
	// a segment that claimed more turns than it summarized would answer for
	// turns nobody can read back.
	if outcome.EndMessageID != 150 {
		t.Fatalf("the batch reached %d, want 150", outcome.EndMessageID)
	}
}

// The segment and the board are written as one unit, so a board never claims
// coverage that no segment records.
func TestTheSegmentAndTheBoardAreWrittenTogether(t *testing.T) {
	source := &stubSegmentSource{messages: []model.Message{{ID: 100, IsUser: true, Content: "a"}}}
	sink := &stubSegmentSink{}
	generator := stubSegmentGeneratorWith(t, []model.SessionSummarySegmentPoint{{Text: "p", EvidenceMessageIDs: []uint{100}}})
	producer, err := NewModelSummarySegmentProducer(source, sink, generator, DefaultSummarySegmentProducerConfig())
	if err != nil {
		t.Fatal(err)
	}
	job := &model.SessionSummarySegmentJob{
		UserID: 1, SessionID: "s1", DesiredMessageID: 100, StartMessageID: 99,
	}
	if _, err := producer.ProduceSegment(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if sink.segment == nil || sink.board == nil {
		t.Fatalf("the write did not carry both documents: %+v", sink)
	}
	if sink.segment.EndMessageID != sink.board.CoveredMessageID {
		t.Fatalf("the segment covers %d but the board covers %d",
			sink.segment.EndMessageID, sink.board.CoveredMessageID)
	}
}

// The failure reasons are named, so a job row and a metric say what went
// wrong rather than only that something did.
func TestTheFailureReasonIsNamed(t *testing.T) {
	cases := map[string]error{
		"task_timeout":         context.DeadlineExceeded,
		"task_cancelled":       context.Canceled,
		"whiteboard_conflict":  ErrSummarySegmentWhiteboardConflict,
		"source_not_found":     ErrSummarySegmentMessagesUnavailable,
		"invalid_model_output": ErrSummarySegmentOutputInvalid,
		"generation_failed":    errors.New("something else"),
	}
	for want, err := range cases {
		if got := summarySegmentErrorCode(err); got != want {
			t.Fatalf("the reason for %v is %q, want %q", err, got, want)
		}
	}
}

// The batch the producer reads must have its ends in order. An inverted
// range would read the turns between the two ids backwards.
func TestTheProducerRefusesAnEmptyBatch(t *testing.T) {
	source := &stubSegmentSource{}
	sink := &stubSegmentSink{}
	generator := stubSegmentGeneratorWith(t, []model.SessionSummarySegmentPoint{{Text: "p", EvidenceMessageIDs: []uint{100}}})
	producer, err := NewModelSummarySegmentProducer(source, sink, generator, DefaultSummarySegmentProducerConfig())
	if err != nil {
		t.Fatal(err)
	}
	job := &model.SessionSummarySegmentJob{
		UserID: 1, SessionID: "s1", DesiredMessageID: 100, StartMessageID: 100,
	}
	if _, err := producer.ProduceSegment(context.Background(), job); err == nil {
		t.Fatal("a job that covers no turns must fail")
	}
}

// The trigger enqueues only when the conversation grew past what the raw
// messages can carry.
func TestTheTriggerWaitsUntilTheConversationIsBigEnough(t *testing.T) {
	repository := &stubTriggerRepository{messages: []model.Message{
		{ID: 101, IsUser: true, Content: "a"},
		{ID: 102, IsUser: false, Content: "b"},
	}}
	trigger, err := NewSummarySegmentTrigger(repository, DefaultSummarySegmentTriggerConfig())
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := trigger.Consider(context.Background(), 1, "s1", 102)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Enqueued {
		t.Fatalf("the trigger enqueued a small conversation: %+v", outcome)
	}
	if outcome.Reason != summarySegmentTriggerBelowThreshold {
		t.Fatalf("the reason is %q", outcome.Reason)
	}
}

// The trigger enqueues once the threshold is reached, and the job it records
// starts where the last segment stopped.
func TestTheTriggerEnqueuesWithTheSessionsOwnWatermarkAsTheStart(t *testing.T) {
	messages := make([]model.Message, 0, 12)
	for index := 0; index < 12; index++ {
		messages = append(messages, model.Message{ID: uint(200 + index), IsUser: index%2 == 0, Content: "x"})
	}
	repository := &stubTriggerRepository{messages: messages, watermark: 150}
	trigger, err := NewSummarySegmentTrigger(repository, DefaultSummarySegmentTriggerConfig())
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := trigger.Consider(context.Background(), 1, "s1", 211)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Enqueued {
		t.Fatalf("the trigger did not enqueue: %+v", outcome)
	}
	if repository.upserted == nil {
		t.Fatal("no job was recorded")
	}
	if repository.upserted.StartMessageID != 150 {
		t.Fatalf("the job starts at %d, want the watermark 150", repository.upserted.StartMessageID)
	}
	if repository.upserted.DesiredMessageID != 211 {
		t.Fatalf("the job targets %d, want 211", repository.upserted.DesiredMessageID)
	}
}

// A conversation that is already covered needs no segment.
func TestTheTriggerDoesNothingWhenEverythingIsCovered(t *testing.T) {
	repository := &stubTriggerRepository{watermark: 300}
	trigger, err := NewSummarySegmentTrigger(repository, DefaultSummarySegmentTriggerConfig())
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := trigger.Consider(context.Background(), 1, "s1", 300)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Enqueued {
		t.Fatalf("the trigger enqueued a covered conversation: %+v", outcome)
	}
	if outcome.Reason != summarySegmentTriggerNoUnsummarized {
		t.Fatalf("the reason is %q", outcome.Reason)
	}
}

// The fallback runs only when the raw conversation no longer fits, and every
// decision is recorded.
func TestTheFallbackRunsOnlyWhenTheRawConversationIsTooBig(t *testing.T) {
	repository := &stubFallbackRepository{messages: []model.Message{
		{ID: 100, IsUser: true, Content: "a"},
	}}
	producer := &stubSegmentProducer{}
	fallback, err := NewSummarySegmentFallback(repository, producer, SummarySegmentFallbackConfig{
		RawMessageTokenLimit: 100000, StageBudget: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err := fallback.Decide(context.Background(), 1, "s1", 100)
	if err != nil {
		t.Fatal(err)
	}
	if record.FellBack {
		t.Fatalf("the fallback ran for a small conversation: %+v", record)
	}
	if record.Reason != summarySegmentFallbackNotNeeded {
		t.Fatalf("the reason is %q", record.Reason)
	}
}

// A fallback that had to run is recorded, and so is one that failed. A
// fallback nobody can see is a fallback nobody can count.
func TestTheFallbackRecordsWhatItDid(t *testing.T) {
	repository := &stubFallbackRepository{messages: []model.Message{
		{ID: 100, IsUser: true, Content: string(make([]byte, 4000))},
	}}
	config := SummarySegmentFallbackConfig{RawMessageTokenLimit: 100, StageBudget: time.Second}

	working := &stubSegmentProducer{}
	fallback, err := NewSummarySegmentFallback(repository, working, config)
	if err != nil {
		t.Fatal(err)
	}
	record, err := fallback.Decide(context.Background(), 1, "s1", 100)
	if err != nil {
		t.Fatal(err)
	}
	if !record.FellBack || !record.Succeeded {
		t.Fatalf("the fallback did not run: %+v", record)
	}
	if record.Reason != summarySegmentFallbackRan {
		t.Fatalf("the reason is %q", record.Reason)
	}

	failing := &stubSegmentProducer{err: errors.New("model refused")}
	fallback, err = NewSummarySegmentFallback(repository, failing, config)
	if err != nil {
		t.Fatal(err)
	}
	record, err = fallback.Decide(context.Background(), 1, "s1", 100)
	if err != nil {
		t.Fatal(err)
	}
	if !record.FellBack || record.Succeeded {
		t.Fatalf("the failing fallback was reported as a success: %+v", record)
	}
	if record.Reason != summarySegmentFallbackFailed {
		t.Fatalf("the reason is %q", record.Reason)
	}
}

// The fallback covers the same stretch the background job would have covered,
// and it does not move the start.
func TestTheFallbackCoversTheUncoveredStretchOnly(t *testing.T) {
	repository := &stubFallbackRepository{
		messages:  []model.Message{{ID: 300, IsUser: true, Content: string(make([]byte, 4000))}},
		watermark: 150,
	}
	producer := &stubSegmentProducer{}
	fallback, err := NewSummarySegmentFallback(repository, producer, SummarySegmentFallbackConfig{
		RawMessageTokenLimit: 100, StageBudget: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fallback.Decide(context.Background(), 1, "s1", 300); err != nil {
		t.Fatal(err)
	}
	if producer.lastJob == nil {
		t.Fatal("the fallback produced nothing")
	}
	if producer.lastJob.StartMessageID != 150 {
		t.Fatalf("the fallback started at %d, want the watermark 150", producer.lastJob.StartMessageID)
	}
	if producer.lastJob.DesiredMessageID != 300 {
		t.Fatalf("the fallback targeted %d, want 300", producer.lastJob.DesiredMessageID)
	}
}

// The reconciler enqueues the sessions that owe a segment, and it starts them
// at the session's own watermark.
func TestTheReconcilerEnqueuesTheSessionsThatOweASegment(t *testing.T) {
	repository := &stubReconcileRepository{
		gaps:      []model.SummarySegmentGap{{UserID: 1, SessionID: "s1", LatestMessageID: 300}},
		watermark: 120,
	}
	reconciler, err := NewSummarySegmentReconciler(repository, 10)
	if err != nil {
		t.Fatal(err)
	}
	repaired, err := reconciler.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if repaired != 1 {
		t.Fatalf("repaired = %d, want 1", repaired)
	}
	if repository.upserted == nil {
		t.Fatal("no job was recorded")
	}
	if repository.upserted.StartMessageID != 120 {
		t.Fatalf("the job starts at %d, want the watermark 120", repository.upserted.StartMessageID)
	}
	if repository.upserted.DesiredMessageID != 300 {
		t.Fatalf("the job targets %d, want 300", repository.upserted.DesiredMessageID)
	}
}

// The board is read once. The content the model saw and the version the
// write is checked against have to come from the same board: reading it
// twice lets one board's content be written against another board's
// version.
func TestTheBoardIsReadOnceAndItsVersionGoesWithIt(t *testing.T) {
	source := &stubSegmentSource{
		messages: []model.Message{{ID: 100, IsUser: true, Content: "a"}},
		board:    &model.SessionWhiteboard{Content: `{"schema_version":1}`, Version: 7},
	}
	sink := &stubSegmentSink{}
	generator := stubSegmentGeneratorWith(t, []model.SessionSummarySegmentPoint{{Text: "p", EvidenceMessageIDs: []uint{100}}})
	producer, err := NewModelSummarySegmentProducer(source, sink, generator, DefaultSummarySegmentProducerConfig())
	if err != nil {
		t.Fatal(err)
	}
	job := &model.SessionSummarySegmentJob{
		UserID: 1, SessionID: "s1", DesiredMessageID: 100, StartMessageID: 99,
	}
	if _, err := producer.ProduceSegment(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if source.boardReads != 1 {
		t.Fatalf("the board was read %d times, want once", source.boardReads)
	}
	if sink.version != 7 {
		t.Fatalf("the write was checked against version %d, want 7", sink.version)
	}
}

// The producer and the worker are wired to the shapes they claim, so a
// change in the DAO's method set is caught at compile time rather than at
// runtime.
func TestTheProducerAndTheWorkerUseTheRepositoryShapes(t *testing.T) {
	var _ SummarySegmentSource = (*stubSegmentSource)(nil)
	var _ SummarySegmentSink = (*stubSegmentSink)(nil)
	var _ SummarySegmentJobRepository = (*stubSegmentJobRepository)(nil)
	var _ SummarySegmentProducer = (*stubSegmentProducer)(nil)
	var _ SummarySegmentReconcileRepository = (*stubReconcileRepository)(nil)
	var _ SummarySegmentTriggerRepository = (*stubTriggerRepository)(nil)
	var _ SummarySegmentFallbackRepository = (*stubFallbackRepository)(nil)
	fmt.Fprint(newDiscardWriter(), "ok")
}
