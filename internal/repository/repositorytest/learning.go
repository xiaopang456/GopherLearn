package repositorytest

import (
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// LearningSuite runs the quiz contract against one store.
//
// It exists for the same reason MemorySuite does: the rules below are not storage
// conveniences. "One open quiz per conversation", "one grade per submission", "a
// score a superseded grader cannot write", and "mastery recomputed from what is
// actually stored" are the product's claims about itself. If one store implements
// them and the other approximately implements them, then whether the system tells a
// learner the truth depends on which repository the process was configured with.
//
// Every subtest builds its own store, so no case inherits another's rows.
func LearningSuite(t *testing.T, newStore func(*testing.T) repository.Repository) {
	t.Helper()
	ctx := t.Context()

	t.Run("a quiz is created in the state it starts in", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "learner")
		chatID := mustSession(t, store, userID)

		quiz := mustQuiz(t, store, userID, chatID, "Redis 持久化")
		if quiz.Status != domain.LearningSessionCreated || quiz.Version != 1 || quiz.CurrentQuestion != 1 {
			t.Fatalf("stored quiz = %+v, want created/version 1/question 1", quiz)
		}
		if quiz.TotalQuestions != domain.LearningQuestionCount {
			t.Fatalf("total = %d, want %d", quiz.TotalQuestions, domain.LearningQuestionCount)
		}
		if quiz.TopicKey != domain.NormalizeLearningTopicKey("Redis 持久化") {
			t.Fatalf("topic key = %q, want the normalized form", quiz.TopicKey)
		}
		if quiz.ID == "" || quiz.CreatedAt.IsZero() {
			t.Fatalf("stored identity missing: %#v", quiz)
		}

		read, err := store.GetLearningSession(ctx, userID, quiz.ID)
		if err != nil {
			t.Fatal(err)
		}
		if read.Status != quiz.Status || read.Version != quiz.Version {
			t.Fatalf("re-read = %+v, want the stored quiz", read)
		}
		active, err := store.GetActiveLearningSession(ctx, userID, chatID)
		if err != nil || active.ID != quiz.ID {
			t.Fatalf("active quiz = %v, %v, want the quiz just created", active, err)
		}
	})

	t.Run("a quiz cannot be opened over somebody else's conversation", func(t *testing.T) {
		store := newStore(t)
		owner := mustUser(t, store, "owner")
		intruder := mustUser(t, store, "intruder")
		theirChat := mustSession(t, store, intruder)

		for _, sessionID := range []string{theirChat, "ses-does-not-exist"} {
			shape := newQuizShape(owner, sessionID, "越权")
			if _, err := store.CreateLearningSession(ctx, shape); !errors.Is(err, repository.ErrNotFound) {
				t.Fatalf("quiz over session %q = %v, want the same not-found answer as a missing one", sessionID, err)
			}
		}
		if _, err := store.CreateLearningSession(ctx, newQuizShape(owner, "", "越权")); !errors.Is(err, repository.ErrInvalidArgument) {
			t.Fatalf("quiz with no conversation = %v, want an argument refusal", err)
		}
	})

	t.Run("only one quiz is open on a conversation at a time", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "learner")
		chatID := mustSession(t, store, userID)
		first := mustQuiz(t, store, userID, chatID, "Go 调度")

		if _, err := store.CreateLearningSession(ctx, newQuizShape(userID, chatID, "Go 调度")); !errors.Is(err, repository.ErrLearningSessionActive) {
			t.Fatalf("second open quiz = %v, want a refusal that names the active-quiz rule", err)
		}
		// A different conversation may hold its own quiz: the rule is one per
		// conversation, not one per person.
		if _, err := store.CreateLearningSession(ctx, newQuizShape(userID, mustSession(t, store, userID), "Go 调度")); err != nil {
			t.Fatalf("a second conversation must be allowed its own quiz: %v", err)
		}

		// Finishing the first must release it: a terminal row that kept its open marker
		// would lock a learner out of quizzes for the rest of the day.
		if _, err := store.TransitionLearningSession(ctx, userID, first.ID,
			domain.LearningSessionCreated, domain.LearningSessionCancelled, first.Version, repository.LearningLease{}); err != nil {
			t.Fatal(err)
		}
		replacement := mustQuiz(t, store, userID, chatID, "Go 调度")
		if replacement.ID == first.ID {
			t.Fatal("the replacement quiz reused the cancelled quiz's id")
		}
	})

	t.Run("two concurrent starts store one quiz", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "learner")
		chatID := mustSession(t, store, userID)

		const racers = 2
		var wg sync.WaitGroup
		errs := make([]error, racers)
		wg.Add(racers)
		for index := range racers {
			go func(slot int) {
				defer wg.Done()
				_, err := store.CreateLearningSession(ctx, newQuizShape(userID, chatID, "并发"))
				errs[slot] = err
			}(index)
		}
		wg.Wait()

		// The claim being tested is "at most one Start creates a quiz", so any refusal
		// counts as losing the race: an in-memory store refuses on the open-quiz rule,
		// while an engine that serializes write transactions refuses with a busy error
		// before the rule is even consulted. What must never happen is two successes.
		// That the refusal is the *rule* rather than the scheduler on a SQL engine is
		// proven separately, by TestLearningUniqueKeysAreEnforcedByTheDatabase, which
		// inserts the competing row directly and leaves no scheduler in the loop.
		winners := 0
		for index, err := range errs {
			if err == nil {
				winners++
				continue
			}
			if errors.Is(err, repository.ErrNotFound) || errors.Is(err, repository.ErrInvalidArgument) {
				t.Fatalf("racer %d was refused for the wrong reason: %v", index, err)
			}
		}
		if winners != 1 {
			t.Fatalf("%d of %d starts stored a quiz, want exactly one: the open marker is a uniqueness rule, "+
				"not a check two callers can each win", winners, racers)
		}
		if _, err := store.GetActiveLearningSession(ctx, userID, chatID); err != nil {
			t.Fatalf("the winning quiz must be the open one: %v", err)
		}
	})

	t.Run("only the edges in the table are writable", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "learner")
		chatID := mustSession(t, store, userID)
		quiz := mustQuiz(t, store, userID, chatID, "状态机")

		// created -> completed is not an edge: a quiz cannot be finished by declaring it
		// finished, which is exactly the shortcut the transition table exists to block.
		_, err := store.TransitionLearningSession(ctx, userID, quiz.ID,
			domain.LearningSessionCreated, domain.LearningSessionCompleted, quiz.Version, repository.LearningLease{})
		if !errors.Is(err, repository.ErrLearningTransition) {
			t.Fatalf("illegal edge = %v, want ErrLearningTransition", err)
		}
		if errors.Is(err, repository.ErrConflict) {
			t.Fatal("an illegal transition must not answer to ErrConflict: no retry makes it succeed")
		}
		unchanged, err := store.GetLearningSession(ctx, userID, quiz.ID)
		if err != nil {
			t.Fatal(err)
		}
		if unchanged.Status != domain.LearningSessionCreated || unchanged.Version != quiz.Version {
			t.Fatalf("a refused transition moved the quiz: %+v", unchanged)
		}

		// The generation edge carries a lease, and a generation with no deadline is
		// indistinguishable from a crashed one, so it is refused outright.
		if _, err := store.TransitionLearningSession(ctx, userID, quiz.ID,
			domain.LearningSessionCreated, domain.LearningSessionAsking, quiz.Version,
			repository.LearningLease{}); !errors.Is(err, repository.ErrInvalidArgument) {
			t.Fatalf("generation without a lease = %v, want a refusal", err)
		}
		asking, err := store.TransitionLearningSession(ctx, userID, quiz.ID,
			domain.LearningSessionCreated, domain.LearningSessionAsking, quiz.Version,
			repository.LearningLease{Token: "gen-1", Until: time.Now().UTC().Add(time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		if asking.GenerationToken != "gen-1" || asking.GenerationExpiresAt == nil {
			t.Fatalf("asking quiz = %+v, want the generation claim recorded", asking)
		}
		if asking.Version != quiz.Version+1 {
			t.Fatalf("version = %d, want %d", asking.Version, quiz.Version+1)
		}

		// A stale version cannot write, and the refusal is a conflict rather than a
		// complaint about the table: the edge was legal, the read was old.
		stale, err := store.TransitionLearningSession(ctx, userID, quiz.ID,
			domain.LearningSessionAsking, domain.LearningSessionWaitingAnswer, quiz.Version, repository.LearningLease{})
		if !errors.Is(err, repository.ErrLearningStaleWrite) {
			t.Fatalf("stale write = %v, want ErrLearningStaleWrite", err)
		}
		if !errors.Is(err, repository.ErrConflict) {
			t.Fatal("a lost race must answer to ErrConflict")
		}
		if stale != nil {
			t.Fatalf("a refused transition returned a quiz: %+v", stale)
		}
	})

	t.Run("questions and the opening of a quiz are one write", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "learner")
		chatID := mustSession(t, store, userID)
		quiz := mustAskingQuiz(t, store, userID, chatID, "证据快照")

		if _, err := store.StoreLearningQuestions(ctx, userID, quiz.ID,
			newQuestions(userID, quiz.ID, "Redis 的持久化"), quiz.Version); err != nil {
			t.Fatal(err)
		}
		stored, err := store.ListLearningQuestions(ctx, userID, quiz.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(stored) != domain.LearningQuestionCount {
			t.Fatalf("stored %d questions, want %d", len(stored), domain.LearningQuestionCount)
		}
		for index, question := range stored {
			if question.Ordinal != index+1 {
				t.Fatalf("question %d has ordinal %d, want the ordinals to run from 1", index, question.Ordinal)
			}
			if len(question.Evidence) == 0 || question.Evidence[0].Snapshot == "" {
				t.Fatalf("question %d lost its evidence snapshot: %+v", index, question)
			}
			if question.ReferenceAnswer == "" || len(question.Rubric) == 0 {
				t.Fatalf("question %d lost what it is graded against: %+v", index, question)
			}
		}
		opened, err := store.GetLearningSession(ctx, userID, quiz.ID)
		if err != nil {
			t.Fatal(err)
		}
		if opened.Status != domain.LearningSessionWaitingAnswer {
			t.Fatalf("status = %q, want waiting_answer", opened.Status)
		}
		if opened.GenerationToken != "" || opened.GenerationExpiresAt != nil {
			t.Fatalf("an opened quiz still holds a generation claim: %+v", opened)
		}
		if _, err := store.ListLearningQuestions(ctx, mustUser(t, store, "nosy"), quiz.ID); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("another user read a quiz's questions: %v", err)
		}
	})

	t.Run("a half question set is not stored at all", func(t *testing.T) {
		cases := []struct {
			name  string
			build func(userID, quizID string) []domain.LearningQuestion
		}{
			{"too few", func(userID, quizID string) []domain.LearningQuestion {
				return newQuestions(userID, quizID, "主题")[:2]
			}},
			{"blank goal", func(userID, quizID string) []domain.LearningQuestion {
				set := newQuestions(userID, quizID, "主题")
				set[1].AssessmentGoal = "  "
				return set
			}},
			{"no evidence", func(userID, quizID string) []domain.LearningQuestion {
				set := newQuestions(userID, quizID, "主题")
				set[2].Evidence = nil
				return set
			}},
			{"wrong owner", func(userID, quizID string) []domain.LearningQuestion {
				set := newQuestions(userID, quizID, "主题")
				set[0].UserID = "someone-else"
				return set
			}},
			{"gapped ordinals", func(userID, quizID string) []domain.LearningQuestion {
				set := newQuestions(userID, quizID, "主题")
				set[2].Ordinal = 7
				return set
			}},
			{"repeated ordinal", func(userID, quizID string) []domain.LearningQuestion {
				set := newQuestions(userID, quizID, "主题")
				set[1].Ordinal = set[0].Ordinal
				return set
			}},
			{"foreign quiz", func(userID, quizID string) []domain.LearningQuestion {
				set := newQuestions(userID, quizID, "主题")
				set[0].LearningSessionID = "lsn-not-there"
				return set
			}},
		}
		for _, testCase := range cases {
			t.Run(testCase.name, func(t *testing.T) {
				store := newStore(t)
				userID := mustUser(t, store, "learner")
				chatID := mustSession(t, store, userID)
				quiz := mustAskingQuiz(t, store, userID, chatID, "部分写入")

				if _, err := store.StoreLearningQuestions(ctx, userID, quiz.ID,
					testCase.build(userID, quiz.ID), quiz.Version); !errors.Is(err, repository.ErrInvalidArgument) {
					t.Fatalf("= %v, want the whole write refused", err)
				}
				// Nothing is left behind, and the quiz is still asking. A refused insert
				// that still opened the quiz would show a learner a set with holes in it.
				if stored, err := store.ListLearningQuestions(ctx, userID, quiz.ID); err == nil && len(stored) != 0 {
					t.Fatalf("%d questions survived a refused write", len(stored))
				}
				unchanged, err := store.GetLearningSession(ctx, userID, quiz.ID)
				if err != nil {
					t.Fatal(err)
				}
				if unchanged.Status != domain.LearningSessionAsking || unchanged.Version != quiz.Version {
					t.Fatalf("a refused write moved the quiz: %+v", unchanged)
				}
			})
		}
	})

	t.Run("questions are answered in order", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "learner")
		chatID := mustSession(t, store, userID)
		quiz, questions := mustOpenQuiz(t, store, userID, chatID, "题目顺序")

		// Question two cannot be claimed while question one is open, however valid the
		// answer text is.
		ahead := newAttemptPtr(userID, quiz, questions[1], "sub-ahead")
		_, orderErr := store.BeginLearningAttempt(ctx, ahead)
		if !errors.Is(orderErr, repository.ErrLearningQuestionOrder) {
			t.Fatalf("answering out of order = %v, want a refusal that names the order", orderErr)
		}
		if errors.Is(orderErr, repository.ErrConflict) {
			t.Fatal("an ordering refusal must not be dressed up as a retryable conflict")
		}
		// A question from a quiz that is not this one is refused too, rather than being
		// answered against whichever row happens to share the id.
		stray := newAttemptPtr(userID, quiz, questions[0], "sub-stray")
		stray.QuestionID = "lq-someone-elses"
		if _, err := store.BeginLearningAttempt(ctx, stray); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("claim over a question that is not this quiz's = %v, want the answer for a missing one", err)
		}

		mustAnswer(t, store, quiz, questions[0], "sub-1", learningGrade(50, 20, 10, "答到了两点。"))
		moved, err := store.GetLearningSession(ctx, userID, quiz.ID)
		if err != nil {
			t.Fatal(err)
		}
		if moved.CurrentQuestion != 2 {
			t.Fatalf("current question = %d, want 2", moved.CurrentQuestion)
		}
		// Now it is question one that is out of order.
		backwards := newAttemptPtr(userID, quiz, questions[0], "sub-back")
		backwards.QuestionOrdinal = 1
		if _, err := store.BeginLearningAttempt(ctx, backwards); !errors.Is(err, repository.ErrLearningQuestionOrder) {
			t.Fatalf("answering a finished question again = %v, want an ordering refusal", err)
		}
	})

	t.Run("a repeated submission is graded once", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "learner")
		chatID := mustSession(t, store, userID)
		quiz, questions := mustOpenQuiz(t, store, userID, chatID, "幂等提交")
		question := questions[0]

		first, err := store.BeginLearningAttempt(ctx, newAttemptPtr(userID, quiz, question, "sub-repeat"))
		if err != nil {
			t.Fatal(err)
		}
		if !first.NeedsGrading {
			t.Fatal("the first claim must ask to be graded")
		}
		// The same submission, the same answer, while it is being graded: busy, not a
		// second grading.
		busy, err := store.BeginLearningAttempt(ctx, newAttemptPtr(userID, quiz, question, "sub-repeat"))
		if !errors.Is(err, repository.ErrLearningAnswerBusy) {
			t.Fatalf("repeat while grading = %v, want a retryable busy refusal", err)
		}
		if !errors.Is(err, repository.ErrConflict) {
			t.Fatal("busy must answer to ErrConflict so a simple caller can retry")
		}
		if busy.NeedsGrading {
			t.Fatal("a busy refusal must not ask to be graded")
		}

		commit, err := store.CompleteLearningAttempt(ctx, userID, "sub-repeat",
			first.Attempt.LeaseToken, learningGrade(40, 15, 10, "还行。"))
		if err != nil {
			t.Fatal(err)
		}
		replay, err := store.BeginLearningAttempt(ctx, newAttemptPtr(userID, quiz, question, "sub-repeat"))
		if err != nil {
			t.Fatal(err)
		}
		if replay.NeedsGrading {
			t.Fatal("a finished submission must not be offered for grading again")
		}
		if replay.Attempt.Status != domain.LearningAttemptCompleted ||
			replay.Attempt.EffectiveTotal != commit.Attempt.EffectiveTotal {
			t.Fatalf("replay = %+v, want the stored grade", replay.Attempt)
		}

		// A second completion of the same submission changes nothing: mastery counts the
		// answer once, because a response lost on the way back is not a second attempt
		// by the learner.
		before, err := store.GetTopicProgress(ctx, userID, quiz.TopicKey)
		if err != nil {
			t.Fatal(err)
		}
		again, err := store.CompleteLearningAttempt(ctx, userID, "sub-repeat", "",
			learningGrade(60, 25, 15, "满分也不该再算一次。"))
		if err != nil {
			t.Fatal(err)
		}
		if !again.Replayed {
			t.Fatalf("second completion = %+v, want the replay answer", again)
		}
		after, err := store.GetTopicProgress(ctx, userID, quiz.TopicKey)
		if err != nil {
			t.Fatal(err)
		}
		if after.Mastery != before.Mastery || after.ValidCount != before.ValidCount {
			t.Fatalf("mastery moved on a replayed grade: %+v -> %+v", before, after)
		}
	})

	t.Run("a different submission while grading is busy too", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "learner")
		chatID := mustSession(t, store, userID)
		quiz, questions := mustOpenQuiz(t, store, userID, chatID, "一次一份")
		question := questions[0]

		if _, err := store.BeginLearningAttempt(ctx, newAttemptPtr(userID, quiz, question, "sub-live")); err != nil {
			t.Fatal(err)
		}
		// One quiz grades one answer at a time, so a second submission for the same
		// question is refused as busy rather than queued behind the first.
		second := newAttemptPtr(userID, quiz, question, "sub-second")
		if _, err := store.BeginLearningAttempt(ctx, second); !errors.Is(err, repository.ErrLearningAnswerBusy) {
			t.Fatalf("second submission while grading = %v, want busy", err)
		}
	})

	t.Run("a superseded grader cannot commit", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "learner")
		chatID := mustSession(t, store, userID)
		quiz, questions := mustOpenQuiz(t, store, userID, chatID, "租约围栏")
		question := questions[0]

		claim, err := store.BeginLearningAttempt(ctx, newAttemptPtr(userID, quiz, question, "sub-fence"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.CompleteLearningAttempt(ctx, userID, "sub-fence", "not-the-token",
			learningGrade(30, 10, 5, "迟到的判分。")); !errors.Is(err, repository.ErrLearningStaleWrite) {
			t.Fatalf("commit with the wrong lease = %v, want a lost-update refusal", err)
		}
		still, err := store.GetLearningSession(ctx, userID, quiz.ID)
		if err != nil {
			t.Fatal(err)
		}
		if still.CurrentQuestion != quiz.CurrentQuestion {
			t.Fatalf("a fenced-out grader advanced the quiz to %d", still.CurrentQuestion)
		}
		// An empty token is the shape of a retry that lost its answer, so it must not be
		// able to overwrite work a live grader still holds.
		if _, err := store.CompleteLearningAttempt(ctx, userID, "sub-fence", "",
			learningGrade(60, 25, 15, "空租约也抢不到。")); !errors.Is(err, repository.ErrLearningStaleWrite) {
			t.Fatalf("commit with no lease = %v, want a lost-update refusal", err)
		}
		if _, err := store.FailLearningAttempt(ctx, userID, "sub-fence", "not-the-token",
			domain.LearningFailureGradingFailed); !errors.Is(err, repository.ErrLearningStaleWrite) {
			t.Fatalf("fail with the wrong lease = %v, want a lost-update refusal", err)
		}
		if _, err := store.CompleteLearningAttempt(ctx, userID, "sub-fence", claim.Attempt.LeaseToken,
			learningGrade(30, 10, 5, "持有租约的一方判到了。")); err != nil {
			t.Fatalf("the lease holder must still be able to commit: %v", err)
		}
	})

	t.Run("an expired lease is reclaimed and the answer retried", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "learner")
		chatID := mustSession(t, store, userID)
		quiz, questions := mustOpenQuiz(t, store, userID, chatID, "租约过期")
		question := questions[0]

		stale := newAttemptPtr(userID, quiz, question, "sub-stale")
		expired := time.Now().UTC().Add(-time.Minute)
		stale.LeaseExpiresAt = &expired
		if _, err := store.BeginLearningAttempt(ctx, stale); err != nil {
			t.Fatal(err)
		}
		// The quiz is grading and the lease is gone: the next claim recovers both, and
		// the abandoned attempt is recorded failed rather than silently overwritten.
		reclaimed, err := store.BeginLearningAttempt(ctx, newAttemptPtr(userID, quiz, question, "sub-fresh"))
		if err != nil {
			t.Fatalf("claim after an expired lease: %v", err)
		}
		if !reclaimed.NeedsGrading {
			t.Fatal("the fresh claim must be graded")
		}
		if reclaimed.Session.Status != domain.LearningSessionGrading {
			t.Fatalf("quiz after reclaim = %q, want grading", reclaimed.Session.Status)
		}
		if reclaimed.Session.CurrentQuestion != 1 {
			t.Fatalf("a reclaim must not advance the question, got %d", reclaimed.Session.CurrentQuestion)
		}
		if _, err := store.CompleteLearningAttempt(ctx, userID, "sub-fresh",
			reclaimed.Attempt.LeaseToken, learningGrade(45, 20, 10, "重来一次也判到了。")); err != nil {
			t.Fatal(err)
		}
		// The abandoned attempt keeps its own submission id and says why it stopped.
		if _, err := store.BeginLearningAttempt(ctx, newAttemptPtr(userID, quiz, question, "sub-stale")); !errors.Is(err, repository.ErrLearningSubmissionConflict) {
			t.Fatalf("reusing an abandoned submission = %v, want a refusal that keeps the failed row", err)
		}
	})

	t.Run("a grading failure leaves the question open", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "learner")
		chatID := mustSession(t, store, userID)
		quiz, questions := mustOpenQuiz(t, store, userID, chatID, "判分失败")
		question := questions[0]

		claim, err := store.BeginLearningAttempt(ctx, newAttemptPtr(userID, quiz, question, "sub-fail"))
		if err != nil {
			t.Fatal(err)
		}
		failed, err := store.FailLearningAttempt(ctx, userID, "sub-fail", claim.Attempt.LeaseToken,
			domain.LearningFailureInvalidGrade)
		if err != nil {
			t.Fatal(err)
		}
		if failed.Status != domain.LearningAttemptFailed || failed.FailureCode != domain.LearningFailureInvalidGrade {
			t.Fatalf("failed attempt = %+v, want the closed failure code recorded", failed)
		}
		if failed.LeaseToken != "" || failed.LeaseExpiresAt != nil {
			t.Fatalf("a failed attempt must release its lease: %+v", failed)
		}
		back, err := store.GetLearningSession(ctx, userID, quiz.ID)
		if err != nil {
			t.Fatal(err)
		}
		// The retry edge, and the question still where it was: a learner must not pay
		// for a failure that happened on our side.
		if back.Status != domain.LearningSessionWaitingAnswer || back.CurrentQuestion != 1 {
			t.Fatalf("quiz after a failed grading = %+v, want waiting_answer on question 1", back)
		}
		if progress, err := store.GetTopicProgress(ctx, userID, quiz.TopicKey); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("progress after a grade that never happened = %+v, %v, want no projection yet", progress, err)
		}
		if _, err := store.CompleteLearningAttempt(ctx, userID, "sub-fail", claim.Attempt.LeaseToken,
			learningGrade(60, 25, 15, "迟到的成功。")); !errors.Is(err, repository.ErrLearningStaleWrite) {
			t.Fatalf("completing a failed attempt = %v, want a lost-update refusal", err)
		}
		// The retry needs a new submission id, which is what keeps "resubmit"
		// distinguishable from "replay" and keeps the failed row as the record.
		if _, err := store.BeginLearningAttempt(ctx, newAttemptPtr(userID, quiz, question, "sub-fail")); !errors.Is(err, repository.ErrLearningSubmissionConflict) {
			t.Fatalf("retry on a failed submission = %v, want a refusal to reuse the id", err)
		}
		if _, err := store.BeginLearningAttempt(ctx, newAttemptPtr(userID, quiz, question, "sub-retry")); err != nil {
			t.Fatalf("retry under a fresh id: %v", err)
		}
	})

	t.Run("an unlawful grade is refused", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "learner")
		chatID := mustSession(t, store, userID)
		quiz, questions := mustOpenQuiz(t, store, userID, chatID, "非法评分")

		claim, err := store.BeginLearningAttempt(ctx, newAttemptPtr(userID, quiz, questions[0], "sub-bad"))
		if err != nil {
			t.Fatal(err)
		}
		for _, grade := range []domain.LearningGrade{
			{Correctness: 61, Completeness: 0, Clarity: 0, Feedback: "超出上限。"},
			{Correctness: 0, Completeness: 26, Clarity: 0, Feedback: "超出上限。"},
			{Correctness: 0, Completeness: 0, Clarity: 16, Feedback: "超出上限。"},
			{Correctness: 20, Completeness: 10, Clarity: 5, Feedback: "   "},
			{Correctness: 20, Completeness: 10, Clarity: 5, Feedback: "没有 confidence 也不行。", Confidence: 1.4},
		} {
			if _, err := store.CompleteLearningAttempt(ctx, userID, "sub-bad", claim.Attempt.LeaseToken,
				grade); !errors.Is(err, repository.ErrInvalidArgument) {
				t.Fatalf("grade %+v = %v, want the write refused", grade, err)
			}
		}
		// Nothing moved: a refusal that still advanced the quiz would let an unlawful
		// number decide what the learner is asked next.
		unchanged, err := store.GetLearningSession(ctx, userID, quiz.ID)
		if err != nil {
			t.Fatal(err)
		}
		if unchanged.CurrentQuestion != 1 || unchanged.Status != domain.LearningSessionGrading {
			t.Fatalf("a refused grade changed the quiz: %+v", unchanged)
		}
	})

	t.Run("mastery and weak points come only from stored grades", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "learner")
		chatID := mustSession(t, store, userID)
		quiz, questions := mustOpenQuiz(t, store, userID, chatID, "掌握度")

		// Three answers of 35+15+10 = 60 each. The totals are deliberately identical:
		// the projection weights by recency, and a case that depends on which of three
		// near-simultaneous commits is newest would be a coin flip rather than a test.
		// The arithmetic itself is pinned where the order is explicit, in
		// TestTopicProgressProjectionMatchesTheDeclaredArithmetic; what this case pins is
		// that the store feeds that function the rows it actually holds.
		var last domain.TopicProgress
		for index := range domain.LearningQuestionCount {
			commit := mustAnswer(t, store, quiz, questions[index],
				"sub-progress-"+string(rune('a'+index)),
				learningGrade(35, 15, 10, "这一答的评语。", "缓存击穿的判定", "虚拟节点的数目"))
			last = commit.Progress
			if commit.Attempt.EffectiveTotal != 60 {
				t.Fatalf("stored total = %d, want the sum of the three dimensions (60)",
					commit.Attempt.EffectiveTotal)
			}
			if commit.Attempt.Grade.Correctness+commit.Attempt.Grade.Completeness+
				commit.Attempt.Grade.Clarity != commit.Attempt.EffectiveTotal {
				t.Fatal("the stored total disagrees with its own stored dimensions")
			}
			if commit.Attempt.Grade.Feedback == "" {
				t.Fatal("the grade was stored without the feedback that came with it")
			}
			if last.ValidCount != index+1 {
				t.Fatalf("after %d answers the projection counts %d", index+1, last.ValidCount)
			}
		}
		if last.Mastery != 60 {
			t.Fatalf("mastery = %d, want 60: three identical scores must project to that score", last.Mastery)
		}
		// Re-reading must agree with what the write returned, or the number a user is
		// shown and the state the server moved to are two different things.
		reread, err := store.GetTopicProgress(ctx, userID, quiz.TopicKey)
		if err != nil {
			t.Fatal(err)
		}
		if reread.Mastery != last.Mastery || reread.ValidCount != last.ValidCount || reread.Level != last.Level {
			t.Fatalf("re-read progress = %+v, want %+v", reread, last)
		}
		if reread.Confidence != 0.5 || reread.Level != domain.TopicProgressDeveloping {
			t.Fatalf("progress = %+v, want confidence 0.5 and developing", reread)
		}
		// Each answer named the same two gaps, so both are counted three times and kept
		// in the order they first appeared.
		if !reflect.DeepEqual(reread.WeakPoints, []string{"缓存击穿的判定", "虚拟节点的数目"}) {
			t.Fatalf("weak points = %q, want both counted gaps in first-seen order", reread.WeakPoints)
		}
		if reread.Topic != quiz.Topic || reread.TopicKey != quiz.TopicKey {
			t.Fatalf("progress is filed under %+v, want the quiz's own topic", reread)
		}
		if reread.LastAttempt == nil {
			t.Fatal("a projection over three completed attempts cannot say when the last one happened")
		}
		listed, err := store.ListTopicProgress(ctx, userID, 10)
		if err != nil || len(listed) != 1 || listed[0].TopicKey != reread.TopicKey {
			t.Fatalf("listing = %+v, %v, want the one topic", listed, err)
		}
	})

	// This is the contract that let two implementations disagree for a whole phase: the
	// in-memory store left the row saying "grading" after the answer had been scored, while
	// the SQL store re-read the row and reported "waiting_answer". Both behaved well enough
	// for their own tests, because a reader recovered the stale state before the next
	// claim - so the divergence was only visible in what a response said to a client.
	t.Run("an advance carries the state the quiz is really in", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "learner")
		chatID := mustSession(t, store, userID)
		quiz, questions := mustOpenQuiz(t, store, userID, chatID, "推进状态")
		current := quiz

		for index := range domain.LearningQuestionCount - 1 {
			commit := mustAnswer(t, store, current, questions[index],
				"sub-advance-"+string(rune('a'+index)),
				learningGrade(35, 15, 10, "这一答的评语。", "没说出取舍"))
			if commit.Session.Status != domain.LearningSessionWaitingAnswer {
				t.Fatalf("after a graded answer the quiz says %q, want it waiting for the next answer",
					commit.Session.Status)
			}
			if commit.Session.CurrentQuestion != index+2 {
				t.Fatalf("advance left the quiz at question %d, want %d",
					commit.Session.CurrentQuestion, index+2)
			}
			if commit.Replayed {
				t.Fatal("a first answer cannot be reported as a replay")
			}
			current = &commit.Session
		}

		// The last answer closes the quiz with its final ordinal intact: a client that saw
		// "question 3 of 3, completed" must not watch the number run past the end.
		final := mustAnswer(t, store, current, questions[domain.LearningQuestionCount-1],
			"sub-advance-final", learningGrade(45, 20, 12, "够了。", "没说出取舍"))
		if final.Session.Status != domain.LearningSessionCompleted {
			t.Fatalf("after the last answer the quiz says %q, want completed", final.Session.Status)
		}
		if final.Session.CurrentQuestion != domain.LearningQuestionCount {
			t.Fatalf("a completed quiz sits at question %d of %d",
				final.Session.CurrentQuestion, domain.LearningQuestionCount)
		}
	})

	t.Run("a skip advances without changing the state", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "learner")
		chatID := mustSession(t, store, userID)
		quiz, _ := mustOpenQuiz(t, store, userID, chatID, "跳过状态")

		skipped, err := store.SkipLearningQuestion(t.Context(), userID, quiz.ID, "sub-skip-state")
		if err != nil {
			t.Fatal(err)
		}
		if skipped.Session.Status != domain.LearningSessionWaitingAnswer {
			t.Fatalf("a skipped card leaves the quiz saying %q, want it still waiting",
				skipped.Session.Status)
		}
		if skipped.Session.CurrentQuestion != 2 {
			t.Fatalf("skip moved the quiz to %d, want 2", skipped.Session.CurrentQuestion)
		}
	})

	t.Run("skipped answers contribute nothing to mastery", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "learner")
		chatID := mustSession(t, store, userID)
		quiz, questions := mustOpenQuiz(t, store, userID, chatID, "跳过")

		skipped, err := store.SkipLearningQuestion(ctx, userID, quiz.ID, "sub-skip")
		if err != nil {
			t.Fatal(err)
		}
		if skipped.NeedsGrading {
			t.Fatal("a skip is not a grade")
		}
		if skipped.Attempt.Status != domain.LearningAttemptSkipped {
			t.Fatalf("skip = %+v, want skipped", skipped.Attempt)
		}
		if skipped.Attempt.QuestionOrdinal != 1 || skipped.Attempt.QuestionID != questions[0].ID {
			t.Fatalf("skip recorded against %+v, want the question that was open", skipped.Attempt)
		}
		if skipped.Session.CurrentQuestion != 2 {
			t.Fatalf("quiz after skip is at question %d, want 2", skipped.Session.CurrentQuestion)
		}
		if progress, err := store.GetTopicProgress(ctx, userID, quiz.TopicKey); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("a skipped answer produced a projection: %+v, %v", progress, err)
		}
		// Repeating the skip is the same operation, not a second skipped attempt.
		again, err := store.SkipLearningQuestion(ctx, userID, quiz.ID, "sub-skip")
		if err != nil {
			t.Fatal(err)
		}
		if again.Attempt.ID != skipped.Attempt.ID || again.Session.CurrentQuestion != skipped.Session.CurrentQuestion {
			t.Fatalf("repeat skip = %+v, want the stored attempt at the same position", again)
		}
		// One submission id cannot be both an answer and a skip.
		if _, err := store.BeginLearningAttempt(ctx, newAttemptPtr(userID, quiz, questions[1], "sub-skip")); !errors.Is(err, repository.ErrLearningSubmissionConflict) {
			t.Fatalf("answering under a skip's id = %v, want a conflict", err)
		}

		// Skipping the rest finishes the quiz without grading anything - the one edge
		// that reaches completed from waiting_answer.
		for range domain.LearningQuestionCount {
			current, err := store.GetLearningSession(ctx, userID, quiz.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.Status.IsTerminal() {
				break
			}
			if _, err := store.SkipLearningQuestion(ctx, userID, quiz.ID,
				"sub-skip-at-"+itoa(current.CurrentQuestion)); err != nil {
				t.Fatal(err)
			}
		}
		done, err := store.GetLearningSession(ctx, userID, quiz.ID)
		if err != nil {
			t.Fatal(err)
		}
		if done.Status != domain.LearningSessionCompleted || done.CompletedAt == nil {
			t.Fatalf("quiz after skipping every question = %+v, want completed with a timestamp", done)
		}
		if _, err := store.CreateLearningSession(ctx, newQuizShape(userID, chatID, "跳过")); err != nil {
			t.Fatalf("a completed quiz must release its conversation: %v", err)
		}
	})

	t.Run("answering a finished quiz is refused", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "learner")
		chatID := mustSession(t, store, userID)
		quiz, questions := mustOpenQuiz(t, store, userID, chatID, "已结束")

		if _, err := store.TransitionLearningSession(ctx, userID, quiz.ID,
			domain.LearningSessionWaitingAnswer, domain.LearningSessionCancelled, quiz.Version,
			repository.LearningLease{}); err != nil {
			t.Fatal(err)
		}
		_, closedErr := store.BeginLearningAttempt(ctx, newAttemptPtr(userID, quiz, questions[0], "sub-closed"))
		if !errors.Is(closedErr, repository.ErrLearningQuizClosed) {
			t.Fatalf("claim on a cancelled quiz = %v, want a refusal that is not a retryable conflict", closedErr)
		}
		if errors.Is(closedErr, repository.ErrConflict) {
			t.Fatal("a closed quiz is not a conflict: retrying cannot open it again")
		}
		if _, err := store.SkipLearningQuestion(ctx, userID, quiz.ID, "sub-closed-skip"); !errors.Is(err, repository.ErrLearningQuizClosed) {
			t.Fatalf("skip on a cancelled quiz = %v, want a closed refusal", err)
		}
	})

	t.Run("a past deadline closes the quiz and releases its work", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "learner")
		chatID := mustSession(t, store, userID)
		shape := newQuizShape(userID, chatID, "过期")
		shape.ExpiresAt = time.Now().UTC().Add(-time.Minute)
		quiz, err := store.CreateLearningSession(ctx, shape)
		if err != nil {
			t.Fatal(err)
		}
		recovered, err := store.RecoverLearningSession(ctx, userID, quiz.ID)
		if err != nil {
			t.Fatal(err)
		}
		if recovered.Status != domain.LearningSessionExpired {
			t.Fatalf("status = %q, want expired", recovered.Status)
		}
		if _, err := store.CreateLearningSession(ctx, newQuizShape(userID, chatID, "过期")); err != nil {
			t.Fatalf("an expired quiz must release its conversation: %v", err)
		}
		// Recovery is idempotent, so a reader that runs it on every request cannot
		// advance a version twice.
		again, err := store.RecoverLearningSession(ctx, userID, quiz.ID)
		if err != nil {
			t.Fatal(err)
		}
		if again.Version != recovered.Version || again.Status != domain.LearningSessionExpired {
			t.Fatalf("second recovery = %+v, want the same row", again)
		}
	})

	t.Run("a deadline that has passed refuses every step", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "learner")
		chatID := mustSession(t, store, userID)
		shape := newQuizShape(userID, chatID, "过期拒绝")
		shape.ExpiresAt = time.Now().UTC().Add(-time.Minute)
		expired, err := store.CreateLearningSession(ctx, shape)
		if err != nil {
			t.Fatal(err)
		}
		// The deadline is checked on the way into a step, not only on read, so an
		// expired quiz refuses work rather than merely being reported as over.
		if _, err := store.TransitionLearningSession(ctx, userID, expired.ID,
			domain.LearningSessionCreated, domain.LearningSessionAsking, expired.Version,
			repository.LearningLease{Token: "gen-late", Until: time.Now().UTC().Add(time.Minute)}); !errors.Is(err, repository.ErrLearningQuizClosed) {
			t.Fatalf("generation claim past the deadline = %v, want a closed refusal", err)
		}
		if _, err := store.StoreLearningQuestions(ctx, userID, expired.ID,
			newQuestions(userID, expired.ID, "主题"), expired.Version); !errors.Is(err, repository.ErrLearningStaleWrite) &&
			!errors.Is(err, repository.ErrLearningQuizClosed) {
			t.Fatalf("storing questions past the deadline = %v, want a refusal to open it", err)
		}
		if _, err := store.SkipLearningQuestion(ctx, userID, expired.ID, "sub-late"); !errors.Is(err, repository.ErrLearningQuizClosed) {
			t.Fatalf("skipping past the deadline = %v, want a closed refusal", err)
		}
	})

	t.Run("an abandoned generation returns to the state it never left", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "learner")
		chatID := mustSession(t, store, userID)
		quiz := mustQuiz(t, store, userID, chatID, "生成中断")

		past := time.Now().UTC().Add(-time.Minute)
		asking, err := store.TransitionLearningSession(ctx, userID, quiz.ID,
			domain.LearningSessionCreated, domain.LearningSessionAsking, quiz.Version,
			repository.LearningLease{Token: "gen-dead", Until: past})
		if err != nil {
			t.Fatal(err)
		}
		if asking.GenerationExpiresAt == nil {
			t.Fatal("the expired generation claim was not recorded")
		}
		recovered, err := store.RecoverLearningSession(ctx, userID, quiz.ID)
		if err != nil {
			t.Fatal(err)
		}
		if recovered.Status != domain.LearningSessionCreated {
			t.Fatalf("status = %q, want created: a crashed generation must be claimable again", recovered.Status)
		}
		if recovered.GenerationToken != "" || recovered.GenerationExpiresAt != nil {
			t.Fatalf("recovered quiz still holds a claim: %+v", recovered)
		}
		// A live lease is not abandoned, and recovery must leave real work alone.
		live, err := store.TransitionLearningSession(ctx, userID, quiz.ID,
			domain.LearningSessionCreated, domain.LearningSessionAsking, recovered.Version,
			repository.LearningLease{Token: "gen-live", Until: time.Now().UTC().Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		untouched, err := store.RecoverLearningSession(ctx, userID, quiz.ID)
		if err != nil {
			t.Fatal(err)
		}
		if untouched.Status != domain.LearningSessionAsking || untouched.Version != live.Version {
			t.Fatalf("a live generation was disturbed: %+v", untouched)
		}
	})

	t.Run("quiz rows are invisible across accounts", func(t *testing.T) {
		store := newStore(t)
		owner := mustUser(t, store, "owner")
		intruder := mustUser(t, store, "intruder")
		chatID := mustSession(t, store, owner)
		quiz, questions := mustOpenQuiz(t, store, owner, chatID, "越权")

		if _, err := store.GetLearningSession(ctx, intruder, quiz.ID); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("another user read a quiz: %v", err)
		}
		if _, err := store.ListLearningQuestions(ctx, intruder, quiz.ID); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("another user read questions: %v", err)
		}
		if history, err := store.ListRecentTopicQuestions(ctx, intruder, quiz.TopicKey, 10); err != nil || len(history) != 0 {
			t.Fatalf("another user's topic history = %d rows, %v, want none", len(history), err)
		}
		if _, err := store.TransitionLearningSession(ctx, intruder, quiz.ID, quiz.Status,
			domain.LearningSessionCancelled, quiz.Version, repository.LearningLease{}); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("another user transitioned a quiz: %v", err)
		}
		if _, err := store.StoreLearningQuestions(ctx, intruder, quiz.ID,
			newQuestions(intruder, quiz.ID, "主题"), quiz.Version); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("another user stored questions into a quiz: %v", err)
		}
		if _, err := store.BeginLearningAttempt(ctx, newAttemptPtr(intruder, quiz, questions[0], "sub-intruder")); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("another user claimed an answer: %v", err)
		}
		if _, err := store.CompleteLearningAttempt(ctx, intruder, "sub-intruder", "t",
			learningGrade(1, 1, 1, "x")); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("another user completed an answer: %v", err)
		}
		if _, err := store.FailLearningAttempt(ctx, intruder, "sub-intruder", "t",
			domain.LearningFailureGradingFailed); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("another user failed an answer: %v", err)
		}
		if _, err := store.SkipLearningQuestion(ctx, intruder, quiz.ID, "sub-intruder-skip"); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("another user skipped a question: %v", err)
		}
		if _, err := store.GetTopicProgress(ctx, intruder, quiz.TopicKey); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("another user read a projection: %v", err)
		}
		if _, err := store.RecoverLearningSession(ctx, intruder, quiz.ID); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("another user recovered a quiz: %v", err)
		}
		if listed, err := store.ListTopicProgress(ctx, intruder, 10); err != nil || len(listed) != 0 {
			t.Fatalf("another user listed progress: %d rows, %v", len(listed), err)
		}
		if _, err := store.GetActiveLearningSession(ctx, intruder, chatID); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("another user found an open quiz by guessing a conversation: %v", err)
		}
	})

	t.Run("topic history is the caller's own and bounded", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "learner")
		intruder := mustUser(t, store, "intruder")
		ownerChat := mustSession(t, store, userID)
		otherChat := mustSession(t, store, userID)
		intruderChat := mustSession(t, store, intruder)

		first, err := store.ListRecentTopicQuestions(ctx, userID, "任何主题", 10)
		if err != nil || len(first) != 0 {
			t.Fatalf("history before any quiz = %d rows, %v, want none", len(first), err)
		}
		_, firstQuestions := mustOpenQuiz(t, store, userID, ownerChat, "一致性哈希")
		_, secondQuestions := mustOpenQuiz(t, store, userID, otherChat, "一致性哈希")
		mustOpenQuiz(t, store, intruder, intruderChat, "一致性哈希")

		key := domain.NormalizeLearningTopicKey("一致性哈希")
		all, err := store.ListRecentTopicQuestions(ctx, userID, key, 20)
		if err != nil {
			t.Fatal(err)
		}
		// Both of the learner's quizzes count, because a question that was generated was
		// seen; the other account's identical questions do not appear at all.
		if len(all) != 2*domain.LearningQuestionCount {
			t.Fatalf("history = %d questions, want %d from two quizzes", len(all), 2*domain.LearningQuestionCount)
		}
		bounded, err := store.ListRecentTopicQuestions(ctx, userID, key, 3)
		if err != nil {
			t.Fatal(err)
		}
		if len(bounded) != 3 {
			t.Fatalf("bounded history = %d, want 3", len(bounded))
		}
		for index := 1; index < len(bounded); index++ {
			if bounded[index].CreatedAt.After(bounded[index-1].CreatedAt) {
				t.Fatal("history must be newest first, so the cap drops the oldest exposures")
			}
		}
		if _, err := store.ListRecentTopicQuestions(ctx, userID, key, 0); !errors.Is(err, repository.ErrInvalidArgument) {
			t.Fatalf("limit 0 = %v, want a refusal rather than 'everything'", err)
		}
		if len(firstQuestions) != domain.LearningQuestionCount || len(secondQuestions) != domain.LearningQuestionCount {
			t.Fatal("each quiz must hold its own full question set")
		}
	})

	// This subtest pins the half of the mastery tie rule that lives in storage: the
	// listing's first row has to be the topic with the newest graded answer. The service
	// side of that rule - among topics that match equally well, the first row the store
	// returns is the one described - is pinned in
	// internal/service/learning_mastery_tiebreak_test.go. Neither half is enough alone:
	// "newest first" is a store promise, "first row wins" is a service promise, and a note
	// that quietly followed row identifiers instead would satisfy neither.
	t.Run("topic progress is listed newest attempt first", func(t *testing.T) {
		store := newStore(t)
		ctx := t.Context()
		userID := mustUser(t, store, "learner")
		firstChat := mustSession(t, store, userID)
		secondChat := mustSession(t, store, userID)

		// A submission id belongs to one answer forever, so a topic practised twice needs
		// two sets of them - and a re-used id would be refused as a conflict long before
		// the ordering question got asked.
		practise := func(round, chatID, topic string) {
			quiz, questions := mustOpenQuiz(t, store, userID, chatID, topic)
			for index := range domain.LearningQuestionCount {
				mustAnswer(t, store, quiz, questions[index],
					"sub-order-"+round+"-"+topic+string(rune('a'+index)),
					learningGrade(40, 15, 9, "这一答的评语。", "一个缺口"))
			}
		}

		practise("r1", firstChat, "主题甲")
		// The gap is a sleep, not a claim about the clock: the repository reads the real
		// one, and two commits landing inside the storage engine's timestamp resolution
		// would turn this assertion into a coin flip. Ten milliseconds is comfortably
		// coarser than the millisecond precision SQLite and MySQL both store at.
		time.Sleep(10 * time.Millisecond)
		practise("r2", secondChat, "主题乙")

		keyFirst := domain.NormalizeLearningTopicKey("主题甲")
		keySecond := domain.NormalizeLearningTopicKey("主题乙")
		listed, err := store.ListTopicProgress(ctx, userID, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(listed) != 2 {
			t.Fatalf("%d topics listed, want one row per topic: %+v", len(listed), listed)
		}
		if listed[0].TopicKey != keySecond || listed[1].TopicKey != keyFirst {
			t.Fatalf("listing = %q then %q, want the newly practised topic first",
				listed[0].TopicKey, listed[1].TopicKey)
		}

		// Re-practising the older topic must move that same row to the front. A projection
		// that sorted by creation time, or that grew a second row per quiz, gets the wrong
		// answer or the wrong count here.
		thirdChat := mustSession(t, store, userID)
		time.Sleep(10 * time.Millisecond)
		practise("r3", thirdChat, "主题甲")
		after, err := store.ListTopicProgress(ctx, userID, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(after) != 2 {
			t.Fatalf("%d rows after re-practising a known topic, want the one row updated", len(after))
		}
		if after[0].TopicKey != keyFirst || after[1].TopicKey != keySecond {
			t.Fatalf("listing = %q then %q, want the topic that was just answered first",
				after[0].TopicKey, after[1].TopicKey)
		}
		if after[0].ID != listed[1].ID || after[1].ID != listed[0].ID {
			t.Fatalf("the two listings are not the same two rows seen from the other side: %q,%q then %q,%q",
				listed[0].ID, listed[1].ID, after[0].ID, after[1].ID)
		}
		// That flipped pair is what excludes an identifier-ordered listing, and it has to
		// be made across the two phases rather than from one of them. The same two row
		// identifiers produced opposite orders, so no ordering of those identifiers can
		// explain both answers - which also means no order-only-by-creation-time
		// implementation can pass. A single-phase "the ids happen to disagree with
		// recency" premise was tried first and cannot hold here: the memory store numbers
		// its rows with an unpadded counter, so "tpr-9" sorts after "tpr-17" and the ids
		// agree with recency in one implementation and disagree in the other.
		if after[0].LastAttempt == nil || after[1].LastAttempt == nil ||
			!after[0].LastAttempt.After(*after[1].LastAttempt) {
			t.Fatalf("the front row is not the one with the newer attempt: %+v", after)
		}
	})

	t.Run("returned rows are detached copies", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "learner")
		chatID := mustSession(t, store, userID)
		quiz, questions := mustOpenQuiz(t, store, userID, chatID, "副本")
		commit := mustAnswer(t, store, quiz, questions[0], "sub-copy",
			learningGrade(50, 20, 10, "答完了。", "缺一个反例"))

		commit.Progress.WeakPoints[0] = "改写"
		commit.Attempt.Grade.MissingPoints[0] = "改写"
		commit.Session.TopicKey = "改写"
		questions[0].Rubric[0] = "改写"
		questions[0].Evidence[0].Snapshot = "改写"

		reread, err := store.GetTopicProgress(ctx, userID, quiz.TopicKey)
		if err != nil {
			t.Fatal(err)
		}
		if reread.WeakPoints[0] == "改写" {
			t.Fatal("a caller rewrote the stored projection through the returned slice")
		}
		stored, err := store.ListLearningQuestions(ctx, userID, quiz.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored[0].Rubric[0] == "改写" || stored[0].Evidence[0].Snapshot == "改写" {
			t.Fatal("a caller rewrote a stored question through the returned slice")
		}
		if _, err := store.GetLearningSession(ctx, userID, "改写"); !errors.Is(err, repository.ErrNotFound) {
			t.Fatal("a mutation of a returned row changed how the quiz is addressed")
		}
	})

	t.Run("a claim refuses anything the domain refuses", func(t *testing.T) {
		store := newStore(t)
		userID := mustUser(t, store, "learner")
		chatID := mustSession(t, store, userID)
		quiz, questions := mustOpenQuiz(t, store, userID, chatID, "非法入参")
		valid := newAttemptPtr(userID, quiz, questions[0], "sub-invalid")

		for name, mutate := range map[string]func(*domain.LearningAttempt){
			"no submission":     func(a *domain.LearningAttempt) { a.SubmissionID = " " },
			"no answer":         func(a *domain.LearningAttempt) { a.Answer = "" },
			"no lease":          func(a *domain.LearningAttempt) { a.LeaseToken = "" },
			"no lease deadline": func(a *domain.LearningAttempt) { a.LeaseExpiresAt = nil },
			"not grading":       func(a *domain.LearningAttempt) { a.Status = domain.LearningAttemptCompleted },
			"wrong owner":       func(a *domain.LearningAttempt) { a.UserID = "someone-else" },
			"nil quiz":          func(a *domain.LearningAttempt) { a.LearningSessionID = "lsn-nope" },
		} {
			attempt := newAttempt(userID, quiz, questions[0], "sub-invalid")
			mutate(&attempt)
			if _, err := store.BeginLearningAttempt(ctx, &attempt); err == nil {
				t.Fatalf("case %q was accepted", name)
			}
		}
		if _, err := store.BeginLearningAttempt(ctx, nil); !errors.Is(err, repository.ErrInvalidArgument) {
			t.Fatalf("nil attempt = %v, want an argument refusal", err)
		}
		if _, err := store.FailLearningAttempt(ctx, userID, "sub-invalid", valid.LeaseToken,
			domain.LearningFailureCode("dial tcp: refused")); !errors.Is(err, repository.ErrInvalidArgument) {
			t.Fatalf("an invented failure code = %v, want a refusal: the code is a closed set", err)
		}
		if _, err := store.CompleteLearningAttempt(ctx, userID, "sub-invalid", valid.LeaseToken,
			domain.LearningGrade{}); !errors.Is(err, repository.ErrInvalidArgument) {
			t.Fatal("an empty grade must be refused")
		}
	})
}

