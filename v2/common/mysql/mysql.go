package mysql

import (
	"GopherAI/common/rag"
	"GopherAI/config"
	"GopherAI/model"
	"fmt"
	stdlog "log"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var DB *gorm.DB

func InitMysql() error {
	db, err := OpenConfigured()
	if err != nil {
		return err
	}
	DB = db
	return migration()
}

// OpenConfigured opens the configured database without running migrations or
// assigning the package global. Read-only maintenance commands use it so a
// dry-run cannot acquire schema-changing side effects from InitMysql.
func OpenConfigured() (*gorm.DB, error) {
	host := config.GetConfig().MysqlHost
	port := config.GetConfig().MysqlPort
	dbname := config.GetConfig().MysqlDatabaseName
	username := config.GetConfig().MysqlUser
	password := config.GetConfig().MysqlPassword
	charset := config.GetConfig().MysqlCharset

	//dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?charset=%s&parseTime=true&loc=Local", username, password, host, port, dbname, charset)
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=%s&parseTime=true&loc=Local", username, password, host, port, dbname, charset)

	logLevel := logger.Warn
	if gin.Mode() == "debug" {
		logLevel = logger.Info
	}
	gormLog := logger.New(stdlog.New(os.Stdout, "\r\n", stdlog.LstdFlags), logger.Config{
		SlowThreshold:             200 * time.Millisecond,
		LogLevel:                  logLevel,
		IgnoreRecordNotFoundError: false,
		ParameterizedQueries:      true,
		Colorful:                  gin.Mode() == "debug",
	})

	db, err := gorm.Open(mysql.New(mysql.Config{
		DSN:                       dsn,
		DefaultStringSize:         256,
		DisableDatetimePrecision:  true,
		DontSupportRenameIndex:    true,
		DontSupportRenameColumn:   true,
		SkipInitializeWithVersion: false,
	}), &gorm.Config{
		Logger: gormLog,
	})
	if err != nil {
		return nil, err
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxIdleConns(10)
	sqlDB.SetMaxOpenConns(100)
	sqlDB.SetConnMaxLifetime(time.Hour)

	return db, nil
}

func migration() error {
	return migrate(DB)
}

func migrate(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("mysql database is not initialized")
	}
	if err := prepareUserMemoryScopeMigration(db); err != nil {
		return err
	}
	if err := db.AutoMigrate(
		new(model.User),
		new(model.Session),
		new(model.Message),
		new(model.SessionSummary),
		new(model.EpisodeMemory),
		new(model.EpisodeProjectionJob),
		// Assertion governance is additive: the migration creates an empty
		// table and backfills nothing. A backfill would have to choose a state
		// for every legacy assertion, and both available choices are untrue -
		// which is why the absence of a row already means unknown.
		new(model.EpisodeAssertionState),
		// The write-side relation tables are additive in the same way. A
		// relation row is a pair's business and a proposal row records what a
		// classifier decided whether or not anything was applied; neither can
		// be backfilled, because no relation existed to backfill from.
		new(model.EpisodeAssertionRelation),
		new(model.EpisodeRelationProposal),
		new(model.KnowledgeDocument),
		new(model.AgentRun),
		new(model.AgentStep),
		new(model.LearningSession),
		new(model.LearningQuestion),
		new(model.LearningAttempt),
		new(model.TopicProgress),
		new(model.MemoryExtractionJob),
		new(model.MemoryCandidate),
		new(model.LongTermMemory),
		new(model.MemoryDreamRun),
		new(model.MemoryRevision),
		new(model.MemoryDraft),
	); err != nil {
		return err
	}
	if err := backfillAgentRunExecutionKinds(db); err != nil {
		return err
	}
	if err := backfillMessageAnswerStates(db); err != nil {
		return err
	}
	if err := ensureUniqueNormalizedUserEmail(db); err != nil {
		return err
	}
	if err := migrateUserMemoryScopes(db); err != nil {
		return err
	}
	if err := db.Model(&model.MemoryCandidate{}).
		Where("content_hash = '' OR content_hash IS NULL").
		Update("content_hash", gorm.Expr("SHA2(TRIM(proposed_content), 256)")).Error; err != nil {
		return fmt.Errorf("backfill memory candidate hashes: %w", err)
	}
	if err := db.Model(&model.LongTermMemory{}).
		Where("content_hash = '' OR content_hash IS NULL").
		Update("content_hash", gorm.Expr("SHA2(TRIM(content), 256)")).Error; err != nil {
		return fmt.Errorf("backfill long-term memory hashes: %w", err)
	}
	return backfillLegacySessions(db)
}

func backfillMessageAnswerStates(db *gorm.DB) error {
	if !db.Migrator().HasTable(&model.Message{}) {
		return nil
	}
	if err := db.Model(&model.Message{}).
		Where("is_user = ? AND (answer_basis = ? OR answer_basis = '' OR answer_basis IS NULL)", true, rag.AnswerBasisLegacyUnspecified).
		Updates(map[string]interface{}{
			"answer_basis": rag.AnswerBasisConversation, "knowledge_status": rag.KnowledgeStatusNotRequired, "knowledge_reason": rag.KnowledgeReasonNone,
		}).Error; err != nil {
		return fmt.Errorf("backfill user message answer states: %w", err)
	}
	if err := db.Model(&model.Message{}).
		Where("is_user = ? AND JSON_LENGTH(COALESCE(sources, JSON_ARRAY())) > 0 AND (answer_basis = ? OR answer_basis = '' OR answer_basis IS NULL)", false, rag.AnswerBasisLegacyUnspecified).
		Updates(map[string]interface{}{
			"answer_basis": rag.AnswerBasisPersonalKnowledge, "knowledge_status": rag.KnowledgeStatusGrounded, "knowledge_reason": rag.KnowledgeReasonNone,
		}).Error; err != nil {
		return fmt.Errorf("backfill grounded assistant message states: %w", err)
	}
	return nil
}

func backfillAgentRunExecutionKinds(db *gorm.DB) error {
	if !db.Migrator().HasTable(&model.AgentRun{}) || !db.Migrator().HasTable(&model.LearningSession{}) {
		return nil
	}
	if err := db.Model(&model.AgentRun{}).
		Where("execution_kind = '' OR execution_kind IS NULL").
		Update("execution_kind", model.AgentRunExecutionChat).Error; err != nil {
		return fmt.Errorf("backfill agent chat execution kinds: %w", err)
	}
	if err := db.Exec(`
		UPDATE agent_runs AS runs
		JOIN learning_sessions AS learning ON learning.agent_run_id = runs.id
		SET runs.execution_kind = ?
		WHERE runs.execution_kind <> ?
	`, model.AgentRunExecutionLearning, model.AgentRunExecutionLearning).Error; err != nil {
		return fmt.Errorf("backfill learning run execution kinds: %w", err)
	}
	return nil
}

func ensureUniqueNormalizedUserEmail(db *gorm.DB) error {
	if !db.Migrator().HasTable(&model.User{}) {
		return nil
	}
	var duplicateGroups int64
	if err := db.Raw(`
		SELECT COUNT(*) FROM (
			SELECT LOWER(TRIM(email)) AS normalized_email
			FROM users
			WHERE email IS NOT NULL AND TRIM(email) <> ''
			GROUP BY LOWER(TRIM(email))
			HAVING COUNT(*) > 1
		) AS duplicate_emails
	`).Scan(&duplicateGroups).Error; err != nil {
		return fmt.Errorf("inspect duplicate normalized user emails: %w", err)
	}
	if duplicateGroups > 0 {
		return fmt.Errorf("refusing user email uniqueness migration: %d normalized email groups conflict", duplicateGroups)
	}
	if !db.Migrator().HasColumn("users", "email_normalized") {
		if err := db.Exec(`
			ALTER TABLE users
			ADD COLUMN email_normalized VARCHAR(100)
			GENERATED ALWAYS AS (NULLIF(LOWER(TRIM(email)), '')) STORED
		`).Error; err != nil {
			return fmt.Errorf("add normalized user email column: %w", err)
		}
	}
	var generatedColumnCount int64
	if err := db.Raw(`
		SELECT COUNT(*)
		FROM information_schema.columns
		WHERE table_schema = DATABASE()
		  AND table_name = 'users'
		  AND column_name = 'email_normalized'
		  AND extra LIKE '%STORED GENERATED%'
	`).Scan(&generatedColumnCount).Error; err != nil {
		return fmt.Errorf("inspect normalized user email column: %w", err)
	}
	if generatedColumnCount != 1 {
		return fmt.Errorf("refusing user email uniqueness migration: email_normalized is not a stored generated column")
	}
	var indexCount, uniqueIndexCount int64
	if err := db.Raw(`
		SELECT COUNT(*), COALESCE(SUM(non_unique = 0), 0)
		FROM information_schema.statistics
		WHERE table_schema = DATABASE()
		  AND table_name = 'users'
		  AND index_name = 'uidx_users_email_normalized'
	`).Row().Scan(&indexCount, &uniqueIndexCount); err != nil {
		return fmt.Errorf("inspect normalized user email index: %w", err)
	}
	if indexCount > 0 && uniqueIndexCount == 0 {
		return fmt.Errorf("refusing user email uniqueness migration: normalized email index exists but is not unique")
	}
	if indexCount == 0 {
		if err := db.Exec(`CREATE UNIQUE INDEX uidx_users_email_normalized ON users (email_normalized)`).Error; err != nil {
			return fmt.Errorf("create normalized user email index: %w", err)
		}
	}
	return nil
}

func prepareUserMemoryScopeMigration(db *gorm.DB) error {
	for _, table := range []string{"memory_candidates", "long_term_memories"} {
		if !db.Migrator().HasTable(table) {
			continue
		}
		if !db.Migrator().HasColumn(table, "memory_scope") {
			if err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN memory_scope VARCHAR(32) NULL", table)).Error; err != nil {
				return fmt.Errorf("add %s memory scope: %w", table, err)
			}
		}
		if !db.Migrator().HasColumn(table, "valid_until") {
			if err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN valid_until DATETIME NULL", table)).Error; err != nil {
				return fmt.Errorf("add %s valid_until: %w", table, err)
			}
		}
	}
	return migrateUserMemoryScopes(db)
}

