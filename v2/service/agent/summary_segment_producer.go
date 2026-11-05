package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"GopherAI/common/rag"
	"GopherAI/model"
)

// The producer turns one claimed job into one segment and the board it
// implies.
//
// The important property is what a retry covers. A job names a start and a
// target: the start is where the last segment stopped, the target is how far
// this attempt wants to reach. A failed attempt writes nothing, so the start
// does not move, and the next attempt reads the same stretch of turns from the
// same start against the same board.
//
// This is the difference from the rolling summary it replaces. That one took
// "the previous summary plus every unsummarized message", so each failure
// made the next attempt's input larger and the summary grew until it could
// not be written at all. Here the input is bounded by the same board and the
// same batch every time.

// The reasons an attempt produced no segment.
var (
	// ErrSummarySegmentMessagesUnavailable means the turns the job covers are
	// no longer readable: the session was deleted, or the messages are gone.
	ErrSummarySegmentMessagesUnavailable = errors.New("summary segment source messages are unavailable")
	// ErrSummarySegmentWhiteboardConflict means another writer moved the board
	// on. It is not a failure of the generation: the work was done, the write
	// was refused, and the next attempt reads the board that won.
	ErrSummarySegmentWhiteboardConflict = errors.New("summary segment whiteboard version conflict")
	// ErrSummarySegmentOutputInvalid means the generation produced something
	// that could not be stored.
	ErrSummarySegmentOutputInvalid = errors.New("summary segment output is invalid")
)

// SummarySegmentSource is what a producer reads to do its work.
type SummarySegmentSource interface {
	// ListSummarySegmentMessages returns the turns the batch covers, oldest
	// first. The range is inclusive of both ends.
	ListSummarySegmentMessages(ctx context.Context, userID int64, sessionID string, startMessageID, endMessageID uint, limit int) ([]model.Message, error)
	// GetWhiteboard returns the current board, or ErrWhiteboardNotFound when
	// the session has none yet.
	GetWhiteboard(ctx context.Context, userID int64, sessionID string) (*model.SessionWhiteboard, error)
	// GetSummarySegmentWatermark returns the message id the session's segments
	// cover and the next sequence number.
	GetSummarySegmentWatermark(ctx context.Context, userID int64, sessionID string) (uint, int, error)
	// ListSummarySegments returns a session's segments in order.
	ListSummarySegments(ctx context.Context, userID int64, sessionID string, limit int) ([]model.SessionSummarySegment, error)
}

// SummarySegmentSink is what a producer writes to.
type SummarySegmentSink interface {
	// SaveSummarySegmentAndWhiteboard writes the segment and the board it
	// implies as one unit. Either both land or neither does.
	SaveSummarySegmentAndWhiteboard(ctx context.Context, segment *model.SessionSummarySegment, board *model.SessionWhiteboard, expectedVersion uint64) error
	// NextSummarySegmentSeq returns the sequence number the next segment takes.
	NextSummarySegmentSeq(ctx context.Context, userID int64, sessionID string) (int, error)
}

// SummarySegmentProducerConfig bounds one attempt.
type SummarySegmentProducerConfig struct {
	// MaxMessages is how many turns one segment may cover.
	MaxMessages int
	// MaxInputTokens is the generator's input budget.
	MaxInputTokens int
	// MaxOutputTokens is the generator's output budget.
	MaxOutputTokens int
	// FetchLimit is how many turns are read from the database at once.
	FetchLimit int
}

// DefaultSummarySegmentProducerConfig returns the defaults.
func DefaultSummarySegmentProducerConfig() SummarySegmentProducerConfig {
	return SummarySegmentProducerConfig{
		MaxMessages:     64,
		MaxInputTokens:  8000,
		MaxOutputTokens: 2000,
		FetchLimit:      256,
	}
}

