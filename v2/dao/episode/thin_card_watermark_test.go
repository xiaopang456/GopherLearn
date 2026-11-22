package episode

import (
	"context"
	"errors"
	"testing"
	"time"

	"GopherAI/model"

	"github.com/google/uuid"
)

// The watermark rule, and the rows it does not apply to.
//
// The thin card may be rewritten at the same message watermark. A new summary
// segment can land without the session having moved - the segment summarizes
// turns the card has already been told about - and the segment's topic, names
// and number are exactly what the card is for. A card that refused the update
// would keep pointing at the session's older state.
//
// Every other row keeps the strict rule, and the version check is kept for
// every row: a stale writer still loses, and a watermark still may not go
// backwards.

// thinCardRow is a thin card row at the given watermark.
func thinCardRow(userID int64, sessionID string, watermark uint, seq int) *model.EpisodeMemory {
	document := model.SessionCardDocument{
		Version:     model.SessionThinCardContentVersion,
		Title:       "缓存池的调参",
		SegmentSeqs: []int{seq},
		Topics:      []model.EpisodeTopic{{Key: "缓存", Label: "缓存"}},
	}
	return &model.EpisodeMemory{
		ID: uuid.NewString(), UserID: userID, SessionID: sessionID,
		ContentKind: model.EpisodeContentSessionCard, Title: document.Title,
		Card: document, SearchText: "缓存池的调参\n缓存\n缓存",
		TopicKeys:           model.StringList{"缓存"},
		SourceLastMessageID: watermark, LastMessageAt: time.Now().UTC(),
		Status: model.EpisodeStatusActive, IndexStatus: model.EpisodeIndexPending,
	}
}

// A thin card is rewritten at the same watermark when a new summary landed.
func TestTheThinCardMayBeRewrittenAtTheSameWatermark(t *testing.T) {
	db := openEpisodeTestDB(t)
	owner, session := createEpisodeOwner(t, db, "thin-watermark")
	repository := NewRepository(db)
	ctx := context.Background()

	card := thinCardRow(owner.ID, session.ID, 40, 1)
	if err := repository.Save(ctx, card, 0); err != nil {
		t.Fatalf("write the thin card: %v", err)
	}
	// The same watermark, a new segment: the card's clues move.
	next := thinCardRow(owner.ID, session.ID, 40, 2)
	next.ID = card.ID
	next.Card.SegmentSeqs = []int{1, 2}
	next.Card.Topics = []model.EpisodeTopic{{Key: "缓存", Label: "缓存"}, {Key: "调参", Label: "调参"}}
	next.TopicKeys = model.StringList{"缓存", "调参"}
	next.SearchText = "缓存池的调参\n缓存\n缓存\n调参\n调参"
	if err := repository.Save(ctx, next, card.Version); err != nil {
		t.Fatalf("the thin card was refused at the same watermark: %v", err)
	}
	stored, err := repository.GetBySessionKind(ctx, owner.ID, session.ID, model.EpisodeContentSessionCard)
	if err != nil {
		t.Fatalf("read the card back: %v", err)
	}
	if stored.Version != card.Version+1 {
		t.Fatalf("the card's version is %d, want %d", stored.Version, card.Version+1)
	}
	if len(stored.Card.SegmentSeqs) != 2 {
		t.Fatalf("the card's segment refs are %v, want both", stored.Card.SegmentSeqs)
	}
}

// A watermark that goes BACKWARDS is still refused, thin card or not.
//
// The relaxation is about a watermark that stands still. A card that reaches
// further back than the row does would replace a wider reach with a narrower
// one, and the read that trusts the watermark would be reading past what the
// card actually covers.
func TestAThinCardMayNotGoBackwards(t *testing.T) {
	db := openEpisodeTestDB(t)
	owner, session := createEpisodeOwner(t, db, "thin-backwards")
	repository := NewRepository(db)
	ctx := context.Background()

	card := thinCardRow(owner.ID, session.ID, 40, 1)
	if err := repository.Save(ctx, card, 0); err != nil {
		t.Fatalf("write the thin card: %v", err)
	}
	backwards := thinCardRow(owner.ID, session.ID, 20, 1)
	backwards.ID = card.ID
	if err := repository.Save(ctx, backwards, card.Version); !errors.Is(err, ErrEpisodeConflict) {
		t.Fatalf("a backwards watermark was accepted: %v", err)
	}
}

