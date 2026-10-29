package agent

import (
	messagedao "GopherAI/dao/message"
	"GopherAI/internal/testdb"
	"GopherAI/model"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	mysqldriver "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestRepositoryRecoversExpiredChatRunAndFencesOldOwner(t *testing.T) {
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Session{}, &model.AgentRun{}, &model.AgentStep{}, &model.Message{}); err != nil {
		t.Fatal(err)
	}
	repository := NewRepository(db)
	messages := messagedao.NewRepository(db)
	ctx := context.Background()
	session := &model.Session{ID: uuid.NewString(), UserID: time.Now().UnixNano(), UserName: "lease-recovery", Mode: model.SessionModeAgent}
	if err := db.Create(session).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Where("session_id = ?", session.ID).Delete(&model.Message{})
		db.Where("run_id IN (?)", db.Model(&model.AgentRun{}).Select("id").Where("session_id = ?", session.ID)).Delete(&model.AgentStep{})
		db.Where("session_id = ?", session.ID).Delete(&model.AgentRun{})
		db.Unscoped().Where("id = ?", session.ID).Delete(&model.Session{})
	})

	now := time.Now()
	requestKey := "request-expired-recovery"
	oldToken := uuid.NewString()
	oldHeartbeat, oldExpiry := now.Add(-time.Minute), now.Add(-time.Second)
	stale := &model.AgentRun{
		ID: uuid.NewString(), UserID: session.UserID, SessionID: session.ID,
		RequestKey: &requestKey, InputHash: strings.Repeat("e", 64), ExecutionKind: model.AgentRunExecutionChat,
		Status: model.AgentRunRunning, Version: 1, StartedAt: oldHeartbeat,
		LeaseToken: oldToken, LeaseExpiresAt: &oldExpiry, HeartbeatAt: &oldHeartbeat,
	}
	if err := db.Create(stale).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.AgentStep{UserID: session.UserID, RunID: stale.ID, Sequence: 4, Type: model.AgentStepPlan, LeaseToken: oldToken}).Error; err != nil {
		t.Fatal(err)
	}
	newToken := uuid.NewString()
	newExpiry := now.Add(time.Minute)
	requested := &model.AgentRun{
		ID: uuid.NewString(), UserID: session.UserID, SessionID: session.ID,
		RequestKey: &requestKey, InputHash: stale.InputHash, ExecutionKind: model.AgentRunExecutionChat,
		Status: model.AgentRunRunning, Version: 1, StartedAt: now,
		LeaseToken: newToken, LeaseExpiresAt: &newExpiry, HeartbeatAt: &now,
	}
	acquired, err := repository.AcquireRun(ctx, requested)
	if err != nil {
		t.Fatal(err)
	}
	if !acquired.Resumed || acquired.Created || acquired.Run.ID != stale.ID || acquired.Run.Version != 2 || acquired.Run.LeaseToken != newToken || acquired.NextStepSequence != 4 {
		t.Fatalf("recovered acquisition = %+v", acquired)
	}
	if err := repository.RenewRunLease(ctx, session.UserID, session.ID, stale.ID, oldToken, now, now.Add(time.Minute)); !errors.Is(err, ErrRunLeaseLost) {
		t.Fatalf("old renew error = %v, want lease lost", err)
	}
	if err := repository.AppendStep(ctx, session.ID, &model.AgentStep{UserID: session.UserID, RunID: stale.ID, Sequence: 5, Type: model.AgentStepPlan, LeaseToken: oldToken}); !errors.Is(err, ErrRunLeaseLost) {
		t.Fatalf("old step error = %v, want lease lost", err)
	}
	if _, _, err := repository.CompleteRun(ctx, session.UserID, session.ID, stale.ID, oldToken, 1, &model.Message{SessionID: session.ID, UserName: session.UserName, Content: "stale", IsUser: false}, "stale", `{}`); !errors.Is(err, ErrRunLeaseLost) {
		t.Fatalf("old completion error = %v, want lease lost", err)
	}
	if err := repository.AppendStep(ctx, session.ID, &model.AgentStep{UserID: session.UserID, RunID: stale.ID, Sequence: 5, Type: model.AgentStepPlan, LeaseToken: newToken}); err != nil {
		t.Fatal(err)
	}

	runID, userRole := stale.ID, model.MessageAgentRunRoleUser
	userMessage := &model.Message{AgentRunID: &runID, AgentRunRole: &userRole, SessionID: session.ID, UserName: session.UserName, Content: "same question", IsUser: true}
	first, err := messages.Create(ctx, userMessage)
	if err != nil {
		t.Fatal(err)
	}
	duplicate := &model.Message{AgentRunID: &runID, AgentRunRole: &userRole, SessionID: session.ID, UserName: session.UserName, Content: "same question", IsUser: true}
	second, err := messages.Create(ctx, duplicate)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == 0 || second.ID != first.ID {
		t.Fatalf("idempotent user messages first=%d second=%d", first.ID, second.ID)
	}

	completed, assistant, err := repository.CompleteRun(ctx, session.UserID, session.ID, stale.ID, newToken, 2, &model.Message{SessionID: session.ID, UserName: session.UserName, Content: "answer", IsUser: false}, "answer", `{"answer":"answer"}`)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != model.AgentRunCompleted || completed.LeaseToken != "" || completed.LeaseExpiresAt != nil || assistant.ID == 0 {
		t.Fatalf("completed=%+v assistant=%+v", completed, assistant)
	}
	var messageCount int64
	if err := db.Model(&model.Message{}).Where("agent_run_id = ?", stale.ID).Count(&messageCount).Error; err != nil || messageCount != 2 {
		t.Fatalf("run message count=%d err=%v", messageCount, err)
	}
}

