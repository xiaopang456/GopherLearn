package episode

import (
	redisCommon "GopherAI/common/redis"
	episodedao "GopherAI/dao/episode"
	"GopherAI/internal/testdb"
	"GopherAI/model"
	"context"
	"os"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/embedding"
	"github.com/google/uuid"
	redisCli "github.com/redis/go-redis/v9"
	mysqldriver "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type integrationEpisodeEmbedder struct{}

func (integrationEpisodeEmbedder) EmbedStrings(_ context.Context, texts []string, _ ...embedding.Option) ([][]float64, error) {
	result := make([][]float64, len(texts))
	for i := range result {
		result[i] = make([]float64, redisCommon.EpisodeVectorDimension)
		result[i][0] = 1
	}
	return result, nil
}

func TestEpisodeReindexRealMySQLRedis(t *testing.T) {
	address := os.Getenv("GOPHERAI_REDIS_TEST_ADDR")
	if address == "" {
		t.Skip("set GOPHERAI_REDIS_TEST_ADDR to run Redis-backed episode integration test")
	}
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), TranslateError: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(&model.User{}, &model.Session{}, &model.EpisodeMemory{}); err != nil {
		t.Fatal(err)
	}
	client := redisCli.NewClient(&redisCli.Options{Addr: address, Password: os.Getenv("GOPHERAI_REDIS_TEST_PASSWORD"), Protocol: 2})
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()
	repository := episodedao.NewRepository(db)
	index, err := episodedao.NewRedisEpisodeIndex(client, redisCommon.EpisodeVectorDimension)
	if err != nil {
		t.Fatal(err)
	}
	if err = index.EnsureIndex(ctx); err != nil {
		t.Fatal(err)
	}
	suffix := uuid.NewString()
	user := model.User{Username: "epri-" + suffix[:8], Password: "secret"}
	if err = db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	session := model.Session{ID: uuid.NewString(), UserID: user.ID, UserName: user.Username, Mode: model.SessionModeAgent, Title: "reindex"}
	if err = db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = index.ResetUser(context.Background(), user.ID)
		db.Unscoped().Where("user_id = ?", user.ID).Delete(&model.EpisodeMemory{})
		db.Unscoped().Where("id = ?", session.ID).Delete(&model.Session{})
		db.Unscoped().Where("id = ?", user.ID).Delete(&model.User{})
	})
	episode := &model.EpisodeMemory{ID: uuid.NewString(), UserID: user.ID, SessionID: session.ID, Title: "RPC", Document: model.EpisodeDocument{Synopsis: "讨论粘包", Topics: []model.EpisodeTopic{{Key: "rpc-粘包", Label: "RPC 粘包"}}, Decisions: []model.EpisodeEvidence{{Text: "使用长度字段", EvidenceMessageIDs: []uint{1}}}, SourceHandles: []string{"session:" + session.ID, "message:1"}}, SearchText: "RPC 粘包 使用长度字段", TopicKeys: model.StringList{"rpc-粘包"}, SourceLastMessageID: 1, LastMessageAt: time.Now(), Status: model.EpisodeStatusActive, IndexStatus: model.EpisodeIndexPending}
	if err = repository.Save(ctx, episode, 0); err != nil {
		t.Fatal(err)
	}
	vector := make([]float32, redisCommon.EpisodeVectorDimension)
	vector[0] = 1
	staleID := "stale-" + suffix
	if err = index.Store(ctx, episodedao.IndexedEpisode{Episode: model.EpisodeMemory{ID: staleID, UserID: user.ID, SessionID: "deleted", Title: "stale", SearchText: "stale", Status: model.EpisodeStatusActive, LastMessageAt: time.Now()}, Vector: vector}); err != nil {
		t.Fatal(err)
	}
	indexer, _ := NewIndexer(index, repository, integrationEpisodeEmbedder{}, redisCommon.EpisodeVectorDimension)
	report, err := indexer.Reindex(ctx, user.ID)
	if err != nil || !report.Complete() || report.Source != 1 || report.Stored != 1 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if exists := client.Exists(ctx, redisCommon.GenerateEpisodeVectorKey(user.ID, staleID)).Val(); exists != 0 {
		t.Fatalf("stale key survived reindex")
	}
	if exists := client.Exists(ctx, redisCommon.GenerateEpisodeVectorKey(user.ID, episode.ID)).Val(); exists != 1 {
		t.Fatalf("active key missing")
	}
}
