package episode

import (
	"GopherAI/common/aihelper"
	episodedao "GopherAI/dao/episode"
	"GopherAI/internal/testdb"
	"GopherAI/model"
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	mysqldriver "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// TestEpisodeGeneratorQwenSmoke is deliberately opt-in. It validates the real
// provider's forced Tool Call, strict schema and source boundary with a tiny
// synthetic conversation; it is not a recall-quality experiment.
func TestEpisodeGeneratorQwenSmoke(t *testing.T) {
	if os.Getenv("GOPHERAI_RUN_EPISODE_SMOKE") != "1" {
		t.Skip("set GOPHERAI_RUN_EPISODE_SMOKE=1 to run the real configured-model smoke test")
	}
	if os.Getenv("OPENAI_API_KEY") == "" {
		t.Skip("OPENAI_API_KEY is not configured")
	}
	dsn := testdb.RequireMySQLDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	chatModel, err := aihelper.NewToolCallingChatModel(ctx)
	if err != nil {
		t.Fatal(err)
	}
	generator, err := NewModelGenerator(chatModel, 4096, 1200)
	if err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), TranslateError: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.Session{}, &model.Message{}, &model.SessionSummary{}, &model.EpisodeMemory{}, &model.EpisodeProjectionJob{}); err != nil {
		t.Fatal(err)
	}
	username := "epsmoke-" + uuid.NewString()[:8]
	user := model.User{Username: username, Password: "smoke-only"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	sessions := []model.Session{
		{ID: uuid.NewString(), UserID: user.ID, UserName: username, Mode: model.SessionModeAgent, Title: "RPC 粘包"},
		{ID: uuid.NewString(), UserID: user.ID, UserName: username, Mode: model.SessionModeAgent, Title: "Redis TTL"},
	}
	for index := range sessions {
		if err := db.Create(&sessions[index]).Error; err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, session := range sessions {
			db.Unscoped().Where("session_id = ?", session.ID).Delete(&model.EpisodeProjectionJob{})
			db.Unscoped().Where("session_id = ?", session.ID).Delete(&model.EpisodeMemory{})
			db.Unscoped().Where("session_id = ?", session.ID).Delete(&model.SessionSummary{})
			db.Unscoped().Where("session_id = ?", session.ID).Delete(&model.Message{})
			db.Unscoped().Delete(&session)
		}
		db.Unscoped().Delete(&user)
	})

	shortMessages := smokeMessages(t, db, sessions[0], []string{"我决定 RPC 帧使用四字节长度前缀，并继续确认半包读取边界。", "已解释长度前缀与 readFull 的配合。"})
	longMessages := smokeMessages(t, db, sessions[1], []string{"目标是理解 Redis TTL。", "已解释基础过期机制。", "我决定继续验证惰性删除，并把主动过期作为待办。", "已列出下一步验证方法。"})
	summary := model.SessionSummary{UserID: user.ID, SessionID: sessions[1].ID, Content: `{"current_goal":"理解 Redis TTL","confirmed_decisions":[],"open_loops":[]}`, CoveredMessageID: longMessages[1].ID, TokenEstimate: 30, Version: 1}
	if err := db.Create(&summary).Error; err != nil {
		t.Fatal(err)
	}
	loader, err := NewDatabaseEvidenceLoader(db, 128)
	if err != nil {
		t.Fatal(err)
	}
	projector, err := NewProjector(loader, generator, episodedao.NewRepository(db), allowAll())
	if err != nil {
		t.Fatal(err)
	}
	shortEpisode, err := projector.Project(ctx, user.ID, sessions[0].ID, shortMessages[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	longEpisode, err := projector.Project(ctx, user.ID, sessions[1].ID, longMessages[3].ID)
	if err != nil {
		t.Fatal(err)
	}
	if shortEpisode.SourceSummaryVersion != 0 || longEpisode.SourceSummaryVersion != 1 || longEpisode.SourceSummaryWatermark != longMessages[1].ID {
		t.Fatalf("short=%#v long=%#v", shortEpisode, longEpisode)
	}
	assertSmokeEvidenceIsUserOwned(t, shortEpisode.Document, shortMessages)
	assertSmokeEvidenceIsUserOwned(t, longEpisode.Document, longMessages[2:])

	continued := smokeMessages(t, db, sessions[0], []string{"我还决定使用最大帧长限制。", "已补充边界检查。"})
	updated, err := projector.Project(ctx, user.ID, sessions[0].ID, continued[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != shortEpisode.ID || updated.Version != shortEpisode.Version+1 || updated.SourceLastMessageID != continued[1].ID {
		t.Fatalf("updated episode=%#v original=%#v", updated, shortEpisode)
	}
}

func smokeMessages(t *testing.T, db *gorm.DB, session model.Session, contents []string) []model.Message {
	t.Helper()
	messages := make([]model.Message, 0, len(contents))
	for index, content := range contents {
		message := model.Message{SessionID: session.ID, UserName: session.UserName, Content: content, IsUser: index%2 == 0, CreatedAt: time.Now().Add(time.Duration(index) * time.Millisecond)}
		if err := db.Create(&message).Error; err != nil {
			t.Fatal(err)
		}
		messages = append(messages, message)
	}
	return messages
}

func assertSmokeEvidenceIsUserOwned(t *testing.T, document model.EpisodeDocument, messages []model.Message) {
	t.Helper()
	users := make(map[uint]struct{})
	for _, message := range messages {
		if message.IsUser {
			users[message.ID] = struct{}{}
		}
	}
	for _, entries := range [][]model.EpisodeEvidence{document.Decisions, document.OpenLoops, document.UserCorrections} {
		for _, entry := range entries {
			for _, id := range entry.EvidenceMessageIDs {
				if _, ok := users[id]; !ok {
					t.Fatal(fmt.Errorf("episode evidence %d is not an allowed user message", id))
				}
			}
		}
	}
}
