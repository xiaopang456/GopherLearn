package gormmysql

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// errCorruptStoredData reports a row whose stored session mode is outside the
// closed domain set. It is deliberately not ErrInvalidRequest: the client did
// not send the value, the stored data is wrong.
var errCorruptStoredData = errors.New("repository/gormmysql: corrupt stored data")

// Repository is a repository.Repository implementation backed by GORM.
type Repository struct {
	db *gorm.DB
}

// NewRepository wraps an opened and migrated *gorm.DB.
func NewRepository(db *gorm.DB) *Repository {
	return &Repository{db: db}
}

var _ repository.Repository = (*Repository)(nil)

// CreateUser stores the user. A generated ID and creation timestamp fill in
// missing values, mirroring the in-memory repository. A duplicate email or ID
// becomes ErrConflict.
func (r *Repository) CreateUser(ctx context.Context, user *domain.User) (*domain.User, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if r.db == nil || user == nil {
		return nil, repository.ErrInvalidArgument
	}

	record := userRecordFrom(*user)
	if record.ID == "" {
		record.ID = newID("user")
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}
	if err := translate(r.db.WithContext(ctx).Create(&record).Error); err != nil {
		return nil, err
	}
	created := record.toDomain()
	return &created, nil
}

// FindUserByEmail looks the address up case-insensitively, matching the
// normalized key the in-memory repository uses.
func (r *Repository) FindUserByEmail(ctx context.Context, email string) (*domain.User, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if r.db == nil {
		return nil, repository.ErrInvalidArgument
	}
	key := normalizeEmail(email)
	if key == "" {
		return nil, repository.ErrInvalidArgument
	}

	var record UserRecord
	err := r.db.WithContext(ctx).Where("email = ?", key).Take(&record).Error
	if err := translate(err); err != nil {
		return nil, err
	}
	user := record.toDomain()
	return &user, nil
}

// GetUser returns the user with userID.
func (r *Repository) GetUser(ctx context.Context, userID string) (*domain.User, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if r.db == nil || strings.TrimSpace(userID) == "" {
		return nil, repository.ErrInvalidArgument
	}

	var record UserRecord
	if err := translate(r.db.WithContext(ctx).Where("id = ?", userID).Take(&record).Error); err != nil {
		return nil, err
	}
	user := record.toDomain()
	return &user, nil
}

// CreateSession stores a session after confirming the owner exists. The mode
// must already be valid; it has no update path afterwards.
func (r *Repository) CreateSession(ctx context.Context, session *domain.Session) (*domain.Session, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if r.db == nil || session == nil {
		return nil, repository.ErrInvalidArgument
	}

	record := sessionRecordFrom(*session)
	record.UserID = strings.TrimSpace(record.UserID)
	if record.UserID == "" || !domain.SessionMode(record.Mode).IsValid() {
		return nil, repository.ErrInvalidArgument
	}
	if record.ID == "" {
		record.ID = newID("session")
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}
	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = record.CreatedAt
	}

	err := r.transaction(ctx, func(tx *gorm.DB) error {
		if err := requireUser(tx, record.UserID); err != nil {
			return err
		}
		return tx.Create(&record).Error
	})
	if err := translate(err); err != nil {
		return nil, err
	}
	created, err := record.toDomain()
	if err != nil {
		return nil, err
	}
	return &created, nil
}