func TestRenewRunLeaseNeverRegressesWhenAnOlderAttemptArrivesLate(t *testing.T) {
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Session{}, &model.AgentRun{}); err != nil {
		t.Fatal(err)
	}
	repository := NewRepository(db)
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	session := &model.Session{ID: uuid.NewString(), UserID: time.Now().UnixNano(), UserName: "lease-monotonic", Mode: model.SessionModeAgent}
	if err := db.Create(session).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Where("session_id = ?", session.ID).Delete(&model.AgentRun{})
		db.Unscoped().Where("id = ?", session.ID).Delete(&model.Session{})
	})

	initialHeartbeat, initialExpiry := now, now.Add(time.Minute)
	leaseToken := uuid.NewString()
	run := &model.AgentRun{
		ID: uuid.NewString(), UserID: session.UserID, SessionID: session.ID,
		InputHash: strings.Repeat("m", 64), ExecutionKind: model.AgentRunExecutionChat,
		Status: model.AgentRunRunning, Version: 1, StartedAt: now,
		LeaseToken: leaseToken, LeaseExpiresAt: &initialExpiry, HeartbeatAt: &initialHeartbeat,
	}
	if err := db.Create(run).Error; err != nil {
		t.Fatal(err)
	}

	newHeartbeat, newExpiry := now.Add(10*time.Second), now.Add(5*time.Minute)
	if err := repository.RenewRunLease(ctx, session.UserID, session.ID, run.ID, leaseToken, newHeartbeat, newExpiry); err != nil {
		t.Fatalf("new renewal: %v", err)
	}

	// This represents an earlier request which timed out at the client but was
	// only executed by MySQL after the newer retry. It must be accepted as the
	// same owner without moving either timestamp backwards.
	staleHeartbeat, staleExpiry := now.Add(5*time.Second), now.Add(2*time.Minute)
	if err := repository.RenewRunLease(ctx, session.UserID, session.ID, run.ID, leaseToken, staleHeartbeat, staleExpiry); err != nil {
		t.Fatalf("late stale renewal: %v", err)
	}

	var stored model.AgentRun
	if err := db.Where("id = ?", run.ID).First(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.HeartbeatAt == nil || stored.HeartbeatAt.Before(newHeartbeat) {
		t.Fatalf("heartbeat regressed: got %v, want at least %v", stored.HeartbeatAt, newHeartbeat)
	}
	if stored.LeaseExpiresAt == nil || stored.LeaseExpiresAt.Before(newExpiry) {
		t.Fatalf("lease expiry regressed: got %v, want at least %v", stored.LeaseExpiresAt, newExpiry)
	}
}

