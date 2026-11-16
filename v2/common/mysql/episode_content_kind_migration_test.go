package mysql

import (
	"GopherAI/internal/testdb"
	"GopherAI/model"
	"fmt"
	"testing"
	"time"

	mysqldriver "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// The card path put two kinds of derived row in one table, and the
// migration is what made that safe. These are the two halves of it: the
// content column has to be able to hold a row that carries no Episode
// document, and the uniqueness has to become per content kind rather than
// per session.

// openMigrationTestDB opens the opt-in test database.
func openMigrationTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent), TranslateError: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// The content column is relaxed to nullable before AutoMigrate runs, so the
// card row can leave it empty instead of holding a placeholder that would
// read as an Episode.
func TestPrepareEpisodeContentKindMigrationMakesTheContentColumnNullable(t *testing.T) {
	db := openMigrationTestDB(t)
	if err := db.Migrator().DropTable(&model.EpisodeMemory{}); err != nil {
		t.Fatal(err)
	}
	// A table with the column as it was before the card path existed: not
	// null, with a default.
	if err := db.Exec(`
		CREATE TABLE episode_memories (
			id VARCHAR(36) NOT NULL PRIMARY KEY,
			user_id BIGINT NOT NULL,
			session_id VARCHAR(36) NOT NULL,
			title VARCHAR(255) NOT NULL,
			content_json JSON NOT NULL,
			search_text TEXT NOT NULL,
			topic_keys JSON NOT NULL,
			source_episodes JSON NOT NULL,
			source_summary_watermark BIGINT NOT NULL DEFAULT 0,
			source_summary_version BIGINT NOT NULL DEFAULT 0,
			source_last_message_id BIGINT NOT NULL DEFAULT 0,
			last_message_at DATETIME NULL,
			created_at DATETIME NULL,
			updated_at DATETIME NULL,
			version BIGINT NOT NULL DEFAULT 0,
			status VARCHAR(16) NOT NULL DEFAULT '',
			index_status VARCHAR(16) NOT NULL DEFAULT '',
			abstain_reason TEXT
		)
	`).Error; err != nil {
		t.Fatal(err)
	}
	var nullable string
	if err := db.Raw(`
		SELECT is_nullable FROM information_schema.columns
		WHERE table_schema = DATABASE() AND table_name = 'episode_memories'
		  AND column_name = 'content_json'
	`).Row().Scan(&nullable); err != nil {
		t.Fatal(err)
	}
	if nullable == "YES" {
		t.Fatal("the fixture is wrong: the column is already nullable")
	}
	if err := prepareEpisodeContentKindMigration(db); err != nil {
		t.Fatal(err)
	}
	if err := db.Raw(`
		SELECT is_nullable FROM information_schema.columns
		WHERE table_schema = DATABASE() AND table_name = 'episode_memories'
		  AND column_name = 'content_json'
	`).Row().Scan(&nullable); err != nil {
		t.Fatal(err)
	}
	if nullable != "YES" {
		t.Fatalf("the content column is still %q", nullable)
	}
	// Running it again is harmless: the migration has to be safe to run on
	// every start-up.
	if err := prepareEpisodeContentKindMigration(db); err != nil {
		t.Fatal(err)
	}
}

