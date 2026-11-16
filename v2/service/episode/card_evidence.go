package episode

import (
	"GopherAI/model"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// ErrCardSourceNotFound is a session the card could not be built from.
var ErrCardSourceNotFound = errors.New("session card source not found")

// defaultCardMessageLimit bounds how many raw messages one card may read.
const defaultCardMessageLimit = 2048

// CardEvidence is the frozen view of one session a card is written from.
//
// It is a different shape from the Episode's evidence on purpose. The Episode
// reads the raw conversation and extracts durable facts from it, so it needs
// the messages and nothing else. A card is written from what the session has
// already compressed - the board, the summary segments - plus the part of the
// conversation nothing has compressed yet, so it needs all four.
//
// The two watermarks it carries are the reason the card can be trusted:
//
//   - SummaryWatermark is how far the compressed state reaches. Messages above
//     it have not been summarized yet.
//   - TargetMessageID is how far the card may reach. It is the job's claimed
//     target, and nothing above it may enter the card.
//
// When the summary lags - the common case while a session is still going -
// the gap between the two is filled with raw messages, so the card never
// silently skips the part of the conversation nothing has summarized.
type CardEvidence struct {
	UserID          int64
	Session         model.Session
	TargetMessageID uint
	TargetMessageAt time.Time

	// Existing is the card this session already has, if any.
	Existing *model.EpisodeMemory
	// PreviousWatermark is how far the existing card already reached. It is
	// zero for a session that has no card yet.
	PreviousWatermark uint

	// Whiteboard is the session's current state, and is used only when it
	// does not reach past the target.
	//
	// A board that was written after the target message describes a state
	// the card is not allowed to cover. Mixing it in would put later facts
	// into an earlier snapshot, which is the one thing a snapshot may not
	// do.
	Whiteboard         *model.SessionWhiteboard
	WhiteboardDocument model.SessionWhiteboardDocument

	// Segments are the summary segments of this session that fall inside the
	// window this card covers.
	Segments       []model.SessionSummarySegment
	SegmentContent []model.SessionSummarySegmentContent

	// Messages are the raw messages the compressed state does not cover yet.
	Messages []model.Message

	// SummaryWatermark is the highest message id the compressed state
	// reaches. It is zero when the session has no compressed state at all.
	SummaryWatermark uint
	// SummaryVersion says which revision of the compressed state was used.
	SummaryVersion uint64
}

// CardEvidenceLoader loads one session's card evidence.
type CardEvidenceLoader interface {
	LoadCardEvidence(context.Context, int64, string, uint) (CardEvidence, error)
}

// DatabaseCardEvidenceLoader reads the card evidence from MySQL.
//
// It reads the session's own compressed state rather than the rolling summary
// the Episode path reads: the board and the segments are the summary this
// deployment actually produces, and the rolling summary is a frozen picture
// of an earlier part of the session.
type DatabaseCardEvidenceLoader struct {
	db              *gorm.DB
	maxMessageCount int
}

// NewDatabaseCardEvidenceLoader builds the loader.
func NewDatabaseCardEvidenceLoader(db *gorm.DB, maxMessageCount int) (*DatabaseCardEvidenceLoader, error) {
	if db == nil {
		return nil, fmt.Errorf("session card evidence database is required")
	}
	if maxMessageCount <= 0 {
		maxMessageCount = defaultCardMessageLimit
	}
	if maxMessageCount < 32 || maxMessageCount > 20000 {
		return nil, fmt.Errorf("session card message limit must be between 32 and 20000")
	}
	return &DatabaseCardEvidenceLoader{db: db, maxMessageCount: maxMessageCount}, nil
}

// LoadCardEvidence reads one session's evidence, frozen at the target.
func (l *DatabaseCardEvidenceLoader) LoadCardEvidence(ctx context.Context, userID int64,
	sessionID string, targetMessageID uint) (CardEvidence, error) {
	sessionID = strings.TrimSpace(sessionID)
	if l == nil || l.db == nil || userID <= 0 || sessionID == "" || targetMessageID == 0 {
		return CardEvidence{}, ErrCardSourceNotFound
	}
	view := CardEvidence{UserID: userID, TargetMessageID: targetMessageID}
	if err := l.db.WithContext(ctx).
		Where("id = ? AND user_id = ?", sessionID, userID).
		First(&view.Session).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return CardEvidence{}, ErrCardSourceNotFound
		}
		return CardEvidence{}, err
	}
	var target model.Message
	if err := l.db.WithContext(ctx).
		Where("id = ? AND session_id = ?", targetMessageID, sessionID).
		First(&target).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return CardEvidence{}, ErrCardSourceNotFound
		}
		return CardEvidence{}, err
	}
	view.TargetMessageAt = target.CreatedAt

	// The card this session already has is the starting point: a card is
	// updated, not rewritten from nothing, and the previous version is what
	// says which parts of the session the card has already accounted for.
	var existing model.EpisodeMemory
	if err := l.db.WithContext(ctx).
		Where("user_id = ? AND session_id = ? AND content_kind = ?",
			userID, sessionID, string(model.EpisodeContentSessionCard)).
		First(&existing).Error; err == nil {
		view.Existing = &existing
		view.PreviousWatermark = existing.SourceLastMessageID
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return CardEvidence{}, err
	}

	// The board, but only when it does not reach past the target.
	var board model.SessionWhiteboard
	if err := l.db.WithContext(ctx).
		Where("user_id = ? AND session_id = ?", userID, sessionID).
		First(&board).Error; err == nil {
		if board.CoveredMessageID <= targetMessageID {
			view.Whiteboard = &board
			if document, decodeErr := model.DecodeSessionWhiteboardDocument(board.Content); decodeErr == nil {
				view.WhiteboardDocument = document
			} else {
				view.Whiteboard = nil
			}
			if board.CoveredMessageID > view.SummaryWatermark {
				view.SummaryWatermark = board.CoveredMessageID
				view.SummaryVersion = board.Version
			}
		}
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return CardEvidence{}, err
	}

	// Every segment of this session, not only the ones the current window
	// happens to show. A card that read only the two segments in front of it
	// would describe a session it has not seen.
	var segments []model.SessionSummarySegment
	if err := l.db.WithContext(ctx).
		Where("user_id = ? AND session_id = ? AND end_message_id <= ?", userID, sessionID, targetMessageID).
		Order("seq ASC").Limit(512).Find(&segments).Error; err != nil {
		return CardEvidence{}, err
	}
	view.Segments = segments
	// The watermark is the message id up to which the compressed state has
	// been read. A segment that cannot be decoded is a gap in that state:
	// the stretch it covered was summarised, and the summary is unreadable,
	// so nothing about that stretch may be claimed as covered. The watermark
	// therefore stops at the last segment that was actually read, and the
	// raw messages from there on fill the gap - the card sees the original
	// words instead of a summary it could not read.
	//
	// Raising the watermark past an unreadable segment would make the card
	// claim a coverage it does not have, and the tail of raw messages would
	// start above the gap and skip it entirely.
	for _, segment := range segments {
		content, decodeErr := model.DecodeSessionSummarySegmentContent(segment.Content)
		if decodeErr != nil {
			// A segment that cannot be read is not a segment. It is left out
			// rather than failing the whole card: the rest of the session's
			// compressed state is still true. What it may not do is let the
			// watermark move past the stretch it covered, so the walk stops
			// here and the raw messages take over from there.
			break
		}
		view.SegmentContent = append(view.SegmentContent, content)
		if segment.EndMessageID > view.SummaryWatermark {
			view.SummaryWatermark = segment.EndMessageID
			if view.SummaryVersion == 0 {
				view.SummaryVersion = segmentSchemaVersion(content)
			}
		}
	}

	// The part of the conversation nothing has compressed yet.
	afterID := view.SummaryWatermark
	if afterID < view.PreviousWatermark {
		afterID = view.PreviousWatermark
	}
	if err := l.db.WithContext(ctx).
		Where("session_id = ? AND id > ? AND id <= ?", sessionID, afterID, targetMessageID).
		Order("id ASC").Limit(l.maxMessageCount + 1).
		Find(&view.Messages).Error; err != nil {
		return CardEvidence{}, err
	}
	if len(view.Messages) > l.maxMessageCount {
		return CardEvidence{}, fmt.Errorf("session card source exceeds %d complete messages", l.maxMessageCount)
	}
	return view, nil
}