// migrateUserMemoryScopes is intentionally data-aware. AutoMigrate can add the
// new columns, but it cannot translate the two AGENT-018 values or keep the
// four retired values frozen. Keeping the old memory_type values in place also
// makes the migration rollback/audit friendly; the column becomes nullable so
// new code can write memory_scope without dual-writing a retired field.
func migrateUserMemoryScopes(db *gorm.DB) error {
	for _, table := range []string{"memory_candidates", "long_term_memories"} {
		if !db.Migrator().HasTable(table) || !db.Migrator().HasColumn(table, "memory_type") {
			continue
		}
		if err := db.Exec(fmt.Sprintf(`
			UPDATE %s
			SET memory_scope = CASE memory_type
				WHEN 'profile' THEN 'global'
				WHEN 'topic_note' THEN 'topic'
				ELSE memory_type
			END
			WHERE memory_scope IS NULL OR memory_scope = ''
		`, table)).Error; err != nil {
			return fmt.Errorf("backfill %s memory scopes: %w", table, err)
		}
		if err := db.Exec(fmt.Sprintf(`
			UPDATE %s
			SET durability = 'stable'
			WHERE memory_scope = 'topic' AND (durability IS NULL OR durability = '')
		`, table)).Error; err != nil {
			return fmt.Errorf("backfill %s topic durability: %w", table, err)
		}
		// Old time_bound rows had no executable expiry. Inventing a future product
		// deadline would silently keep stale goals active, so give them the
		// smallest valid historical interval. The recall predicate excludes them
		// immediately while preserving their content and audit rows.
		if err := db.Exec(fmt.Sprintf(`
			UPDATE %s
			SET valid_until = DATE_ADD(created_at, INTERVAL 1 SECOND)
			WHERE durability = 'time_bound' AND valid_until IS NULL
		`, table)).Error; err != nil {
			return fmt.Errorf("backfill %s time-bound expiry: %w", table, err)
		}
		if err := db.Exec(fmt.Sprintf("ALTER TABLE %s MODIFY COLUMN memory_type VARCHAR(32) NULL", table)).Error; err != nil {
			return fmt.Errorf("make legacy %s memory type nullable: %w", table, err)
		}
	}
	return nil
}

