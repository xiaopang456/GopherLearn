package message

import (
	"GopherAI/common/mysql"
	"GopherAI/model"
	"context"
	"fmt"
	"slices"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const DefaultRecentMessageLimit = 128

const (
	DefaultHistoryPageLimit = 50
	MaxHistoryPageLimit     = 100
)

type HistoryPage struct {
	Messages     []model.Message
	HasMore      bool
	NextBeforeID uint
}

type Repository struct{ db *gorm.DB }

func NewRepository(db *gorm.DB) *Repository { return &Repository{db: db} }
func DefaultRepository() *Repository        { return NewRepository(mysql.DB) }

// ListRecent returns a bounded candidate window in chronological order. The
// ContextBuilder performs the final token-based selection; limit is only a
// database safety guard so a large session is never loaded in full.
func (r *Repository) ListRecent(ctx context.Context, sessionID string, limit int) ([]model.Message, error) {
	return r.ListRecentAfter(ctx, sessionID, 0, limit)
}

// ListRecentWithCount returns a bounded chronological tail plus the durable
// row count used by legacy-chat admission control. It never loads the omitted
// prefix into memory.
func (r *Repository) ListRecentWithCount(ctx context.Context, sessionID string, limit int) ([]model.Message, int64, error) {
	if r == nil || r.db == nil {
		return nil, 0, fmt.Errorf("message repository is not initialized")
	}
	var count int64
	if err := r.db.WithContext(ctx).Model(&model.Message{}).Where("session_id = ?", sessionID).Count(&count).Error; err != nil {
		return nil, 0, err
	}
	messages, err := r.ListRecent(ctx, sessionID, limit)
	return messages, count, err
}

// ListOwnedHistoryPage reads one newest-first cursor page, then returns it in
// chronological display order. The session join is the ownership boundary.
func (r *Repository) ListOwnedHistoryPage(ctx context.Context, userID int64, sessionID string, beforeID uint, limit int) (HistoryPage, error) {
	if r == nil || r.db == nil || userID <= 0 || sessionID == "" {
		return HistoryPage{}, fmt.Errorf("invalid owned history read")
	}
	if limit <= 0 {
		limit = DefaultHistoryPageLimit
	}
	if limit > MaxHistoryPageLimit {
		limit = MaxHistoryPageLimit
	}
	var newestFirst []model.Message
	query := r.db.WithContext(ctx).
		Joins("JOIN sessions ON sessions.id = messages.session_id AND sessions.deleted_at IS NULL").
		Where("sessions.user_id = ? AND messages.session_id = ?", userID, sessionID)
	if beforeID > 0 {
		query = query.Where("messages.id < ?", beforeID)
	}
	if err := query.Order("messages.id DESC").Limit(limit + 1).Find(&newestFirst).Error; err != nil {
		return HistoryPage{}, err
	}
	hasMore := len(newestFirst) > limit
	if hasMore {
		newestFirst = newestFirst[:limit]
	}
	for left, right := 0, len(newestFirst)-1; left < right; left, right = left+1, right-1 {
		newestFirst[left], newestFirst[right] = newestFirst[right], newestFirst[left]
	}
	nextBeforeID := uint(0)
	if hasMore && len(newestFirst) > 0 {
		nextBeforeID = newestFirst[0].ID
	}
	return HistoryPage{Messages: newestFirst, HasMore: hasMore, NextBeforeID: nextBeforeID}, nil
}

// ListOwnedWordsAfter reads one learner's conversation words after one message
// id, oldest first.
//
// It is the read of the stretch a summary has NOT covered yet: the summary's
// last covered message is the high-water mark, and everything after it exists
// only as words. The session join is the ownership boundary, exactly as in
// ListOwnedHistoryPage, so this read cannot reach a conversation the learner
// does not own however the caller names it.
func (r *Repository) ListOwnedWordsAfter(ctx context.Context, userID int64, sessionID string,
	afterID uint, limit int) ([]model.Message, error) {
	if r == nil || r.db == nil || userID <= 0 || sessionID == "" {
		return nil, fmt.Errorf("invalid owned message read")
	}
	if limit <= 0 {
		limit = DefaultHistoryPageLimit
	}
	if limit > MaxHistoryPageLimit {
		limit = MaxHistoryPageLimit
	}
	var messages []model.Message
	err := r.db.WithContext(ctx).
		Joins("JOIN sessions ON sessions.id = messages.session_id AND sessions.deleted_at IS NULL").
		Where("sessions.user_id = ? AND messages.session_id = ? AND messages.id > ?", userID, sessionID, afterID).
		Order("messages.id ASC").Limit(limit).
		Find(&messages).Error
	return messages, err
}

// ListRecentAfter is identical to ListRecent but excludes messages already
// represented by a rolling session summary.
func (r *Repository) ListRecentAfter(ctx context.Context, sessionID string, afterID uint, limit int) ([]model.Message, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("message repository is not initialized")
	}
	if limit <= 0 {
		limit = DefaultRecentMessageLimit
	}
	if limit > 512 {
		limit = 512
	}
	var newestFirst []model.Message
	err := r.db.WithContext(ctx).
		Where("session_id = ? AND id > ?", sessionID, afterID).
		Order("id desc").
		Limit(limit).
		Find(&newestFirst).Error
	if err != nil {
		return nil, err
	}
	for left, right := 0, len(newestFirst)-1; left < right; left, right = left+1, right-1 {
		newestFirst[left], newestFirst[right] = newestFirst[right], newestFirst[left]
	}
	return newestFirst, nil
}