// The uniqueness becomes per content kind, and the old index is only dropped
// once the new one exists.
func TestEnsureEpisodeContentKindUniquenessSwapsTheIndexInOrder(t *testing.T) {
	db := openMigrationTestDB(t)
	if err := db.Migrator().DropTable(&model.EpisodeMemory{}); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.EpisodeMemory{}); err != nil {
		t.Fatal(err)
	}
	// The old index is what the table had before the card path existed.
	if err := db.Exec(`
		CREATE UNIQUE INDEX idx_episode_owner_session
		ON episode_memories (user_id, session_id)
	`).Error; err != nil {
		t.Fatal(err)
	}
	if err := ensureEpisodeContentKindUniqueness(db); err != nil {
		t.Fatal(err)
	}
	var oldIndexCount, newIndexCount int64
	if err := db.Raw(`
		SELECT COUNT(*) FROM information_schema.statistics
		WHERE table_schema = DATABASE() AND table_name = 'episode_memories'
		  AND index_name = 'idx_episode_owner_session'
	`).Row().Scan(&oldIndexCount); err != nil {
		t.Fatal(err)
	}
	if err := db.Raw(`
		SELECT COUNT(*) FROM information_schema.statistics
		WHERE table_schema = DATABASE() AND table_name = 'episode_memories'
		  AND index_name = 'idx_episode_owner_session_kind'
	`).Row().Scan(&newIndexCount); err != nil {
		t.Fatal(err)
	}
	if oldIndexCount != 0 {
		t.Fatalf("the old index is still there: %d", oldIndexCount)
	}
	if newIndexCount == 0 {
		t.Fatal("the new index is missing")
	}
	// The whole point of the swap: one session may now hold both an Episode
	// and a card, and neither overwrites the other.
	// The names are unique per run: the test database is shared by every
	// package that wants it, and a second run must not walk into the rows
	// the first one left behind.
	runID := fmt.Sprintf("%x", time.Now().UnixNano())
	user := model.User{Username: "mig-owner-" + runID, Password: "secret"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	// The rows belong to a session: the table carries the session as a
	// foreign key, so a row without one is refused by the server rather
	// than by the code.
	sessionID := "mig-sess-" + runID
	session := model.Session{ID: sessionID, UserID: user.ID,
		UserName: user.Username, Mode: model.SessionModeAgent, Title: "迁移测试"}
	if err := db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	// The last message time is not decoration: the column is not nullable and
	// the model's own validation rejects a zero one. A row written without it
	// is a row the database refuses on a strict server, so the fixture
	// carries the same time the product's writer would.
	stamp := time.Now().UTC().Truncate(time.Second)
	episode := model.EpisodeMemory{
		ID: "mig-ep-" + runID, UserID: user.ID, SessionID: sessionID,
		ContentKind: model.EpisodeContentEpisode, Title: "一个 Episode",
		Document:   model.EpisodeDocument{Synopsis: "一段概述", Topics: []model.EpisodeTopic{}},
		SearchText: "一段概述", TopicKeys: model.StringList{}, SourceEpisodes: model.EpisodeSourceRefs{},
		SourceLastMessageID: 1, LastMessageAt: stamp, Version: 1, Status: model.EpisodeStatusActive,
		IndexStatus: model.EpisodeIndexIndexed,
	}
	card := model.EpisodeMemory{
		ID: "mig-card-" + runID, UserID: user.ID, SessionID: sessionID,
		ContentKind: model.EpisodeContentSessionCard, Title: "一张卡片",
		Card: model.SessionCardDocument{
			Version: model.SessionCardContentVersion, Title: "一张卡片", Synopsis: "一段简述",
		},
		SearchText: "一张卡片", TopicKeys: model.StringList{}, SourceEpisodes: model.EpisodeSourceRefs{},
		SourceLastMessageID: 1, LastMessageAt: stamp, Version: 1, Status: model.EpisodeStatusActive,
		IndexStatus: model.EpisodeIndexIndexed,
	}
	if err := db.Create(&episode).Error; err != nil {
		t.Fatalf("the episode row was written: %v", err)
	}
	if err := db.Create(&card).Error; err != nil {
		t.Fatalf("the card row could not be written beside the episode row: %v", err)
	}
}

// The swap refuses to drop the old index when the new one is missing.
//
// A table whose rows could collide is a state this function cannot repair by
// dropping anything, and refusing to guess is better than dropping the only
// uniqueness the table has.
func TestEnsureEpisodeContentKindUniquenessRefusesWithoutTheNewIndex(t *testing.T) {
	db := openMigrationTestDB(t)
	if err := db.Migrator().DropTable(&model.EpisodeMemory{}); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.EpisodeMemory{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`
		CREATE UNIQUE INDEX idx_episode_owner_session
		ON episode_memories (user_id, session_id)
	`).Error; err != nil {
		t.Fatal(err)
	}
	// The new index is dropped, so the state the function refuses is the one
	// on disk.
	if err := db.Exec(`DROP INDEX idx_episode_owner_session_kind ON episode_memories`).Error; err != nil {
		t.Fatal(err)
	}
	if err := ensureEpisodeContentKindUniqueness(db); err == nil {
		t.Fatal("the migration dropped the old index without the new one")
	}
	var oldIndexCount int64
	if err := db.Raw(`
		SELECT COUNT(*) FROM information_schema.statistics
		WHERE table_schema = DATABASE() AND table_name = 'episode_memories'
		  AND index_name = 'idx_episode_owner_session'
	`).Row().Scan(&oldIndexCount); err != nil {
		t.Fatal(err)
	}
	if oldIndexCount == 0 {
		t.Fatal("the old index was dropped anyway")
	}
}