// segmentSchemaVersion is the version a segment's content declares.
func segmentSchemaVersion(content model.SessionSummarySegmentContent) uint64 {
	if content.SchemaVersion > 0 {
		return uint64(content.SchemaVersion)
	}
	return 1
}

// KnownMessageIDs is every message this evidence may cite.
//
// A card may point at a raw message, at a summary segment, or at both. The
// messages it may point at are the ones it was actually shown: the tail it
// read, the messages the segments were compressed from, and the messages the
// board's entries came from.
func (e CardEvidence) KnownMessageIDs() map[uint]struct{} {
	out := make(map[uint]struct{}, len(e.Messages))
	for _, message := range e.Messages {
		out[message.ID] = struct{}{}
	}
	for _, content := range e.SegmentContent {
		for _, point := range content.Points {
			for _, id := range point.EvidenceMessageIDs {
				out[id] = struct{}{}
			}
		}
	}
	if e.Whiteboard != nil {
		for _, entry := range e.WhiteboardDocument.Constraints {
			for _, id := range entry.SourceMessageIDs {
				out[id] = struct{}{}
			}
		}
		for _, entry := range e.WhiteboardDocument.Plan {
			for _, id := range entry.SourceMessageIDs {
				out[id] = struct{}{}
			}
		}
		for _, entry := range e.WhiteboardDocument.OpenItems {
			for _, id := range entry.SourceMessageIDs {
				out[id] = struct{}{}
			}
		}
		if e.WhiteboardDocument.Goal != nil {
			for _, id := range e.WhiteboardDocument.Goal.SourceMessageIDs {
				out[id] = struct{}{}
			}
		}
		for _, anchor := range e.WhiteboardDocument.Unverified {
			out[anchor.MessageID] = struct{}{}
		}
	}
	return out
}

// KnownSegmentSeqs is every summary segment this evidence may cite.
func (e CardEvidence) KnownSegmentSeqs() map[int]struct{} {
	out := make(map[int]struct{}, len(e.Segments))
	for _, segment := range e.Segments {
		out[segment.Seq] = struct{}{}
	}
	return out
}

// Validate checks that the evidence is internally consistent.
func (e CardEvidence) Validate() error {
	if e.UserID <= 0 || e.Session.ID == "" || e.Session.UserID != e.UserID ||
		e.TargetMessageID == 0 || e.TargetMessageAt.IsZero() {
		return ErrCardSourceNotFound
	}
	if e.SummaryWatermark > e.TargetMessageID {
		return fmt.Errorf("session card summary watermark exceeds the target message")
	}
	if e.PreviousWatermark > e.TargetMessageID {
		return fmt.Errorf("session card previous watermark exceeds the target message")
	}
	previous := uint(0)
	for _, message := range e.Messages {
		if message.SessionID != e.Session.ID || message.ID <= previous || message.ID > e.TargetMessageID {
			return fmt.Errorf("invalid session card message source")
		}
		previous = message.ID
	}
	return nil
}
