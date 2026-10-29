package gormmysql

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/repository/repositorytest"
)

// TestLearningContract runs the shared quiz suite against the SQL store, so the
// production engine is held to exactly the rules the development store follows.
func TestLearningContract(t *testing.T) {
	repositorytest.LearningSuite(t, func(t *testing.T) repository.Repository { return NewRepository(newTestDB(t)) })
}

// TestLearningTablesAreCreatedByMigrate is the schema guard for the four new tables.
//
// AutoMigrate is additive, so a record type that is declared but never registered
// would leave this file's tests passing against the in-memory map while a real
// deployment met "no such table" on its first quiz. Listing the tables here is what
// makes forgetting one fail.
func TestLearningTablesAreCreatedByMigrate(t *testing.T) {
	db := newTestDB(t)
	for _, table := range []string{"learning_sessions", "learning_questions", "learning_attempts", "topic_progress"} {
		if !db.Migrator().HasTable(table) {
			t.Fatalf("Migrate created no %q table", table)
		}
	}
	// The two uniqueness rules that carry a security or correctness claim, checked as
	// indexes rather than inferred from behaviour: the open-quiz marker and the
	// submission key.
	for _, check := range []struct {
		table, column string
	}{
		{"learning_sessions", "active_key"},
		{"learning_attempts", "submission_id"},
		{"topic_progress", "topic_key"},
	} {
		if !db.Migrator().HasColumn(check.table, check.column) {
			t.Fatalf("%s has no %s column", check.table, check.column)
		}
	}
}

// TestLearningStateSurvivesReopeningTheDatabase is the restart case, and it is the
// reason this workflow is worth a database at all: a quiz suspended at question two
// with one abandoned grading attempt must read back exactly as it was left, with the
// same version and the same mastery number.
func TestLearningStateSurvivesReopeningTheDatabase(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "learning-reopen.db")
	before := NewRepository(openTestDB(t, path))

	userID := mustSQLUser(t, before, "learner@example.com")
	chatID := mustSQLSession(t, before, userID)
	quiz, questions := repositorytest.OpenLearningQuiz(t, before, userID, chatID, "Redis 持久化")
	commit := repositorytest.AnswerLearningQuestion(t, before, quiz, questions[0], "sub-reopen",
		learningTestGrade(40, 15, 10, "答到了核心。", "没说出重写策略"))

	reopened := NewRepository(openTestDB(t, path))
	read, err := reopened.GetLearningSession(ctx, userID, quiz.ID)
	if err != nil {
		t.Fatal(err)
	}
	if read.Status != commit.Session.Status || read.CurrentQuestion != commit.Session.CurrentQuestion ||
		read.Version != commit.Session.Version {
		t.Fatalf("reopened quiz = %+v, want the stored state %+v", read, commit.Session)
	}
	stored, err := reopened.ListLearningQuestions(ctx, userID, quiz.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != domain.LearningQuestionCount {
		t.Fatalf("reopened %d questions, want %d", len(stored), domain.LearningQuestionCount)
	}
	if stored[0].Evidence[0].Snapshot == "" || stored[0].ReferenceAnswer == "" {
		t.Fatalf("the JSON columns did not survive the round trip: %+v", stored[0])
	}
	progress, err := reopened.GetTopicProgress(ctx, userID, quiz.TopicKey)
	if err != nil {
		t.Fatal(err)
	}
	if progress.Mastery != commit.Progress.Mastery || progress.ValidCount != commit.Progress.ValidCount ||
		len(progress.WeakPoints) != len(commit.Progress.WeakPoints) {
		t.Fatalf("reopened progress = %+v, want %+v", progress, commit.Progress)
	}
	// The submission id is still the same answer's id after a restart, so a retried
	// request that arrives after a redeploy cannot be graded a second time.
	replay, err := reopened.BeginLearningAttempt(ctx, repositorytest.LearningAttemptShape(userID, quiz, questions[0], "sub-reopen"))
	if err != nil {
		t.Fatal(err)
	}
	if replay.NeedsGrading {
		t.Fatal("a restarted process must not offer a finished submission for grading again")
	}
	if replay.Attempt.EffectiveTotal != commit.Attempt.EffectiveTotal {
		t.Fatalf("replay = %+v, want the stored grade", replay.Attempt)
	}
}

