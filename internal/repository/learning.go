package repository

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// This file is the in-memory half of the learning store. It exists so the
// development mode of the binary runs the same quiz rules as a MySQL deployment -
// one open quiz per conversation, one attempt per submission, a grade that cannot be
// committed by a superseded grader - and so the shared conformance suite can hold
// both to one answer.
//
// Everything below takes the write lock even for reads. That is deliberate: a
// "read" in this workflow is usually read-then-decide-then-write (recover, claim,
// advance), and releasing the lock between the two halves would let a concurrent
// caller win the same race this store is supposed to settle.

// learningSubmissionKey scopes a submission id to its owner. Two users may pick the
// same uuid; one user may never point the same uuid at two different answers.
func learningSubmissionKey(userID, submissionID string) string {
	return userID + "\x00" + submissionID
}

// learningProgressKey is the in-memory shape of the (user_id, topic_key) unique
// index.
func learningProgressKey(userID, topicKey string) string {
	return userID + "\x00" + topicKey
}

func cloneLearningStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	return append([]string(nil), values...)
}

func cloneLearningEvidence(values []domain.LearningEvidence) []domain.LearningEvidence {
	if len(values) == 0 {
		return nil
	}
	return append([]domain.LearningEvidence(nil), values...)
}

func cloneLearningQuestion(question domain.LearningQuestion) domain.LearningQuestion {
	question.Rubric = cloneLearningStrings(question.Rubric)
	question.Evidence = cloneLearningEvidence(question.Evidence)
	return question
}

func cloneLearningQuestions(questions []domain.LearningQuestion) []domain.LearningQuestion {
	if len(questions) == 0 {
		return nil
	}
	cloned := make([]domain.LearningQuestion, 0, len(questions))
	for _, question := range questions {
		cloned = append(cloned, cloneLearningQuestion(question))
	}
	return cloned
}

func cloneLearningAttempt(attempt domain.LearningAttempt) domain.LearningAttempt {
	attempt.Rubric = cloneLearningStrings(attempt.Rubric)
	attempt.Evidence = cloneLearningEvidence(attempt.Evidence)
	attempt.Grade.MissingPoints = cloneLearningStrings(attempt.Grade.MissingPoints)
	return attempt
}

func cloneLearningProgress(progress domain.TopicProgress) domain.TopicProgress {
	progress.WeakPoints = cloneLearningStrings(progress.WeakPoints)
	return progress
}

// CreateLearningSession stores a new quiz.
func (r *InMemoryRepository) CreateLearningSession(ctx context.Context, session *domain.LearningSession) (*domain.LearningSession, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if session == nil {
		return nil, ErrInvalidArgument
	}
	if err := session.ValidateForCreate(); err != nil {
		return nil, ErrInvalidArgument
	}

	record := *session
	now := time.Now().UTC()
	if record.ID == "" {
		record.ID = r.nextID("lsn")
	}
	record.CreatedAt, record.UpdatedAt = now, now

	r.mu.Lock()
	defer r.mu.Unlock()
	r.initLocked()

	if _, exists := r.sessions[record.SessionID]; !exists {
		return nil, ErrNotFound
	}
	if r.sessions[record.SessionID].UserID != record.UserID {
		// Somebody else's conversation and no such conversation are the same answer, so
		// starting a quiz cannot be used to probe which sessions exist.
		return nil, ErrNotFound
	}
	if _, exists := r.learningSessions[record.ID]; exists {
		return nil, ErrConflict
	}
	if open := r.openLearningSessionLocked(record.UserID, record.SessionID); open != nil {
		return nil, ErrLearningSessionActive
	}

	r.learningSessions[record.ID] = record
	r.learningSessionOrder = append(r.learningSessionOrder, record.ID)
	stored := record
	return &stored, nil
}

// openLearningSessionLocked returns the caller's unfinished quiz on one chat
// session, if there is one.
//
// The caller must hold the lock. Terminal quizzes are excluded by status, not by a
// cleared flag, because the flag is an optimization: a finished row that somehow
// still looks open must never block the next quiz.
func (r *InMemoryRepository) openLearningSessionLocked(userID, chatSessionID string) *domain.LearningSession {
	for _, id := range r.learningSessionOrder {
		record := r.learningSessions[id]
		if record.UserID != userID || record.SessionID != chatSessionID {
			continue
		}
		if record.Status.IsTerminal() {
			continue
		}
		found := record
		return &found
	}
	return nil
}

