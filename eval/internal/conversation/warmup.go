package conversation

import (
	"context"
	"fmt"

	messagedao "GopherAI/dao/message"
	sessiondao "GopherAI/dao/session"
	"GopherAI/model"
	agent "GopherAI/service/agent"
	"gorm.io/gorm"
)

type guardedStore struct{ sandbox *Sandbox }

func (g guardedStore) with(ctx context.Context, fn func(*gorm.DB) error) error {
	return g.sandbox.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := g.sandbox.checkMarker(tx); err != nil {
			return err
		}
		return fn(tx)
	})
}
func (g guardedStore) Create(ctx context.Context, m *model.Message) (*model.Message, error) {
	var out *model.Message
	err := g.with(ctx, func(tx *gorm.DB) error {
		var err error
		out, err = messagedao.NewRepository(tx).Create(ctx, m)
		return err
	})
	return out, err
}

// CreateMessage writes one turn and returns it with the id it was given.
//
// It is the store's own name for the message repository's create: a store
// that holds turns names them the way the run does.
func (g guardedStore) CreateMessage(ctx context.Context, message model.Message) (*model.Message, error) {
	return g.Create(ctx, &message)
}
func (g guardedStore) ListAfter(ctx context.Context, session string, id uint, limit int) ([]model.Message, error) {
	var out []model.Message
	err := g.with(ctx, func(tx *gorm.DB) error {
		var err error
		out, err = messagedao.NewRepository(tx).ListAfter(ctx, session, id, limit)
		return err
	})
	return out, err
}

// ListRecentAfter is the newest-first fetch the context builder reads its raw
// window through. It has to reach the repository's own newest-first query: the
// chronological prefix below the same name returns the oldest slice above the
// watermark, and a window assembled from it drops every message the fetch did
// not reach - the newest ones first.
func (g guardedStore) ListRecentAfter(ctx context.Context, session string, id uint, limit int) ([]model.Message, error) {
	var out []model.Message
	err := g.with(ctx, func(tx *gorm.DB) error {
		var err error
		out, err = messagedao.NewRepository(tx).ListRecentAfter(ctx, session, id, limit)
		return err
	})
	return out, err
}
func (g guardedStore) GetSummary(ctx context.Context, userID int64, session string) (*model.SessionSummary, error) {
	var out *model.SessionSummary
	err := g.with(ctx, func(tx *gorm.DB) error {
		var err error
		out, err = sessiondao.NewRepository(tx).GetSummary(ctx, userID, session)
		return err
	})
	return out, err
}
func (g guardedStore) SaveSummary(ctx context.Context, s *model.SessionSummary, version uint64) error {
	return g.with(ctx, func(tx *gorm.DB) error { return sessiondao.NewRepository(tx).SaveSummary(ctx, s, version) })
}

// Prepare warms one existing evaluation session with the product summarizer.
// The generator is supplied explicitly; a caller that uses a real model must
// separately enforce the authorised budget and ledger before making requests.
func (s *Sandbox) Prepare(ctx context.Context, owner int64, sessionID, username string, turns []Turn, generator agent.SummaryGenerator, config agent.SummaryConfig) ([]PreparedTurn, error) {
	if err := s.verify(ctx); err != nil {
		return nil, err
	}
	if s.db == nil {
		return nil, fmt.Errorf("sandbox database unavailable")
	}
	guard := guardedStore{sandbox: s}
	if err := guard.with(ctx, func(tx *gorm.DB) error {
		var users, owned, sessions int64
		if err := tx.Model(&model.User{}).Count(&users).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.User{}).Where("id = ? AND username = ?", owner, username).Count(&owned).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.Session{}).Count(&sessions).Error; err != nil {
			return err
		}
		if users != 1 || owned != 1 || sessions != 1 {
			return fmt.Errorf("evaluation identity is not isolated")
		}
		var session model.Session
		if err := tx.Where("id = ? AND user_id = ? AND user_name = ? AND mode = ?", sessionID, owner, username, model.SessionModeAgent).First(&session).Error; err != nil {
			return err
		}
		var count int64
		if err := tx.Model(&model.Message{}).Count(&count).Error; err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("session is not empty")
		}
		if err := tx.Model(&model.SessionSummary{}).Count(&count).Error; err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("session has a summary")
		}
		return nil
	}); err != nil {
		return nil, err
	}
	provider, err := agent.NewRollingSummaryProvider(guard, guard, generator, config)
	if err != nil {
		return nil, err
	}
	// The replay waits the product's own failure cooldown out before it writes
	// the next turn. A replay that runs a whole history in one breath would
	// spend the cooldown in a single moment and never compact again, which is
	// not how a conversation with a person goes.
	return PrepareWithCooldown(ctx, guard, provider, owner, sessionID, username, turns, config.FailureCooldown)
}
