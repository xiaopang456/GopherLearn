package learning

import (
	"GopherAI/internal/testdb"
	"GopherAI/model"
	"context"
	"testing"
	"time"

	mysqldriver "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// AGENT-017 reads TopicProgress on every agent turn, so the isolation and
// ordering guarantees have to hold against real MySQL rather than a fake.
//
// Each run uses a unique owner id, following the AGENT-012A finding that a
// fixed test owner lets rows from an earlier run pollute the next one.
func TestRepositoryListTopicProgressIsolationAndOrdering(t *testing.T) {
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.TopicProgress{}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	repository := NewRepository(db)

	owner := time.Now().UnixNano()%1_000_000 + 900_000
	other := owner + 1
	defer func() {
		db.Where("user_id IN ?", []int64{owner, other}).Delete(&model.TopicProgress{})
	}()

	newest := time.Now().UTC().Truncate(time.Second)
	middle := newest.Add(-24 * time.Hour)
	oldest := newest.Add(-72 * time.Hour)

	rows := []model.TopicProgress{
		{UserID: owner, Topic: "Redis 缓存", TopicKey: "redis-缓存", MasteryScore: 62, Confidence: 0.66,
			ValidAttempts: 4, Level: model.TopicProgressDeveloping, WeakPoints: []string{"缓存击穿与穿透的区别"}, LastAttemptAt: &middle},
		{UserID: owner, Topic: "Go 并发", TopicKey: "go-并发", MasteryScore: 91, Confidence: 0.83,
			ValidAttempts: 5, Level: model.TopicProgressMastered, WeakPoints: nil, LastAttemptAt: &newest},
		{UserID: owner, Topic: "RPC 粘包", TopicKey: "rpc-粘包", MasteryScore: 40, Confidence: 0.5,
			ValidAttempts: 3, Level: model.TopicProgressDeveloping, WeakPoints: []string{"半包时缓冲区不能直接复位"}, LastAttemptAt: &oldest},
		{UserID: other, Topic: "Redis 缓存", TopicKey: "redis-缓存", MasteryScore: 10, Confidence: 0.5,
			ValidAttempts: 3, Level: model.TopicProgressDeveloping, WeakPoints: []string{"别人的薄弱点"}, LastAttemptAt: &newest},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatal(err)
		}
	}

	listed, err := repository.ListTopicProgress(ctx, owner, 50)
	if err != nil {
		t.Fatalf("list topic progress: %v", err)
	}
	if len(listed) != 3 {
		t.Fatalf("listed %d rows, want the 3 owned by this user", len(listed))
	}
	for _, row := range listed {
		if row.UserID != owner {
			t.Fatalf("another user's progress leaked: user_id=%d", row.UserID)
		}
	}
	// Most recently assessed first, so a bounded read keeps what matters.
	if listed[0].TopicKey != "go-并发" || listed[2].TopicKey != "rpc-粘包" {
		t.Fatalf("unexpected ordering: %s, %s, %s", listed[0].TopicKey, listed[1].TopicKey, listed[2].TopicKey)
	}
	// WeakPoints round-trips through the JSON serializer, which is where
	// BUG-AGENT-008 previously bit on a different table.
	if len(listed[1].WeakPoints) != 1 || listed[1].WeakPoints[0] != "缓存击穿与穿透的区别" {
		t.Errorf("weak points did not round-trip: %#v", listed[1].WeakPoints)
	}

	if _, err := repository.ListTopicProgress(ctx, 0, 50); err == nil {
		t.Error("an untrusted user id must be rejected")
	}
	if _, err := repository.ListTopicProgress(ctx, owner, 0); err == nil {
		t.Error("a zero limit must be rejected")
	}
	if _, err := repository.ListTopicProgress(ctx, owner, 500); err == nil {
		t.Error("an unbounded limit must be rejected")
	}

	bounded, err := repository.ListTopicProgress(ctx, owner, 2)
	if err != nil {
		t.Fatalf("bounded list: %v", err)
	}
	if len(bounded) != 2 || bounded[0].TopicKey != "go-并发" {
		t.Fatalf("limit was not applied to the newest rows: %#v", bounded)
	}
}
