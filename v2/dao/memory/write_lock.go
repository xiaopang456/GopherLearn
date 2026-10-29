package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"gorm.io/gorm"
)

var ErrMemoryWriteBusy = errors.New("memory write boundary is busy")

const memoryWriteLockTimeoutSeconds = 5

// withUserMemoryLock serializes durable-memory mutations for one user across
// application instances. GORM Connection pins GET_LOCK, the transaction, and
// RELEASE_LOCK to the same MySQL connection.
func withUserMemoryLock(ctx context.Context, db *gorm.DB, userID int64, fn func(*gorm.DB) error) error {
	if db == nil || userID <= 0 || fn == nil {
		return fmt.Errorf("invalid memory write lock request")
	}
	lockName := fmt.Sprintf("gopherai:memory:user:%d", userID)
	return db.WithContext(ctx).Connection(func(conn *gorm.DB) error {
		var acquired sql.NullInt64
		if err := conn.Raw("SELECT GET_LOCK(?, ?)", lockName, memoryWriteLockTimeoutSeconds).Row().Scan(&acquired); err != nil {
			return fmt.Errorf("acquire memory write lock: %w", err)
		}
		if !acquired.Valid || acquired.Int64 != 1 {
			return ErrMemoryWriteBusy
		}
		defer func() {
			var released sql.NullInt64
			_ = conn.Raw("SELECT RELEASE_LOCK(?)", lockName).Row().Scan(&released)
		}()
		return fn(conn)
	})
}
