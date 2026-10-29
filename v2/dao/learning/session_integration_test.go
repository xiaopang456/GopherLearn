package learning

import (
	"GopherAI/internal/testdb"
	"GopherAI/model"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	mysqldriver "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestRepositoryLearningStateOwnershipAndActiveUniqueness(t *testing.T) {
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Session{}, &model.AgentRun{}, &model.LearningSession{}, &model.LearningQuestion{}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	repository := NewRepository(db)
	chat := model.Session{ID: uuid.NewString(), UserID: 701, UserName: "learning-integration", Mode: model.SessionModeAgent, Title: "learning"}
	if err := db.Create(&chat).Error; err != nil {
		t.Fatal(err)
	}
	defer func() {
		learningIDs := db.Model(&model.LearningSession{}).Select("id").Where("session_id = ?", chat.ID)
		db.Where("learning_session_id IN (?)", learningIDs).Delete(&model.LearningQuestion{})
		db.Where("session_id = ?", chat.ID).Delete(&model.LearningSession{})
		db.Where("session_id = ?", chat.ID).Delete(&model.AgentRun{})
		db.Where("id = ?", chat.ID).Delete(&model.Session{})
	}()
	createRun := func() model.AgentRun {
		run := model.AgentRun{ID: uuid.NewString(), UserID: chat.UserID, SessionID: chat.ID, Status: model.AgentRunRunning, Version: 1, StartedAt: time.Now()}
		if err := db.Create(&run).Error; err != nil {
			t.Fatal(err)
		}
		return run
	}
	newLearning := func(run model.AgentRun) *model.LearningSession {
		activeKey := model.LearningActiveKey(chat.UserID, chat.ID)
		return &model.LearningSession{
			ID: uuid.NewString(), UserID: chat.UserID, SessionID: chat.ID, AgentRunID: run.ID,
			Topic: "Redis", TopicKey: "redis", Status: model.LearningSessionCreated,
			CurrentQuestion: 1, TotalQuestions: 3, Version: 1, ActiveKey: &activeKey,
			ExpiresAt: time.Now().Add(time.Hour),
		}
	}

	first := newLearning(createRun())
	if err := repository.CreateSession(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := newLearning(createRun())
	if err := repository.CreateSession(ctx, second); !errors.Is(err, ErrActiveLearningSession) {
		t.Fatalf("second active create error = %v, want ErrActiveLearningSession", err)
	}
	if _, err := repository.GetByID(ctx, 999, chat.ID, first.ID); !errors.Is(err, ErrLearningSessionNotFound) {
		t.Fatalf("cross-user lookup error = %v", err)
	}
	asking, err := repository.Transition(ctx, chat.UserID, chat.ID, first.ID, 1, model.LearningSessionCreated, model.LearningSessionAsking)
	if err != nil {
		t.Fatal(err)
	}
	evidence := []model.LearningEvidence{{
		EvidenceID: 1, DocumentID: "doc-1", DocumentName: "redis.md", DocVersion: 2,
		ChunkID: "chunk-1", StartLine: 4, EndLine: 12, ContentHash: "hash", Snapshot: "snapshot-v2",
	}}
	questions := make([]model.LearningQuestion, 3)
	for i := range questions {
		questions[i] = model.LearningQuestion{
			ID: uuid.NewString(), UserID: chat.UserID, LearningSessionID: first.ID, Ordinal: i + 1,
			Type: model.LearningQuestionShortAnswer, Question: "question", AssessmentGoal: "goal", ReferenceAnswer: "answer",
			Rubric: []string{"point"}, Evidence: evidence,
		}
	}
	waiting, err := repository.SaveQuestionsAndWait(ctx, chat.UserID, chat.ID, first.ID, asking.Version, questions)
	if err != nil {
		t.Fatal(err)
	}
	if waiting.Status != model.LearningSessionWaitingAnswer || waiting.Version != 3 {
		t.Fatalf("waiting session = %#v", waiting)
	}
	current, err := repository.GetQuestion(ctx, chat.UserID, first.ID, 1)
	if err != nil || current.Evidence[0].Snapshot != "snapshot-v2" {
		t.Fatalf("current question = %#v, err = %v", current, err)
	}
	history, err := repository.ListTopicQuestions(ctx, chat.UserID, "redis", 10)
	if err != nil || len(history) != 3 || history[0].UserID != chat.UserID {
		t.Fatalf("topic history = %#v, err = %v", history, err)
	}
	otherHistory, err := repository.ListTopicQuestions(ctx, 999, "redis", 10)
	if err != nil || len(otherHistory) != 0 {
		t.Fatalf("cross-user history = %#v, err = %v", otherHistory, err)
	}
	cancelled, err := repository.Transition(ctx, chat.UserID, chat.ID, first.ID, waiting.Version, model.LearningSessionWaitingAnswer, model.LearningSessionCancelled)
	if err != nil || cancelled.ActiveKey != nil {
		t.Fatalf("cancelled session = %#v, err = %v", cancelled, err)
	}
	if err := repository.CreateSession(ctx, second); err != nil {
		t.Fatalf("create after terminal release: %v", err)
	}
}

func TestRepositoryAnswerIdempotencyRecoveryProgressAndCorrection(t *testing.T) {
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Session{}, &model.AgentRun{}, &model.LearningSession{}, &model.LearningQuestion{}, &model.LearningAttempt{}, &model.TopicProgress{}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	repository := NewRepository(db)
	chat := model.Session{ID: uuid.NewString(), UserID: 702, UserName: "learning-answer-integration", Mode: model.SessionModeAgent, Title: "learning answer"}
	if err := db.Create(&chat).Error; err != nil {
		t.Fatal(err)
	}
	run := model.AgentRun{ID: uuid.NewString(), UserID: chat.UserID, SessionID: chat.ID, Status: model.AgentRunRunning, Version: 1, StartedAt: time.Now()}
	if err := db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	activeKey := model.LearningActiveKey(chat.UserID, chat.ID)
	session := &model.LearningSession{ID: uuid.NewString(), UserID: chat.UserID, SessionID: chat.ID, AgentRunID: run.ID, Topic: "RPC", TopicKey: "rpc", Status: model.LearningSessionCreated, CurrentQuestion: 1, TotalQuestions: 3, Version: 1, ActiveKey: &activeKey, ExpiresAt: time.Now().Add(time.Hour)}
	defer func() {
		db.Where("user_id = ?", chat.UserID).Delete(&model.LearningAttempt{})
		db.Where("user_id = ?", chat.UserID).Delete(&model.TopicProgress{})
		db.Where("learning_session_id = ?", session.ID).Delete(&model.LearningQuestion{})
		db.Where("id = ?", session.ID).Delete(&model.LearningSession{})
		db.Where("id = ?", run.ID).Delete(&model.AgentRun{})
		db.Where("id = ?", chat.ID).Delete(&model.Session{})
	}()
	if err := repository.CreateSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	asking, err := repository.Transition(ctx, chat.UserID, chat.ID, session.ID, 1, model.LearningSessionCreated, model.LearningSessionAsking)
	if err != nil {
		t.Fatal(err)
	}
	evidence := []model.LearningEvidence{{EvidenceID: 1, DocumentID: "doc-rpc", DocumentName: "rpc.md", DocVersion: 4, ChunkID: "chunk-1", StartLine: 10, EndLine: 20, ContentHash: "hash-rpc", Snapshot: "Deadline 必须沿 RPC 调用链传播。"}}
	questions := make([]model.LearningQuestion, 3)
	for i := range questions {
		questions[i] = model.LearningQuestion{ID: uuid.NewString(), UserID: chat.UserID, LearningSessionID: session.ID, Ordinal: i + 1, Type: model.LearningQuestionShortAnswer, Question: "question", AssessmentGoal: "goal", ReferenceAnswer: "answer", Rubric: []string{"point"}, Evidence: evidence}
	}
	if _, err := repository.SaveQuestionsAndWait(ctx, chat.UserID, chat.ID, session.ID, asking.Version, questions); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	claimInput := func(question int, submission, lease string, at time.Time) ClaimAnswerInput {
		return ClaimAnswerInput{Attempt: model.LearningAttempt{ID: uuid.NewString(), UserID: chat.UserID, SessionID: chat.ID, LearningSessionID: session.ID, QuestionID: questions[question-1].ID, SubmissionID: submission, Status: model.LearningAttemptGrading, Answer: "my answer", LeaseToken: lease}, Now: at, LeaseEnds: at.Add(time.Minute)}
	}
	first, err := repository.ClaimAnswer(ctx, claimInput(1, "submission-1", "lease-1", now))
	if err != nil || !first.NeedsGrading || first.Attempt.Evidence[0].DocVersion != 4 {
		t.Fatalf("first claim = %#v, err = %v", first, err)
	}
	if _, err := repository.ClaimAnswer(ctx, claimInput(1, "submission-1", "other-lease", now)); !errors.Is(err, ErrGradingInProgress) {
		t.Fatalf("same submission while grading error = %v", err)
	}
	if _, err := repository.ClaimAnswer(ctx, claimInput(1, "submission-other", "lease-other", now)); !errors.Is(err, ErrGradingInProgress) {
		t.Fatalf("concurrent submission error = %v", err)
	}
	completed, err := repository.CompleteAnswer(ctx, chat.UserID, first.Attempt.ID, "lease-1", GradeCommit{Correctness: 60, Completeness: 25, Clarity: 15, Feedback: "correct", Confidence: 1}, now)
	if err != nil || completed.Session.CurrentQuestion != 2 || completed.Progress.ValidAttempts != 1 || completed.Progress.Level != model.TopicProgressInsufficient {
		t.Fatalf("completed = %#v, err = %v", completed, err)
	}
	replay, err := repository.ClaimAnswer(ctx, claimInput(1, "submission-1", "lease-replay", now))
	if err != nil || !replay.Replay || replay.NeedsGrading || replay.Attempt.ID != first.Attempt.ID {
		t.Fatalf("replay = %#v, err = %v", replay, err)
	}

	staleAt := now.Add(time.Minute)
	stale := claimInput(2, "submission-stale", "lease-stale", staleAt)
	stale.LeaseEnds = staleAt.Add(time.Millisecond)
	if _, err := repository.ClaimAnswer(ctx, stale); err != nil {
		t.Fatal(err)
	}
	recovered, err := repository.ClaimAnswer(ctx, claimInput(2, "submission-2", "lease-2", staleAt.Add(2*time.Millisecond)))
	if err != nil || !recovered.NeedsGrading {
		t.Fatalf("recovered claim = %#v, err = %v", recovered, err)
	}
	if _, err := repository.CompleteAnswer(ctx, chat.UserID, recovered.Attempt.ID, "lease-2", GradeCommit{Correctness: 45, Completeness: 20, Clarity: 10, Feedback: "mostly correct", MissingPoints: []string{"Deadline 传播"}, Confidence: .8}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	third, err := repository.ClaimAnswer(ctx, claimInput(3, "submission-3", "lease-3", now.Add(2*time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	final, err := repository.CompleteAnswer(ctx, chat.UserID, third.Attempt.ID, "lease-3", GradeCommit{Correctness: 50, Completeness: 20, Clarity: 10, Feedback: "good", MissingPoints: []string{"错误码"}, Confidence: .9}, now.Add(2*time.Minute))
	if err != nil || final.Session.Status != model.LearningSessionCompleted || final.Progress.ValidAttempts != 3 {
		t.Fatalf("final = %#v, err = %v", final, err)
	}
	corrected, progress, err := repository.CorrectAttempt(ctx, chat.UserID, first.Attempt.ID, 50, "人工复核发现关键概念错误", now.Add(3*time.Minute))
	if err != nil || corrected.AutoTotal != 100 || corrected.EffectiveTotal != 50 || corrected.CorrectionReason == "" || progress.MasteryScore >= final.Progress.MasteryScore {
		t.Fatalf("corrected = %#v, progress = %#v, err = %v", corrected, progress, err)
	}
}
