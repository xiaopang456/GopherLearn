package agent

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"GopherAI/model"
)

// The stubs here stand in for the database and for the model. They exist so
// the worker's decisions can be tested without either.

// newDiscardWriter returns a writer that throws everything away.
func newDiscardWriter() io.Writer { return io.Discard }

// stubSegmentGenerator is a generator that answers with a fixed document.
type stubSegmentGenerator struct {
	result SegmentGenerationResult
	err    error
	calls  int
	last   SegmentGenerationInput
}

func (g *stubSegmentGenerator) GenerateSegment(_ context.Context, input SegmentGenerationInput) (SegmentGenerationResult, error) {
	g.calls++
	g.last = input
	if g.err != nil {
		return SegmentGenerationResult{}, g.err
	}
	if g.result.SegmentContent == "" {
		return SegmentGenerationResult{}, errors.New("the stub generator has no answer")
	}
	return g.result, nil
}

// stubSegmentGeneratorWith builds a generator that answers with one segment
// and one board.
func stubSegmentGeneratorWith(t *testing.T, points []model.SessionSummarySegmentPoint) *stubSegmentGenerator {
	t.Helper()
	content := model.SessionSummarySegmentContent{
		SchemaVersion: model.SessionSummarySegmentSchemaV1,
		Topic:         "t",
		Points:        points,
	}
	encoded, err := model.MarshalSessionSummarySegmentContent(content)
	if err != nil {
		t.Fatal(err)
	}
	board := model.SessionWhiteboardDocument{SchemaVersion: model.SessionWhiteboardSchemaV1}
	boardContent, err := model.MarshalSessionWhiteboardDocument(board)
	if err != nil {
		t.Fatal(err)
	}
	return &stubSegmentGenerator{result: SegmentGenerationResult{
		Segment:           content,
		SegmentContent:    encoded,
		Whiteboard:        board,
		WhiteboardContent: boardContent,
	}}
}

// stubSegmentSource is the source the producer reads from.
type stubSegmentSource struct {
	messages    []model.Message
	board       *model.SessionWhiteboard
	segments    []model.SessionSummarySegment
	watermark   uint
	seq         int
	readStart   uint
	readEnd     uint
	readCalls   int
	boardReads  int
	messagesErr error
}

