package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"GopherAI/common/observability"
	"GopherAI/common/rag"
	"GopherAI/model"
)

// The fallback is what happens when the background cannot keep up.
//
// The order is fixed: the background retries, then the fallback runs
// synchronously before the answer, and only then is the raw conversation
// truncated. The fallback is last because it lengthens the wait before the
// answer, and that wait is exactly what the background was meant to remove.
//
// The fallback waits for the background to be done with the session. It stands
// back while the background has the session queued or in flight, and it takes
// the session only when the background gave up on it. Two generations of the
// same stretch would cost twice and race on the board.
//
// Every fallback is recorded. A fallback that is not recorded is a fallback
// nobody can count, and a count nobody can read is the same as no count.

// SummarySegmentFallbackConfig bounds the fallback.
type SummarySegmentFallbackConfig struct {
	// RawMessageTokenLimit is how many tokens the raw conversation may be
	// worth before the fallback runs. It is the point at which sending the
	// raw conversation would crowd the context, not a target to reach.
	RawMessageTokenLimit int
	// StageBudget is how long the fallback may spend generating before the
	// answer is allowed to go out.
	StageBudget time.Duration
	// Lease is how long the fallback holds the session while it generates.
	// It has to outlast the stage budget, or the background worker would take
	// the session back while the fallback is still working on it.
	Lease time.Duration
	// MaxAttempts is how many attempts the job is allowed before it is
	// failed. It is the queue's number, not the fallback's: the fallback
	// hands the job back and the queue decides what is left of it.
	MaxAttempts int
}

// DefaultSummarySegmentFallbackConfig returns the defaults.
func DefaultSummarySegmentFallbackConfig() SummarySegmentFallbackConfig {
	return SummarySegmentFallbackConfig{
		RawMessageTokenLimit: 6000, StageBudget: 8 * time.Second,
		Lease: 30 * time.Second, MaxAttempts: 3,
	}
}

// Validate checks the configuration.
func (c SummarySegmentFallbackConfig) Validate() error {
	if c.RawMessageTokenLimit < 100 || c.RawMessageTokenLimit > 1000000 {
		return fmt.Errorf("invalid summary segment raw message token limit")
	}
	if c.StageBudget <= 0 || c.StageBudget > 5*time.Minute {
		return fmt.Errorf("invalid summary segment fallback stage budget")
	}
	if c.Lease <= c.StageBudget || c.Lease > 5*time.Minute {
		return fmt.Errorf("invalid summary segment fallback lease")
	}
	if c.MaxAttempts < 1 || c.MaxAttempts > 10 {
		return fmt.Errorf("invalid summary segment fallback max attempts")
	}
	return nil
}

// SummarySegmentFallbackRepository is what the fallback reads and writes.
type SummarySegmentFallbackRepository interface {
	// ListSummarySegmentMessages returns turns in a range, oldest first.
	ListSummarySegmentMessages(ctx context.Context, userID int64, sessionID string, startMessageID, endMessageID uint, limit int) ([]model.Message, error)
	// GetSummarySegmentWatermark returns the message id the session's segments
	// cover and the next sequence number.
	GetSummarySegmentWatermark(ctx context.Context, userID int64, sessionID string) (uint, int, error)
	// GetSummarySegmentJobBySession returns the session's job row, or
	// model.ErrSummarySegmentJobNotFound when the session has none.
	GetSummarySegmentJobBySession(ctx context.Context, userID int64, sessionID string) (*model.SessionSummarySegmentJob, error)
	// TakeOverSummarySegmentJob moves the job into the processing state for
	// this attempt, and refuses while another holder has the lease.
	TakeOverSummarySegmentJob(ctx context.Context, userID int64, sessionID string, desired, start uint, now time.Time, lease time.Duration) (*model.SessionSummarySegmentJob, error)
	// CompleteSummarySegmentJob records that the attempt produced its segment.
	CompleteSummarySegmentJob(ctx context.Context, claimed *model.SessionSummarySegmentJob, coveredThrough uint, now time.Time) error
	// RetrySummarySegmentJob puts a failed attempt back in the queue.
	RetrySummarySegmentJob(ctx context.Context, claimed *model.SessionSummarySegmentJob, errorCode string, availableAt time.Time, maxAttempts int) error
}

// SummarySegmentFallbackRecord is the record of one fallback decision.
//
// It is written into the turn's running record so the cost of the fallback is
// visible next to the answer it delayed. The reason is a fixed set of labels
// rather than free text, because it is counted.
type SummarySegmentFallbackRecord struct {
	// FellBack is true when the synchronous generation ran.
	FellBack bool
	// Reason says why the decision went the way it did.
	Reason string
	// UnsummarizedMessages is how many turns were still only raw messages.
	UnsummarizedMessages int
	// UnsummarizedTokens is what those turns were worth.
	UnsummarizedTokens int
	// Succeeded is true when the synchronous generation produced a segment.
	Succeeded bool
	// Duration is how long the synchronous generation took.
	Duration time.Duration
}