func backfillLegacySessions(db *gorm.DB) error {
	if err := db.Exec(`
		UPDATE sessions AS sessions
		JOIN users AS users ON users.username = sessions.user_name
		SET sessions.user_id = users.id
		WHERE sessions.user_id = 0
	`).Error; err != nil {
		return fmt.Errorf("backfill legacy session user ids: %w", err)
	}
	if err := db.Model(&model.Session{}).
		Where("mode IS NULL OR mode = ''").
		Update("mode", model.SessionModeNormal).Error; err != nil {
		return fmt.Errorf("backfill legacy session modes: %w", err)
	}
	return nil
}

func InsertUser(user *model.User) (*model.User, error) {
	err := DB.Create(&user).Error
	return user, err
}

func GetUserByUsername(username string) (*model.User, error) {
	user := new(model.User)
	err := DB.Where("username = ?", username).First(user).Error
	return user, err
}

func GetUserByLogin(identifier string) (*model.User, error) {
	user := new(model.User)
	err := DB.Where("username = ? OR email_normalized = ?", identifier, identifier).First(user).Error
	return user, err
}

func UserEmailExists(email string) (bool, error) {
	var count int64
	err := DB.Model(&model.User{}).Where("email_normalized = ?", email).Count(&count).Error
	return count > 0, err
}

func UpdateUserPasswordIfMatch(userID int64, expectedHash, replacementHash string) (bool, error) {
	result := DB.Model(&model.User{}).
		Where("id = ? AND password = ?", userID, expectedHash).
		Update("password", replacementHash)
	return result.RowsAffected == 1, result.Error
}