// Validate checks the configuration.
func (c SummarySegmentProducerConfig) Validate() error {
	if c.MaxMessages < 1 || c.MaxMessages > 512 ||
		c.MaxInputTokens < 512 || c.MaxInputTokens > 100000 ||
		c.MaxOutputTokens < 128 || c.MaxOutputTokens > 8000 ||
		c.FetchLimit < c.MaxMessages || c.FetchLimit > 1024 {
		return fmt.Errorf("invalid summary segment producer config")
	}
	return nil
}

// ModelSummarySegmentProducer produces a segment with a chat model and stores
// it together with the board.
type ModelSummarySegmentProducer struct {
	source    SummarySegmentSource
	sink      SummarySegmentSink
	generator SegmentGenerator
	config    SummarySegmentProducerConfig
}

// NewModelSummarySegmentProducer builds a producer.
func NewModelSummarySegmentProducer(
	source SummarySegmentSource,
	sink SummarySegmentSink,
	generator SegmentGenerator,
	config SummarySegmentProducerConfig,
) (*ModelSummarySegmentProducer, error) {
	if source == nil || sink == nil || generator == nil {
		return nil, fmt.Errorf("summary segment producer dependencies are required")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &ModelSummarySegmentProducer{source: source, sink: sink, generator: generator, config: config}, nil
}

// ProduceSegment generates one segment and writes it with the board.
//
// The order of the work is the order of the ways it can go wrong: the source
// turns are read first, then the batch is fixed, then the board is read, then
// the model is asked, and only then is anything written.
func (p *ModelSummarySegmentProducer) ProduceSegment(ctx context.Context, job *model.SessionSummarySegmentJob) (SummarySegmentOutcome, error) {
	if job == nil {
		return SummarySegmentOutcome{}, fmt.Errorf("summary segment job is required")
	}
	// The batch is fixed before anything else, and it is the same batch the
	// last attempt read: the job's start does not move because nothing was
	// written, and the target only ever moves forward with new turns.
	start := job.StartMessageID
	target := job.DesiredMessageID
	if target <= start {
		return SummarySegmentOutcome{}, fmt.Errorf("%w: the job covers no turns", ErrSummarySegmentMessagesUnavailable)
	}
	// The batch is read from the start and stops at the target. How many turns
	// that is depends on the conversation, not on the difference between the
	// two ids: message ids are not consecutive, so a range of ids is not a
	// count of messages. The count is what bounds the batch.
	messages, err := p.source.ListSummarySegmentMessages(ctx, job.UserID, job.SessionID, start, target, p.config.MaxMessages)
	if err != nil {
		return SummarySegmentOutcome{}, fmt.Errorf("%w: %v", ErrSummarySegmentMessagesUnavailable, err)
	}
	if len(messages) == 0 {
		return SummarySegmentOutcome{}, fmt.Errorf("%w: no turns in the batch", ErrSummarySegmentMessagesUnavailable)
	}
	// The batch ends at the last turn that was actually read. A segment that
	// said it covered more turns than it summarized would answer for turns
	// nobody can read back.
	end := messages[len(messages)-1].ID

	whiteboardContent, whiteboardVersion, previousSegment, err := p.readContext(ctx, job)
	if err != nil {
		return SummarySegmentOutcome{}, err
	}

	result, err := p.generator.GenerateSegment(ctx, SegmentGenerationInput{
		Whiteboard:      whiteboardContent,
		PreviousSegment: previousSegment,
		Messages:        messages,
		FirstMessageID:  start,
		LastMessageID:   end,
	})
	if err != nil {
		return SummarySegmentOutcome{}, fmt.Errorf("generate summary segment: %w", err)
	}
	return p.persist(ctx, job, result, messages, start, end, whiteboardVersion)
}

// readContext reads the board and the previous segment the generator needs.
//
// The board comes back with its version because that version is the token the
// write is compared against. Reading the board twice would let the content
// and the version come from two different boards: the content the model saw
// would be one board and the version the write is checked against another.
func (p *ModelSummarySegmentProducer) readContext(ctx context.Context, job *model.SessionSummarySegmentJob) (string, uint64, string, error) {
	whiteboardContent := ""
	var whiteboardVersion uint64
	board, err := p.source.GetWhiteboard(ctx, job.UserID, job.SessionID)
	if err != nil {
		if !errors.Is(err, model.ErrWhiteboardNotFound) {
			return "", 0, "", fmt.Errorf("read session whiteboard: %w", err)
		}
	} else {
		whiteboardContent = board.Content
		whiteboardVersion = board.Version
	}
	previousSegment := ""
	segments, err := p.source.ListSummarySegments(ctx, job.UserID, job.SessionID, 1)
	if err != nil {
		return "", 0, "", fmt.Errorf("read summary segments: %w", err)
	}
	if len(segments) > 0 {
		previousSegment = segments[len(segments)-1].Content
	}
	return whiteboardContent, whiteboardVersion, previousSegment, nil
}

// persist writes the segment and the board together.
//
// They are written as one unit because they claim the same stretch of the
// conversation: a board that says it covers a turn no segment records would
// be a board that answered for turns nobody can read back.
func (p *ModelSummarySegmentProducer) persist(
	ctx context.Context,
	job *model.SessionSummarySegmentJob,
	result SegmentGenerationResult,
	messages []model.Message,
	start, end uint,
	whiteboardVersion uint64,
) (SummarySegmentOutcome, error) {
	seq, err := p.sink.NextSummarySegmentSeq(ctx, job.UserID, job.SessionID)
	if err != nil {
		return SummarySegmentOutcome{}, fmt.Errorf("allocate summary segment sequence: %w", err)
	}
	changes, err := model.MarshalSessionSummarySegmentChanges(result.Changes)
	if err != nil {
		return SummarySegmentOutcome{}, fmt.Errorf("%w: %v", ErrSummarySegmentOutputInvalid, err)
	}
	namesAndNumbers := model.MarshalSessionSummarySegmentTerms(result.Segment.Names, result.Segment.Numbers)

	segment := &model.SessionSummarySegment{
		UserID:            job.UserID,
		SessionID:         job.SessionID,
		Seq:               seq,
		StartMessageID:    start,
		EndMessageID:      end,
		StartAt:           messages[0].CreatedAt,
		EndAt:             messages[len(messages)-1].CreatedAt,
		Topic:             result.Segment.Topic,
		Content:           result.SegmentContent,
		NamesAndNumbers:   namesAndNumbers,
		WhiteboardChanges: changes,
		TokenEstimate:     rag.EstimateTokens(result.SegmentContent),
		IndexStatus:       model.SessionSummarySegmentIndexPending,
	}
	if segment.StartAt.IsZero() {
		segment.StartAt = time.Now()
	}
	if segment.EndAt.IsZero() {
		segment.EndAt = segment.StartAt
	}

	board := &model.SessionWhiteboard{
		UserID:           job.UserID,
		SessionID:        job.SessionID,
		Content:          result.WhiteboardContent,
		CoveredMessageID: end,
		TokenEstimate:    rag.EstimateTokens(result.WhiteboardContent),
	}
	// The version is the one the board had when the model read it. If another
	// writer moved the board on in the meantime, this write is refused and
	// the next attempt reads the board that won.
	if err := p.sink.SaveSummarySegmentAndWhiteboard(ctx, segment, board, whiteboardVersion); err != nil {
		if errors.Is(err, ErrSummarySegmentWhiteboardConflict) {
			return SummarySegmentOutcome{}, ErrSummarySegmentWhiteboardConflict
		}
		return SummarySegmentOutcome{}, fmt.Errorf("save summary segment: %w", err)
	}
	return SummarySegmentOutcome{
		SegmentSeq:        seq,
		StartMessageID:    start,
		EndMessageID:      end,
		WhiteboardVersion: whiteboardVersion + 1,
		WhiteboardChanges: len(result.Changes),
	}, nil
}