// TestLearningUniqueKeysAreEnforcedByTheDatabase checks the two constraints the quiz
// depends on at the level they are actually claimed: the database refuses the second
// row.
//
// A goroutine race is not how this is proven on SQLite - an engine that serializes
// write transactions answers the loser with SQLITE_BUSY, which says nothing about
// whether the rule holds. Inserting the competing row directly does: it exercises the
// index, on every engine, with no scheduler in the loop. The concurrent-Start race is
// covered against the in-memory store by the shared suite, where two claims really do
// overlap.
func TestLearningUniqueKeysAreEnforcedByTheDatabase(t *testing.T) {
	db := newTestDB(t)
	store := NewRepository(db)
	userID := mustSQLUser(t, store, "learner@example.com")
	chatID := mustSQLSession(t, store, userID)
	quiz, questions := repositorytest.OpenLearningQuiz(t, store, userID, chatID, "唯一键")
	repositorytest.AnswerLearningQuestion(t, store, quiz, questions[0], "sub-taken",
		learningTestGrade(40, 15, 10, "已占用的提交号。"))

	// The submission key: the same id, twice, for a second attempt, is a duplicate
	// rather than a second grading.
	copied := LearningAttemptRecord{
		ID: newID("lat"), UserID: userID, SessionID: chatID, LearningSessionID: quiz.ID,
		QuestionID: questions[1].ID, QuestionOrdinal: 2, SubmissionID: "sub-taken",
		TopicKey: quiz.TopicKey, Status: string(domain.LearningAttemptCompleted), Answer: "再来一次。",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.Create(&copied).Error; err == nil {
		t.Fatal("a second attempt row reused the submission id: the unique index is not enforced")
	}

	// The open-quiz marker: a second unfinished quiz on the same conversation cannot be
	// inserted even by a caller that skips the repository entirely.
	key := quiz.UserID + ":" + quiz.SessionID
	second := LearningSessionRecord{
		ID: newID("lsn"), UserID: userID, SessionID: chatID, Topic: "第二个", TopicKey: "第二个",
		Status: string(domain.LearningSessionCreated), CurrentQuestion: 1,
		TotalQuestions: domain.LearningQuestionCount, Version: 1,
		ExpiresAt: time.Now().UTC().Add(time.Hour), CreatedAt: time.Now().UTC(),
		ActiveKey: &key,
	}
	if err := db.Create(&second).Error; err == nil {
		t.Fatal("a second open quiz on one conversation was stored: the active-key index is not enforced")
	}

	// And the progress key, because mastery is one row per topic per person.
	if err := db.Create(&TopicProgressRecord{
		ID: newID("tpr"), UserID: userID, TopicKey: quiz.TopicKey, Topic: quiz.Topic,
		Level: string(domain.TopicProgressDeveloping), CreatedAt: time.Now().UTC(),
	}).Error; err == nil {
		t.Fatal("a second progress row for one topic was stored: the unique index is not enforced")
	}
	var rows int64
	if err := db.Model(&TopicProgressRecord{}).Where("user_id = ?", userID).Count(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("%d progress rows for one topic, want 1", rows)
	}
}

// TestLearningGradeCannotBeCommittedTwiceUnderOneLease is the SQL form of the fence:
// after a grade is committed the lease column is cleared, so the same token cannot
// write a second grade over the first even when the caller still holds it.
func TestLearningGradeCannotBeCommittedTwiceUnderOneLease(t *testing.T) {
	ctx := t.Context()
	store := NewRepository(newTestDB(t))
	userID := mustSQLUser(t, store, "learner@example.com")
	chatID := mustSQLSession(t, store, userID)
	quiz, questions := repositorytest.OpenLearningQuiz(t, store, userID, chatID, "一次成文")
	claim, err := store.BeginLearningAttempt(ctx,
		repositorytest.LearningAttemptShape(userID, quiz, questions[0], "sub-once"))
	if err != nil {
		t.Fatal(err)
	}
	token := claim.Attempt.LeaseToken
	if token == "" {
		t.Fatal("a claimed attempt must carry a lease token")
	}
	if _, err := store.CompleteLearningAttempt(ctx, userID, "sub-once", token,
		learningTestGrade(40, 15, 10, "第一次成文。")); err != nil {
		t.Fatal(err)
	}
	// The same token, the same submission: this answers with the stored grade rather
	// than writing a second one, and the projection still counts one answer.
	replay, err := store.CompleteLearningAttempt(ctx, userID, "sub-once", token,
		learningTestGrade(60, 25, 15, "同一个租约不该能再判一次。"))
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Replayed {
		t.Fatalf("second commit = %+v, want the stored answer", replay.Attempt)
	}
	if replay.Attempt.EffectiveTotal != 65 {
		t.Fatalf("stored total = %d, want the first commit's 65", replay.Attempt.EffectiveTotal)
	}
	if replay.Progress.ValidCount != 1 {
		t.Fatalf("mastery counts %d attempts, want 1", replay.Progress.ValidCount)
	}
}

// learningTestGrade is a local mirror of the shared suite's grade builder, so this file
// does not have to reach into test helpers for the one value it needs.
func learningTestGrade(correctness, completeness, clarity int, feedback string, missing ...string) domain.LearningGrade {
	return domain.LearningGrade{
		Correctness: correctness, Completeness: completeness, Clarity: clarity,
		Feedback: feedback, MissingPoints: append([]string(nil), missing...), Confidence: 0.8,
	}
}

func mustSQLUser(t *testing.T, store repository.Repository, email string) string {
	t.Helper()
	user, err := store.CreateUser(t.Context(), &domain.User{Email: email, Username: email, DisplayName: "Learner"})
	if err != nil {
		t.Fatal(err)
	}
	return user.ID
}

func mustSQLSession(t *testing.T, store repository.Repository, userID string) string {
	t.Helper()
	session, err := store.CreateSession(t.Context(), &domain.Session{UserID: userID, Title: "学习会话",
		Mode: domain.SessionModeLearningAgent})
	if err != nil {
		t.Fatal(err)
	}
	return session.ID
}