// newQuizShape builds a lawful created quiz over one conversation.
func newQuizShape(userID, chatSessionID, topic string) *domain.LearningSession {
	return &domain.LearningSession{
		UserID: userID, SessionID: chatSessionID,
		Topic: topic, TopicKey: domain.NormalizeLearningTopicKey(topic),
		Status: domain.LearningSessionCreated, CurrentQuestion: 1,
		TotalQuestions: domain.LearningQuestionCount, Version: 1,
		ExpiresAt: time.Now().UTC().Add(domain.LearningDefaultLifetime),
	}
}

// mustQuiz creates a quiz and returns it.
func mustQuiz(t *testing.T, store repository.Repository, userID, chatSessionID,
	topic string) *domain.LearningSession {
	t.Helper()
	created, err := store.CreateLearningSession(t.Context(), newQuizShape(userID, chatSessionID, topic))
	if err != nil {
		t.Fatalf("create quiz: %v", err)
	}
	return created
}

// mustAskingQuiz moves a fresh quiz into the state that is generating questions, so
// the generation step itself can be tested.
func mustAskingQuiz(t *testing.T, store repository.Repository, userID, chatSessionID,
	topic string) *domain.LearningSession {
	t.Helper()
	quiz := mustQuiz(t, store, userID, chatSessionID, topic)
	asking, err := store.TransitionLearningSession(t.Context(), userID, quiz.ID,
		domain.LearningSessionCreated, domain.LearningSessionAsking, quiz.Version,
		repository.LearningLease{Token: "gen-" + quiz.ID, Until: time.Now().UTC().Add(time.Hour)})
	if err != nil {
		t.Fatalf("begin generation: %v", err)
	}
	return asking
}