func TestAcquireRunAllowsConcurrentIndependentSessionsUnderRepeatableRead(t *testing.T) {
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Session{}, &model.AgentRun{}, &model.Message{}); err != nil {
		t.Fatal(err)
	}
	var isolation string
	if err := db.Raw("SELECT @@transaction_isolation").Scan(&isolation).Error; err != nil {
		t.Fatal(err)
	}
	if strings.ToUpper(isolation) != "REPEATABLE-READ" {
		t.Fatalf("server isolation=%q, want REPEATABLE-READ production default", isolation)
	}
	const total = 32
	baseUserID := time.Now().UnixNano()
	sessions := make([]model.Session, total)
	for index := range sessions {
		sessions[index] = model.Session{
			ID: uuid.NewString(), UserID: baseUserID + int64(index),
			UserName: fmt.Sprintf("parallel-%d", index), Mode: model.SessionModeAgent,
		}
	}
	if err := db.Create(&sessions).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ids := make([]string, 0, len(sessions))
		for _, session := range sessions {
			ids = append(ids, session.ID)
		}
		db.Where("session_id IN ?", ids).Delete(&model.AgentRun{})
		db.Unscoped().Where("id IN ?", ids).Delete(&model.Session{})
	})
	repository := NewRepository(db)
	start := make(chan struct{})
	errs := make([]error, total)
	var wait sync.WaitGroup
	for index := range sessions {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			now, leaseEnds := time.Now(), time.Now().Add(time.Minute)
			key := "parallel-" + uuid.NewString()
			run := &model.AgentRun{
				ID: uuid.NewString(), UserID: sessions[index].UserID, SessionID: sessions[index].ID,
				RequestKey: &key, InputHash: strings.Repeat("a", 64), ExecutionKind: model.AgentRunExecutionChat,
				Status: model.AgentRunRunning, Version: 1, StartedAt: now, LeaseToken: uuid.NewString(),
				LeaseExpiresAt: &leaseEnds, HeartbeatAt: &now,
			}
			_, errs[index] = repository.AcquireRun(context.Background(), run)
		}(index)
	}
	close(start)
	wait.Wait()
	for index, acquireErr := range errs {
		if acquireErr != nil {
			t.Fatalf("independent acquire %d error=%v", index, acquireErr)
		}
	}
}

func TestAcquireRunSameRequestKeyAcrossSessionsHasOneWinner(t *testing.T) {
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Session{}, &model.AgentRun{}, &model.Message{}); err != nil {
		t.Fatal(err)
	}
	userID := time.Now().UnixNano()
	sessions := []model.Session{
		{ID: uuid.NewString(), UserID: userID, UserName: "same-key", Mode: model.SessionModeAgent},
		{ID: uuid.NewString(), UserID: userID, UserName: "same-key", Mode: model.SessionModeAgent},
	}
	if err := db.Create(&sessions).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ids := []string{sessions[0].ID, sessions[1].ID}
		db.Where("session_id IN ?", ids).Delete(&model.AgentRun{})
		db.Unscoped().Where("id IN ?", ids).Delete(&model.Session{})
	})
	key := "same-key-" + uuid.NewString()
	runs := make([]*model.AgentRun, len(sessions))
	for index := range sessions {
		now, leaseEnds := time.Now(), time.Now().Add(time.Minute)
		runs[index] = &model.AgentRun{
			ID: uuid.NewString(), UserID: userID, SessionID: sessions[index].ID, RequestKey: &key,
			InputHash: strings.Repeat("b", 64), ExecutionKind: model.AgentRunExecutionChat,
			Status: model.AgentRunRunning, Version: 1, StartedAt: now, LeaseToken: uuid.NewString(),
			LeaseExpiresAt: &leaseEnds, HeartbeatAt: &now,
		}
	}
	start := make(chan struct{})
	results := make([]*RunAcquisition, len(runs))
	errs := make([]error, len(runs))
	var wait sync.WaitGroup
	for index := range runs {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			results[index], errs[index] = NewRepository(db).AcquireRun(context.Background(), runs[index])
		}(index)
	}
	close(start)
	wait.Wait()
	successes, conflicts := 0, 0
	for index, acquireErr := range errs {
		switch {
		case acquireErr == nil && results[index] != nil && results[index].Created:
			successes++
		case errors.Is(acquireErr, ErrRunRequestConflict):
			conflicts++
		default:
			t.Fatalf("result[%d]=%+v error=%v", index, results[index], acquireErr)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d errors=%v", successes, conflicts, errs)
	}
}

