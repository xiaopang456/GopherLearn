package conversation

// The layered summary, written by the product's own producer.
//
// The tests in the other file drive the evaluation's entry and read what the
// run wrote. This one goes one step further down: it drives the producer the
// product's background worker uses - the code that reads a batch, asks the
// model, and writes the segment and the board together - over the same
// evaluation boundary and the same local stand-in.
//
// The question it answers is the one a run cannot answer by itself: when a
// summary never fits, does anything get written? The producer is where that
// is decided. A failed attempt writes nothing, so the session's watermark
// stays where it was and the next attempt reads the same stretch of turns
// from the same start.

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/cloudwego/eino-ext/components/model/openai"
	modelcomponent "github.com/cloudwego/eino/components/model"
	"gorm.io/gorm"

	"GopherAI/common/aihelper"
	"GopherAI/common/memoryrecall"
	"GopherAI/config"
	"GopherAI/model"
	agent "GopherAI/service/agent"
)

// segmentStore is what the producer reads from and writes to.
//
// It is the same local database the run uses, and the same rows: the turns
// the producer reads are the turns that were stored, and the segment it
// writes is a row that can be read back.
type segmentStore struct {
	db *gorm.DB
	// writes is how many times a segment and a board were written together.
	writes int
}

// ListSummarySegmentMessages reads the turns of the batch.
func (s *segmentStore) ListSummarySegmentMessages(ctx context.Context, userID int64, sessionID string,
	startMessageID, endMessageID uint, limit int) ([]model.Message, error) {
	var messages []model.Message
	if err := s.db.WithContext(ctx).Where("session_id = ? AND id >= ? AND id <= ?", sessionID, startMessageID, endMessageID).
		Order("id asc").Limit(limit).Find(&messages).Error; err != nil {
		return nil, err
	}
	return messages, nil
}

// GetWhiteboard reads the board the session has, if it has one.
func (s *segmentStore) GetWhiteboard(ctx context.Context, userID int64, sessionID string) (*model.SessionWhiteboard, error) {
	var board model.SessionWhiteboard
	if err := s.db.WithContext(ctx).Where("user_id = ? AND session_id = ?", userID, sessionID).
		First(&board).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, model.ErrWhiteboardNotFound
		}
		return nil, err
	}
	return &board, nil
}

// GetSummarySegmentWatermark says how far the session's segments reach.
func (s *segmentStore) GetSummarySegmentWatermark(ctx context.Context, userID int64, sessionID string) (uint, int, error) {
	var segments []model.SessionSummarySegment
	if err := s.db.WithContext(ctx).Where("user_id = ? AND session_id = ?", userID, sessionID).
		Order("seq asc").Find(&segments).Error; err != nil {
		return 0, 0, err
	}
	if len(segments) == 0 {
		return 0, 1, nil
	}
	return segments[len(segments)-1].EndMessageID, segments[len(segments)-1].Seq + 1, nil
}

// GetLatestSummarySegment reads the segment before the batch.
func (s *segmentStore) GetLatestSummarySegment(ctx context.Context, userID int64, sessionID string) (*model.SessionSummarySegment, error) {
	var segment model.SessionSummarySegment
	if err := s.db.WithContext(ctx).Where("user_id = ? AND session_id = ?", userID, sessionID).
		Order("seq desc").First(&segment).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, model.ErrSummarySegmentNotFound
		}
		return nil, err
	}
	return &segment, nil
}

// SaveSummarySegmentAndWhiteboard writes the segment and the board together.
func (s *segmentStore) SaveSummarySegmentAndWhiteboard(ctx context.Context, segment *model.SessionSummarySegment,
	board *model.SessionWhiteboard, expectedVersion uint64) error {
	current, err := s.GetWhiteboard(ctx, board.UserID, board.SessionID)
	if err != nil && !errors.Is(err, model.ErrWhiteboardNotFound) {
		return err
	}
	version := uint64(0)
	if current != nil {
		version = current.Version
	}
	if version != expectedVersion {
		return agent.ErrSummarySegmentWhiteboardConflict
	}
	if err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(segment).Error; err != nil {
			return err
		}
		board.Version = expectedVersion + 1
		return tx.Save(board).Error
	}); err != nil {
		return err
	}
	s.writes++
	return nil
}

