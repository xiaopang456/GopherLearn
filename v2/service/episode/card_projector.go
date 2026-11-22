package episode

import (
	"GopherAI/model"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	episodedao "GopherAI/dao/episode"
	"github.com/google/uuid"
)

// CardProjectionRepository is the store the card projector writes through.
//
// It is an interface of its own rather than the Episode's, because the two
// ask the store different questions. The Episode wants the derived row of a
// session; the card wants the card of a session, and one session may now hold
// both an Episode written earlier and a card written later.
type CardProjectionRepository interface {
	GetBySessionKind(context.Context, int64, string, model.EpisodeContentKind) (*model.EpisodeMemory, error)
	Save(context.Context, *model.EpisodeMemory, uint64) error
}

// CardProjector writes the session card of one session.
//
// It is the Episode projector with a different generator and a different
// document, and it deliberately reuses the worker's machinery rather than
// building a second one: the same persistent job, the same idle trigger, the
// same lease, the same retry budget, the same optimistic versioning. A second
// scheduler would be a second thing to get wrong.
//
// The projector never writes a card it did not read. A failed generation
// returns an error and the worker retries it, and because nothing was
// written the previous card and its real coverage watermark are still there.
// The failure mode this avoids is the expensive one: a card that says "this
// session covered nothing" because the model call went wrong, written over a
// card that knew better.
type CardProjector struct {
	loader     CardEvidenceLoader
	generator  CardGenerator
	repository CardProjectionRepository
}

// NewCardProjector builds the projector.
func NewCardProjector(loader CardEvidenceLoader, generator CardGenerator,
	repository CardProjectionRepository) (*CardProjector, error) {
	if loader == nil || generator == nil || repository == nil {
		return nil, fmt.Errorf("session card projector dependencies are required")
	}
	return &CardProjector{loader: loader, generator: generator, repository: repository}, nil
}

// Project writes the card of one session up to the claimed target.
//
// It implements ProjectionRunner, so the existing worker drives it unchanged.
func (p *CardProjector) Project(ctx context.Context, userID int64, sessionID string, targetMessageID uint) (*model.EpisodeMemory, error) {
	if p == nil || userID <= 0 || strings.TrimSpace(sessionID) == "" || targetMessageID == 0 {
		return nil, fmt.Errorf("invalid session card projection request")
	}
	evidence, err := p.loader.LoadCardEvidence(ctx, userID, sessionID, targetMessageID)
	if err != nil {
		return nil, err
	}
	if err := evidence.Validate(); err != nil {
		return nil, err
	}
	existing := evidence.Existing
	if existing == nil {
		existing, err = p.repository.GetBySessionKind(ctx, userID, sessionID, model.EpisodeContentSessionCard)
		if err != nil && !errors.Is(err, episodedao.ErrEpisodeNotFound) {
			return nil, err
		}
		if errors.Is(err, episodedao.ErrEpisodeNotFound) {
			existing = nil
		}
	}
	// A repeat at a watermark the card already covers returns what is there
	// without asking the model again. The same rule the Episode path follows:
	// without it, every retry would re-run the model to re-discover what the
	// card already says.
	if existing != nil && existing.SourceLastMessageID >= targetMessageID {
		return existing, nil
	}
	document, err := p.generator.Generate(ctx, evidence)
	if err != nil {
		return nil, err
	}
	card := &model.EpisodeMemory{
		ID: uuid.NewString(), UserID: userID, SessionID: sessionID,
		ContentKind:         model.EpisodeContentSessionCard,
		Title:               CardTitle(document),
		Card:                document,
		SearchText:          CardSearchText(document),
		TopicKeys:           document.CanonicalTopicKeys(),
		SourceEpisodes:      model.EpisodeSourceRefs{},
		SourceLastMessageID: targetMessageID, LastMessageAt: evidence.TargetMessageAt,
		Status: model.EpisodeStatusActive, IndexStatus: model.EpisodeIndexPending,
	}
	if card.Title == "" || strings.TrimSpace(card.SearchText) == "" {
		return nil, ErrEmptyCard
	}
	if evidence.SummaryWatermark > 0 {
		card.SourceSummaryWatermark = evidence.SummaryWatermark
		card.SourceSummaryVersion = evidence.SummaryVersion
		if card.SourceSummaryVersion == 0 {
			card.SourceSummaryVersion = 1
		}
	}
	expectedVersion := uint64(0)
	if existing != nil {
		card.ID = existing.ID
		expectedVersion = existing.Version
	}
	if err := p.repository.Save(ctx, card, expectedVersion); err != nil {
		return nil, err
	}
	return card, nil
}

// CardIndexLag is how long ago a card's session last moved.
func CardIndexLag(card *model.EpisodeMemory, now time.Time) time.Duration {
	if card == nil || card.UpdatedAt.IsZero() || now.Before(card.UpdatedAt) {
		return 0
	}
	return now.Sub(card.UpdatedAt)
}