// ListSessions returns the owner's sessions in creation order.
func (r *Repository) ListSessions(ctx context.Context, userID string) ([]domain.Session, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if r.db == nil || strings.TrimSpace(userID) == "" {
		return nil, repository.ErrInvalidArgument
	}

	var records []SessionRecord
	err := r.transaction(ctx, func(tx *gorm.DB) error {
		if err := requireUser(tx, userID); err != nil {
			return err
		}
		return tx.Where("user_id = ?", userID).Order(orderByCreation).Find(&records).Error
	})
	if err := translate(err); err != nil {
		return nil, err
	}
	sessions := make([]domain.Session, 0, len(records))
	for _, record := range records {
		session, err := record.toDomain()
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	return sessions, nil
}

// GetSession returns a session only when it belongs to userID. A session owned
// by someone else is reported as ErrNotFound, the same way a missing one is.
func (r *Repository) GetSession(ctx context.Context, userID, sessionID string) (*domain.Session, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	record, err := r.sessionRecord(ctx, userID, sessionID)
	if err != nil {
		return nil, err
	}
	session, err := record.toDomain()
	if err != nil {
		return nil, err
	}
	return &session, nil
}

func (r *Repository) sessionRecord(ctx context.Context, userID, sessionID string) (SessionRecord, error) {
	if r.db == nil || strings.TrimSpace(userID) == "" || strings.TrimSpace(sessionID) == "" {
		return SessionRecord{}, repository.ErrInvalidArgument
	}
	var record SessionRecord
	err := r.db.WithContext(ctx).
		Where("id = ? AND user_id = ?", sessionID, userID).
		Take(&record).Error
	if err := translate(err); err != nil {
		return SessionRecord{}, err
	}
	return record, nil
}

// DeleteSession removes an owned session together with its messages inside one
// transaction. The cascade is explicit rather than a database foreign key so
// that the same statement sequence runs unchanged on every engine.
func (r *Repository) DeleteSession(ctx context.Context, userID, sessionID string) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if r.db == nil || strings.TrimSpace(userID) == "" || strings.TrimSpace(sessionID) == "" {
		return repository.ErrInvalidArgument
	}

	err := r.transaction(ctx, func(tx *gorm.DB) error {
		if err := requireOwnedSession(tx, userID, sessionID); err != nil {
			return err
		}
		// Citations belong to their message, so they leave with the session. They
		// go first and are selected through the messages table, because after the
		// message rows are gone nothing names them anymore.
		if err := tx.Where("message_id IN (SELECT id FROM messages WHERE session_id = ?)", sessionID).
			Delete(&MessageSourceRecord{}).Error; err != nil {
			return err
		}
		if err := tx.Where("session_id = ?", sessionID).Delete(&MessageRecord{}).Error; err != nil {
			return err
		}
		// The rolling summary compresses this session's messages, so it goes with them:
		// a surviving row would point its boundary at a message that no longer exists.
		if err := tx.Where("session_id = ?", sessionID).Delete(&SessionSummaryRecord{}).Error; err != nil {
			return err
		}
		// The topic-memory consent is a permission over this conversation and nothing else, so
		// a surviving row would be a standing "yes" with no conversation to act on - and the
		// next conversation the learner starts must not inherit a decision they made about a
		// different one.
		if err := tx.Where("session_id = ?", sessionID).Delete(&TopicMemoryRecord{}).Error; err != nil {
			return err
		}
		// The episodic card and its projection job are derived from this
		// conversation and die with it, in the same transaction: a card whose
		// source session is gone must not outlive the delete by even one
		// crashed step. The derived retrieval view is cleaned separately and
		// best-effort by the caller - every read re-checks the store, so a
		// lagging view costs recall, never correctness.
		if err := tx.Where("user_id = ? AND session_id = ?", userID, sessionID).
			Delete(&EpisodeJobRecord{}).Error; err != nil {
			return err
		}
		if err := tx.Where("user_id = ? AND session_id = ?", userID, sessionID).
			Delete(&EpisodeRecord{}).Error; err != nil {
			return err
		}
		return tx.Where("id = ? AND user_id = ?", sessionID, userID).Delete(&SessionRecord{}).Error
	})
	return translate(err)
}