// mustOpenQuiz returns a waiting quiz with its questions stored.
func mustOpenQuiz(t *testing.T, store repository.Repository, userID, chatSessionID,
	topic string) (*domain.LearningSession, []domain.LearningQuestion) {
	t.Helper()
	ctx := t.Context()
	quiz := mustAskingQuiz(t, store, userID, chatSessionID, topic)
	opened, err := store.StoreLearningQuestions(ctx, userID, quiz.ID, newQuestions(userID, quiz.ID, topic), quiz.Version)
	if err != nil {
		t.Fatalf("store questions: %v", err)
	}
	stored, err := store.ListLearningQuestions(ctx, userID, quiz.ID)
	if err != nil {
		t.Fatal(err)
	}
	return opened, stored
}

// newQuestions builds a lawful question set whose evidence snapshots are real chunks.
func newQuestions(userID, learningSessionID, subject string) []domain.LearningQuestion {
	questions := make([]domain.LearningQuestion, 0, domain.LearningQuestionCount)
	for ordinal := 1; ordinal <= domain.LearningQuestionCount; ordinal++ {
		questions = append(questions, domain.LearningQuestion{
			UserID: userID, LearningSessionID: learningSessionID, Ordinal: ordinal,
			Type:            domain.LearningQuestionShortAnswer,
			Question:        subject + "：第 " + itoa(ordinal) + " 问",
			AssessmentGoal:  "考查" + subject + "的核心取舍",
			ReferenceAnswer: "RDB 面向恢复速度，AOF 面向持久性。",
			Rubric:          []string{"指出快照与追加日志的差别", "说出一种取舍"},
			Evidence: []domain.LearningEvidence{{
				Ordinal: 1, DocumentID: "doc-1", CollectionID: "col-1", Title: "Redis 笔记",
				Section: "持久化", DocumentVer: 1, ChunkID: "chk-" + itoa(ordinal),
				ChunkOrder: ordinal, StartLine: 1, EndLine: 12,
				Snapshot: "Redis 提供 RDB 快照与 AOF 追加日志两种持久化方式。",
			}},
		})
	}
	return questions
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := ""
	for value > 0 {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
	}
	return digits
}