// ListAfter returns a bounded chronological prefix after a high-water mark.
// It is used by rolling compaction and deliberately does not load an entire
// unbounded session.
func (r *Repository) ListAfter(ctx context.Context, sessionID string, afterID uint, limit int) ([]model.Message, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("message repository is not initialized")
	}
	if limit <= 0 {
		limit = 512
	}
	if limit > 1024 {
		limit = 1024
	}
	var messages []model.Message
	err := r.db.WithContext(ctx).
		Where("session_id = ? AND id > ?", sessionID, afterID).
		Order("id asc").
		Limit(limit).
		Find(&messages).Error
	return messages, err
}

func GetMessagesBySessionID(sessionID string) ([]model.Message, error) {
	var msgs []model.Message
	err := mysql.DB.Where("session_id = ?", sessionID).Order("created_at asc").Find(&msgs).Error
	return msgs, err
}

func GetMessagesBySessionIDs(sessionIDs []string) ([]model.Message, error) {
	var msgs []model.Message
	if len(sessionIDs) == 0 {
		return msgs, nil
	}
	err := mysql.DB.Where("session_id IN ?", sessionIDs).Order("created_at asc").Find(&msgs).Error
	return msgs, err
}

func CreateMessage(message *model.Message) (*model.Message, error) {
	return CreateMessageWithContext(context.Background(), message)
}

func CreateMessageWithContext(ctx context.Context, message *model.Message) (*model.Message, error) {
	return DefaultRepository().Create(ctx, message)
}

func (r *Repository) Create(ctx context.Context, message *model.Message) (*model.Message, error) {
	if r == nil || r.db == nil {
		return message, fmt.Errorf("message repository is not initialized")
	}
	db := r.db.WithContext(ctx)
	if message != nil && message.AgentRunID != nil && message.AgentRunRole != nil {
		err := db.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "agent_run_id"}, {Name: "agent_run_role"}},
			DoNothing: true,
		}).Create(message).Error
		if err != nil {
			return message, err
		}
		if message.ID == 0 {
			err = db.Where("agent_run_id = ? AND agent_run_role = ?", *message.AgentRunID, *message.AgentRunRole).First(message).Error
		}
		return message, err
	}
	err := db.Create(message).Error
	return message, err
}

// CreateMessageIdempotent persists one RabbitMQ delivery at most once. A
// consumer can write successfully and lose its Ack when the channel closes;
// RabbitMQ will then redeliver the same persistence key, which must not create
// a duplicate chat row.
func CreateMessageIdempotent(ctx context.Context, message *model.Message) error {
	return DefaultRepository().CreateIdempotent(ctx, message)
}

func (r *Repository) CreateIdempotent(ctx context.Context, message *model.Message) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("message repository is not initialized")
	}
	if message == nil || message.PersistenceKey == nil || *message.PersistenceKey == "" {
		return fmt.Errorf("message persistence key is required")
	}
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "persistence_key"}},
		DoNothing: true,
	}).Create(message).Error
}

func GetAllMessages() ([]model.Message, error) {
	var msgs []model.Message
	err := mysql.DB.Order("created_at asc").Find(&msgs).Error
	return msgs, err
}

// ReadOwnedMessages returns the named messages that belong to this learner.
//
// The join to sessions is the ownership check and is not optional: message ids
// are sequential across every account, so a query keyed on id alone would serve
// one learner another's conversation on a typo. Rows outside the account are
// absent from the result rather than reported, because the caller has no
// legitimate use for knowing that an id it may not read exists.
func (r *Repository) ReadOwnedMessages(ctx context.Context, userID int64, messageIDs []uint) ([]model.Message, error) {
	if r == nil || r.db == nil || userID <= 0 {
		return nil, fmt.Errorf("invalid owned message read")
	}
	if len(messageIDs) == 0 || len(messageIDs) > 50 {
		return nil, fmt.Errorf("owned message read takes between one and fifty ids")
	}
	var messages []model.Message
	err := r.db.WithContext(ctx).
		Joins("JOIN sessions ON sessions.id = messages.session_id AND sessions.deleted_at IS NULL").
		Where("sessions.user_id = ? AND messages.id IN ?", userID, messageIDs).
		Order("messages.id ASC").
		Find(&messages).Error
	return messages, err
}