// NextSummarySegmentSeq is the sequence the next segment takes.
func (s *segmentStore) NextSummarySegmentSeq(ctx context.Context, userID int64, sessionID string) (int, error) {
	_, seq, err := s.GetSummarySegmentWatermark(ctx, userID, sessionID)
	return seq, err
}

// segments is how many segments the store holds.
func (s *segmentStore) segments(ctx context.Context, sessionID string) int64 {
	var count int64
	s.db.WithContext(ctx).Model(&model.SessionSummarySegment{}).Where("session_id = ?", sessionID).Count(&count)
	return count
}

// covered is how far the session's board reaches.
func (s *segmentStore) covered(ctx context.Context, sessionID string) uint {
	board, err := s.GetWhiteboard(ctx, 7001, sessionID)
	if err != nil {
		return 0
	}
	return board.CoveredMessageID
}

// producerChat is the model the producer asks, reached through the
// evaluation's own boundary.
//
// The role table comes from the product's sources, so a request the product
// sends is classified as the product's, and a request it does not send is
// refused before it reaches the provider.
func producerChat(t *testing.T, upstream *standInUpstream) modelcomponent.ToolCallingChatModel {
	t.Helper()
	root := repositoryRoot(t)
	roles, err := LoadRolePrompts(root, memoryrecall.ModeLegacyV2)
	if err != nil {
		t.Fatalf("the role table could not be read: %v", err)
	}
	ledger := &ModelLedger{}
	t.Cleanup(func() {
		if !ledger.Complete() {
			t.Errorf("the producer's calls were not all classified: %+v", ledger.Calls())
		}
	})
	wire := realWire(ledger, roles, upstream.base())
	t.Cleanup(wire.Close)
	dialect, err := config.NormalizeChatThinkingDialect(config.ChatThinkingDialectDeepSeek)
	if err != nil {
		t.Fatal(err)
	}
	chat, err := openai.NewChatModel(context.Background(), &openai.ChatModelConfig{
		BaseURL:     wire.URL + "/v1",
		Model:       "stand-in",
		APIKey:      "placeholder-proxy-injected",
		ExtraFields: aihelper.ThinkingExtraFields(dialect, false),
	})
	if err != nil {
		t.Fatal(err)
	}
	return chat
}