func TestAcquireRunReplaysBeforeApplyingMessageQuota(t *testing.T) {
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Session{}, &model.AgentRun{}, &model.Message{}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	session := model.Session{ID: uuid.NewString(), UserID: time.Now().UnixNano(), UserName: "message-limit", Mode: model.SessionModeAgent}
	if err := db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Where("session_id = ?", session.ID).Delete(&model.Message{})
		db.Where("session_id = ?", session.ID).Delete(&model.AgentRun{})
		db.Unscoped().Where("id = ?", session.ID).Delete(&model.Session{})
	})
	messages := make([]model.Message, model.MaxMessagesPerSession-1)
	for index := range messages {
		messages[index] = model.Message{SessionID: session.ID, UserName: session.UserName, Content: "bounded", IsUser: index%2 == 0}
	}
	if err := db.CreateInBatches(&messages, 250).Error; err != nil {
		t.Fatal(err)
	}
	replayKey := "message-limit-replay"
	inputHash := strings.Repeat("a", 64)
	finished := time.Now()
	completed := model.AgentRun{
		ID: uuid.NewString(), UserID: session.UserID, SessionID: session.ID, ExecutionKind: model.AgentRunExecutionChat,
		Status: model.AgentRunCompleted, Version: 2, StartedAt: finished.Add(-time.Second), FinishedAt: &finished,
		RequestKey: &replayKey, InputHash: inputHash, ResultJSON: `{"answer":"already done"}`,
	}
	if err := db.Create(&completed).Error; err != nil {
		t.Fatal(err)
	}
	request := func(key string) *model.AgentRun {
		now := time.Now()
		expires := now.Add(time.Minute)
		return &model.AgentRun{
			ID: uuid.NewString(), UserID: session.UserID, SessionID: session.ID, ExecutionKind: model.AgentRunExecutionChat,
			Status: model.AgentRunRunning, Version: 1, StartedAt: now, RequestKey: &key, InputHash: inputHash,
			LeaseToken: uuid.NewString(), LeaseExpiresAt: &expires, HeartbeatAt: &now,
		}
	}
	replay, err := NewRepository(db).AcquireRun(ctx, request(replayKey))
	if err != nil || replay == nil || replay.Created || replay.Run.ID != completed.ID {
		t.Fatalf("replay=%+v error=%v", replay, err)
	}
	if _, err := NewRepository(db).AcquireRun(ctx, request("message-limit-new")); !errors.Is(err, ErrMessageLimitReached) {
		t.Fatalf("new request error=%v, want ErrMessageLimitReached", err)
	}
}