// newAttempt builds the claim one answer puts in flight. The rubric and evidence are
// the store's to copy from the question, so they are left empty here.
func newAttempt(userID string, quiz *domain.LearningSession, question domain.LearningQuestion,
	submission string) domain.LearningAttempt {
	expires := time.Now().UTC().Add(time.Minute)
	return domain.LearningAttempt{
		UserID: userID, SessionID: quiz.SessionID, LearningSessionID: quiz.ID,
		QuestionID: question.ID, QuestionOrdinal: question.Ordinal, SubmissionID: submission,
		TopicKey: quiz.TopicKey, Status: domain.LearningAttemptGrading,
		Answer:         "两种方式各有侧重：快照恢复快，追加日志丢得少。",
		LeaseToken:     "lease-" + submission,
		LeaseExpiresAt: &expires,
	}
}

// newAttemptPtr is newAttempt with the pointer the repository takes.
func newAttemptPtr(userID string, quiz *domain.LearningSession, question domain.LearningQuestion,
	submission string) *domain.LearningAttempt {
	attempt := newAttempt(userID, quiz, question, submission)
	return &attempt
}

// learningGrade builds a lawful grade from its three dimensions, so every expected
// total in this suite is arithmetic a reader can check.
func learningGrade(correctness, completeness, clarity int, feedback string, missing ...string) domain.LearningGrade {
	return domain.LearningGrade{
		Correctness: correctness, Completeness: completeness, Clarity: clarity,
		Feedback: feedback, MissingPoints: append([]string(nil), missing...), Confidence: 0.8,
	}
}

