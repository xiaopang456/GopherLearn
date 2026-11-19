package episode

import (
	"GopherAI/model"
	"context"
	"errors"
	"fmt"
	"strings"

	episodedao "GopherAI/dao/episode"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// The thin card's source.
//
// The loader reads what the program already has - the session's title, the
// time of its last message, the topic and the names its summary segments
// already recorded - and hands it to the deriver. It reads NO raw messages
// and it asks NO model for anything.
//
// The difference from the full card's loader is the point of the whole
// exercise: a full card reads the conversation and asks a model to compress
// it, and a thin card reads what is already compressed and names where it
// is.

// ThinCardSourceLoader loads the source a thin card is derived from.
type ThinCardSourceLoader interface {
	LoadThinCardSource(context.Context, int64, string) (ThinCardSource, error)
}

// DatabaseThinCardSourceLoader reads the source from MySQL.
type DatabaseThinCardSourceLoader struct {
	db *gorm.DB
}

// NewDatabaseThinCardSourceLoader builds the loader.
func NewDatabaseThinCardSourceLoader(db *gorm.DB) (*DatabaseThinCardSourceLoader, error) {
	if db == nil {
		return nil, fmt.Errorf("thin card source database is required")
	}
	return &DatabaseThinCardSourceLoader{db: db}, nil
}

// LoadThinCardSource reads one session's thin card source.
//
// It reads three things and no more: the session row, the time of its last
// message, and the summary segments it has. The segments are the only place
// the card's clues come from, and they were written by the summary producer
// - which is why the card's topics and names are the learner's own words and
// not a model's paraphrase of them.
//
// A session with no messages has no card yet: nothing has happened in it, and
// a card for a session that did not happen is a card about nothing.
func (l *DatabaseThinCardSourceLoader) LoadThinCardSource(ctx context.Context,
	userID int64, sessionID string) (ThinCardSource, error) {
	sessionID = strings.TrimSpace(sessionID)
	if l == nil || l.db == nil || userID <= 0 || sessionID == "" {
		return ThinCardSource{}, ErrCardSourceNotFound
	}
	var session model.Session
	if err := l.db.WithContext(ctx).
		Where("id = ? AND user_id = ?", sessionID, userID).
		First(&session).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ThinCardSource{}, ErrCardSourceNotFound
		}
		return ThinCardSource{}, err
	}
	// The last time the session was talked in is the time of its last
	// message. The session row's own updated_at is not that: a session can be
	// touched without a message landing in it, and a card that says a session
	// was last talked in when it was not is worse than one that says nothing.
	var lastMessage model.Message
	if err := l.db.WithContext(ctx).
		Where("session_id = ? AND id > 0", sessionID).
		Order("id DESC").Limit(1).
		First(&lastMessage).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ThinCardSource{}, ErrCardSourceNotFound
		}
		return ThinCardSource{}, err
	}
	source := ThinCardSource{
		SessionID:     sessionID,
		Title:         session.Title,
		LastTalkedAt:  lastMessage.CreatedAt,
		LastMessageID: lastMessage.ID,
	}
	// The segments. They are read whole and in order, because the card names
	// them all: a card that pointed at half of a session's segments would
	// send the reader to a place that does not hold the whole session.
	var segments []model.SessionSummarySegment
	if err := l.db.WithContext(ctx).
		Where("user_id = ? AND session_id = ?", userID, sessionID).
		Order("seq ASC").Limit(512).Find(&segments).Error; err != nil {
		return ThinCardSource{}, err
	}
	var topics, names []string
	for _, segment := range segments {
		source.SegmentSeqs = append(source.SegmentSeqs, segment.Seq)
		content, decodeErr := model.DecodeSessionSummarySegmentContent(segment.Content)
		if decodeErr != nil {
			// A segment that cannot be read is left out. The card is a
			// finding aid, and a ref to a segment nobody can read is a ref
			// to nothing - but the segments around it are still true.
			continue
		}
		if topic := strings.TrimSpace(content.Topic); topic != "" {
			topics = append(topics, topic)
		}
		names = append(names, segmentNames(content)...)
	}
	source.Topics = topics
	source.Names = names
	return source, nil
}

// segmentNames reads the names a segment's content recorded.
//
// The names live in the segment's own content, and the row carries the
// merged form of them as well. The content is read first because it is the
// structured one: the row's column is a single string that holds both names
// and numbers, and a card that carried a number as a name would put a
// number in the learner's own words.
func segmentNames(content model.SessionSummarySegmentContent) []string {
	out := make([]string, 0, len(content.Names))
	for _, name := range content.Names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		out = append(out, name)
	}
	return out
}

// ThinCardProjector writes the thin card of one session.
//
// It is the CardProjector with a different generator: the same worker, the
// same job, the same lease, the same optimistic versioning - and no model call
// at all.
//
// The write is skipped when the card already says the same thing, and the
// index is only touched when the searchable text changed. Both are the same
// rule: the card is written once and then kept current, not rewritten.
type ThinCardProjector struct {
	loader     ThinCardSourceLoader
	deriver    ThinCardDeriver
	repository CardProjectionRepository
}