func TestRepositoryExpiresOnlyStaleChatRuns(t *testing.T) {
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Session{}, &model.AgentRun{}, &model.Message{}); err != nil {
		t.Fatal(err)
	}
	repository := NewRepository(db)
	ctx := context.Background()
	newSession := func(name string) *model.Session {
		return &model.Session{ID: uuid.NewString(), UserID: time.Now().UnixNano(), UserName: name, Mode: model.SessionModeAgent}
	}
	newChat := func(session *model.Session, key string, at time.Time) *model.AgentRun {
		leaseEnds := at.Add(time.Minute)
		return &model.AgentRun{ID: uuid.NewString(), UserID: session.UserID, SessionID: session.ID, RequestKey: &key, InputHash: strings.Repeat("f", 64), ExecutionKind: model.AgentRunExecutionChat, Status: model.AgentRunRunning, Version: 1, StartedAt: at, LeaseToken: uuid.NewString(), LeaseExpiresAt: &leaseEnds, HeartbeatAt: &at}
	}

	staleSession := newSession("stale-chat")
	if err := db.Create(staleSession).Error; err != nil {
		t.Fatal(err)
	}
	staleAt := time.Now().Add(-2 * time.Minute)
	staleKey := "request-stale-other"
	stale := newChat(staleSession, staleKey, staleAt)
	if err := db.Create(stale).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	newKey := "request-after-stale"
	replacement := newChat(staleSession, newKey, now)
	acquired, err := repository.AcquireRun(ctx, replacement)
	if err != nil || !acquired.Created {
		t.Fatalf("replacement=%+v err=%v", acquired, err)
	}
	old, err := repository.GetRun(ctx, stale.UserID, stale.SessionID, stale.ID)
	if err != nil || old.Status != model.AgentRunTimeout || old.TerminationReason != "lease_expired" {
		t.Fatalf("expired run=%+v err=%v", old, err)
	}

	learningSession := newSession("learning-not-reclaimed")
	if err := db.Create(learningSession).Error; err != nil {
		t.Fatal(err)
	}
	learning := &model.AgentRun{ID: uuid.NewString(), UserID: learningSession.UserID, SessionID: learningSession.ID, ExecutionKind: model.AgentRunExecutionLearning, Status: model.AgentRunRunning, Version: 1, StartedAt: staleAt}
	if err := repository.CreateRun(ctx, learning); err != nil {
		t.Fatal(err)
	}
	blockedKey := "request-learning-blocked"
	blocked := newChat(learningSession, blockedKey, now)
	if _, err := repository.AcquireRun(ctx, blocked); !errors.Is(err, ErrRunInProgress) {
		t.Fatalf("chat over learning error=%v, want busy", err)
	}
	stillLearning, err := repository.GetRun(ctx, learning.UserID, learning.SessionID, learning.ID)
	if err != nil || stillLearning.Status != model.AgentRunRunning {
		t.Fatalf("learning run=%+v err=%v", stillLearning, err)
	}

	for _, session := range []*model.Session{staleSession, learningSession} {
		db.Where("session_id = ?", session.ID).Delete(&model.AgentRun{})
		db.Unscoped().Where("id = ?", session.ID).Delete(&model.Session{})
	}
}

func TestRepositorySweepsOnlyExpiredChatRunLeases(t *testing.T) {
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Session{}, &model.AgentRun{}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	session := &model.Session{ID: uuid.NewString(), UserID: now.UnixNano(), UserName: "lease-sweep", Mode: model.SessionModeAgent}
	if err := db.Create(session).Error; err != nil {
		t.Fatal(err)
	}
	newRun := func(key string, lease time.Time, kind model.AgentRunExecutionKind) *model.AgentRun {
		heartbeat := lease.Add(-time.Second)
		return &model.AgentRun{
			ID: uuid.NewString(), UserID: session.UserID, SessionID: session.ID, RequestKey: &key,
			InputHash: strings.Repeat("a", 64), ExecutionKind: kind, Status: model.AgentRunRunning,
			Version: 1, StartedAt: heartbeat, LeaseToken: uuid.NewString(), LeaseExpiresAt: &lease, HeartbeatAt: &heartbeat,
		}
	}
	expired := newRun("sweep-expired", now.Add(-time.Second), model.AgentRunExecutionChat)
	healthy := newRun("sweep-healthy", now.Add(time.Minute), model.AgentRunExecutionChat)
	learning := newRun("sweep-learning", now.Add(-time.Second), model.AgentRunExecutionLearning)
	learning.RequestKey = nil
	learning.InputHash = ""
	if err := db.Create([]*model.AgentRun{expired, healthy, learning}).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Where("session_id = ?", session.ID).Delete(&model.AgentRun{})
		db.Unscoped().Where("id = ?", session.ID).Delete(&model.Session{})
	})

	count, err := NewRepository(db).ExpireAbandonedChatRuns(context.Background(), now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expired count=%d, want 1", count)
	}
	var got []model.AgentRun
	if err := db.Where("id IN ?", []string{expired.ID, healthy.ID, learning.ID}).Find(&got).Error; err != nil {
		t.Fatal(err)
	}
	statuses := map[string]model.AgentRun{}
	for _, run := range got {
		statuses[run.ID] = run
	}
	if run := statuses[expired.ID]; run.Status != model.AgentRunTimeout || run.TerminationReason != "lease_expired" || run.LeaseExpiresAt != nil {
		t.Fatalf("expired run=%+v", run)
	}
	if run := statuses[healthy.ID]; run.Status != model.AgentRunRunning || run.LeaseExpiresAt == nil {
		t.Fatalf("healthy run=%+v", run)
	}
	if run := statuses[learning.ID]; run.Status != model.AgentRunRunning {
		t.Fatalf("learning run=%+v", run)
	}
}