// ReadOwnedMessagesBySession returns the named messages, each bound to the
// session that authorised reading it.
//
// This is the scoped form of ReadOwnedMessages, and the scope is the point: a
// read-back grant says "this id, from that session", because summary evidence
// lives in the session being answered while episode evidence lives in whatever
// session the episode describes. A query keyed on owner plus id accepts either
// from anywhere the account owns, which puts one conversation's rows inside the
// context of another. Each session group is matched against a session the
// account owns, and the join stays the ownership check. Sessions are sorted so
// the emitted SQL is reproducible.
func (r *Repository) ReadOwnedMessagesBySession(ctx context.Context, userID int64, messageIDsBySession map[string][]uint) ([]model.Message, error) {
	if r == nil || r.db == nil || userID <= 0 || len(messageIDsBySession) == 0 {
		return nil, fmt.Errorf("invalid scoped owned message read")
	}
	sessions := make([]string, 0, len(messageIDsBySession))
	total := 0
	for sessionID, messageIDs := range messageIDsBySession {
		if strings.TrimSpace(sessionID) == "" || len(messageIDs) == 0 {
			return nil, fmt.Errorf("scoped owned message read needs a session and at least one id")
		}
		total += len(messageIDs)
		sessions = append(sessions, sessionID)
	}
	if total > 50 {
		return nil, fmt.Errorf("scoped owned message read takes at most fifty ids")
	}
	slices.Sort(sessions)
	conditions := make([]string, 0, len(sessions))
	args := make([]any, 0, len(sessions)*2)
	for _, sessionID := range sessions {
		conditions = append(conditions, "(messages.session_id = ? AND messages.id IN ?)")
		args = append(args, sessionID, messageIDsBySession[sessionID])
	}
	var messages []model.Message
	err := r.db.WithContext(ctx).
		Joins("JOIN sessions ON sessions.id = messages.session_id AND sessions.deleted_at IS NULL").
		Where("sessions.user_id = ?", userID).
		Where("("+strings.Join(conditions, " OR ")+")", args...).
		Order("messages.id ASC").
		Find(&messages).Error
	return messages, err
}

// SearchOwnedMessagesBySession finds the messages of one session that contain
// a keyword.
//
// It is the last step of the read-back chain: a card says what a session was
// about, its summary says what was said in it, and this answers "what exactly
// did they write there" for the one detail none of the compressions kept.
//
// The scope is the whole point. The query is bound to an owner and to one
// session, and the session join is the ownership check, so a search cannot
// reach a conversation the learner did not name and this run was not shown.
// The keyword is matched inside the database rather than in Go, because a
// search that read the whole session into memory first would be an unbounded
// read of somebody's history.
//
// THE ORDER IS THE NEWEST FIRST, and that is the whole point of the change.
//
// A conversation settles its arrangements at its end: the number that was
// finally agreed on, the thing that was decided last, the words that
// replaced an earlier number. A search that returned the OLDEST matching
// messages would answer a question about how the conversation began - which
// is not what anybody asks an earlier conversation for - and would make the
// end of a long conversation unreachable: the matches the learner needs are
// the last ones, and a limit of five oldest matches never reaches them.
//
// The newest matches come first, and the ids of the ones that are not the
// answer are still shown, so a reader that wants an earlier one can ask for
// the message id by name.
func (r *Repository) SearchOwnedMessagesBySession(ctx context.Context, userID int64,
	sessionID string, keyword string, limit int) ([]model.Message, error) {
	if r == nil || r.db == nil || userID <= 0 {
		return nil, fmt.Errorf("invalid owned message search")
	}
	sessionID = strings.TrimSpace(sessionID)
	keyword = strings.TrimSpace(keyword)
	if sessionID == "" || keyword == "" {
		return nil, fmt.Errorf("owned message search needs a session and a keyword")
	}
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	var messages []model.Message
	err := r.db.WithContext(ctx).
		Joins("JOIN sessions ON sessions.id = messages.session_id AND sessions.deleted_at IS NULL").
		Where("sessions.user_id = ? AND messages.session_id = ?", userID, sessionID).
		Where("messages.content LIKE ?", "%"+escapeLike(keyword)+"%").
		Order("messages.id DESC").Limit(limit).
		Find(&messages).Error
	return messages, err
}

// escapeLike escapes the characters MySQL's LIKE treats as wildcards.
//
// A keyword the learner typed may contain a percent sign, and a search that
// treated it as a wildcard would match every message of the session - the
// whole conversation, returned because someone typed "%".
func escapeLike(value string) string {
	replacer := strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_")
	return replacer.Replace(value)
}

// ListBySession returns every message of one session, oldest first, bounded
// by limit. The finalize extraction reads the session once when it ends; the
// bound keeps an unusually long session from turning the read into an
// unbounded scan, and the extraction's own window parameter narrows what the
// model actually sees.
func (r *Repository) ListBySession(ctx context.Context, sessionID string, limit int) ([]model.Message, error) {
	if r == nil || r.db == nil || sessionID == "" || limit < 1 {
		return nil, fmt.Errorf("invalid session message query")
	}
	var messages []model.Message
	err := r.db.WithContext(ctx).
		Where("session_id = ?", sessionID).
		Order("id ASC").
		Limit(limit).
		Find(&messages).Error
	return messages, err
}
