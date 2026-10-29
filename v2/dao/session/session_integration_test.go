package session

import (
	"GopherAI/internal/testdb"
	"GopherAI/model"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	mysqldriver "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestRepositoryPersistsModeAndEnforcesOwnership(t *testing.T) {
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.Session{}); err != nil {
		t.Fatal(err)
	}
	suffix := uuid.NewString()
	firstUser := model.User{Username: fmt.Sprintf("session-a-%s", suffix), Password: "secret"}
	secondUser := model.User{Username: fmt.Sprintf("session-b-%s", suffix), Password: "secret"}
	if err := db.Create(&firstUser).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&secondUser).Error; err != nil {
		t.Fatal(err)
	}
	repository := NewRepository(db)
	created := &model.Session{
		ID:       uuid.NewString(),
		UserID:   firstUser.ID,
		UserName: firstUser.Username,
		Mode:     model.SessionModeRAG,
		Title:    "persistent RAG session",
	}
	if err := repository.Create(context.Background(), created); err != nil {
		t.Fatal(err)
	}

	reloaded, err := repository.GetByIDAndUserID(context.Background(), created.ID, firstUser.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Mode != model.SessionModeRAG {
		t.Fatalf("reloaded mode = %q, want %q", reloaded.Mode, model.SessionModeRAG)
	}
	modelType, err := reloaded.Mode.ModelType()
	if err != nil || modelType != "2" {
		t.Fatalf("persisted mode resolved model type = %q, %v; want 2", modelType, err)
	}
	if _, err := repository.GetByIDAndUserID(context.Background(), created.ID, secondUser.ID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("cross-user lookup error = %v, want ErrSessionNotFound", err)
	}
	listed, err := repository.ListByUserID(context.Background(), firstUser.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) == 0 || listed[0].ID != created.ID {
		t.Fatalf("owned session missing from database list: %#v", listed)
	}
}

func TestRepositoryCreateOrGetIsIdempotent(t *testing.T) {
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.Session{}); err != nil {
		t.Fatal(err)
	}
	repository := NewRepository(db)
	user := model.User{Username: "session-idem-" + uuid.NewString()[:8], Password: "secret"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Unscoped().Delete(&user) })
	userID := user.ID
	requestKey := "session-request-" + uuid.NewString()
	inputHash := strings.Repeat("a", 64)
	makeSession := func() *model.Session {
		key := requestKey
		return &model.Session{ID: uuid.NewString(), UserID: userID, UserName: "session-idempotency-test", Mode: model.SessionModeAgent, CreationRequestKey: &key, CreationInputHash: inputHash}
	}
	results := make([]*model.Session, 2)
	created := make([]bool, 2)
	errs := make([]error, 2)
	var wait sync.WaitGroup
	for i := range results {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			results[index], created[index], errs[index] = repository.CreateOrGet(context.Background(), makeSession())
		}(i)
	}
	wait.Wait()
	defer db.Unscoped().Where("user_id = ? AND creation_request_key = ?", userID, requestKey).Delete(&model.Session{})
	if errs[0] != nil || errs[1] != nil || results[0].ID != results[1].ID {
		t.Fatalf("results=%+v created=%v errors=%v", results, created, errs)
	}
	if created[0] == created[1] {
		t.Fatalf("exactly one caller must create: %v", created)
	}
	changed := makeSession()
	changed.CreationInputHash = strings.Repeat("b", 64)
	if _, _, err := repository.CreateOrGet(context.Background(), changed); !errors.Is(err, ErrSessionRequestConflict) {
		t.Fatalf("changed request error=%v, want ErrSessionRequestConflict", err)
	}
}

func TestRepositoryReplaysBeforeApplyingSessionQuota(t *testing.T) {
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.Session{}); err != nil {
		t.Fatal(err)
	}
	user := model.User{Username: "session-quota-" + uuid.NewString(), Password: "secret"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	requestKey := "session-quota-replay"
	inputHash := strings.Repeat("c", 64)
	sessions := make([]model.Session, model.MaxSessionsPerUser)
	for index := range sessions {
		sessions[index] = model.Session{ID: uuid.NewString(), UserID: user.ID, UserName: user.Username, Mode: model.SessionModeNormal}
	}
	sessions[0].Mode = model.SessionModeAgent
	sessions[0].CreationRequestKey = &requestKey
	sessions[0].CreationInputHash = inputHash
	if err := db.CreateInBatches(&sessions, 25).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Unscoped().Where("user_id = ?", user.ID).Delete(&model.Session{})
		db.Unscoped().Delete(&user)
	})
	repository := NewRepository(db)
	replayKey := requestKey
	replay, created, err := repository.CreateOrGet(context.Background(), &model.Session{
		ID: uuid.NewString(), UserID: user.ID, UserName: user.Username, Mode: model.SessionModeAgent,
		CreationRequestKey: &replayKey, CreationInputHash: inputHash,
	})
	if err != nil || created || replay.ID != sessions[0].ID {
		t.Fatalf("replay=%+v created=%v error=%v", replay, created, err)
	}
	_, _, err = repository.CreateOrGet(context.Background(), &model.Session{
		ID: uuid.NewString(), UserID: user.ID, UserName: user.Username, Mode: model.SessionModeNormal,
	})
	if !errors.Is(err, ErrSessionLimitReached) {
		t.Fatalf("new session error=%v, want ErrSessionLimitReached", err)
	}
}