func TestRepositoryRunStateMachineAndOwnership(t *testing.T) {
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Session{}, &model.AgentRun{}, &model.AgentStep{}); err != nil {
		t.Fatal(err)
	}
	repository := NewRepository(db)
	ctx := context.Background()
	session := &model.Session{
		ID:       uuid.NewString(),
		UserID:   101,
		UserName: "agent-repository-test",
		Mode:     model.SessionModeAgent,
		Title:    "agent run test",
	}
	if err := db.Create(session).Error; err != nil {
		t.Fatal(err)
	}
	run := &model.AgentRun{
		ID:            uuid.NewString(),
		UserID:        session.UserID,
		SessionID:     session.ID,
		ExecutionKind: model.AgentRunExecutionLearning,
		Status:        model.AgentRunRunning,
		Version:       1,
		StartedAt:     time.Now(),
	}
	if err := repository.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	foreignRun := *run
	foreignRun.ID = uuid.NewString()
	foreignRun.UserID = 202
	if err := repository.CreateRun(ctx, &foreignRun); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("cross-user run creation error = %v, want ErrSessionNotFound", err)
	}
	if _, err := repository.GetRun(ctx, 202, run.SessionID, run.ID); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("cross-user run lookup error = %v, want ErrRunNotFound", err)
	}
	step := &model.AgentStep{
		UserID:    run.UserID,
		RunID:     run.ID,
		Sequence:  1,
		Type:      model.AgentStepRetrieve,
		SourceIDs: []string{"chunk-1", "chunk-2"},
	}
	if err := repository.AppendStep(ctx, run.SessionID, step); err != nil {
		t.Fatal(err)
	}
	steps, err := repository.ListSteps(ctx, run.UserID, run.SessionID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || len(steps[0].SourceIDs) != 2 {
		t.Fatalf("persisted steps = %#v", steps)
	}

	completed, err := repository.TransitionRun(
		ctx, run.UserID, run.SessionID, run.ID, 1,
		model.AgentRunRunning, model.AgentRunCompleted, "answered", "final answer",
	)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Version != 2 || completed.Status != model.AgentRunCompleted || completed.FinishedAt == nil {
		t.Fatalf("completed run = %#v", completed)
	}
	if _, err := repository.TransitionRun(
		ctx, run.UserID, run.SessionID, run.ID, 1,
		model.AgentRunRunning, model.AgentRunCompleted, "answered", "duplicate",
	); !errors.Is(err, ErrRunConflict) {
		t.Fatalf("repeated completion error = %v, want ErrRunConflict", err)
	}
	if _, err := repository.TransitionRun(
		ctx, run.UserID, run.SessionID, run.ID, completed.Version,
		model.AgentRunCompleted, model.AgentRunRunning, "", "",
	); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("terminal rollback error = %v, want ErrInvalidTransition", err)
	}
}