// NewThinCardProjector builds the projector.
func NewThinCardProjector(loader ThinCardSourceLoader, deriver ThinCardDeriver,
	repository CardProjectionRepository) (*ThinCardProjector, error) {
	if loader == nil || deriver == nil || repository == nil {
		return nil, fmt.Errorf("thin card projector dependencies are required")
	}
	return &ThinCardProjector{loader: loader, deriver: deriver, repository: repository}, nil
}

// Project writes the thin card of one session.
//
// The target is the session's own last message: a thin card is about the
// session as it is, and the target only says how far the card's own reach
// goes. It is read from the source rather than claimed by the caller.
//
// It returns the card it wrote and the card that was there before. The
// previous version is what says whether the searchable text changed, and
// therefore whether the index has to be written at all.
func (p *ThinCardProjector) Project(ctx context.Context, userID int64, sessionID string, targetMessageID uint) (*model.EpisodeMemory, *model.EpisodeMemory, error) {
	if p == nil || userID <= 0 || strings.TrimSpace(sessionID) == "" {
		return nil, nil, fmt.Errorf("invalid thin card projection request")
	}
	source, err := p.loader.LoadThinCardSource(ctx, userID, sessionID)
	if err != nil {
		return nil, nil, err
	}
	existing, err := p.repository.GetBySessionKind(ctx, userID, sessionID, model.EpisodeContentSessionCard)
	if err != nil && !errors.Is(err, episodedao.ErrEpisodeNotFound) {
		return nil, nil, err
	}
	if errors.Is(err, episodedao.ErrEpisodeNotFound) {
		existing = nil
	}
	fresh, err := p.deriver.DeriveThinCard(ctx, source)
	if err != nil {
		return nil, nil, err
	}
	document := fresh
	if existing != nil && existing.Card.Version == model.SessionThinCardContentVersion {
		// The card a session already has is the starting point: a card is
		// updated, not rewritten from nothing.
		document = MergeThinCard(existing.Card, fresh)
	}
	if existing != nil && !ThinCardNeedsWrite(existing, document) {
		// Nothing about the card changed. Writing it again would bump its
		// version and make every other reader of it check a version that
		// says the same thing.
		return existing, existing, nil
	}
	card := &model.EpisodeMemory{
		ID: uuid.NewString(), UserID: userID, SessionID: sessionID,
		ContentKind:         model.EpisodeContentSessionCard,
		Title:               CardTitle(document),
		Card:                document,
		SearchText:          ThinCardSearchText(document),
		TopicKeys:           document.CanonicalTopicKeys(),
		SourceEpisodes:      model.EpisodeSourceRefs{},
		SourceLastMessageID: source.LastMessageID, LastMessageAt: source.LastTalkedAt,
		Status: model.EpisodeStatusActive, IndexStatus: model.EpisodeIndexPending,
	}
	if card.Title == "" || strings.TrimSpace(card.SearchText) == "" {
		return nil, nil, ErrEmptyCard
	}
	expectedVersion := uint64(0)
	if existing != nil {
		card.ID = existing.ID
		expectedVersion = existing.Version
	}
	if err := p.repository.Save(ctx, card, expectedVersion); err != nil {
		return nil, nil, err
	}
	return card, existing, nil
}

// ThinCardService keeps one session's thin card current.
//
// It is the whole of the write side: derive what the program already knows,
// fold it into the card that is there, write it if it changed, and index it
// if its searchable text changed.
//
// The order is the point. The index is written after the row, and only when
// the words changed - an index write is a write to a shared store that every
// other attempt is also writing to.
type ThinCardService struct {
	projector *ThinCardProjector
	indexer   *ThinCardIndexer
}

// NewThinCardService builds the service.
func NewThinCardService(projector *ThinCardProjector, indexer *ThinCardIndexer) (*ThinCardService, error) {
	if projector == nil || indexer == nil {
		return nil, fmt.Errorf("thin card service dependencies are required")
	}
	return &ThinCardService{projector: projector, indexer: indexer}, nil
}

// SyncThinCard brings one session's thin card up to date.
//
// It costs no model call. Everything it writes comes from data the program
// already had.
//
// The name is the interface's: the agent package must not depend on this
// one, so the method is named in the terms the caller uses.
func (s *ThinCardService) SyncThinCard(ctx context.Context, userID int64, sessionID string) error {
	_, err := s.SyncCard(ctx, userID, sessionID)
	return err
}

// SyncCard brings one session's thin card up to date and puts it in the index.
//
// It returns the card it wrote, and it writes the index BEFORE it returns.
//
// The two are one operation because a card that is in the store and not in
// the index is a card nobody can find: the row says the conversation
// happened, the search says nothing about it, and the learner is told the
// product does not remember. The background path is the one that depends on
// this: it is the repair path for a session that moved while the product was
// down, and it has no other chance to write the index.
func (s *ThinCardService) SyncCard(ctx context.Context, userID int64, sessionID string) (*model.EpisodeMemory, error) {
	if s == nil || userID <= 0 || strings.TrimSpace(sessionID) == "" {
		return nil, fmt.Errorf("invalid thin card sync request")
	}
	card, previous, err := s.projector.Project(ctx, userID, sessionID, 0)
	if err != nil {
		return nil, err
	}
	if err := s.indexer.Index(ctx, card, previous); err != nil {
		// The card is returned with the error: it IS in the store, and the
		// caller that retries has to know that the row is there.
		return card, err
	}
	return card, nil
}
