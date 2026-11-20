package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"GopherAI/model"
)

// The database side of the session lock.
//
// The two claims it defends are the ones the learner's click rests on: a
// conversation that is not theirs is not read, and the conversation that IS
// theirs brings its own record with it.

// fakeCrossStore is the store the reader reads from.
type fakeCrossStore struct {
	conversations map[string]*model.Session
	segments      map[string][]model.SessionSummarySegment
	words         map[string][]model.Message
	ownershipErr  error
}

func (f *fakeCrossStore) GetConversation(_ context.Context, userID int64,
	sessionID string) (*model.Session, error) {
	row, ok := f.conversations[sessionID]
	if !ok || row.UserID != userID {
		return nil, ErrCrossSessionNotFound
	}
	return row, nil
}

func (f *fakeCrossStore) ConversationOwnership(_ context.Context, userID int64,
	sessionID string) (bool, bool, error) {
	if f.ownershipErr != nil {
		return false, false, f.ownershipErr
	}
	row, ok := f.conversations[sessionID]
	if !ok {
		return false, false, nil
	}
	return true, row.UserID == userID, nil
}

func (f *fakeCrossStore) ListSegments(_ context.Context, _ int64, sessionID string,
	_ int) ([]model.SessionSummarySegment, error) {
	return f.segments[sessionID], nil
}

func (f *fakeCrossStore) ListWords(_ context.Context, _ int64, sessionID string,
	_ int) ([]model.Message, error) {
	return f.words[sessionID], nil
}

// A conversation of somebody else is refused, and nothing of it is read.
//
// The ownership check comes FIRST: a conversation that is not the learner's
// is refused before a single word of it is looked at.
func TestTheReaderRefusesAConversationOfSomebodyElse(t *testing.T) {
	store := &fakeCrossStore{
		conversations: map[string]*model.Session{
			"session-x": {ID: "session-x", UserID: 99, Title: "别人的会话"},
		},
		segments: map[string][]model.SessionSummarySegment{
			"session-x": {{SessionID: "session-x", Seq: 1, Content: `{"schema_version":1,"topic":"不该被读到","points":[{"text":"不该被读到的内容"}]}`}},
		},
	}
	reader, err := NewDatabaseCrossSessionReader(store)
	if err != nil {
		t.Fatal(err)
	}
	locked, err := reader.LockSession(context.Background(), 7, "session-x")
	if err == nil {
		t.Fatalf("a conversation of somebody else was locked: %+v", locked)
	}
	if !errors.Is(err, ErrSessionNotOwned) {
		t.Fatalf("the error is %v, want it to name the ownership boundary", err)
	}
	if locked != nil {
		t.Fatalf("the reader returned a conversation of somebody else: %+v", locked)
	}
}

// A conversation that is not there at all is not the same refusal.
//
// "There is no such conversation" and "that is somebody else's conversation"
// are different facts, and only the second one says that somebody reached
// across a boundary.
func TestTheReaderTellsAMissingConversationFromSomebodyElses(t *testing.T) {
	store := &fakeCrossStore{conversations: map[string]*model.Session{}}
	reader, err := NewDatabaseCrossSessionReader(store)
	if err != nil {
		t.Fatal(err)
	}
	_, err = reader.LockSession(context.Background(), 7, "session-missing")
	if err == nil {
		t.Fatal("a missing conversation was locked")
	}
	if errors.Is(err, ErrSessionNotOwned) {
		t.Fatalf("a missing conversation was reported as somebody else's: %v", err)
	}
	if !errors.Is(err, ErrCrossSessionNotFound) {
		t.Fatalf("the error is %v, want it to name the missing conversation", err)
	}
}

// A conversation of the learner's brings its own summary.
func TestTheReaderBringsTheSummaryOfTheLockedConversation(t *testing.T) {
	content, err := json.Marshal(model.SessionSummarySegmentContent{
		SchemaVersion: model.SessionSummarySegmentSchemaV1,
		Topic:         "三道题",
		Points: []model.SessionSummarySegmentPoint{
			{Text: "第二道练的是读写拆分", EvidenceMessageIDs: []uint{3}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeCrossStore{
		conversations: map[string]*model.Session{
			"session-a": {ID: "session-a", UserID: 7, Title: "洄漪"},
		},
		segments: map[string][]model.SessionSummarySegment{
			"session-a": {{SessionID: "session-a", Seq: 1, Content: string(content)}},
		},
	}
	reader, err := NewDatabaseCrossSessionReader(store)
	if err != nil {
		t.Fatal(err)
	}
	locked, err := reader.LockSession(context.Background(), 7, "session-a")
	if err != nil {
		t.Fatalf("lock the conversation: %v", err)
	}
	if locked == nil || locked.Title != "洄漪" {
		t.Fatalf("the lock is %+v", locked)
	}
	if locked.Segments != 1 {
		t.Fatalf("the lock carries %d segments, want one", locked.Segments)
	}
	if locked.Evidence == "" {
		t.Fatal("the lock carries no evidence")
	}
	if locked.Messages != 0 {
		t.Fatalf("the lock carries %d messages, want none", locked.Messages)
	}
}

// A conversation with no summary brings its own words.
//
// The body of a conversation that was never summarized is the conversation
// itself, and reading nothing would leave the question unanswerable.
func TestTheReaderFallsBackToTheConversationsOwnWords(t *testing.T) {
	store := &fakeCrossStore{
		conversations: map[string]*model.Session{
			"session-b": {ID: "session-b", UserID: 7, Title: "浔漳"},
		},
		words: map[string][]model.Message{
			"session-b": {
				{Content: "第二道练的是什么", IsUser: true},
				{Content: "第二道练读写拆分", IsUser: false},
			},
		},
	}
	reader, err := NewDatabaseCrossSessionReader(store)
	if err != nil {
		t.Fatal(err)
	}
	locked, err := reader.LockSession(context.Background(), 7, "session-b")
	if err != nil {
		t.Fatalf("lock the conversation: %v", err)
	}
	if locked == nil || locked.Messages != 2 {
		t.Fatalf("the lock is %+v", locked)
	}
	if locked.Evidence == "" {
		t.Fatal("the lock carries no evidence")
	}
}