// mustAnswer claims and commits one answer, failing the test if either step refuses.
func mustAnswer(t *testing.T, store repository.Repository, quiz *domain.LearningSession,
	question domain.LearningQuestion, submission string, grade domain.LearningGrade) repository.LearningGradeCommit {
	t.Helper()
	claim, err := store.BeginLearningAttempt(t.Context(), newAttemptPtr(quiz.UserID, quiz, question, submission))
	if err != nil {
		t.Fatalf("claim %s: %v", submission, err)
	}
	if !claim.NeedsGrading {
		t.Fatalf("claim %s asked for no grading", submission)
	}
	commit, err := store.CompleteLearningAttempt(t.Context(), quiz.UserID, submission,
		claim.Attempt.LeaseToken, grade)
	if err != nil {
		t.Fatalf("commit %s: %v", submission, err)
	}
	return commit
}

// OpenLearningQuiz is the shared suite's quiz builder, exported for the store-specific
// test files that need a real database opened and a quiz inside it. They get the same
// rows the suite's own cases get, so a store cannot pass its private tests against a
// shape the shared contract never builds.
func OpenLearningQuiz(t *testing.T, store repository.Repository, userID, chatSessionID,
	topic string) (*domain.LearningSession, []domain.LearningQuestion) {
	t.Helper()
	return mustOpenQuiz(t, store, userID, chatSessionID, topic)
}

// AnswerLearningQuestion is the suite's claim-then-commit step, exported for the same
// reason.
func AnswerLearningQuestion(t *testing.T, store repository.Repository, quiz *domain.LearningSession,
	question domain.LearningQuestion, submission string,
	grade domain.LearningGrade) repository.LearningGradeCommit {
	t.Helper()
	return mustAnswer(t, store, quiz, question, submission, grade)
}

// LearningAttemptShape is the suite's attempt builder, exported so a store-specific
// test claims an attempt that satisfies exactly the same identity rules.
func LearningAttemptShape(userID string, quiz *domain.LearningSession, question domain.LearningQuestion,
	submission string) *domain.LearningAttempt {
	return newAttemptPtr(userID, quiz, question, submission)
}