// AppendMessage appends one message to an owned session, stores its citations if
// it has any, and refreshes the session activity timestamp - all in one
// transaction. The citation rows are written with the message because half a
// grounded answer is worse than a failed one: an answer saved without its sources
// reads as a claim with nothing behind it.
func (r *Repository) AppendMessage(ctx context.Context, userID, sessionID string, message *domain.Message) (*domain.Message, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if r.db == nil || message == nil {
		return nil, repository.ErrInvalidArgument
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(sessionID) == "" {
		return nil, repository.ErrInvalidArgument
	}

	record := MessageRecord{
		ID:        message.ID,
		SessionID: sessionID,
		Role:      message.Role,
		Content:   message.Content,
		CreatedAt: message.CreatedAt.UTC(),
	}
	if record.ID == "" {
		record.ID = newID("message")
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}
	storedAt := record.CreatedAt
	sources := messageSourceRecords(record.ID, message.Sources)

	err := r.transaction(ctx, func(tx *gorm.DB) error {
		if err := requireOwnedSession(tx, userID, sessionID); err != nil {
			return err
		}
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		if len(sources) > 0 {
			if err := tx.Create(&sources).Error; err != nil {
				return err
			}
		}
		// UpdateColumn keeps GORM from substituting its own timestamp, so the
		// session and the message agree on when this turn happened.
		result := tx.Model(&SessionRecord{}).
			Where("id = ? AND user_id = ?", sessionID, userID).
			UpdateColumn("updated_at", storedAt)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return repository.ErrNotFound
		}
		return nil
	})
	if err := translate(err); err != nil {
		return nil, err
	}
	saved := record.toDomain()
	saved.Sources = domain.CloneSources(message.Sources)
	return &saved, nil
}

// ListMessages returns copies of the owned session's messages in append order,
// each with the citations it was stored with.
func (r *Repository) ListMessages(ctx context.Context, userID, sessionID string) ([]domain.Message, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if r.db == nil || strings.TrimSpace(userID) == "" || strings.TrimSpace(sessionID) == "" {
		return nil, repository.ErrInvalidArgument
	}

	var records []MessageRecord
	var sourceRecords []MessageSourceRecord
	err := r.transaction(ctx, func(tx *gorm.DB) error {
		if err := requireOwnedSession(tx, userID, sessionID); err != nil {
			return err
		}
		if err := tx.Where("session_id = ?", sessionID).Order(orderByCreation).Find(&records).Error; err != nil {
			return err
		}
		if len(records) == 0 {
			return nil
		}
		// One read for every citation of the session, then attached per message:
		// a query per message would make history cost N+1 round trips.
		ids := make([]string, 0, len(records))
		for _, record := range records {
			ids = append(ids, record.ID)
		}
		return tx.Where("message_id IN ?", ids).Order(orderByPosition).Find(&sourceRecords).Error
	})
	if err := translate(err); err != nil {
		return nil, err
	}
	byMessage := make(map[string][]domain.Source, len(sourceRecords))
	for _, sourceRecord := range sourceRecords {
		byMessage[sourceRecord.MessageID] = append(byMessage[sourceRecord.MessageID], sourceRecord.toDomain())
	}
	messages := make([]domain.Message, 0, len(records))
	for _, record := range records {
		message := record.toDomain()
		message.Sources = byMessage[record.ID]
		messages = append(messages, message)
	}
	return messages, nil
}

const orderByCreation = "created_at ASC, id ASC"
const orderByPosition = "message_id ASC, position ASC"

func (r *Repository) transaction(ctx context.Context, fn func(tx *gorm.DB) error) error {
	return r.db.WithContext(ctx).Transaction(fn)
}

