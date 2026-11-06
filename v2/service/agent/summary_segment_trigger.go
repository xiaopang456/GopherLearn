package agent

import (
	"context"
	"fmt"
	"log"
	"time"

	"GopherAI/common/rag"
	"GopherAI/model"
)

// The trigger decides whether a finished turn needs a segment, and the
// fallback decides what to do when the background cannot keep up.
//
// Both are about the same thing: how much of the conversation is still only
// in the raw messages. The trigger keeps that number from growing; the
// fallback is what happens when it grew anyway.

// SummarySegmentTriggerConfig is the threshold at which a segment is wanted.
type SummarySegmentTriggerConfig struct {
	// TriggerMessages is how many unsummarized turns are enough on their own.
	TriggerMessages int
	// TriggerTokens is how many unsummarized tokens are enough on their own.
	TriggerTokens int
	// FetchLimit is how many turns are read to measure them.
	FetchLimit int
}

// DefaultSummarySegmentTriggerConfig returns the defaults.
//
// The two thresholds are alternatives, not a pair: either one being reached is
// enough to want a segment. The message count catches a long exchange of
// short turns, and the token count catches a few very long ones.
func DefaultSummarySegmentTriggerConfig() SummarySegmentTriggerConfig {
	return SummarySegmentTriggerConfig{TriggerMessages: 10, TriggerTokens: 6000, FetchLimit: 512}
}

// Validate checks the configuration.
func (c SummarySegmentTriggerConfig) Validate() error {
	if c.TriggerMessages < 1 || c.TriggerMessages > 1000 ||
		c.TriggerTokens < 1 || c.TriggerTokens > 1000000 ||
		c.FetchLimit < c.TriggerMessages || c.FetchLimit > 1024 {
		return fmt.Errorf("invalid summary segment trigger configuration")
	}
	return nil
}

// SummarySegmentTriggerRepository is what the trigger reads and writes.
type SummarySegmentTriggerRepository interface {
	// ListSummarySegmentMessages returns turns in a range, oldest first.
	ListSummarySegmentMessages(ctx context.Context, userID int64, sessionID string, startMessageID, endMessageID uint, limit int) ([]model.Message, error)
	// GetSummarySegmentWatermark returns the message id the session's segments
	// cover and the next sequence number.
	GetSummarySegmentWatermark(ctx context.Context, userID int64, sessionID string) (uint, int, error)
	// UpsertSummarySegmentJob records that a session wants a segment.
	UpsertSummarySegmentJob(ctx context.Context, job *model.SessionSummarySegmentJob) error
}

// SummarySegmentEnqueuer is what the service calls after a turn is answered.
//
// It is the write side of the layered summary. The enqueue itself makes no
// model call, and it happens after the answer was stored, so nothing about it
// can make a learner wait.
type SummarySegmentEnqueuer interface {
	Consider(ctx context.Context, userID int64, sessionID string, latestMessageID uint) (TriggerOutcome, error)
}

// discardSummarySegmentEnqueuer drops the enqueue.
type discardSummarySegmentEnqueuer struct{}

func (discardSummarySegmentEnqueuer) Consider(context.Context, int64, string, uint) (TriggerOutcome, error) {
	return TriggerOutcome{}, nil
}

// DiscardSummarySegmentEnqueues is the enqueuer a deployment uses when the
// layered summary is not running.
var DiscardSummarySegmentEnqueues SummarySegmentEnqueuer = discardSummarySegmentEnqueuer{}

// SummarySegmentTrigger enqueues a segment when a turn leaves enough
// unsummarized conversation behind.
type SummarySegmentTrigger struct {
	repository SummarySegmentTriggerRepository
	config     SummarySegmentTriggerConfig
	now        func() time.Time
	logf       func(string, ...any)
}

// NewSummarySegmentTrigger builds a trigger.
func NewSummarySegmentTrigger(repository SummarySegmentTriggerRepository, config SummarySegmentTriggerConfig) (*SummarySegmentTrigger, error) {
	if repository == nil {
		return nil, fmt.Errorf("summary segment trigger repository is required")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &SummarySegmentTrigger{
		repository: repository,
		config:     config,
		now:        time.Now,
		logf:       log.Printf,
	}, nil
}

// TriggerOutcome says what the trigger decided.
type TriggerOutcome struct {
	// Enqueued is true when a job was recorded.
	Enqueued bool
	// Reason names why the decision went the way it did.
	Reason string
	// UnsummarizedMessages is how many turns are still only in the raw
	// messages.
	UnsummarizedMessages int
	// UnsummarizedTokens is how many tokens those turns are worth.
	UnsummarizedTokens int
}

// The reasons the trigger gives.
const (
	summarySegmentTriggerEnqueued       = "enqueued"
	summarySegmentTriggerBelowThreshold = "below_threshold"
	summarySegmentTriggerNoUnsummarized = "nothing_unsummarized"
)

// Consider decides whether the session needs a segment.
//
// It is called after a turn is answered, so the newest turn is already
// stored. The segment is wanted when the conversation has grown past what the
// raw messages can carry.
//
// The enqueue names the first turn after the session's own watermark as the
// start, so the segment covers only the turns that are not covered yet, and it
// never moves the start backwards: a job that is already running keeps its
// start.
func (t *SummarySegmentTrigger) Consider(ctx context.Context, userID int64, sessionID string, latestMessageID uint) (TriggerOutcome, error) {
	if t == nil || t.repository == nil {
		return TriggerOutcome{}, fmt.Errorf("summary segment trigger is not initialized")
	}
	if userID <= 0 || sessionID == "" || latestMessageID == 0 {
		return TriggerOutcome{}, fmt.Errorf("invalid summary segment trigger event")
	}
	watermark, _, err := t.repository.GetSummarySegmentWatermark(ctx, userID, sessionID)
	if err != nil {
		return TriggerOutcome{}, err
	}
	if watermark >= latestMessageID {
		return TriggerOutcome{Reason: summarySegmentTriggerNoUnsummarized}, nil
	}
	messages, err := t.repository.ListSummarySegmentMessages(ctx, userID, sessionID, watermark+1, latestMessageID, t.config.FetchLimit)
	if err != nil {
		return TriggerOutcome{}, err
	}
	if len(messages) == 0 {
		return TriggerOutcome{Reason: summarySegmentTriggerNoUnsummarized}, nil
	}
	tokens := 0
	for _, message := range messages {
		tokens += rag.EstimateTokens(message.Content)
	}
	outcome := TriggerOutcome{
		UnsummarizedMessages: len(messages),
		UnsummarizedTokens:   tokens,
	}
	if len(messages) < t.config.TriggerMessages && tokens < t.config.TriggerTokens {
		outcome.Reason = summarySegmentTriggerBelowThreshold
		return outcome, nil
	}
	job := &model.SessionSummarySegmentJob{
		UserID:           userID,
		SessionID:        sessionID,
		DesiredMessageID: latestMessageID,
		// The start is the first turn after the watermark, not the watermark
		// itself: the watermark is the last turn a segment already covers, and
		// a segment that covers it again would answer for a turn twice.
		StartMessageID: watermark + 1,
		Status:         model.SessionSummarySegmentJobPending,
		AvailableAt:    t.now(),
	}
	if err := t.repository.UpsertSummarySegmentJob(ctx, job); err != nil {
		return outcome, err
	}
	outcome.Enqueued = true
	outcome.Reason = summarySegmentTriggerEnqueued
	return outcome, nil
}
