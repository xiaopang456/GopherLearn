package episode

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"GopherAI/model"
)

// The indexer's own tests.
//
// The claim they defend is the one the background path rests on: a card that
// the index does not hold is a card nobody can find, and the row has to say
// so when that happens.

// fakeThinCardWriter is the index the indexer writes into.
type fakeThinCardWriter struct {
	stored []model.EpisodeMemory
	err    error
}

func (f *fakeThinCardWriter) Store(_ context.Context, card model.EpisodeMemory) error {
	if f.err != nil {
		return f.err
	}
	f.stored = append(f.stored, card)
	return nil
}

// fakeThinCardStatus is the row the indexer records its outcome on.
type fakeThinCardStatus struct {
	statuses []model.EpisodeIndexStatus
	err      error
}

func (f *fakeThinCardStatus) UpdateIndexStatus(_ context.Context, _ int64, _ string,
	_ uint64, status model.EpisodeIndexStatus) error {
	if f.err != nil {
		return f.err
	}
	f.statuses = append(f.statuses, status)
	return nil
}

// indexedCard is a card the index already holds.
func indexedCard() *model.EpisodeMemory {
	document, err := ProgrammaticThinCardDeriver{}.DeriveThinCard(context.Background(), thinCardSource())
	if err != nil {
		panic(err)
	}
	return &model.EpisodeMemory{
		ID: "card-1", UserID: 7, SessionID: "session-1", Version: 3,
		Title: document.Title, Card: document,
		SearchText: ThinCardSearchText(document),
		Status:     model.EpisodeStatusActive, IndexStatus: model.EpisodeIndexIndexed,
	}
}

// A card the row still calls pending is written into the index even when its
// words did not change.
//
// This is the retry. An index write that failed leaves the row pending, and
// the next attempt finds the card exactly as it was: same words, same
// version. A rule that only looked at the words would say "nothing changed"
// and leave the card out of the index for good.
func TestACardThatIsStillPendingIsWrittenIntoTheIndex(t *testing.T) {
	writer := &fakeThinCardWriter{}
	status := &fakeThinCardStatus{}
	indexer, err := NewThinCardIndexer(writer, status)
	if err != nil {
		t.Fatal(err)
	}
	card := indexedCard()
	card.IndexStatus = model.EpisodeIndexPending
	if err := indexer.Index(context.Background(), card, card); err != nil {
		t.Fatalf("index the pending card: %v", err)
	}
	if len(writer.stored) != 1 {
		t.Fatalf("the pending card was written %d times, want once", len(writer.stored))
	}
	if len(status.statuses) != 1 || status.statuses[0] != model.EpisodeIndexIndexed {
		t.Fatalf("the row's statuses are %v, want one indexed", status.statuses)
	}
}

// A failed index write is recorded on the row and returned.
//
// The row is the diagnosis: the failure has to be visible in the store and
// not only in a log line, because the row's status is what tells the next
// attempt that there is something to make good.
func TestAFailedIndexWriteIsRecordedOnTheRowAndReturned(t *testing.T) {
	writer := &fakeThinCardWriter{err: errors.New("redis is down")}
	status := &fakeThinCardStatus{}
	indexer, err := NewThinCardIndexer(writer, status)
	if err != nil {
		t.Fatal(err)
	}
	card := indexedCard()
	card.IndexStatus = model.EpisodeIndexPending
	err = indexer.Index(context.Background(), card, nil)
	if err == nil {
		t.Fatal("a failed index write was reported as a success")
	}
	if !errors.Is(err, ErrThinCardIndex) {
		t.Fatalf("the error is %v, want it to name the index stage", err)
	}
	if len(status.statuses) != 1 || status.statuses[0] != model.EpisodeIndexFailed {
		t.Fatalf("the row's statuses are %v, want one failed", status.statuses)
	}
	if card.IndexStatus != model.EpisodeIndexFailed {
		t.Fatalf("the card's status is %q, want failed", card.IndexStatus)
	}
}