func requireUser(tx *gorm.DB, userID string) error {
	var count int64
	if err := tx.Model(&UserRecord{}).Where("id = ?", userID).Count(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func requireOwnedSession(tx *gorm.DB, userID, sessionID string) error {
	var count int64
	if err := tx.Model(&SessionRecord{}).
		Where("id = ? AND user_id = ?", sessionID, userID).
		Count(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		return repository.ErrNotFound
	}
	return nil
}

func checkContext(ctx context.Context) error {
	if ctx == nil {
		return repository.ErrInvalidArgument
	}
	return ctx.Err()
}

// GetSessionSummary reads the session's rolling summary. A session that does not
// exist, one owned by another user, and one that has never been summarized all
// return ErrNotFound: the three must not be distinguishable from here.
func (r *Repository) GetSessionSummary(ctx context.Context, userID, sessionID string) (*domain.SessionSummary, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if r.db == nil || strings.TrimSpace(userID) == "" || strings.TrimSpace(sessionID) == "" {
		return nil, repository.ErrInvalidArgument
	}

	var record SessionSummaryRecord
	err := r.transaction(ctx, func(tx *gorm.DB) error {
		if err := requireOwnedSession(tx, userID, sessionID); err != nil {
			return err
		}
		return tx.Where("session_id = ?", sessionID).Take(&record).Error
	})
	if err != nil {
		return nil, translate(err)
	}
	summary := record.toDomain()
	return &summary, nil
}

// CommitSessionSummary is the optimistic lock the conversation service refreshes
// under. The update is conditioned on the version the caller read, in one statement,
// so two concurrent commits cannot both land: whichever loses the race updates zero
// rows and is told to re-read rather than rolling the winner's text back.
//
// Creating the first row is the same rule expressed as a key: the session id is the
// primary key, so a second first-commit is a duplicate key, which translate reports
// as the same ErrConflict.
func (r *Repository) CommitSessionSummary(ctx context.Context, summary *domain.SessionSummary, expectVersion int64) (*domain.SessionSummary, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if summary == nil || strings.TrimSpace(summary.UserID) == "" || strings.TrimSpace(summary.SessionID) == "" ||
		strings.TrimSpace(summary.Content) == "" || strings.TrimSpace(summary.CoveredMessageID) == "" ||
		expectVersion < 0 {
		return nil, repository.ErrInvalidArgument
	}

	committed := *summary
	err := r.transaction(ctx, func(tx *gorm.DB) error {
		if err := requireOwnedSession(tx, committed.UserID, committed.SessionID); err != nil {
			return err
		}
		if expectVersion == 0 {
			var existing int64
			if err := tx.Model(&SessionSummaryRecord{}).
				Where("session_id = ?", committed.SessionID).Count(&existing).Error; err != nil {
				return err
			}
			if existing > 0 {
				return repository.ErrConflict
			}
			record := SessionSummaryRecord{
				SessionID:        committed.SessionID,
				UserID:           committed.UserID,
				Content:          committed.Content,
				CoveredMessageID: committed.CoveredMessageID,
				Version:          1,
				UpdatedAt:        time.Now().UTC(),
			}
			if err := tx.Create(&record).Error; err != nil {
				return err
			}
			committed.Version = record.Version
			committed.UpdatedAt = record.UpdatedAt.UTC()
			return nil
		}

		result := tx.Model(&SessionSummaryRecord{}).
			Where("session_id = ? AND version = ?", committed.SessionID, expectVersion).
			Updates(map[string]any{
				"content":            committed.Content,
				"covered_message_id": committed.CoveredMessageID,
				"version":            expectVersion + 1,
				"updated_at":         time.Now().UTC(),
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return repository.ErrConflict
		}
		committed.Version = expectVersion + 1
		var record SessionSummaryRecord
		if err := tx.Where("session_id = ?", committed.SessionID).Take(&record).Error; err != nil {
			return err
		}
		committed.UpdatedAt = record.UpdatedAt.UTC()
		return nil
	})
	if err != nil {
		return nil, translate(err)
	}
	return &committed, nil
}

// translate maps driver and GORM errors onto the repository error vocabulary.
// Duplicate keys are matched beyond gorm.ErrDuplicatedKey because the shipped
// error type differs per driver: MySQL reports error 1062 through its own
// error struct, while other engines only surface a text message.
func translate(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, repository.ErrNotFound),
		errors.Is(err, repository.ErrConflict),
		errors.Is(err, repository.ErrInvalidArgument),
		errors.Is(err, repository.ErrMemoryProvenanceMissing),
		// The three learning refusals that are not conflicts have to survive
		// untranslated, or a caller that distinguishes "fix your code" from "that quiz
		// is over" would see neither. The rest wrap ErrConflict and already pass.
		errors.Is(err, repository.ErrLearningTransition),
		errors.Is(err, repository.ErrLearningQuestionOrder),
		errors.Is(err, repository.ErrLearningQuizClosed),
		errors.Is(err, errCorruptStoredData):
		return err
	case errors.Is(err, gorm.ErrDuplicatedKey), isDuplicateKey(err):
		return repository.ErrConflict
	case errors.Is(err, gorm.ErrRecordNotFound):
		return repository.ErrNotFound
	default:
		return err
	}
}

func isDuplicateKey(err error) bool {
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) {
		return mysqlErr.Number == 1062
	}
	message := err.Error()
	return strings.Contains(message, "Duplicate entry") ||
		strings.Contains(message, "UNIQUE constraint failed")
}