// producerStore is a store with the turns of one batch in it.
func producerStore(t *testing.T, sessionID string) *segmentStore {
	t.Helper()
	store := newLocalLayeredStore(t)
	ctx := context.Background()
	if err := store.Seed(ctx, model.User{ID: 7001, Username: "l4-eval-learner"}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(ctx, model.Session{ID: sessionID, UserID: 7001,
		UserName: "l4-eval-learner", Mode: model.SessionModeAgent, Title: "Producer run"}); err != nil {
		t.Fatal(err)
	}
	for _, turn := range summaryTurns() {
		if _, err := store.CreateMessage(ctx, model.Message{SessionID: sessionID, UserName: "l4-eval-learner",
			Content: turn.User, IsUser: true, CreatedAt: turn.UserAt.UTC()}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreateMessage(ctx, model.Message{SessionID: sessionID, UserName: "l4-eval-learner",
			Content: turn.Assistant, CreatedAt: turn.AssistantAt.UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	return &segmentStore{db: store.db}
}

// producer is the product's own producer, asked to write one batch.
func producer(t *testing.T, chat modelcomponent.ToolCallingChatModel, store *segmentStore) *agent.ModelSummarySegmentProducer {
	t.Helper()
	generator, err := agent.NewModelSegmentGenerator(chat, 8000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	generator.UseAssistantContent(true)
	generator.UseSegmentBudget(450)
	produced, err := agent.NewModelSummarySegmentProducer(store, store, generator,
		agent.DefaultSummarySegmentProducerConfig())
	if err != nil {
		t.Fatal(err)
	}
	return produced
}

// A batch that never fitted wrote nothing, and the session's watermark did
// not move.
//
// The producer is the code that decides this: it reads the board, asks the
// model, and only then writes. A generation that failed comes back before the
// write, so nothing is written and the board stays where it was - the next
// attempt reads the same stretch of turns from the same start.
func TestTheBatchThatNeverFittedWroteNothingAndTheWatermarkDidNotMove(t *testing.T) {
	upstream := newStandInUpstream(t, []standInAnswer{
		{content: overBudgetSegment()},
		{content: overBudgetSegment()},
		{content: overBudgetSegment()},
	})
	sessionID := "producer-never-fits"
	store := producerStore(t, sessionID)
	produced := producer(t, producerChat(t, upstream), store)

	outcome, err := produced.ProduceSegment(context.Background(), &model.SessionSummarySegmentJob{
		UserID: 7001, SessionID: sessionID,
		StartMessageID: 1, BatchMessageID: 10, DesiredMessageID: 10,
	})
	if err == nil {
		t.Fatalf("a batch that never fitted was written: %+v", outcome)
	}

	// The provider was asked three times and not a fourth.
	if upstream.count() != 3 {
		t.Fatalf("the provider was asked %d times, want 3", upstream.count())
	}
	// Nothing was written: not a segment, not a board, and no write at all.
	if store.writes != 0 {
		t.Fatalf("the producer wrote %d times although the batch failed", store.writes)
	}
	if count := store.segments(context.Background(), sessionID); count != 0 {
		t.Fatalf("the session holds %d segments although the batch failed", count)
	}
	if covered := store.covered(context.Background(), sessionID); covered != 0 {
		t.Fatalf("the board reaches %d although the batch failed", covered)
	}
}

// The batch that fitted wrote its segment and moved the watermark.
//
// The other half of the same rule: the write is not skipped because a
// summary had to be written again. The segment that is kept is the second
// answer, and the watermark moves to the end of the batch.
func TestTheBatchThatFittedWroteItsSegmentAndMovedTheWatermark(t *testing.T) {
	upstream := newStandInUpstream(t, []standInAnswer{
		{content: overBudgetSegment()},
		{content: fittingSegment()},
	})
	sessionID := "producer-fits"
	store := producerStore(t, sessionID)
	produced := producer(t, producerChat(t, upstream), store)

	outcome, err := produced.ProduceSegment(context.Background(), &model.SessionSummarySegmentJob{
		UserID: 7001, SessionID: sessionID,
		StartMessageID: 1, BatchMessageID: 10, DesiredMessageID: 10,
	})
	if err != nil {
		t.Fatalf("the batch was not written: %v", err)
	}
	if outcome.EndMessageID != 10 {
		t.Fatalf("the segment covers %d, want 10", outcome.EndMessageID)
	}
	if store.writes != 1 {
		t.Fatalf("the producer wrote %d times, want 1", store.writes)
	}
	if count := store.segments(context.Background(), sessionID); count != 1 {
		t.Fatalf("the session holds %d segments, want 1", count)
	}
	if covered := store.covered(context.Background(), sessionID); covered != 10 {
		t.Fatalf("the board reaches %d, want 10", covered)
	}
	// The segment that was written is the one the second answer produced.
	var segment model.SessionSummarySegment
	if err = store.db.Where("session_id = ?", sessionID).First(&segment).Error; err != nil {
		t.Fatal(err)
	}
	content, err := model.DecodeSessionSummarySegmentContent(segment.Content)
	if err != nil {
		t.Fatalf("the segment that was written could not be read: %v", err)
	}
	if len(content.Points) != 1 {
		t.Fatalf("the segment holds %d points, want the shorter answer's one", len(content.Points))
	}
}

// A call that failed is a call that was spent, and the batch it belonged to
// wrote nothing.
//
// The failure is the provider's, not the program's: the model was asked and
// the answer never came. The cost of the call is still counted, and the batch
// still ends without a segment.
func TestTheFailedCallIsCountedAndTheBatchWroteNothing(t *testing.T) {
	upstream := newStandInUpstream(t, []standInAnswer{
		{status: http.StatusInternalServerError},
	})
	sessionID := "producer-the-call-failed"
	store := producerStore(t, sessionID)
	produced := producer(t, producerChat(t, upstream), store)

	if _, err := produced.ProduceSegment(context.Background(), &model.SessionSummarySegmentJob{
		UserID: 7001, SessionID: sessionID,
		StartMessageID: 1, BatchMessageID: 10, DesiredMessageID: 10,
	}); err == nil {
		t.Fatal("a batch whose call failed was written")
	}
	if upstream.count() != 1 {
		t.Fatalf("the provider was asked %d times, want 1", upstream.count())
	}
	if store.writes != 0 {
		t.Fatalf("the producer wrote %d times although the call failed", store.writes)
	}
	if covered := store.covered(context.Background(), sessionID); covered != 0 {
		t.Fatalf("the board reaches %d although the call failed", covered)
	}
}
