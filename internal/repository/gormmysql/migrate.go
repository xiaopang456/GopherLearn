package gormmysql

import (
	"context"
	"fmt"

	"gorm.io/gorm"

	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// Migrate creates or extends the phase-1 conversation tables plus the citation
// rows that belong to an assistant message. GORM's AutoMigrate is
// additive: it creates missing tables, columns and indexes and never drops or
// rewrites existing rows, so running it against a populated database keeps the
// data. It returns as soon as any statement fails, which leaves the schema
// unchanged for the tables that were already complete.
func Migrate(ctx context.Context, db *gorm.DB) error {
	if ctx == nil || db == nil {
		return repository.ErrInvalidArgument
	}
	models := []any{&UserRecord{}, &SessionRecord{}, &MessageRecord{}, &MessageSourceRecord{}, &SessionSummaryRecord{},
		&TopicMemoryRecord{},
		&MemoryRecord{}, &MemorySourceRecord{},
		&LearningSessionRecord{}, &LearningQuestionRecord{}, &LearningAttemptRecord{}, &TopicProgressRecord{},
		&EpisodeRecord{}, &EpisodeJobRecord{}}
	if err := db.WithContext(ctx).AutoMigrate(models...); err != nil {
		return fmt.Errorf("repository/gormmysql: migrate: %w", err)
	}
	return nil
}