func TestRepositorySerializesRunningRunsAndReplaysRequest(t *testing.T) {
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Session{}, &model.AgentRun{}, &model.Message{}); err != nil {
		t.Fatal(err)
	}
	repository := NewRepository(db)
	ctx := context.Background()
	session := &model.Session{ID: uuid.NewString(), UserID: time.Now().UnixNano(), UserName: "agent-concurrent", Mode: model.SessionModeAgent}
	if err := db.Create(session).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Where("session_id = ?", session.ID).Delete(&model.AgentRun{})
		db.Unscoped().Where("id = ?", session.ID).Delete(&model.Session{})
	})
	newRun := func(requestKey string) *model.AgentRun {
		now, leaseEnds := time.Now(), time.Now().Add(time.Minute)
		return &model.AgentRun{ID: uuid.NewString(), UserID: session.UserID, SessionID: session.ID, RequestKey: &requestKey, InputHash: strings.Repeat("a", 64), ExecutionKind: model.AgentRunExecutionChat, Status: model.AgentRunRunning, Version: 1, StartedAt: now, LeaseToken: uuid.NewString(), LeaseExpiresAt: &leaseEnds, HeartbeatAt: &now}
	}
	runs := []*model.AgentRun{newRun("request-concurrent-a"), newRun("request-concurrent-b")}
	errs := make([]error, len(runs))
	var wait sync.WaitGroup
	for i := range runs {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, errs[index] = repository.AcquireRun(ctx, runs[index])
		}(i)
	}
	wait.Wait()
	successes, busy := 0, 0
	var winner *model.AgentRun
	for i, acquireErr := range errs {
		switch {
		case acquireErr == nil:
			successes++
			winner = runs[i]
		case errors.Is(acquireErr, ErrRunInProgress):
			busy++
		default:
			t.Fatalf("concurrent acquire error=%v", acquireErr)
		}
	}
	if successes != 1 || busy != 1 {
		t.Fatalf("successes=%d busy=%d errors=%v", successes, busy, errs)
	}
	completed, _, err := repository.CompleteRun(ctx, winner.UserID, winner.SessionID, winner.ID, winner.LeaseToken, 1, &model.Message{SessionID: winner.SessionID, UserName: session.UserName, Content: "answer", IsUser: false}, "answer", `{"runId":"`+winner.ID+`","answer":"answer"}`)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := repository.AcquireRun(ctx, winner)
	if err != nil || replay.Created || replay.Run.ID != winner.ID || replay.Run.ResultJSON == "" {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	changed := *winner
	changed.ID = uuid.NewString()
	changed.InputHash = strings.Repeat("b", 64)
	conflict, err := repository.AcquireRun(ctx, &changed)
	if !errors.Is(err, ErrRunRequestConflict) || conflict == nil || conflict.Run.ID != winner.ID {
		t.Fatalf("conflict=%+v err=%v", conflict, err)
	}

	waitingKey := "request-waiting-run"
	waiting := &model.AgentRun{ID: uuid.NewString(), UserID: session.UserID, SessionID: session.ID, ExecutionKind: model.AgentRunExecutionLearning, Status: model.AgentRunRunning, Version: 1, StartedAt: time.Now(), InputSummary: waitingKey}
	if err := repository.CreateRun(ctx, waiting); err != nil {
		t.Fatal(err)
	}
	waiting, err = repository.TransitionRun(ctx, waiting.UserID, waiting.SessionID, waiting.ID, 1, model.AgentRunRunning, model.AgentRunWaitingUser, "waiting", "")
	if err != nil {
		t.Fatal(err)
	}
	activeKey := "request-active-run"
	active := newRun(activeKey)
	active.InputHash = strings.Repeat("d", 64)
	if _, err := repository.AcquireRun(ctx, active); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.TransitionRun(ctx, waiting.UserID, waiting.SessionID, waiting.ID, waiting.Version, model.AgentRunWaitingUser, model.AgentRunRunning, "", ""); !errors.Is(err, ErrRunInProgress) {
		t.Fatalf("waiting to running conflict=%v", err)
	}
	_ = completed
}

