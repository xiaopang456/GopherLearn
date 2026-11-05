package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"GopherAI/model"

	"github.com/google/uuid"
)

// The worker's correctness rests on the database behaving in three ways:
// a segment and its board land together or not at all, the watermark says
// what is already covered, and a session that fell behind is findable. These
// tests run against a real database, because none of the three is a property
// the in-memory fakes can show.

func layerSegmentFixture(t *testing.T, fixture *layerFixture, count int) []model.Message {
	t.Helper()
	messages := make([]model.Message, 0, count)
	for index := 0; index < count; index++ {
		message := model.Message{
			SessionID: fixture.session.ID,
			// The column holds twenty characters, and the fixture's own name
			// is longer than that.
			UserName: "layer",
			Content:  "turn",
			IsUser:   index%2 == 0,
		}
		if err := fixture.db.Create(&message).Error; err != nil {
			t.Fatal(err)
		}
		messages = append(messages, message)
	}
	return messages
}

// The segment and the board claim the same stretch of the conversation, so
// they are written as one unit. A board that landed without its segment would
// answer for turns no segment records.
func TestTheSegmentAndItsBoardLandTogether(t *testing.T) {
	fixture := newLayerFixture(t)
	ctx := context.Background()
	messages := layerSegmentFixture(t, fixture, 2)

	segment := &model.SessionSummarySegment{
		UserID: fixture.user.ID, SessionID: fixture.session.ID, Seq: 1,
		StartMessageID: messages[0].ID, EndMessageID: messages[1].ID,
		StartAt: time.Now(), EndAt: time.Now(),
		Topic:   "t",
		Content: `{"schema_version":1,"topic":"t","points":[{"text":"p","evidence_message_ids":[1]}]}`,
	}
	board := &model.SessionWhiteboard{
		UserID: fixture.user.ID, SessionID: fixture.session.ID,
		Content: `{"schema_version":1}`, CoveredMessageID: messages[1].ID,
	}
	if err := fixture.repo.SaveSummarySegmentAndWhiteboard(ctx, segment, board, 0); err != nil {
		t.Fatal(err)
	}
	stored, err := fixture.repo.GetWhiteboard(ctx, fixture.user.ID, fixture.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Version != 1 || stored.CoveredMessageID != messages[1].ID {
		t.Fatalf("stored board=%+v", stored)
	}
	segments, err := fixture.repo.ListSummarySegments(ctx, fixture.user.ID, fixture.session.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 1 {
		t.Fatalf("segments=%+v", segments)
	}
}

// When the board moved on, the segment must not land either. A half-written
// pair would claim coverage that no segment backs.
func TestASegmentIsNotWrittenWhenTheBoardMovedOn(t *testing.T) {
	fixture := newLayerFixture(t)
	ctx := context.Background()
	messages := layerSegmentFixture(t, fixture, 3)

	first := &model.SessionWhiteboard{
		UserID: fixture.user.ID, SessionID: fixture.session.ID,
		Content: `{"schema_version":1}`, CoveredMessageID: messages[0].ID,
	}
	if err := fixture.repo.SaveWhiteboard(ctx, first, 0); err != nil {
		t.Fatal(err)
	}
	// The board moves on without this writer.
	moved := &model.SessionWhiteboard{
		UserID: fixture.user.ID, SessionID: fixture.session.ID,
		Content: `{"schema_version":1}`, CoveredMessageID: messages[1].ID,
	}
	if err := fixture.repo.SaveWhiteboard(ctx, moved, 1); err != nil {
		t.Fatal(err)
	}
	segment := &model.SessionSummarySegment{
		UserID: fixture.user.ID, SessionID: fixture.session.ID, Seq: 1,
		StartMessageID: messages[0].ID, EndMessageID: messages[2].ID,
		StartAt: time.Now(), EndAt: time.Now(),
		Topic:   "t",
		Content: `{"schema_version":1,"topic":"t","points":[{"text":"p","evidence_message_ids":[1]}]}`,
	}
	board := &model.SessionWhiteboard{
		UserID: fixture.user.ID, SessionID: fixture.session.ID,
		Content: `{"schema_version":1}`, CoveredMessageID: messages[2].ID,
	}
	err := fixture.repo.SaveSummarySegmentAndWhiteboard(ctx, segment, board, 1)
	if !errors.Is(err, ErrWhiteboardConflict) {
		t.Fatalf("the write returned %v, want a version conflict", err)
	}
	segments, err := fixture.repo.ListSummarySegments(ctx, fixture.user.ID, fixture.session.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 0 {
		t.Fatalf("the segment landed even though the board moved on: %+v", segments)
	}
}

// The watermark says what the segments already cover. It is what the next
// segment starts after, and what a retry keeps starting at.
func TestTheWatermarkSaysWhatTheSegmentsCover(t *testing.T) {
	fixture := newLayerFixture(t)
	ctx := context.Background()
	messages := layerSegmentFixture(t, fixture, 4)

	watermark, seq, err := fixture.repo.GetSummarySegmentWatermark(ctx, fixture.user.ID, fixture.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if watermark != 0 || seq != 0 {
		t.Fatalf("an empty session reads watermark=%d seq=%d", watermark, seq)
	}
	segment := &model.SessionSummarySegment{
		UserID: fixture.user.ID, SessionID: fixture.session.ID, Seq: 1,
		StartMessageID: messages[0].ID, EndMessageID: messages[1].ID,
		StartAt: time.Now(), EndAt: time.Now(),
		Topic:   "t",
		Content: `{"schema_version":1,"topic":"t","points":[{"text":"p","evidence_message_ids":[1]}]}`,
	}
	if err := fixture.repo.SaveSummarySegment(ctx, segment); err != nil {
		t.Fatal(err)
	}
	watermark, seq, err = fixture.repo.GetSummarySegmentWatermark(ctx, fixture.user.ID, fixture.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if watermark != messages[1].ID || seq != 1 {
		t.Fatalf("watermark=%d seq=%d", watermark, seq)
	}
}

// A session whose latest turn is not covered yet is a gap, and the gap
// query is what the reconciler uses to find it.
func TestTheGapQueryFindsTheSessionsThatOweASegment(t *testing.T) {
	fixture := newLayerFixture(t)
	ctx := context.Background()
	messages := layerSegmentFixture(t, fixture, 3)
	// The last turn has to be the assistant's for the conversation to count
	// as finished.
	assistant := messages[len(messages)-1]
	assistant.IsUser = false
	if err := fixture.db.Save(&assistant).Error; err != nil {
		t.Fatal(err)
	}

	gaps, err := fixture.repo.ListSummarySegmentGaps(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(gaps) != 1 || gaps[0].SessionID != fixture.session.ID {
		t.Fatalf("gaps=%+v", gaps)
	}
	if gaps[0].LatestMessageID != assistant.ID {
		t.Fatalf("the gap names %d, want %d", gaps[0].LatestMessageID, assistant.ID)
	}

	// Once a segment covers it, the gap is gone.
	segment := &model.SessionSummarySegment{
		UserID: fixture.user.ID, SessionID: fixture.session.ID, Seq: 1,
		StartMessageID: messages[0].ID, EndMessageID: assistant.ID,
		StartAt: time.Now(), EndAt: time.Now(),
		Topic:   "t",
		Content: `{"schema_version":1,"topic":"t","points":[{"text":"p","evidence_message_ids":[1]}]}`,
	}
	if err := fixture.repo.SaveSummarySegment(ctx, segment); err != nil {
		t.Fatal(err)
	}
	gaps, err = fixture.repo.ListSummarySegmentGaps(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(gaps) != 0 {
		t.Fatalf("the gap was not closed: %+v", gaps)
	}
}

// A job that is due now must be claimable right away.
//
// The column holds milliseconds and MySQL rounds what it is given, so a
// timestamp written as 12:00:00.6005 comes back as 12:00:00.601. A claim
// that compares the stored value against the original nanosecond value then
// decides the job is not due yet, and refuses it. The failure was
// intermittent - roughly every other run - because it depended on where the
// clock's fractional millisecond happened to land.
func TestAJobThatIsDueNowIsClaimableRightAway(t *testing.T) {
	fixture := newLayerFixture(t)
	ctx := context.Background()
	for attempt := 0; attempt < 20; attempt++ {
		// Each attempt gets its own session, because the attempt counter on
		// the job carries over and would otherwise run the job out of tries
		// before the rounding has had a chance to show itself.
		session := model.Session{
			ID: uuid.NewString(), UserID: fixture.user.ID,
			UserName: "layer", Mode: model.SessionModeAgent, Title: "due",
		}
		if err := fixture.db.Create(&session).Error; err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			fixture.db.Unscoped().Where("session_id = ?", session.ID).Delete(&model.SessionSummarySegmentJob{})
			fixture.db.Unscoped().Delete(&session)
		})
		// A fractional millisecond above the halfway point, which is the
		// case MySQL rounds away from zero.
		now := time.Now().Truncate(time.Second).Add(600500 * time.Microsecond)
		job := &model.SessionSummarySegmentJob{
			UserID: fixture.user.ID, SessionID: session.ID,
			DesiredMessageID: 20, StartMessageID: 5,
			Status: model.SessionSummarySegmentJobPending, AvailableAt: now,
		}
		if err := fixture.repo.UpsertSummarySegmentJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		claimed, err := fixture.repo.ClaimSummarySegmentJob(ctx, fixture.user.ID, job.ID, now, time.Minute, 3)
		if err != nil {
			t.Fatalf("a job that was due was refused on attempt %d: %v", attempt, err)
		}
		if err := fixture.repo.CompleteSummarySegmentJob(ctx, claimed, now); err != nil {
			t.Fatal(err)
		}
	}
}

// The batch read is bounded by the session the caller owns. Another account's
// turns are absent from the result rather than reported.
func TestTheBatchReadDoesNotCrossTheAccountBoundary(t *testing.T) {
	fixture := newLayerFixture(t)
	ctx := context.Background()
	messages := layerSegmentFixture(t, fixture, 2)

	read, err := fixture.repo.ListSummarySegmentMessages(ctx, fixture.user.ID, fixture.session.ID,
		messages[0].ID, messages[1].ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(read) != 2 {
		t.Fatalf("read %d turns, want 2", len(read))
	}
	foreign, err := fixture.repo.ListSummarySegmentMessages(ctx, fixture.other.ID, fixture.session.ID,
		messages[0].ID, messages[1].ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(foreign) != 0 {
		t.Fatalf("another account's turns were returned: %+v", foreign)
	}
}