// A card the index already holds is not written again.
//
// The index is a shared store that every other attempt is also writing to,
// and a card that did not change has not earned a write.
func TestACardThatIsAlreadyIndexedIsNotWrittenAgain(t *testing.T) {
	writer := &fakeThinCardWriter{}
	status := &fakeThinCardStatus{}
	indexer, err := NewThinCardIndexer(writer, status)
	if err != nil {
		t.Fatal(err)
	}
	card := indexedCard()
	if err := indexer.Index(context.Background(), card, card); err != nil {
		t.Fatalf("index the unchanged card: %v", err)
	}
	if len(writer.stored) != 0 {
		t.Fatalf("an unchanged card was written into the index %d times", len(writer.stored))
	}
	if len(status.statuses) != 0 {
		t.Fatalf("an unchanged card wrote its status %v", status.statuses)
	}
}

// A card whose words changed is written even when the row claims it is
// indexed.
//
// The row's status is about the index, and the words are about the card. A
// card that says something new has to be in the index under its new words.
func TestACardWhoseWordsChangedIsWrittenAgain(t *testing.T) {
	writer := &fakeThinCardWriter{}
	status := &fakeThinCardStatus{}
	indexer, err := NewThinCardIndexer(writer, status)
	if err != nil {
		t.Fatal(err)
	}
	card := indexedCard()
	previous := *card
	card.Card.Title = "新的标题"
	card.Title = "新的标题"
	card.SearchText = ThinCardSearchText(card.Card)
	if err := indexer.Index(context.Background(), card, &previous); err != nil {
		t.Fatalf("index the renamed card: %v", err)
	}
	if len(writer.stored) != 1 {
		t.Fatalf("a renamed card was written %d times, want once", len(writer.stored))
	}
}

// A card that is not active is not in the index, and the row says so.
func TestACardThatIsNotActiveIsNotInTheIndex(t *testing.T) {
	writer := &fakeThinCardWriter{}
	status := &fakeThinCardStatus{}
	indexer, err := NewThinCardIndexer(writer, status)
	if err != nil {
		t.Fatal(err)
	}
	card := indexedCard()
	card.Status = model.EpisodeStatusAbstained
	card.IndexStatus = model.EpisodeIndexPending
	if err := indexer.Index(context.Background(), card, nil); err != nil {
		t.Fatalf("index the inactive card: %v", err)
	}
	if len(writer.stored) != 0 {
		t.Fatalf("an inactive card was written into the index %d times", len(writer.stored))
	}
	if len(status.statuses) != 1 || status.statuses[0] != model.EpisodeIndexFailed {
		t.Fatalf("the row's statuses are %v, want one failed", status.statuses)
	}
}

// The job row says which stage failed.
//
// A thin card that is not in the index is a card nobody can find, and a job
// recorded as "projection_failed" would send whoever reads it to the
// derivation - the one stage that worked.
func TestTheJobRowNamesTheThinCardIndexStage(t *testing.T) {
	wrapped := fmt.Errorf("%w: redis is down", ErrThinCardIndex)
	if code := projectionErrorCode(wrapped); code != "thin_card_index_failed" {
		t.Fatalf("the error code is %q, want thin_card_index_failed", code)
	}
	if code := projectionErrorCode(errors.New("something else")); code != "projection_failed" {
		t.Fatalf("the error code is %q, want projection_failed", code)
	}
}

// The status write outlives the caller's context.
//
// The outcome is the diagnosis, and a diagnosis lost because the request was
// cancelled is the one that is needed most.
func TestTheIndexStatusIsRecordedEvenWhenTheCallerGaveUp(t *testing.T) {
	writer := &fakeThinCardWriter{}
	status := &fakeThinCardStatus{}
	indexer, err := NewThinCardIndexer(writer, status)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	card := indexedCard()
	card.IndexStatus = model.EpisodeIndexPending
	_ = indexer.Index(ctx, card, nil)
	if len(status.statuses) != 1 || status.statuses[0] != model.EpisodeIndexIndexed {
		t.Fatalf("the row's statuses are %v, want one indexed", status.statuses)
	}
}