// The reasons a fallback decision gives.
const (
	// summarySegmentFallbackNotNeeded means the raw conversation still fits.
	summarySegmentFallbackNotNeeded = "not_needed"
	// summarySegmentFallbackRan means the fallback generated a segment before
	// the answer went out.
	summarySegmentFallbackRan = "generated_before_answer"
	// summarySegmentFallbackFailed means the fallback tried and failed, so the
	// raw conversation was truncated instead.
	summarySegmentFallbackFailed = "generation_failed"
	// summarySegmentFallbackSkipped means the fallback was needed but no
	// generator was attached, so the raw conversation was truncated.
	summarySegmentFallbackSkipped = "generator_unavailable"
	// summarySegmentFallbackBackgroundQueued means the background still has
	// the session queued and has not run out of attempts.
	summarySegmentFallbackBackgroundQueued = "background_queued"
	// summarySegmentFallbackBackgroundRunning means the background is working
	// on the session right now.
	summarySegmentFallbackBackgroundRunning = "background_running"
)

// SummarySegmentFallback decides whether the raw conversation is too big to
// send and generates a segment instead.
type SummarySegmentFallback struct {
	repository SummarySegmentFallbackRepository
	producer   SummarySegmentProducer
	config     SummarySegmentFallbackConfig
	now        func() time.Time
}