// A stale writer still loses, even on a thin card.
//
// The relaxation is about the watermark, not about the version: a writer that
// read the card before somebody else changed it must not overwrite the newer
// one.
func TestAStaleWriterStillLosesOnAThinCard(t *testing.T) {
	db := openEpisodeTestDB(t)
	owner, session := createEpisodeOwner(t, db, "thin-stale")
	repository := NewRepository(db)
	ctx := context.Background()

	card := thinCardRow(owner.ID, session.ID, 40, 1)
	if err := repository.Save(ctx, card, 0); err != nil {
		t.Fatalf("write the thin card: %v", err)
	}
	next := thinCardRow(owner.ID, session.ID, 40, 2)
	next.ID = card.ID
	if err := repository.Save(ctx, next, card.Version); err != nil {
		t.Fatalf("write the second version: %v", err)
	}
	stale := thinCardRow(owner.ID, session.ID, 40, 3)
	stale.ID = card.ID
	if err := repository.Save(ctx, stale, card.Version); !errors.Is(err, ErrEpisodeConflict) {
		t.Fatalf("a stale writer was accepted: %v", err)
	}
}

// The Episode row still refuses a write at the same watermark.
//
// The relaxation belongs to the thin card and to nothing else. An Episode is
// a projection of the conversation as far as a reader read it, and a second
// projection of the same reach is a different projection: the rule that kept
// it from replacing a wider reach with a narrower one is unchanged.
func TestTheEpisodeRowStillRefusesAWriteAtTheSameWatermark(t *testing.T) {
	db := openEpisodeTestDB(t)
	owner, session := createEpisodeOwner(t, db, "episode-watermark")
	repository := NewRepository(db)
	ctx := context.Background()

	episode := &model.EpisodeMemory{
		ID: uuid.NewString(), UserID: owner.ID, SessionID: session.ID, Title: "RPC",
		Document: model.EpisodeDocument{
			Synopsis: "讨论粘包",
			Topics:   []model.EpisodeTopic{{Key: "粘包", Label: "粘包"}},
			Decisions: []model.EpisodeEvidence{
				{Text: "使用长度字段", EvidenceMessageIDs: []uint{1}}},
			SourceHandles: []string{"session:" + session.ID, "message:1"},
		},
		SearchText:          "讨论粘包\n粘包\n粘包\n使用长度字段",
		TopicKeys:           model.StringList{"粘包"},
		SourceLastMessageID: 40, LastMessageAt: time.Now().UTC(),
		Status: model.EpisodeStatusActive, IndexStatus: model.EpisodeIndexPending,
	}
	if err := repository.Save(ctx, episode, 0); err != nil {
		t.Fatalf("write the episode: %v", err)
	}
	again := *episode
	again.Document.Synopsis = "讨论粘包和拆包"
	again.SearchText = "讨论粘包和拆包"
	if err := repository.Save(ctx, &again, episode.Version); !errors.Is(err, ErrEpisodeConflict) {
		t.Fatalf("the episode was rewritten at the same watermark: %v", err)
	}
}

// The full card still refuses a write at the same watermark.
//
// The full card asks a model to compress the conversation, and a repeat at the
// same reach would spend that call again for a card that says the same thing.
func TestTheFullCardStillRefusesAWriteAtTheSameWatermark(t *testing.T) {
	db := openEpisodeTestDB(t)
	owner, session := createEpisodeOwner(t, db, "card-watermark")
	repository := NewRepository(db)
	ctx := context.Background()

	document := model.SessionCardDocument{
		Version: model.SessionCardContentVersion, Title: "满卡片", Synopsis: "讨论粘包",
		Topics: []model.EpisodeTopic{{Key: "粘包", Label: "粘包"}},
	}
	card := &model.EpisodeMemory{
		ID: uuid.NewString(), UserID: owner.ID, SessionID: session.ID,
		ContentKind: model.EpisodeContentSessionCard, Title: document.Title,
		Card: document, SearchText: "满卡片\n粘包\n粘包",
		TopicKeys:           model.StringList{"粘包"},
		SourceLastMessageID: 40, LastMessageAt: time.Now().UTC(),
		Status: model.EpisodeStatusActive, IndexStatus: model.EpisodeIndexPending,
	}
	if err := repository.Save(ctx, card, 0); err != nil {
		t.Fatalf("write the card: %v", err)
	}
	again := *card
	again.Card.Synopsis = "讨论粘包和拆包"
	if err := repository.Save(ctx, &again, card.Version); !errors.Is(err, ErrEpisodeConflict) {
		t.Fatalf("the full card was rewritten at the same watermark: %v", err)
	}
}
