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

var (
	ErrProjectionSourceNotFound = errors.New("episode projection source not found")
	ErrProjectionTargetMissing  = errors.New("episode projection target message missing")
)

const defaultProjectionMessageLimit = 4096

// ProjectionEvidence is a target-frozen view of one Session. Summary is only
// used when its watermark does not exceed TargetMessageID. Messages always
// contain complete rows after that watermark and through the claimed target.
type ProjectionEvidence struct {
	UserID          int64
	Session         model.Session
	Summary         *model.SessionSummary
	SummaryMessages []model.Message
	Messages        []model.Message
	Existing        *model.EpisodeMemory
	TargetMessageID uint
	TargetMessageAt time.Time
}

type EvidenceLoader interface {
	Load(context.Context, int64, string, uint) (ProjectionEvidence, error)
}

type DatabaseEvidenceLoader struct {
	db              *gorm.DB
	maxMessageCount int
}

func NewDatabaseEvidenceLoader(db *gorm.DB, maxMessageCount int) (*DatabaseEvidenceLoader, error) {
	if db == nil {
		return nil, fmt.Errorf("episode evidence database is required")
	}
	if maxMessageCount <= 0 {
		maxMessageCount = defaultProjectionMessageLimit
	}
	if maxMessageCount < 32 || maxMessageCount > 20000 {
		return nil, fmt.Errorf("episode evidence message limit must be between 32 and 20000")
	}
	return &DatabaseEvidenceLoader{db: db, maxMessageCount: maxMessageCount}, nil
}

func (l *DatabaseEvidenceLoader) Load(ctx context.Context, userID int64, sessionID string, targetMessageID uint) (ProjectionEvidence, error) {
	sessionID = strings.TrimSpace(sessionID)
	if l == nil || l.db == nil || userID <= 0 || sessionID == "" || targetMessageID == 0 {
		return ProjectionEvidence{}, ErrProjectionSourceNotFound
	}
	view := ProjectionEvidence{UserID: userID, TargetMessageID: targetMessageID}
	if err := l.db.WithContext(ctx).
		Where("id = ? AND user_id = ?", sessionID, userID).
		First(&view.Session).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ProjectionEvidence{}, ErrProjectionSourceNotFound
		}
		return ProjectionEvidence{}, err
	}
	var target model.Message
	if err := l.db.WithContext(ctx).
		Where("id = ? AND session_id = ?", targetMessageID, sessionID).
		First(&target).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ProjectionEvidence{}, ErrProjectionTargetMissing
		}
		return ProjectionEvidence{}, err
	}
	view.TargetMessageAt = target.CreatedAt

	var summary model.SessionSummary
	if err := l.db.WithContext(ctx).
		Where("user_id = ? AND session_id = ? AND covered_message_id <= ?", userID, sessionID, targetMessageID).
		First(&summary).Error; err == nil {
		view.Summary = &summary
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return ProjectionEvidence{}, err
	}
	if view.Summary != nil {
		document, decodeErr := model.DecodeSessionSummaryDocument(view.Summary.Content)
		if decodeErr != nil {
			return ProjectionEvidence{}, fmt.Errorf("decode episode summary source: %w", decodeErr)
		}
		ids := document.EvidenceMessageIDs()
		if len(ids) > 0 {
			if err := l.db.WithContext(ctx).
				Where("session_id = ? AND id IN ?", sessionID, ids).
				Order("id ASC").Find(&view.SummaryMessages).Error; err != nil {
				return ProjectionEvidence{}, err
			}
			if len(view.SummaryMessages) != len(ids) {
				return ProjectionEvidence{}, fmt.Errorf("episode summary evidence message missing")
			}
		}
	}
	afterID := uint(0)
	if view.Summary != nil {
		afterID = view.Summary.CoveredMessageID
	}

	if err := l.db.WithContext(ctx).
		Where("session_id = ? AND id > ? AND id <= ?", sessionID, afterID, targetMessageID).
		Order("id ASC").Limit(l.maxMessageCount + 1).
		Find(&view.Messages).Error; err != nil {
		return ProjectionEvidence{}, err
	}
	if len(view.Messages) > l.maxMessageCount {
		return ProjectionEvidence{}, fmt.Errorf("episode projection source exceeds %d complete messages", l.maxMessageCount)
	}
	if afterID < targetMessageID {
		if len(view.Messages) == 0 || view.Messages[len(view.Messages)-1].ID != targetMessageID {
			return ProjectionEvidence{}, ErrProjectionTargetMissing
		}
	}

	var existing model.EpisodeMemory
	if err := l.db.WithContext(ctx).
		Where("user_id = ? AND session_id = ?", userID, sessionID).
		First(&existing).Error; err == nil {
		view.Existing = &existing
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return ProjectionEvidence{}, err
	}
	return view, nil
}

func (e ProjectionEvidence) Validate() error {
	if e.UserID <= 0 || e.Session.ID == "" || e.Session.UserID != e.UserID || e.TargetMessageID == 0 || e.TargetMessageAt.IsZero() {
		return ErrProjectionSourceNotFound
	}
	if e.Summary != nil {
		if e.Summary.UserID != e.UserID || e.Summary.SessionID != e.Session.ID || e.Summary.CoveredMessageID > e.TargetMessageID {
			return fmt.Errorf("invalid episode summary source")
		}
		document, err := model.DecodeSessionSummaryDocument(e.Summary.Content)
		if err != nil {
			return fmt.Errorf("invalid episode summary document: %w", err)
		}
		expectedIDs := document.EvidenceMessageIDs()
		if len(expectedIDs) != len(e.SummaryMessages) {
			return fmt.Errorf("invalid episode summary evidence messages")
		}
		for index, id := range expectedIDs {
			message := e.SummaryMessages[index]
			if message.ID != id || message.SessionID != e.Session.ID || !message.IsUser || message.ID > e.Summary.CoveredMessageID {
				return fmt.Errorf("invalid episode summary evidence message")
			}
		}
	} else if len(e.SummaryMessages) > 0 {
		return fmt.Errorf("episode summary messages require a summary")
	}
	afterID := uint(0)
	if e.Summary != nil {
		afterID = e.Summary.CoveredMessageID
	}
	previous := afterID
	for _, message := range e.Messages {
		if message.SessionID != e.Session.ID || message.ID <= previous || message.ID > e.TargetMessageID {
			return fmt.Errorf("invalid episode message source")
		}
		if err := message.EpisodeSources.Validate(); err != nil {
			return fmt.Errorf("invalid message episode provenance: %w", err)
		}
		if message.IsUser && len(message.EpisodeSources) > 0 {
			return fmt.Errorf("user message cannot carry episode provenance")
		}
		previous = message.ID
	}
	if afterID < e.TargetMessageID && previous != e.TargetMessageID {
		return ErrProjectionTargetMissing
	}
	return nil
}