// NewSummarySegmentFallback builds a fallback.
func NewSummarySegmentFallback(
	repository SummarySegmentFallbackRepository,
	producer SummarySegmentProducer,
	config SummarySegmentFallbackConfig,
) (*SummarySegmentFallback, error) {
	if repository == nil {
		return nil, fmt.Errorf("summary segment fallback repository is required")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &SummarySegmentFallback{repository: repository, producer: producer, config: config, now: time.Now}, nil
}

// Decide says whether the fallback should run for this turn.
//
// The condition is one number: the raw conversation that is not covered by any
// segment, measured in tokens. When it is under the limit, the raw messages
// fit in the context and nothing is owed. When it is over, sending them
// would crowd out everything else, so a segment is generated first.
//
// The measurement is over the whole uncovered stretch, not over the last
// few turns: a conversation that fell behind once stays behind, and a
// measurement of only the tail would not see that.
//
// The background goes first. The fallback runs only when the background is not
// going to: a job that is queued or in flight belongs to the background
// worker, and a job that failed means the background ran out of attempts. A
// fallback that ran next to the background would pay for a second generation
// of the same stretch and race it on the board.
func (f *SummarySegmentFallback) Decide(ctx context.Context, userID int64, sessionID string, latestMessageID uint) (SummarySegmentFallbackRecord, error) {
	if f == nil || f.repository == nil {
		return SummarySegmentFallbackRecord{}, fmt.Errorf("summary segment fallback is not initialized")
	}
	record := SummarySegmentFallbackRecord{Reason: summarySegmentFallbackNotNeeded}
	if userID <= 0 || sessionID == "" || latestMessageID == 0 {
		return record, fmt.Errorf("invalid summary segment fallback event")
	}
	watermark, _, err := f.repository.GetSummarySegmentWatermark(ctx, userID, sessionID)
	if err != nil {
		return record, err
	}
	if watermark >= latestMessageID {
		// Every turn is covered already. The decision is still a decision,
		// and an unrecorded decision is one nobody can count.
		observability.Memory.SummarySegmentFallback(record.Reason, record.UnsummarizedTokens)
		return record, nil
	}
	messages, err := f.repository.ListSummarySegmentMessages(ctx, userID, sessionID, watermark+1, latestMessageID, 1024)
	if err != nil {
		return record, err
	}
	tokens := 0
	for _, message := range messages {
		tokens += rag.EstimateTokens(message.Content)
	}
	record.UnsummarizedMessages = len(messages)
	record.UnsummarizedTokens = tokens
	if len(messages) == 0 || tokens <= f.config.RawMessageTokenLimit {
		observability.Memory.SummarySegmentFallback(record.Reason, record.UnsummarizedTokens)
		return record, nil
	}
	if f.producer == nil {
		record.Reason = summarySegmentFallbackSkipped
		observability.Memory.SummarySegmentFallback(record.Reason, record.UnsummarizedTokens)
		return record, nil
	}
	// The background's state decides whether this attempt happens at all.
	job, err := f.repository.GetSummarySegmentJobBySession(ctx, userID, sessionID)
	if err != nil && !errors.Is(err, model.ErrSummarySegmentJobNotFound) {
		return record, err
	}
	if reason, busy := summarySegmentBackgroundOwns(job, f.now()); busy {
		record.Reason = reason
		observability.Memory.SummarySegmentFallback(record.Reason, record.UnsummarizedTokens)
		return record, nil
	}
	// The lease is what keeps the background worker from starting the same
	// generation at the same time. The takeover refuses while somebody else
	// holds it, so a session never has two generations running.
	claimed, err := f.repository.TakeOverSummarySegmentJob(ctx, userID, sessionID, latestMessageID, watermark+1, f.now(), f.config.Lease)
	if err != nil {
		if errors.Is(err, model.ErrSummarySegmentJobConflict) {
			record.Reason = summarySegmentFallbackBackgroundRunning
			observability.Memory.SummarySegmentFallback(record.Reason, record.UnsummarizedTokens)
			return record, nil
		}
		return record, err
	}
	// The fallback covers the stretch from the first uncovered turn to the
	// newest one. It does not move the start on its own: the takeover did
	// that, and the segment it produces starts where the watermark ends.
	started := time.Now()
	taskCtx, cancel := context.WithTimeout(ctx, f.config.StageBudget)
	outcome, produceErr := f.producer.ProduceSegment(taskCtx, claimed)
	cancel()
	record.Duration = time.Since(started)
	record.FellBack = true
	if produceErr != nil {
		record.Reason = summarySegmentFallbackFailed
		observability.Memory.SummarySegmentFallback(record.Reason, record.UnsummarizedTokens)
		f.releaseAttempt(ctx, claimed, produceErr)
		return record, nil
	}
	record.Succeeded = true
	record.Reason = summarySegmentFallbackRan
	observability.Memory.SummarySegmentFallback(record.Reason, record.UnsummarizedTokens)
	f.finishAttempt(ctx, claimed, outcome)
	return record, nil
}

// summarySegmentBackgroundOwns says whether the background worker is going to
// deal with the session, and why the fallback stands back when it is.
//
// A job that is queued has attempts left, so the background is going to try
// it. A job that is being processed is being handled right now. A job that
// failed is the one case where the background is not going to come back, and
// a session with no job at all has nothing running against it.
func summarySegmentBackgroundOwns(job *model.SessionSummarySegmentJob, now time.Time) (string, bool) {
	if job == nil {
		return "", false
	}
	switch job.Status {
	case model.SessionSummarySegmentJobPending:
		return summarySegmentFallbackBackgroundQueued, true
	case model.SessionSummarySegmentJobProcessing:
		if job.LeaseEndsAt != nil && job.LeaseEndsAt.After(now) {
			return summarySegmentFallbackBackgroundRunning, true
		}
		// The holder is gone. The lease will be reclaimed, and the fallback
		// may take the session instead of waiting for the reclaim.
		return "", false
	default:
		return "", false
	}
}

// finishAttempt records the segment the fallback produced.
func (f *SummarySegmentFallback) finishAttempt(ctx context.Context, claimed *model.SessionSummarySegmentJob, outcome SummarySegmentOutcome) {
	if err := f.repository.CompleteSummarySegmentJob(ctx, claimed, outcome.EndMessageID, f.now()); err != nil {
		log.Printf("summary segment fallback completion failed: session=%s error_type=%T", claimed.SessionID, err)
	}
}

// releaseAttempt puts a failed attempt back where it was found.
//
// The fallback does not decide whether the background may try again: it hands
// the job back with the error it failed on, and the queue's own rules say
// whether there are attempts left.
func (f *SummarySegmentFallback) releaseAttempt(ctx context.Context, claimed *model.SessionSummarySegmentJob, cause error) {
	if err := f.repository.RetrySummarySegmentJob(ctx, claimed, summarySegmentFallbackErrorCode(cause), f.now(), f.config.MaxAttempts); err != nil {
		log.Printf("summary segment fallback release failed: session=%s error_type=%T", claimed.SessionID, err)
	}
}

// summarySegmentFallbackErrorCode names why the attempt failed.
func summarySegmentFallbackErrorCode(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "task_timeout"
	case errors.Is(err, context.Canceled):
		return "task_cancelled"
	case errors.Is(err, ErrSummarySegmentWhiteboardConflict):
		return "whiteboard_conflict"
	case errors.Is(err, ErrSummarySegmentMessagesUnavailable):
		return "source_not_found"
	case errors.Is(err, ErrSummarySegmentOutputInvalid):
		return "invalid_model_output"
	default:
		return "generation_failed"
	}
}