// GetLearningSession reads one of the caller's own quizzes.
func (r *InMemoryRepository) GetLearningSession(ctx context.Context, userID, learningSessionID string) (*domain.LearningSession, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(learningSessionID) == "" {
		return nil, ErrInvalidArgument
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	r.initLocked()

	record, exists := r.learningSessions[learningSessionID]
	if !exists || record.UserID != userID {
		return nil, ErrNotFound
	}
	stored := record
	return &stored, nil
}

// GetActiveLearningSession returns the quiz currently open on one conversation.
func (r *InMemoryRepository) GetActiveLearningSession(ctx context.Context, userID, chatSessionID string) (*domain.LearningSession, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(chatSessionID) == "" {
		return nil, ErrInvalidArgument
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	r.initLocked()

	if open := r.openLearningSessionLocked(userID, chatSessionID); open != nil {
		return open, nil
	}
	return nil, ErrNotFound
}

// TransitionLearningSession moves one quiz along a legal edge at the version the
// caller read.
func (r *InMemoryRepository) TransitionLearningSession(ctx context.Context, userID, learningSessionID string,
	from, to domain.LearningSessionStatus, expectVersion uint64, lease LearningLease) (*domain.LearningSession, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(learningSessionID) == "" {
		return nil, ErrInvalidArgument
	}
	if !from.CanTransitionTo(to) {
		return nil, ErrLearningTransition
	}

	now := time.Now().UTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.initLocked()

	record, err := r.loadLearningSessionForWriteLocked(userID, learningSessionID)
	if err != nil {
		return nil, err
	}
	if record.Status != from {
		if record.Status.IsTerminal() {
			// The quiz was closed between the read and this call. Re-reading cannot help.
			return nil, ErrLearningQuizClosed
		}
		// The caller's read is stale rather than merely wrong: the state it believed in
		// was real, it has just moved. Reporting that as ErrLearningTransition would tell
		// the caller to fix its code when it should re-read.
		return nil, ErrLearningStaleWrite
	}
	if record.Version != expectVersion {
		return nil, ErrLearningStaleWrite
	}
	if record.IsExpired(now) {
		// A deadline cannot be jumped over by asking for a different edge. The expired
		// write below is what makes this idempotent for the next caller.
		return nil, ErrLearningQuizClosed
	}

	if err := r.applyLearningTransitionLocked(record, to, lease, now); err != nil {
		return nil, err
	}
	return record, nil
}

// applyLearningTransitionLocked writes one legal transition, including the
// bookkeeping that belongs to it: the active marker goes away with a terminal state,
// completion is stamped once, and a generation claim exists exactly while the quiz is
// asking.
//
// The caller has already checked the edge, the version and the deadline.
func (r *InMemoryRepository) applyLearningTransitionLocked(record *domain.LearningSession,
	to domain.LearningSessionStatus, lease LearningLease, now time.Time) error {
	from := record.Status
	if !from.CanTransitionTo(to) {
		return ErrLearningTransition
	}

	record.Status = to
	record.UpdatedAt = now
	record.Version++
	if to == domain.LearningSessionAsking {
		if lease.Token == "" || lease.Until.IsZero() {
			return ErrInvalidArgument
		}
		record.GenerationToken = lease.Token
		deadline := lease.Until
		record.GenerationExpiresAt = &deadline
	}
	if from == domain.LearningSessionAsking {
		record.GenerationToken = ""
		record.GenerationExpiresAt = nil
	}
	if to.IsTerminal() {
		record.CompletedAt = nil
		if to == domain.LearningSessionCompleted {
			stamped := now
			record.CompletedAt = &stamped
		}
	}
	r.learningSessions[record.ID] = *record
	return nil
}

// StoreLearningQuestions inserts a quiz's questions and opens it for answers, in one
// critical section.
func (r *InMemoryRepository) StoreLearningQuestions(ctx context.Context, userID, learningSessionID string,
	questions []domain.LearningQuestion, expectVersion uint64) (*domain.LearningSession, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(learningSessionID) == "" {
		return nil, ErrInvalidArgument
	}
	if len(questions) != domain.LearningQuestionCount {
		return nil, ErrInvalidArgument
	}

	now := time.Now().UTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.initLocked()

	record, err := r.loadLearningSessionForWriteLocked(userID, learningSessionID)
	if err != nil {
		return nil, err
	}
	if record.Status != domain.LearningSessionAsking {
		return nil, ErrLearningStaleWrite
	}
	if record.Version != expectVersion {
		return nil, ErrLearningStaleWrite
	}
	if record.IsExpired(now) {
		return nil, ErrLearningQuizClosed
	}

	// Every question is checked before anything is written, so a set that is unlawful
	// in its third member cannot leave the first two stored.
	seen := make(map[int]struct{}, len(questions))
	stored := make([]domain.LearningQuestion, 0, len(questions))
	for index := range questions {
		question := questions[index]
		if question.UserID != userID || question.LearningSessionID != learningSessionID {
			return nil, ErrInvalidArgument
		}
		if question.ID == "" {
			question.ID = r.nextID("lq")
		}
		if question.CreatedAt.IsZero() {
			question.CreatedAt = now
		}
		if _, duplicate := seen[question.Ordinal]; duplicate {
			return nil, ErrInvalidArgument
		}
		seen[question.Ordinal] = struct{}{}
		if err := question.ValidateForCreate(); err != nil {
			return nil, ErrInvalidArgument
		}
		stored = append(stored, cloneLearningQuestion(question))
	}
	for ordinal := 1; ordinal <= len(stored); ordinal++ {
		if _, exists := seen[ordinal]; !exists {
			// Ordinals that do not run from 1 would make "question two of three" mean
			// nothing, and the current-question pointer could never reach every item.
			return nil, ErrInvalidArgument
		}
	}
	sort.SliceStable(stored, func(left, right int) bool { return stored[left].Ordinal < stored[right].Ordinal })

	if err := r.applyLearningTransitionLocked(record, domain.LearningSessionWaitingAnswer, LearningLease{}, now); err != nil {
		return nil, err
	}
	r.learningQuestions[learningSessionID] = stored
	return record, nil
}

// ListLearningQuestions returns a quiz's questions in the order they are asked.
func (r *InMemoryRepository) ListLearningQuestions(ctx context.Context, userID, learningSessionID string) ([]domain.LearningQuestion, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(learningSessionID) == "" {
		return nil, ErrInvalidArgument
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	r.initLocked()

	record, exists := r.learningSessions[learningSessionID]
	if !exists || record.UserID != userID {
		return nil, ErrNotFound
	}
	return cloneLearningQuestions(r.learningQuestions[learningSessionID]), nil
}

// ListRecentTopicQuestions returns exposure history for one topic across every quiz
// the caller ever started, newest first.
func (r *InMemoryRepository) ListRecentTopicQuestions(ctx context.Context, userID, topicKey string, limit int) ([]domain.LearningQuestion, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(topicKey) == "" || limit <= 0 {
		return nil, ErrInvalidArgument
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	r.initLocked()

	var found []domain.LearningQuestion
	// Walking the quizzes oldest-first and appending keeps the result ordered by quiz
	// recency without depending on how ids sort as text.
	for _, id := range r.learningSessionOrder {
		record := r.learningSessions[id]
		if record.UserID != userID || record.TopicKey != topicKey {
			continue
		}
		for _, question := range r.learningQuestions[id] {
			found = append(found, question)
		}
	}
	sort.SliceStable(found, func(left, right int) bool {
		if !found[left].CreatedAt.Equal(found[right].CreatedAt) {
			return found[left].CreatedAt.After(found[right].CreatedAt)
		}
		return found[left].Ordinal > found[right].Ordinal
	})
	if len(found) > limit {
		found = found[:limit]
	}
	return cloneLearningQuestions(found), nil
}

// BeginLearningAttempt stores one answer and claims the quiz for grading.
func (r *InMemoryRepository) BeginLearningAttempt(ctx context.Context, attempt *domain.LearningAttempt) (LearningAttemptClaim, error) {
	if err := checkContext(ctx); err != nil {
		return LearningAttemptClaim{}, err
	}
	if attempt == nil {
		return LearningAttemptClaim{}, ErrInvalidArgument
	}
	claimed := cloneLearningAttempt(*attempt)
	if err := claimed.ValidateForClaim(); err != nil {
		return LearningAttemptClaim{}, ErrInvalidArgument
	}

	now := time.Now().UTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.initLocked()

	record, err := r.loadLearningSessionForWriteLocked(claimed.UserID, claimed.LearningSessionID)
	if err != nil {
		return LearningAttemptClaim{}, err
	}
	recovered, err := r.recoverLearningLocked(record, now)
	if err != nil {
		return LearningAttemptClaim{}, err
	}
	record = recovered

	// The submission is settled before anything about the quiz's current position. A
	// client retrying an answer it already sent must get the stored outcome even after
	// the quiz has moved on; otherwise a response lost in transit turns into a
	// complaint about ordering, and the caller retries under a new id and gets graded
	// twice for one answer.
	if existingID, taken := r.learningAttemptBySubmit[learningSubmissionKey(claimed.UserID, claimed.SubmissionID)]; taken {
		existing := r.learningAttempts[existingID]
		switch {
		case existing.LearningSessionID != claimed.LearningSessionID ||
			existing.QuestionOrdinal != claimed.QuestionOrdinal ||
			existing.Answer != claimed.Answer:
			return LearningAttemptClaim{}, ErrLearningSubmissionConflict
		case existing.Status == domain.LearningAttemptCompleted || existing.Status == domain.LearningAttemptSkipped:
			return LearningAttemptClaim{Attempt: cloneLearningAttempt(existing), NeedsGrading: false,
				Session: *record}, nil
		case existing.Status == domain.LearningAttemptGrading:
			return LearningAttemptClaim{}, ErrLearningAnswerBusy
		default:
			// A failed attempt keeps its submission id forever: that row is the record
			// that this answer was tried and could not be scored. Retrying means a new
			// id, which is also what makes "resubmit" distinguishable from "replay".
			return LearningAttemptClaim{}, ErrLearningSubmissionConflict
		}
	}

	switch record.Status {
	case domain.LearningSessionGrading:
		// recoverLearningLocked already reclaimed any attempt whose lease ran out, and
		// the lookup above settled anything already in flight, so arriving back here
		// means a lease is live: somebody is scoring an answer on this quiz right now.
		return LearningAttemptClaim{}, ErrLearningAnswerBusy
	case domain.LearningSessionWaitingAnswer:
	default:
		return LearningAttemptClaim{}, ErrLearningQuizClosed
	}
	if claimed.QuestionOrdinal != record.CurrentQuestion {
		return LearningAttemptClaim{}, ErrLearningQuestionOrder
	}
	if claimed.QuestionOrdinal < 1 || claimed.QuestionOrdinal > record.TotalQuestions {
		return LearningAttemptClaim{}, ErrLearningQuestionOrder
	}
	question, exists := r.findLearningQuestionLocked(record.ID, claimed.QuestionID, claimed.QuestionOrdinal)
	if !exists {
		// A question that is not part of this quiz is answered the way a quiz the caller
		// cannot see is: not found, with nothing said about why.
		return LearningAttemptClaim{}, ErrNotFound
	}
	// The rubric and evidence are taken from the stored question here rather than from
	// anything the caller supplies, so a grade is reproducible from the attempt alone
	// after the document behind the question is edited.
	claimed.Rubric = cloneLearningStrings(question.Rubric)
	claimed.Evidence = cloneLearningEvidence(question.Evidence)

	claimed.ID = r.nextID("lat")
	claimed.CreatedAt = now
	if claimed.GradingAt == nil {
		stamped := now
		claimed.GradingAt = &stamped
	}
	r.learningAttempts[claimed.ID] = cloneLearningAttempt(claimed)
	r.learningAttemptOrder = append(r.learningAttemptOrder, claimed.ID)
	r.learningAttemptBySubmit[learningSubmissionKey(claimed.UserID, claimed.SubmissionID)] = claimed.ID

	if err := r.applyLearningTransitionLocked(record, domain.LearningSessionGrading, LearningLease{}, now); err != nil {
		return LearningAttemptClaim{}, err
	}
	return LearningAttemptClaim{Attempt: cloneLearningAttempt(claimed), NeedsGrading: true, Session: *record}, nil
}

// CompleteLearningAttempt commits a grade, advances the quiz, and re-projects mastery.
func (r *InMemoryRepository) CompleteLearningAttempt(ctx context.Context, userID, submissionID, leaseToken string,
	grade domain.LearningGrade) (LearningGradeCommit, error) {
	if err := checkContext(ctx); err != nil {
		return LearningGradeCommit{}, err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(submissionID) == "" {
		return LearningGradeCommit{}, ErrInvalidArgument
	}
	// An empty lease token is accepted here on purpose: a committed attempt has no
	// token left to give, and a caller retrying after a response it lost cannot know
	// one. The fence is not weakened, because an attempt that is still grading always
	// carries a non-empty token, so an empty one can never match the row a live grader
	// is working on.
	if err := grade.Validate(); err != nil {
		return LearningGradeCommit{}, ErrInvalidArgument
	}

	now := time.Now().UTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.initLocked()

	id, taken := r.learningAttemptBySubmit[learningSubmissionKey(userID, submissionID)]
	if !taken {
		return LearningGradeCommit{}, ErrNotFound
	}
	attempt := r.learningAttempts[id]
	if attempt.Status == domain.LearningAttemptCompleted {
		// The grade is already in. Returning it unchanged makes a lost response on the
		// grader's side harmless, but nothing is written a second time and mastery is
		// not counted twice.
		record, exists := r.learningSessions[attempt.LearningSessionID]
		if !exists {
			return LearningGradeCommit{}, ErrNotFound
		}
		progress, hasProgress := r.learningProgress[learningProgressKey(userID, attempt.TopicKey)]
		if !hasProgress {
			return LearningGradeCommit{}, ErrNotFound
		}
		return LearningGradeCommit{Attempt: cloneLearningAttempt(attempt), Session: record,
			Progress: cloneLearningProgress(progress), Replayed: true}, nil
	}
	if attempt.Status != domain.LearningAttemptGrading {
		return LearningGradeCommit{}, ErrLearningStaleWrite
	}
	if attempt.LeaseToken != leaseToken {
		// Fenced out: this quiz was taken over while the caller was scoring it, so the
		// winner's number stands and this one is dropped rather than interleaved.
		return LearningGradeCommit{}, ErrLearningStaleWrite
	}
	record, err := r.loadLearningSessionForWriteLocked(userID, attempt.LearningSessionID)
	if err != nil {
		return LearningGradeCommit{}, err
	}
	if record.Status != domain.LearningSessionGrading || record.CurrentQuestion != attempt.QuestionOrdinal {
		return LearningGradeCommit{}, ErrLearningStaleWrite
	}

	committed := cloneLearningAttempt(attempt)
	committed.Grade = grade
	committed.EffectiveTotal = grade.Total()
	committed.Status = domain.LearningAttemptCompleted
	committed.LeaseToken = ""
	committed.LeaseExpiresAt = nil
	stamped := now
	committed.CompletedAt = &stamped
	committed.GradingAt = nil
	r.learningAttempts[id] = cloneLearningAttempt(committed)

	// Mastery is recomputed here rather than by the caller that just wrote the grade,
	// because a score that reached storage without its projection being rebuilt would
	// leave the next explanation reading a picture of a learner who has already
	// answered something else.
	progress := r.projectTopicLocked(userID, committed.TopicKey, record.Topic, now)

	if err := r.advanceLearningLocked(record, now); err != nil {
		return LearningGradeCommit{}, err
	}
	return LearningGradeCommit{Attempt: cloneLearningAttempt(committed), Session: *record,
		Progress: cloneLearningProgress(progress)}, nil
}

// FailLearningAttempt records that an answer could not be scored and hands the
// question back.
func (r *InMemoryRepository) FailLearningAttempt(ctx context.Context, userID, submissionID, leaseToken string,
	code domain.LearningFailureCode) (domain.LearningAttempt, error) {
	if err := checkContext(ctx); err != nil {
		return domain.LearningAttempt{}, err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(submissionID) == "" || strings.TrimSpace(leaseToken) == "" {
		return domain.LearningAttempt{}, ErrInvalidArgument
	}
	if !code.IsValid() {
		return domain.LearningAttempt{}, ErrInvalidArgument
	}

	now := time.Now().UTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.initLocked()

	id, taken := r.learningAttemptBySubmit[learningSubmissionKey(userID, submissionID)]
	if !taken {
		return domain.LearningAttempt{}, ErrNotFound
	}
	attempt := r.learningAttempts[id]
	if attempt.Status != domain.LearningAttemptGrading {
		return domain.LearningAttempt{}, ErrLearningStaleWrite
	}
	if attempt.LeaseToken != leaseToken {
		return domain.LearningAttempt{}, ErrLearningStaleWrite
	}
	record, err := r.loadLearningSessionForWriteLocked(userID, attempt.LearningSessionID)
	if err != nil {
		return domain.LearningAttempt{}, err
	}

	failed := cloneLearningAttempt(attempt)
	failed.Status = domain.LearningAttemptFailed
	failed.FailureCode = code
	failed.LeaseToken = ""
	failed.LeaseExpiresAt = nil
	stamped := now
	failed.GradingAt = &stamped
	r.learningAttempts[id] = cloneLearningAttempt(failed)

	// The question is not advanced. A learner who was answered-then-lost-to-a-server-
	// error and is now told "that question is over" has paid for a failure that was
	// nobody's fault but ours.
	if record.Status == domain.LearningSessionGrading {
		if err := r.applyLearningTransitionLocked(record, domain.LearningSessionWaitingAnswer, LearningLease{}, now); err != nil {
			return domain.LearningAttempt{}, err
		}
	}
	return cloneLearningAttempt(failed), nil
}

// SkipLearningQuestion records a declined answer and moves the quiz on.
func (r *InMemoryRepository) SkipLearningQuestion(ctx context.Context, userID, learningSessionID, submissionID string) (LearningAttemptClaim, error) {
	if err := checkContext(ctx); err != nil {
		return LearningAttemptClaim{}, err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(learningSessionID) == "" || strings.TrimSpace(submissionID) == "" {
		return LearningAttemptClaim{}, ErrInvalidArgument
	}

	now := time.Now().UTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.initLocked()

	record, err := r.loadLearningSessionForWriteLocked(userID, learningSessionID)
	if err != nil {
		return LearningAttemptClaim{}, err
	}
	recovered, err := r.recoverLearningLocked(record, now)
	if err != nil {
		return LearningAttemptClaim{}, err
	}
	record = recovered

	if existingID, taken := r.learningAttemptBySubmit[learningSubmissionKey(userID, submissionID)]; taken {
		existing := r.learningAttempts[existingID]
		// A repeat of a skip is the same operation, so it answers with the stored
		// attempt. The quiz's current position is deliberately not part of the match:
		// the position has moved on precisely because the skip worked.
		if existing.LearningSessionID != learningSessionID || existing.Status != domain.LearningAttemptSkipped {
			return LearningAttemptClaim{}, ErrLearningSubmissionConflict
		}
		return LearningAttemptClaim{Attempt: cloneLearningAttempt(existing), NeedsGrading: false, Session: *record}, nil
	}

	switch record.Status {
	case domain.LearningSessionWaitingAnswer:
	case domain.LearningSessionGrading:
		return LearningAttemptClaim{}, ErrLearningAnswerBusy
	default:
		return LearningAttemptClaim{}, ErrLearningQuizClosed
	}
	if record.CurrentQuestion > record.TotalQuestions {
		return LearningAttemptClaim{}, ErrLearningQuizClosed
	}
	question, exists := r.findLearningQuestionLocked(record.ID, "", record.CurrentQuestion)
	if !exists {
		return LearningAttemptClaim{}, ErrNotFound
	}

	attempt := domain.LearningAttempt{
		ID: r.nextID("lat"), UserID: userID, SessionID: record.SessionID,
		LearningSessionID: record.ID, QuestionID: question.ID, QuestionOrdinal: question.Ordinal,
		SubmissionID: submissionID, TopicKey: record.TopicKey, Status: domain.LearningAttemptSkipped,
		Rubric: cloneLearningStrings(question.Rubric), Evidence: cloneLearningEvidence(question.Evidence),
		CreatedAt: now, CompletedAt: &now,
	}
	r.learningAttempts[attempt.ID] = cloneLearningAttempt(attempt)
	r.learningAttemptOrder = append(r.learningAttemptOrder, attempt.ID)
	r.learningAttemptBySubmit[learningSubmissionKey(userID, submissionID)] = attempt.ID

	if err := r.advanceLearningLocked(record, now); err != nil {
		return LearningAttemptClaim{}, err
	}
	return LearningAttemptClaim{Attempt: cloneLearningAttempt(attempt), NeedsGrading: false, Session: *record}, nil
}

// advanceLearningLocked moves a quiz to its next card, or closes it when the card it was
// on was the last one.
//
// The status is part of an advance, not a detail left to the reader. A quiz whose last
// answer has been graded and stored must not still say "grading": that is the server
// describing a judgement it has already made as one still in progress, and a client told
// so has nothing correct to show. A skip is the opposite case - the state is already
// "waiting for an answer" and stays that way while the card changes - so it rewrites the
// same status, which is the only self-transition allowed here.
func (r *InMemoryRepository) advanceLearningLocked(record *domain.LearningSession, now time.Time) error {
	if record.CurrentQuestion >= record.TotalQuestions {
		return r.applyLearningTransitionLocked(record, domain.LearningSessionCompleted, LearningLease{}, now)
	}
	target := domain.LearningSessionWaitingAnswer
	if record.Status != domain.LearningSessionGrading {
		target = record.Status
	}
	if target != record.Status && !record.Status.CanTransitionTo(target) {
		return ErrLearningTransition
	}
	record.CurrentQuestion++
	record.Status = target
	record.Version++
	record.UpdatedAt = now
	r.learningSessions[record.ID] = *record
	return nil
}

// RecoverLearningSession applies the deadline and abandoned-work rules.
func (r *InMemoryRepository) RecoverLearningSession(ctx context.Context, userID, learningSessionID string) (*domain.LearningSession, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(learningSessionID) == "" {
		return nil, ErrInvalidArgument
	}

	now := time.Now().UTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.initLocked()

	record, err := r.loadLearningSessionForWriteLocked(userID, learningSessionID)
	if err != nil {
		return nil, err
	}
	return r.recoverLearningLocked(record, now)
}

// recoverLearningLocked is the one exit rule for every non-terminal state, and it is
// the reason no quiz in this store can become unreachable:
//
//	past the deadline        -> expired, and its in-flight work released
//	asking, lease not live   -> back to created, so a new generation may be claimed
//	grading, lease not live  -> attempt recorded failed, quiz back to waiting_answer
//
// The caller holds the lock and is handed back the quiz as it now stands.
func (r *InMemoryRepository) recoverLearningLocked(record *domain.LearningSession, now time.Time) (*domain.LearningSession, error) {
	if record.Status.IsTerminal() {
		stored := *record
		return &stored, nil
	}

	if record.IsExpired(now) {
		if err := r.releaseGradingLocked(record, domain.LearningFailureLeaseExpired, now); err != nil {
			return nil, err
		}
		// Every non-terminal state reaches expired through the table, so the deadline
		// needs no side door: asking, created, waiting_answer and grading all have the
		// edge, which is what keeps an abandoned quiz from staying open.
		if err := r.applyLearningTransitionLocked(record, domain.LearningSessionExpired, LearningLease{}, now); err != nil {
			return nil, err
		}
		stored := *record
		return &stored, nil
	}

	if record.Status == domain.LearningSessionAsking && record.GenerationAbandoned(now) {
		if err := r.applyLearningTransitionLocked(record, domain.LearningSessionCreated, LearningLease{}, now); err != nil {
			return nil, err
		}
	}

	if record.Status == domain.LearningSessionGrading {
		live := false
		for _, id := range r.learningAttemptOrder {
			attempt := r.learningAttempts[id]
			if attempt.UserID != record.UserID || attempt.LearningSessionID != record.ID {
				continue
			}
			if attempt.Status != domain.LearningAttemptGrading {
				continue
			}
			if attempt.LeaseExpired(now) {
				failed := cloneLearningAttempt(attempt)
				failed.Status = domain.LearningAttemptFailed
				failed.FailureCode = domain.LearningFailureLeaseExpired
				failed.LeaseToken = ""
				failed.LeaseExpiresAt = nil
				stamped := now
				failed.GradingAt = &stamped
				r.learningAttempts[id] = cloneLearningAttempt(failed)
				continue
			}
			live = true
		}
		if !live {
			// Grading with nobody grading it: the only way back is the retry edge, and
			// leaving it here would strand the quiz with no path a user could take.
			if err := r.applyLearningTransitionLocked(record, domain.LearningSessionWaitingAnswer, LearningLease{}, now); err != nil {
				return nil, err
			}
		}
	}

	stored := *record
	return &stored, nil
}

// releaseGradingLocked fails whichever attempt is holding this quiz's grading work,
// used on the path where the quiz itself expires.
func (r *InMemoryRepository) releaseGradingLocked(record *domain.LearningSession, code domain.LearningFailureCode, now time.Time) error {
	for _, id := range r.learningAttemptOrder {
		attempt := r.learningAttempts[id]
		if attempt.UserID != record.UserID || attempt.LearningSessionID != record.ID {
			continue
		}
		if attempt.Status != domain.LearningAttemptGrading {
			continue
		}
		failed := cloneLearningAttempt(attempt)
		failed.Status = domain.LearningAttemptFailed
		failed.FailureCode = code
		failed.LeaseToken = ""
		failed.LeaseExpiresAt = nil
		stamped := now
		failed.GradingAt = &stamped
		r.learningAttempts[id] = cloneLearningAttempt(failed)
	}
	return nil
}

// projectTopicLocked recomputes one topic's progress from stored attempts and puts
// the result where both progress reads and the next recomputation will find it.
func (r *InMemoryRepository) projectTopicLocked(userID, topicKey, topic string, now time.Time) domain.TopicProgress {
	completed := r.completedAttemptsForTopicLocked(userID, topicKey, domain.LearningRecentAttemptWindow)
	progress := domain.ProjectTopicProgress(userID, topicKey, topic, completed, now)
	key := learningProgressKey(userID, topicKey)
	if existing, exists := r.learningProgress[key]; exists {
		progress.ID = existing.ID
		progress.CreatedAt = existing.CreatedAt
	} else {
		progress.ID = r.nextID("tpr")
		progress.CreatedAt = now
		r.learningProgressOrder = append(r.learningProgressOrder, key)
	}
	progress.UpdatedAt = now
	r.learningProgress[key] = cloneLearningProgress(progress)
	return cloneLearningProgress(progress)
}

// completedAttemptsForTopicLocked returns the window mastery is computed over:
// completed attempts for one owner and one topic, newest first.
//
// The order is the arithmetic - a projection fed oldest-first scores the same
// answers differently - so it has to be defined, not incidental. Attempts are walked
// in the order this store received them and then sorted by completion time only, so
// two attempts completed in the same instant keep their insertion order. That is the
// in-memory form of the SQL rule below, where an id carrying a fixed-width
// nanosecond timestamp does the same job.
func (r *InMemoryRepository) completedAttemptsForTopicLocked(userID, topicKey string, limit int) []domain.LearningAttempt {
	var found []domain.LearningAttempt
	for _, id := range r.learningAttemptOrder {
		attempt := r.learningAttempts[id]
		if attempt.UserID != userID || attempt.TopicKey != topicKey || attempt.Status != domain.LearningAttemptCompleted {
			continue
		}
		found = append(found, cloneLearningAttempt(attempt))
	}
	sort.SliceStable(found, func(left, right int) bool {
		return attemptCompletion(found[left]).After(attemptCompletion(found[right]))
	})
	if limit > 0 && len(found) > limit {
		found = found[:limit]
	}
	return found
}

// attemptCompletion is the completion time of an attempt, with the creation time as
// its fallback so a row that cannot say when it finished still has a defined order.
func attemptCompletion(attempt domain.LearningAttempt) time.Time {
	if attempt.CompletedAt != nil {
		return *attempt.CompletedAt
	}
	return attempt.CreatedAt
}

// GetTopicProgress reads one topic's projection.
func (r *InMemoryRepository) GetTopicProgress(ctx context.Context, userID, topicKey string) (*domain.TopicProgress, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(topicKey) == "" {
		return nil, ErrInvalidArgument
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	r.initLocked()

	progress, exists := r.learningProgress[learningProgressKey(userID, topicKey)]
	if !exists {
		return nil, ErrNotFound
	}
	stored := cloneLearningProgress(progress)
	return &stored, nil
}

// ListTopicProgress returns the caller's topics, most recently attempted first.
func (r *InMemoryRepository) ListTopicProgress(ctx context.Context, userID string, limit int) ([]domain.TopicProgress, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" || limit <= 0 {
		return nil, ErrInvalidArgument
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	r.initLocked()

	var found []domain.TopicProgress
	for _, key := range r.learningProgressOrder {
		progress := r.learningProgress[key]
		if progress.UserID != userID {
			continue
		}
		found = append(found, cloneLearningProgress(progress))
	}
	sort.SliceStable(found, func(left, right int) bool {
		ltime, rtime := progressActivity(found[left]), progressActivity(found[right])
		if !ltime.Equal(rtime) {
			return ltime.After(rtime)
		}
		return found[left].UpdatedAt.After(found[right].UpdatedAt)
	})
	if len(found) > limit {
		found = found[:limit]
	}
	return found, nil
}

// progressActivity is the recency a listing sorts on, with the update time as its
// fallback for a projection that has no attempt yet.
func progressActivity(progress domain.TopicProgress) time.Time {
	return progress.LastActive()
}

// findLearningQuestionLocked looks one question up, by id and ordinal together when
// an id is given, or by ordinal alone when the caller only knows the position.
func (r *InMemoryRepository) findLearningQuestionLocked(learningSessionID, questionID string,
	ordinal int) (domain.LearningQuestion, bool) {
	for _, question := range r.learningQuestions[learningSessionID] {
		if question.Ordinal != ordinal {
			continue
		}
		if questionID != "" && question.ID != questionID {
			continue
		}
		return question, true
	}
	return domain.LearningQuestion{}, false
}

// loadLearningSessionForWriteLocked reads a quiz the caller owns, refusing a foreign
// or missing one identically.
func (r *InMemoryRepository) loadLearningSessionForWriteLocked(userID, learningSessionID string) (*domain.LearningSession, error) {
	record, exists := r.learningSessions[learningSessionID]
	if !exists || record.UserID != userID {
		return nil, ErrNotFound
	}
	stored := record
	return &stored, nil
}