func TestRepositoryConcurrentAcquireCompleteAndReaperKeepTerminalState(t *testing.T) {
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Session{}, &model.AgentRun{}, &model.Message{}); err != nil {
		t.Fatal(err)
	}

	baseUserID := time.Now().UnixNano()
	const workerCount = 8
	const rounds = 16
	const ballastCount = 96
	workerSessions := make([]model.Session, 0, workerCount)
	ballastSessions := make([]model.Session, 0, ballastCount)
	for index := 0; index < workerCount+ballastCount; index++ {
		session := model.Session{
			ID: uuid.NewString(), UserID: baseUserID + int64(index),
			UserName: fmt.Sprintf("run-lock-order-%d", index), Mode: model.SessionModeAgent,
		}
		if err := db.Create(&session).Error; err != nil {
			t.Fatal(err)
		}
		if index < workerCount {
			workerSessions = append(workerSessions, session)
		} else {
			ballastSessions = append(ballastSessions, session)
		}
	}
	allSessions := append(append([]model.Session(nil), workerSessions...), ballastSessions...)
	t.Cleanup(func() {
		ids := make([]string, 0, len(allSessions))
		for _, session := range allSessions {
			ids = append(ids, session.ID)
		}
		db.Where("session_id IN ?", ids).Delete(&model.Message{})
		db.Where("session_id IN ?", ids).Delete(&model.AgentRun{})
		db.Unscoped().Where("id IN ?", ids).Delete(&model.Session{})
	})

	now := time.Now().UTC()
	expiredAt := now.Add(-time.Minute)
	for index, session := range ballastSessions {
		key := fmt.Sprintf("ballast-expired-%03d", index)
		run := model.AgentRun{
			ID: uuid.NewString(), UserID: session.UserID, SessionID: session.ID,
			RequestKey: &key, InputHash: strings.Repeat("e", 64), ExecutionKind: model.AgentRunExecutionChat,
			Status: model.AgentRunRunning, Version: 1, StartedAt: expiredAt,
			LeaseToken: uuid.NewString(), LeaseExpiresAt: &expiredAt, HeartbeatAt: &expiredAt,
		}
		if err := db.Create(&run).Error; err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := make(chan struct{})
	errs := make(chan error, workerCount+1)
	var wait sync.WaitGroup
	for workerIndex, session := range workerSessions {
		workerIndex, session := workerIndex, session
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			repository := NewRepository(db)
			for round := 0; round < rounds; round++ {
				requestKey := fmt.Sprintf("lock-order-%02d-%03d", workerIndex, round)
				heartbeat, leaseEnd := time.Now().UTC(), time.Now().UTC().Add(time.Minute)
				run := &model.AgentRun{
					ID: uuid.NewString(), UserID: session.UserID, SessionID: session.ID,
					RequestKey: &requestKey, InputHash: strings.Repeat("a", 64), ExecutionKind: model.AgentRunExecutionChat,
					Status: model.AgentRunRunning, Version: 1, StartedAt: heartbeat,
					LeaseToken: uuid.NewString(), LeaseExpiresAt: &leaseEnd, HeartbeatAt: &heartbeat,
				}
				acquired, err := repository.AcquireRun(ctx, run)
				if err != nil {
					errs <- fmt.Errorf("worker %d round %d acquire: %w", workerIndex, round, err)
					return
				}
				answer := fmt.Sprintf("answer-%d-%d", workerIndex, round)
				if _, _, err := repository.CompleteRun(ctx, session.UserID, session.ID, acquired.Run.ID, acquired.Run.LeaseToken, acquired.Run.Version,
					&model.Message{SessionID: session.ID, UserName: session.UserName, Content: answer}, answer,
					fmt.Sprintf(`{"runId":%q,"answer":%q}`, acquired.Run.ID, answer)); err != nil {
					errs <- fmt.Errorf("worker %d round %d complete: %w", workerIndex, round, err)
					return
				}
			}
		}()
	}
	wait.Add(1)
	go func() {
		defer wait.Done()
		<-start
		repository := NewRepository(db)
		for {
			count, err := repository.ExpireAbandonedChatRuns(ctx, now, 32)
			if err != nil {
				errs <- fmt.Errorf("reaper: %w", err)
				return
			}
			if count == 0 {
				return
			}
		}
	}()
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		return
	}

	var running int64
	if err := db.Model(&model.AgentRun{}).Where("session_id IN ? AND status = ?", sessionIDs(allSessions), model.AgentRunRunning).Count(&running).Error; err != nil {
		t.Fatal(err)
	}
	if running != 0 {
		t.Fatalf("running runs=%d, want 0", running)
	}
	var completed int64
	if err := db.Model(&model.AgentRun{}).Where("session_id IN ? AND status = ?", sessionIDs(workerSessions), model.AgentRunCompleted).Count(&completed).Error; err != nil {
		t.Fatal(err)
	}
	if completed != workerCount*rounds {
		t.Fatalf("completed runs=%d want=%d", completed, workerCount*rounds)
	}
}

func sessionIDs(sessions []model.Session) []string {
	ids := make([]string, 0, len(sessions))
	for _, session := range sessions {
		ids = append(ids, session.ID)
	}
	return ids
}
