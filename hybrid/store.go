package hybrid

import (
	messageDAO "GopherAI/dao/message"
	sessionDAO "GopherAI/dao/session"
	"GopherAI/model"
	"context"
	"errors"
	"github.com/xiaopang456/GopherLearn/conversation"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"math"
	"strconv"
	"time"
)

// Store adapts only ordinary conversation storage. All durable facts use V2 models.
type Store struct{ db *gorm.DB }

var _ conversation.Store = (*Store)(nil)

func numericUser(value string) (int64, error) {
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n <= 0 || strconv.FormatInt(n, 10) != value {
		return 0, conversation.ErrInvalidArgument
	}
	return n, nil
}
func messageID(value string) (uint, error) {
	n, err := strconv.ParseUint(value, 10, strconv.IntSize)
	if err != nil || n == 0 || strconv.FormatUint(n, 10) != value {
		return 0, conversation.ErrInvalidArgument
	}
	return uint(n), nil
}
func storeError(err error) error {
	switch {
	case errors.Is(err, sessionDAO.ErrSessionNotFound), errors.Is(err, sessionDAO.ErrSummaryNotFound), errors.Is(err, gorm.ErrRecordNotFound):
		return conversation.ErrNotFound
	case errors.Is(err, sessionDAO.ErrSummaryConflict):
		return conversation.ErrConflict
	default:
		return err
	}
}
func owned(ctx context.Context, db *gorm.DB, userID, sessionID string) (*model.Session, error) {
	uid, err := numericUser(userID)
	if err != nil {
		return nil, err
	}
	row, err := sessionDAO.NewRepository(db).GetByIDAndUserID(ctx, sessionID, uid)
	if err != nil {
		return nil, storeError(err)
	}
	// The stored mode is authoritative. Other modes continue through V2 entry.
	if row.Mode != model.SessionModeNormal {
		return nil, conversation.ErrInvalidArgument
	}
	return row, nil
}
func (s *Store) GetSession(ctx context.Context, uid, sid string) (*conversation.Session, error) {
	row, err := owned(ctx, s.db, uid, sid)
	if err != nil {
		return nil, err
	}
	return &conversation.Session{ID: row.ID, UserID: strconv.FormatInt(row.UserID, 10), Title: row.Title, Mode: conversation.ChatMode, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}, nil
}
func domainMessage(row model.Message) conversation.Message {
	role := "assistant"
	if row.IsUser {
		role = "user"
	}
	return conversation.Message{ID: strconv.FormatUint(uint64(row.ID), 10), SessionID: row.SessionID, Role: role, Content: row.Content, CreatedAt: row.CreatedAt}
}
func (s *Store) ListMessages(ctx context.Context, uid, sid string) ([]conversation.Message, error) {
	result := make([]conversation.Message, 0)
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		session, err := owned(ctx, tx, uid, sid)
		if err != nil {
			return err
		}
		var pages [][]model.Message
		before := uint(0)
		for {
			page, err := messageDAO.NewRepository(tx).ListOwnedHistoryPage(ctx, session.UserID, sid, before, messageDAO.MaxHistoryPageLimit)
			if err != nil {
				return err
			}
			pages = append(pages, page.Messages)
			if !page.HasMore {
				break
			}
			before = page.NextBeforeID
		}
		for i := len(pages) - 1; i >= 0; i-- {
			for _, row := range pages[i] {
				result = append(result, domainMessage(row))
			}
		}
		return nil
	})
	return result, storeError(err)
}
func (s *Store) AppendMessage(ctx context.Context, uid, sid string, msg *conversation.Message) (*conversation.Message, error) {
	if msg == nil || msg.ID != "" || len(msg.Sources) > 0 || (msg.Role != "user" && msg.Role != "assistant") {
		return nil, conversation.ErrInvalidArgument
	}
	var result conversation.Message
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Lock the original owned row for this append. MySQL locking reads see
		// current state even under REPEATABLE READ, and serialize deletion or
		// ownership/mode changes until this message/touch transaction completes.
		session, err := owned(ctx, tx.Clauses(clause.Locking{Strength: "UPDATE"}), uid, sid)
		if err != nil {
			return err
		}
		created := msg.CreatedAt
		if created.IsZero() {
			created = time.Now().UTC()
		}
		row, err := messageDAO.NewRepository(tx).Create(ctx, &model.Message{SessionID: sid, UserName: session.UserName, Content: msg.Content, IsUser: msg.Role == "user", CreatedAt: created})
		if err != nil {
			return err
		}
		// Preserve the frozen synchronous append + session timestamp transaction.
		touch := tx.Model(&model.Session{}).Where("id = ? AND user_id = ? AND mode = ?", sid, session.UserID, model.SessionModeNormal).UpdateColumn("updated_at", created)
		if touch.Error != nil {
			return touch.Error
		}
		if touch.RowsAffected == 0 {
			// With the original MySQL options, an unchanged timestamp reports
			// zero changed rows. Revalidate rather than equating that with absence.
			if _, err := owned(ctx, tx.Clauses(clause.Locking{Strength: "UPDATE"}), uid, sid); err != nil {
				return err
			}
		} else if touch.RowsAffected != 1 {
			return conversation.ErrNotFound
		}
		result = domainMessage(*row)
		return nil
	})
	if err != nil {
		return nil, storeError(err)
	}
	return &result, nil
}
func domainSummary(row *model.SessionSummary) (*conversation.SessionSummary, error) {
	if row.Version > math.MaxInt64 {
		return nil, conversation.ErrInvalidArgument
	}
	return &conversation.SessionSummary{SessionID: row.SessionID, UserID: strconv.FormatInt(row.UserID, 10), Content: row.Content, CoveredMessageID: strconv.FormatUint(uint64(row.CoveredMessageID), 10), Version: int64(row.Version), UpdatedAt: row.UpdatedAt}, nil
}
func (s *Store) GetSessionSummary(ctx context.Context, uid, sid string) (*conversation.SessionSummary, error) {
	var result *conversation.SessionSummary
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		session, err := owned(ctx, tx, uid, sid)
		if err != nil {
			return err
		}
		row, err := sessionDAO.NewRepository(tx).GetSummary(ctx, session.UserID, sid)
		if err != nil {
			return err
		}
		result, err = domainSummary(row)
		return err
	})
	return result, storeError(err)
}
func (s *Store) CommitSessionSummary(ctx context.Context, value *conversation.SessionSummary, expected int64) (*conversation.SessionSummary, error) {
	if value == nil || expected < 0 {
		return nil, conversation.ErrInvalidArgument
	}
	boundary, err := messageID(value.CoveredMessageID)
	if err != nil {
		return nil, err
	}
	var result *conversation.SessionSummary
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		session, err := owned(ctx, tx, value.UserID, value.SessionID)
		if err != nil {
			return err
		}
		row := &model.SessionSummary{UserID: session.UserID, SessionID: session.ID, Content: value.Content, CoveredMessageID: boundary}
		if err := sessionDAO.NewRepository(tx).SaveSummary(ctx, row, uint64(expected)); err != nil {
			return err
		}
		// SaveSummary updates the watermark/version in place. Read the persisted row
		// back so its original timestamps and identity remain authoritative.
		row, err = sessionDAO.NewRepository(tx).GetSummary(ctx, session.UserID, session.ID)
		if err != nil {
			return err
		}
		result, err = domainSummary(row)
		return err
	})
	return result, storeError(err)
}