func (s *stubSegmentSource) ListSummarySegmentMessages(_ context.Context, userID int64, sessionID string, startMessageID, endMessageID uint, limit int) ([]model.Message, error) {
	s.readCalls++
	s.readStart = startMessageID
	s.readEnd = endMessageID
	if s.messagesErr != nil {
		return nil, s.messagesErr
	}
	// The range is inclusive of both ends, the way the repository reads it.
	var out []model.Message
	for _, message := range s.messages {
		if message.ID >= startMessageID && message.ID <= endMessageID {
			out = append(out, message)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *stubSegmentSource) GetWhiteboard(_ context.Context, userID int64, sessionID string) (*model.SessionWhiteboard, error) {
	s.boardReads++
	if s.board == nil {
		return nil, model.ErrWhiteboardNotFound
	}
	return s.board, nil
}

func (s *stubSegmentSource) GetSummarySegmentWatermark(_ context.Context, userID int64, sessionID string) (uint, int, error) {
	return s.watermark, s.seq, nil
}

func (s *stubSegmentSource) ListSummarySegments(_ context.Context, userID int64, sessionID string, limit int) ([]model.SessionSummarySegment, error) {
	return s.segments, nil
}

func (s *stubSegmentSource) GetLatestSummarySegment(_ context.Context, userID int64, sessionID string) (*model.SessionSummarySegment, error) {
	if len(s.segments) == 0 {
		return nil, model.ErrSummarySegmentNotFound
	}
	latest := s.segments[len(s.segments)-1]
	return &latest, nil
}

// stubSegmentSink is what the producer writes to.
type stubSegmentSink struct {
	segment *model.SessionSummarySegment
	board   *model.SessionWhiteboard
	version uint64
	err     error
}

func (s *stubSegmentSink) SaveSummarySegmentAndWhiteboard(_ context.Context, segment *model.SessionSummarySegment, board *model.SessionWhiteboard, expectedVersion uint64) error {
	if s.err != nil {
		return s.err
	}
	s.segment = segment
	s.board = board
	s.version = expectedVersion
	return nil
}

func (s *stubSegmentSink) NextSummarySegmentSeq(_ context.Context, userID int64, sessionID string) (int, error) {
	return 1, nil
}

// stubReconcileRepository is what the reconciler reads and writes.
type stubReconcileRepository struct {
	gaps      []model.SummarySegmentGap
	upserted  *model.SessionSummarySegmentJob
	watermark uint
	err       error
}

func (r *stubReconcileRepository) ListSummarySegmentGaps(_ context.Context, limit int) ([]model.SummarySegmentGap, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.gaps, nil
}

func (r *stubReconcileRepository) UpsertSummarySegmentJob(_ context.Context, job *model.SessionSummarySegmentJob) error {
	if r.err != nil {
		return r.err
	}
	r.upserted = job
	return nil
}

func (r *stubReconcileRepository) GetSummarySegmentWatermark(_ context.Context, userID int64, sessionID string) (uint, int, error) {
	return r.watermark, 1, nil
}

// stubTriggerRepository is what the trigger reads and writes.
type stubTriggerRepository struct {
	messages    []model.Message
	watermark   uint
	upserted    *model.SessionSummarySegmentJob
	messagesErr error
}

func (r *stubTriggerRepository) ListSummarySegmentMessages(_ context.Context, userID int64, sessionID string, startMessageID, endMessageID uint, limit int) ([]model.Message, error) {
	if r.messagesErr != nil {
		return nil, r.messagesErr
	}
	return r.messages, nil
}

func (r *stubTriggerRepository) GetSummarySegmentWatermark(_ context.Context, userID int64, sessionID string) (uint, int, error) {
	return r.watermark, 1, nil
}

func (r *stubTriggerRepository) UpsertSummarySegmentJob(_ context.Context, job *model.SessionSummarySegmentJob) error {
	r.upserted = job
	return nil
}

// stubFallbackRepository is what the fallback reads and writes.
type stubFallbackRepository struct {
	messages    []model.Message
	watermark   uint
	messagesErr error
	job         *model.SessionSummarySegmentJob
	taken       *model.SessionSummarySegmentJob
	takeoverErr error
	completed   []*model.SessionSummarySegmentJob
	retried     []*model.SessionSummarySegmentJob
}

func (r *stubFallbackRepository) ListSummarySegmentMessages(_ context.Context, userID int64, sessionID string, startMessageID, endMessageID uint, limit int) ([]model.Message, error) {
	if r.messagesErr != nil {
		return nil, r.messagesErr
	}
	return r.messages, nil
}

func (r *stubFallbackRepository) GetSummarySegmentWatermark(_ context.Context, userID int64, sessionID string) (uint, int, error) {
	return r.watermark, 1, nil
}

func (r *stubFallbackRepository) GetSummarySegmentJobBySession(_ context.Context, userID int64, sessionID string) (*model.SessionSummarySegmentJob, error) {
	if r.job == nil {
		return nil, model.ErrSummarySegmentJobNotFound
	}
	return r.job, nil
}

func (r *stubFallbackRepository) TakeOverSummarySegmentJob(_ context.Context, userID int64, sessionID string, desired, start uint, now time.Time, lease time.Duration) (*model.SessionSummarySegmentJob, error) {
	if r.takeoverErr != nil {
		return nil, r.takeoverErr
	}
	taken := &model.SessionSummarySegmentJob{
		ID: "fallback-job", UserID: userID, SessionID: sessionID,
		DesiredMessageID: desired, StartMessageID: start,
		Status: model.SessionSummarySegmentJobProcessing, Attempts: 1,
		LeaseToken: "fallback-token",
	}
	r.taken = taken
	return taken, nil
}

func (r *stubFallbackRepository) CompleteSummarySegmentJob(_ context.Context, claimed *model.SessionSummarySegmentJob, coveredThrough uint, now time.Time) error {
	r.completed = append(r.completed, claimed)
	return nil
}

func (r *stubFallbackRepository) RetrySummarySegmentJob(_ context.Context, claimed *model.SessionSummarySegmentJob, errorCode string, availableAt time.Time, maxAttempts int) error {
	r.retried = append(r.retried, claimed)
	return nil
}

// errStubProducer fails on demand.
type errStubProducer struct{ err error }

func (p *errStubProducer) ProduceSegment(context.Context, *model.SessionSummarySegmentJob) (SummarySegmentOutcome, error) {
	return SummarySegmentOutcome{}, p.err
}

// stubSegmentRepository is the whole storage surface at once. The assembly
// asks for one repository, so one stub answers all of it.
//
// It is written out rather than assembled from the smaller stubs because
// several of them answer the same method with different data, and a struct
// that embeds them all would have no way to pick one.
type stubSegmentRepository struct {
	messages  []model.Message
	watermark uint
	board     *model.SessionWhiteboard
	segments  []model.SessionSummarySegment
	gaps      []model.SummarySegmentGap
	upserted  *model.SessionSummarySegmentJob
	seq       int
	readStart uint
	readEnd   uint
	readLimit int
}

func (r *stubSegmentRepository) ListSummarySegmentMessages(_ context.Context, userID int64, sessionID string, startMessageID, endMessageID uint, limit int) ([]model.Message, error) {
	r.readStart = startMessageID
	r.readEnd = endMessageID
	r.readLimit = limit
	if limit > 0 && len(r.messages) > limit {
		return r.messages[:limit], nil
	}
	return r.messages, nil
}

func (r *stubSegmentRepository) GetWhiteboard(_ context.Context, userID int64, sessionID string) (*model.SessionWhiteboard, error) {
	if r.board == nil {
		return nil, model.ErrWhiteboardNotFound
	}
	return r.board, nil
}

func (r *stubSegmentRepository) GetSummarySegmentWatermark(_ context.Context, userID int64, sessionID string) (uint, int, error) {
	return r.watermark, r.seq, nil
}

func (r *stubSegmentRepository) ListSummarySegments(_ context.Context, userID int64, sessionID string, limit int) ([]model.SessionSummarySegment, error) {
	return r.segments, nil
}

func (r *stubSegmentRepository) GetLatestSummarySegment(_ context.Context, userID int64, sessionID string) (*model.SessionSummarySegment, error) {
	if len(r.segments) == 0 {
		return nil, model.ErrSummarySegmentNotFound
	}
	latest := r.segments[len(r.segments)-1]
	return &latest, nil
}

func (r *stubSegmentRepository) SaveSummarySegmentAndWhiteboard(_ context.Context, segment *model.SessionSummarySegment, board *model.SessionWhiteboard, expectedVersion uint64) error {
	return nil
}

func (r *stubSegmentRepository) NextSummarySegmentSeq(_ context.Context, userID int64, sessionID string) (int, error) {
	return r.seq + 1, nil
}

func (r *stubSegmentRepository) ListDueSummarySegmentJobs(_ context.Context, limit, maxAttempts int, now time.Time) ([]model.SessionSummarySegmentJob, error) {
	return nil, nil
}

func (r *stubSegmentRepository) ClaimSummarySegmentJob(_ context.Context, userID int64, jobID string, now time.Time, lease time.Duration, maxAttempts int) (*model.SessionSummarySegmentJob, error) {
	return nil, errors.New("nothing to claim")
}

func (r *stubSegmentRepository) CompleteSummarySegmentJob(_ context.Context, claimed *model.SessionSummarySegmentJob, coveredThrough uint, now time.Time) error {
	return nil
}

func (r *stubSegmentRepository) RetrySummarySegmentJob(_ context.Context, claimed *model.SessionSummarySegmentJob, errorCode string, availableAt time.Time, maxAttempts int) error {
	return nil
}

func (r *stubSegmentRepository) RecoverExpiredSummarySegmentJobs(_ context.Context, now time.Time, maxAttempts int) (int64, error) {
	return 0, nil
}

func (r *stubSegmentRepository) ListSummarySegmentGaps(_ context.Context, limit int) ([]model.SummarySegmentGap, error) {
	return r.gaps, nil
}

func (r *stubSegmentRepository) UpsertSummarySegmentJob(_ context.Context, job *model.SessionSummarySegmentJob) error {
	r.upserted = job
	return nil
}

func (r *stubSegmentRepository) GetSummarySegmentJobBySession(_ context.Context, userID int64, sessionID string) (*model.SessionSummarySegmentJob, error) {
	return nil, model.ErrSummarySegmentJobNotFound
}

func (r *stubSegmentRepository) TakeOverSummarySegmentJob(_ context.Context, userID int64, sessionID string, desired, start uint, now time.Time, lease time.Duration) (*model.SessionSummarySegmentJob, error) {
	return nil, model.ErrSummarySegmentJobConflict
}
