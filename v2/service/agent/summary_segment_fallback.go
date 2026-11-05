package agent

import (
	"context"
	"fmt"
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
}

// DefaultSummarySegmentFallbackConfig returns the defaults.
func DefaultSummarySegmentFallbackConfig() SummarySegmentFallbackConfig {
	return SummarySegmentFallbackConfig{RawMessageTokenLimit: 6000, StageBudget: 8 * time.Second}
}

// Validate checks the configuration.
func (c SummarySegmentFallbackConfig) Validate() error {
	if c.RawMessageTokenLimit < 100 || c.RawMessageTokenLimit > 1000000 {
		return fmt.Errorf("invalid summary segment raw message token limit")
	}
	if c.StageBudget <= 0 || c.StageBudget > 5*time.Minute {
		return fmt.Errorf("invalid summary segment fallback stage budget")
	}
	return nil
}

// SummarySegmentFallbackRepository is what the fallback reads.
type SummarySegmentFallbackRepository interface {
	// ListSummarySegmentMessages returns turns in a range, oldest first.
	ListSummarySegmentMessages(ctx context.Context, userID int64, sessionID string, startMessageID, endMessageID uint, limit int) ([]model.Message, error)
	// GetSummarySegmentWatermark returns the message id the session's segments
	// cover and the next sequence number.
	GetSummarySegmentWatermark(ctx context.Context, userID int64, sessionID string) (uint, int, error)
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
)

// SummarySegmentFallback decides whether the raw conversation is too big to
// send and generates a segment instead.
type SummarySegmentFallback struct {
	repository SummarySegmentFallbackRepository
	producer   SummarySegmentProducer
	config     SummarySegmentFallbackConfig
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
	return &SummarySegmentFallback{repository: repository, producer: producer, config: config}, nil
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
		return record, nil
	}
	if f.producer == nil {
		record.Reason = summarySegmentFallbackSkipped
		observability.Memory.SummarySegmentFallback(record.Reason, record.UnsummarizedTokens)
		return record, nil
	}
	// The fallback covers the same stretch the background job would have
	// covered. It does not move the start: the segment it produces starts at
	// the session's watermark like any other segment.
	job := &model.SessionSummarySegmentJob{
		UserID:           userID,
		SessionID:        sessionID,
		DesiredMessageID: latestMessageID,
		StartMessageID:   watermark,
		Status:           model.SessionSummarySegmentJobProcessing,
	}
	started := time.Now()
	taskCtx, cancel := context.WithTimeout(ctx, f.config.StageBudget)
	_, produceErr := f.producer.ProduceSegment(taskCtx, job)
	cancel()
	record.Duration = time.Since(started)
	record.FellBack = true
	if produceErr != nil {
		record.Reason = summarySegmentFallbackFailed
		observability.Memory.SummarySegmentFallback(record.Reason, record.UnsummarizedTokens)
		return record, nil
	}
	record.Succeeded = true
	record.Reason = summarySegmentFallbackRan
	observability.Memory.SummarySegmentFallback(record.Reason, record.UnsummarizedTokens)
	return record, nil
}
