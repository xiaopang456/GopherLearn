package mysql

import (
	"GopherAI/internal/testdb"
	"GopherAI/model"
	"testing"
	"time"

	mysqldriver "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type legacySession struct {
	ID        string `gorm:"primaryKey;type:varchar(36)"`
	UserName  string `gorm:"index;not null"`
	Title     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (legacySession) TableName() string { return "sessions" }

type legacyLongTermMemory struct {
	ID, Subject, TopicKey, Durability, Status, TombstoneHash string
	UserID                                                   int64
	MemoryType                                               string `gorm:"type:varchar(32);not null"`
	Content                                                  string `gorm:"type:text"`
	ContentHash, OriginContentHash                           string
	StructuredJSON                                           string           `gorm:"type:json;not null"`
	Tags, SourceRefs                                         model.StringList `gorm:"type:json;not null"`
	Confidence                                               float64
	Importance                                               int
	Verified, UserConfirmed                                  bool
	LastUsedAt, DeletedAt                                    *time.Time
	UsageCount                                               uint64
	CreatedAt, UpdatedAt                                     time.Time
}

func (legacyLongTermMemory) TableName() string { return "long_term_memories" }

type legacyMemoryCandidate struct {
	ID                                                        string `gorm:"primaryKey;type:varchar(36)"`
	JobID, Subject, ContentHash, TopicKey, Durability, Status string
	UserID                                                    int64
	MemoryType                                                string
	ProposedContent                                           string           `gorm:"type:text"`
	StructuredJSON                                            string           `gorm:"type:json;not null"`
	Tags, SourceRefs                                          model.StringList `gorm:"type:json;not null"`
	Confidence                                                float64
	Importance                                                int
	Sensitivity, DreamRunID, RejectionReason                  string
	CreatedAt, UpdatedAt                                      time.Time
	ProcessedAt                                               *time.Time
}

func (legacyMemoryCandidate) TableName() string { return "memory_candidates" }

func TestMigrateBackfillsLegacySessionOwnershipAndMode(t *testing.T) {
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []interface{}{&model.MemoryRevision{}, &model.MemoryDreamRun{}, &legacyMemoryCandidate{}, &model.MemoryExtractionJob{}, &legacyLongTermMemory{}, &model.EpisodeProjectionJob{}, &model.EpisodeMemory{}, &model.LearningAttempt{}, &model.LearningQuestion{}, &model.LearningSession{}, &model.AgentStep{}, &model.AgentRun{}, &model.Message{}, &legacySession{}, &model.KnowledgeDocument{}, &model.User{}} {
		_ = db.Migrator().DropTable(table)
	}
	if err := db.AutoMigrate(&model.User{}, &legacySession{}, &legacyLongTermMemory{}, &legacyMemoryCandidate{}, &model.MemoryRevision{}); err != nil {
		t.Fatal(err)
	}
	user := model.User{Username: "legacy-user", Password: "secret"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	secondBlankEmailUser := model.User{Username: "legacy-user-blank-two", Password: "secret"}
	if err := db.Create(&secondBlankEmailUser).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&legacySession{ID: "legacy-session", UserName: user.Username, Title: "old chat"}).Error; err != nil {
		t.Fatal(err)
	}
	created := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second)
	legacyRows := []legacyLongTermMemory{
		{ID: "legacy-profile", UserID: user.ID, MemoryType: "profile", Subject: "teaching", Content: "先讲链路", ContentHash: model.MemoryContentHash("先讲链路"), StructuredJSON: `{}`, Tags: model.StringList{}, SourceRefs: model.StringList{"source:test"}, Confidence: .9, Importance: 8, Durability: "stable", Status: "active", UserConfirmed: true, CreatedAt: created, UpdatedAt: created},
		{ID: "legacy-topic", UserID: user.ID, MemoryType: "topic_note", Subject: "RPC", Content: "粘包不是丢包", ContentHash: model.MemoryContentHash("粘包不是丢包"), StructuredJSON: `{}`, Tags: model.StringList{"rpc"}, SourceRefs: model.StringList{"source:test"}, Confidence: .9, Importance: 8, TopicKey: "rpc-粘包", Status: "active", CreatedAt: created, UpdatedAt: created},
		{ID: "legacy-frozen", UserID: user.ID, MemoryType: "experience", Subject: "old", Content: "旧正文必须保留", ContentHash: model.MemoryContentHash("旧正文必须保留"), StructuredJSON: `{}`, Tags: model.StringList{}, SourceRefs: model.StringList{"source:test"}, Confidence: .8, Importance: 6, Status: "active", CreatedAt: created, UpdatedAt: created},
		{ID: "legacy-deleted", UserID: user.ID, MemoryType: "topic_note", ContentHash: model.MemoryContentHash("deleted"), StructuredJSON: `{}`, Tags: model.StringList{}, SourceRefs: model.StringList{}, Confidence: .8, Importance: 6, Status: "deleted", TombstoneHash: model.MemoryContentHash("deleted"), DeletedAt: &created, CreatedAt: created, UpdatedAt: created},
	}
	if err := db.Create(&legacyRows).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&legacyMemoryCandidate{ID: "legacy-candidate", JobID: "legacy-job", UserID: user.ID, MemoryType: "topic_note", Subject: "candidate", ProposedContent: "候选正文", ContentHash: model.MemoryContentHash("候选正文"), StructuredJSON: `{}`, Tags: model.StringList{"rpc"}, SourceRefs: model.StringList{"source:test"}, Confidence: .8, Importance: 6, Sensitivity: "safe", TopicKey: "rpc", Status: "pending", CreatedAt: created, UpdatedAt: created}).Error; err != nil {
		t.Fatal(err)
	}
	revision := model.MemoryRevision{ID: "legacy-revision", UserID: user.ID, MemoryID: "legacy-profile", Action: model.MemoryUserCorrect, Actor: model.MemoryRevisionUser, Reason: "keep audit", BeforeSnapshot: `{"type":"profile","content":"before"}`, AfterSnapshot: `{"type":"profile","content":"after"}`, CreatedAt: created}
	if err := db.Create(&revision).Error; err != nil {
		t.Fatal(err)
	}

	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasColumn("users", "email_normalized") {
		t.Fatal("normalized email generated column was not created")
	}
	var normalizedIndexCount int64
	if err := db.Raw(`
		SELECT COUNT(*) FROM information_schema.statistics
		WHERE table_schema = DATABASE()
		  AND table_name = 'users'
		  AND index_name = 'uidx_users_email_normalized'
		  AND non_unique = 0
	`).Scan(&normalizedIndexCount).Error; err != nil {
		t.Fatal(err)
	}
	if normalizedIndexCount != 1 {
		t.Fatalf("normalized unique index count=%d", normalizedIndexCount)
	}
	emailUser := model.User{Username: "email-user", Email: "case@example.com", Password: "legacy-hash"}
	if err := db.Create(&emailUser).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.User{Username: "email-duplicate", Email: " CASE@EXAMPLE.COM ", Password: "hash"}).Error; err == nil {
		t.Fatal("normalized duplicate email was inserted")
	}
	previousDB := DB
	DB = db
	t.Cleanup(func() { DB = previousDB })
	foundByEmail, err := GetUserByLogin("case@example.com")
	if err != nil || foundByEmail.ID != emailUser.ID {
		t.Fatalf("find by normalized email user=%+v err=%v", foundByEmail, err)
	}
	updated, err := UpdateUserPasswordIfMatch(emailUser.ID, "legacy-hash", "bcrypt-hash")
	if err != nil || !updated {
		t.Fatalf("conditional password upgrade updated=%v err=%v", updated, err)
	}
	updated, err = UpdateUserPasswordIfMatch(emailUser.ID, "legacy-hash", "other-hash")
	if err != nil || updated {
		t.Fatalf("stale password hash update updated=%v err=%v", updated, err)
	}
	var migrated model.Session
	if err := db.First(&migrated, "id = ?", "legacy-session").Error; err != nil {
		t.Fatal(err)
	}
	if migrated.UserID != user.ID {
		t.Fatalf("migrated user_id = %d, want %d", migrated.UserID, user.ID)
	}
	if migrated.Mode != model.SessionModeNormal {
		t.Fatalf("migrated mode = %q, want %q", migrated.Mode, model.SessionModeNormal)
	}
	chatRun := model.AgentRun{ID: "migration-chat-run", UserID: user.ID, SessionID: migrated.ID, ExecutionKind: model.AgentRunExecutionChat, Status: model.AgentRunCompleted, Version: 1, StartedAt: created}
	learningRun := model.AgentRun{ID: "migration-learning-run", UserID: user.ID, SessionID: migrated.ID, ExecutionKind: model.AgentRunExecutionChat, Status: model.AgentRunWaitingUser, Version: 1, StartedAt: created}
	if err := db.Create(&[]model.AgentRun{chatRun, learningRun}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.LearningSession{ID: "migration-learning", UserID: user.ID, SessionID: migrated.ID, AgentRunID: learningRun.ID, Topic: "migration", TopicKey: "migration", Status: model.LearningSessionWaitingAnswer, CurrentQuestion: 1, TotalQuestions: 3, Version: 1, ExpiresAt: time.Now().Add(time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	if err := backfillAgentRunExecutionKinds(db); err != nil {
		t.Fatal(err)
	}
	var backfilledChat, backfilledLearning model.AgentRun
	if err := db.First(&backfilledChat, "id = ?", chatRun.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&backfilledLearning, "id = ?", learningRun.ID).Error; err != nil {
		t.Fatal(err)
	}
	if backfilledChat.ExecutionKind != model.AgentRunExecutionChat || backfilledLearning.ExecutionKind != model.AgentRunExecutionLearning {
		t.Fatalf("execution kind backfill chat=%q learning=%q", backfilledChat.ExecutionKind, backfilledLearning.ExecutionKind)
	}
	var runMessageIndex int64
	if err := db.Raw(`
		SELECT COUNT(*) FROM information_schema.statistics
		WHERE table_schema = DATABASE()
		  AND table_name = 'messages'
		  AND index_name = 'idx_messages_agent_run_role'
		  AND non_unique = 0
	`).Scan(&runMessageIndex).Error; err != nil {
		t.Fatal(err)
	}
	if runMessageIndex != 2 {
		t.Fatalf("agent run message unique index columns=%d, want 2", runMessageIndex)
	}
	var global, topic, frozen model.LongTermMemory
	if err := db.First(&global, "id = ?", "legacy-profile").Error; err != nil {
		t.Fatal(err)
	}
	if global.Scope != model.MemoryScopeGlobal || global.Content != "先讲链路" || !global.UserConfirmed || global.Durability != model.MemoryDurabilityStable {
		t.Fatalf("global migration=%+v", global)
	}
	if err := db.First(&topic, "id = ?", "legacy-topic").Error; err != nil {
		t.Fatal(err)
	}
	if topic.Scope != model.MemoryScopeTopic || topic.TopicKey != "rpc-粘包" || topic.Durability != model.MemoryDurabilityStable || topic.Content != "粘包不是丢包" {
		t.Fatalf("topic migration=%+v", topic)
	}
	if err := db.First(&frozen, "id = ?", "legacy-frozen").Error; err != nil {
		t.Fatal(err)
	}
	if frozen.Scope != model.MemoryScopeLegacyExperience || frozen.Content != "旧正文必须保留" || frozen.Scope.Valid() {
		t.Fatalf("frozen migration=%+v", frozen)
	}
	var tombstone model.LongTermMemory
	if err := db.First(&tombstone, "id = ?", "legacy-deleted").Error; err != nil {
		t.Fatal(err)
	}
	if tombstone.Scope != model.MemoryScopeTopic || tombstone.TombstoneHash != model.MemoryContentHash("deleted") || tombstone.Content != "" || tombstone.DeletedAt == nil {
		t.Fatalf("tombstone migration=%+v", tombstone)
	}
	var candidate model.MemoryCandidate
	if err := db.First(&candidate, "id = ?", "legacy-candidate").Error; err != nil {
		t.Fatal(err)
	}
	if candidate.Scope != model.MemoryScopeTopic || candidate.Durability != model.MemoryDurabilityStable || candidate.ProposedContent != "候选正文" {
		t.Fatalf("candidate migration=%+v", candidate)
	}
	newContent := "迁移后的新写入不再双写退役字段"
	newMemory := model.LongTermMemory{ID: "new-memory", UserID: user.ID, Scope: model.MemoryScopeGlobal, Subject: "new", Content: newContent, ContentHash: model.MemoryContentHash(newContent), StructuredJSON: `{}`, Tags: model.StringList{}, Durability: model.MemoryDurabilityStable, SourceRefs: model.StringList{"user_api:test"}, Confidence: 1, Importance: 8, Status: model.LongTermMemoryActive, UserConfirmed: true, CreatedAt: time.Now()}
	if err := db.Create(&newMemory).Error; err != nil {
		t.Fatalf("new memory insert after legacy migration: %v", err)
	}
	var keptRevision model.MemoryRevision
	if err := db.First(&keptRevision, "id = ?", "legacy-revision").Error; err != nil {
		t.Fatal(err)
	}
	if keptRevision.BeforeSnapshot != revision.BeforeSnapshot || keptRevision.AfterSnapshot != revision.AfterSnapshot {
		t.Fatalf("revision changed=%+v", keptRevision)
	}
	if err := db.Migrator().DropTable(&model.MemoryCandidate{}, &model.LongTermMemory{}); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil {
		t.Fatalf("empty-schema migration: %v", err)
	}
	for _, table := range []string{"memory_candidates", "long_term_memories"} {
		if !db.Migrator().HasColumn(table, "memory_scope") || !db.Migrator().HasColumn(table, "valid_until") {
			t.Fatalf("empty-schema migration missed AGENT-027 columns on %s", table)
		}
	}
	if err := db.Exec(`DROP INDEX uidx_users_email_normalized ON users`).Error; err != nil {
		t.Fatal(err)
	}
	duplicateUsers := []model.User{
		{Username: "duplicate-email-a", Email: "duplicate-check@example.com", Password: "hash"},
		{Username: "duplicate-email-b", Email: " DUPLICATE-CHECK@EXAMPLE.COM ", Password: "hash"},
	}
	if err := db.Create(&duplicateUsers).Error; err != nil {
		t.Fatal(err)
	}
	if err := ensureUniqueNormalizedUserEmail(db); err == nil {
		t.Fatal("normalized duplicate email migration did not fail closed")
	}
	if err := db.Unscoped().Where("username IN ?", []string{"duplicate-email-a", "duplicate-email-b"}).Delete(&model.User{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := ensureUniqueNormalizedUserEmail(db); err != nil {
		t.Fatalf("restore normalized email index after duplicate cleanup: %v", err)
	}
}
