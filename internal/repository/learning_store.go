package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// The learning store is the persistence half of the quiz workflow: one unfinished
// quiz per conversation, questions that keep their own evidence snapshot, one
// idempotent attempt per submission, and a mastery projection that only this layer
// computes.
//
// It lives on Repository rather than beside it for the same reason the memory
// operations do - a quiz needs the same user, the same chat session, the same
// transaction, and the same conformance suite as everything else in here, and a
// second interface with its own ownership rules is how two stores start to
// disagree about who owns what.
//
// Two decisions are worth stating before the method list, because they are the ones
// a later reader is tempted to undo. First: every state change is conditioned on
// (status, version), including the ones an earlier implementation performed with a
// bare row lock. One state machine, one discipline. Second: committing a grade
// re-projects topic progress inside the same transaction, because a score that
// reached storage without its projection being recomputed leaves the next
// explanation reading a stale picture of the learner, and no caller can be trusted
// to make the second call.
type LearningStore interface {
	// CreateLearningSession stores a new quiz. Exactly one quiz may be unfinished
	// per (user, chat session) at a time; that boundary is a uniqueness rule, not a
	// check-then-insert, so two concurrent Starts produce one quiz and one
	// ErrLearningSessionActive rather than two quizzes the user cannot tell apart.
	//
	// The chat session must exist and belong to userID. A conversation that is
	// somebody else's and one that never existed are the same answer (ErrNotFound),
	// so a quiz cannot be used to probe another account.
	CreateLearningSession(ctx context.Context, session *domain.LearningSession) (*domain.LearningSession, error)

	// GetLearningSession reads one of the caller's own quizzes. Foreign id and
	// missing id are the same answer.
	GetLearningSession(ctx context.Context, userID, learningSessionID string) (*domain.LearningSession, error)

	// GetActiveLearningSession returns the quiz that is currently open on one chat
	// session, or ErrNotFound when there is none. A quiz past its deadline is
	// reported as expired by RecoverLearningSession, not hidden here: this method
	// answers "is there a quiz", and an expired one still has questions a user may
	// need to look at.
	GetActiveLearningSession(ctx context.Context, userID, chatSessionID string) (*domain.LearningSession, error)

	// TransitionLearningSession moves one quiz from a state the caller believes it
	// is in to a state the transition table allows, at the version the caller read.
	//
	// A pair outside the table is ErrLearningTransition, always refused before the
	// row is touched. A row that has moved on is ErrLearningStaleWrite, which
	// satisfies ErrConflict. Reaching a terminal state clears the active marker, so
	// the next Start is allowed - a terminal row that kept its marker would lock a
	// conversation out of quizzes forever, which is the failure mode this whole
	// method exists to avoid.
	//
	// lease describes the work the caller is claiming, and is only meaningful on the
	// two edges that start or end a generation. Moving into asking records it; moving
	// out of asking clears it. Every other edge ignores it, so a caller cannot leave a
	// generation claim on a quiz that is being cancelled.
	TransitionLearningSession(ctx context.Context, userID, learningSessionID string,
		from, to domain.LearningSessionStatus, expectVersion uint64, lease LearningLease) (*domain.LearningSession, error)

	// StoreLearningQuestions is the one step that both inserts a quiz's questions
	// and moves it asking -> waiting_answer, in one transaction. Either every
	// question is stored and the quiz is awaiting answers, or nothing is: a half
	// a set of questions is a quiz whose remaining items cannot be distinguished
	// from a quiz that never started.
	//
	// The count must equal the quiz's TotalQuestions, ordinals must run from 1
	// without gaps or repeats, and every question must belong to this quiz's owner.
	StoreLearningQuestions(ctx context.Context, userID, learningSessionID string,
		questions []domain.LearningQuestion, expectVersion uint64) (*domain.LearningSession, error)

	// ListLearningQuestions returns a quiz's questions in ordinal order, which is
	// the order they are asked in.
	ListLearningQuestions(ctx context.Context, userID, learningSessionID string) ([]domain.LearningQuestion, error)

	// ListRecentTopicQuestions returns the caller's most recent questions for one
	// topic key across every quiz they ever started, newest first, capped by limit.
	//
	// This is exposure history, not evidence: a question the learner saw and skipped
	// still counts as seen, which is why status is not a filter here. A non-positive
	// limit is ErrInvalidArgument rather than "no limit", so a caller cannot ask for
	// somebody's whole history by accident.
	ListRecentTopicQuestions(ctx context.Context, userID, topicKey string, limit int) ([]domain.LearningQuestion, error)

	// BeginLearningAttempt stores one answer and moves its quiz into grading, in one
	// transaction, fenced by a lease.
	//
	// The submission id decides what happens on a repeat:
	//   - the same submission on the same answer is already completed or skipped, and
	//     the stored outcome is returned with Replayed set and nothing written;
	//   - the same submission attached to a different question, quiz or answer is
	//     ErrLearningSubmissionConflict, because one id may not mean two things;
	//   - the same submission still grading under a live lease is
	//     ErrLearningAnswerBusy, which is a "try again later", not a failure;
	//   - a different submission while another answer is being graded is also busy -
	//     one quiz grades one answer at a time.
	//
	// A grading row whose lease has expired is abandoned work: it is recorded failed
	// with LearningFailureLeaseExpired, the quiz returns to waiting_answer, and the
	// new claim proceeds. Without that step a process killed mid-grading would strand
	// the quiz in a state nothing can leave.
	//
	// The quiz must be waiting_answer (or recoverable as above) at the attempt's
	// QuestionOrdinal, which is what keeps questions in order: an answer for question
	// three cannot be accepted while question two is still open.
	BeginLearningAttempt(ctx context.Context, attempt *domain.LearningAttempt) (LearningAttemptClaim, error)

	// CompleteLearningAttempt commits a grade for a leased attempt, advances the quiz
	// to the next question or to completed, and re-projects the topic's progress -
	// all under the same lease token and in one transaction.
	//
	// The lease token is the fence: a grader that was overtaken while it was thinking
	// cannot write over the answer that won. The grade is re-validated here, so a
	// total that disagrees with its own dimensions never reaches storage, and the
	// stored total is always the arithmetic of the three dimensions.
	CompleteLearningAttempt(ctx context.Context, userID, submissionID, leaseToken string,
		grade domain.LearningGrade) (LearningGradeCommit, error)

	// FailLearningAttempt records that an answer could not be graded and returns the
	// quiz to waiting_answer with a closed failure code. The question is not advanced
	// and the answer may be resubmitted: a scoring failure the learner pays for is
	// the worst outcome available here.
	FailLearningAttempt(ctx context.Context, userID, submissionID, leaseToken string,
		code domain.LearningFailureCode) (domain.LearningAttempt, error)

	// SkipLearningQuestion records a skipped attempt for the current question and
	// advances the quiz, under the same ordinal and idempotency rules as an answer.
	// A skip is not a grade and contributes nothing to mastery.
	SkipLearningQuestion(ctx context.Context, userID, learningSessionID, submissionID string) (LearningAttemptClaim, error)

	// RecoverLearningSession applies the two rules that must not wait for a
	// background job, on a quiz the caller is about to read or write: a quiz past its
	// deadline becomes expired, and a quiz parked in grading or asking whose
	// in-flight work has no live lease is returned to a state a user can act in
	// (waiting_answer when there are questions, asking when there are none).
	//
	// It is idempotent and it reports the quiz as it stands afterwards, including
	// when nothing was recovered, so a caller can read it and continue.
	RecoverLearningSession(ctx context.Context, userID, learningSessionID string) (*domain.LearningSession, error)

	// GetTopicProgress reads one topic's projection. A topic with no completed
	// attempts yet is ErrNotFound: "you have not been tested on this" and "no such
	// topic" are the same answer to a caller, and neither is a server error.
	GetTopicProgress(ctx context.Context, userID, topicKey string) (*domain.TopicProgress, error)

	// ListTopicProgress returns the caller's topics, most recently attempted first,
	// capped by limit. A non-positive limit is ErrInvalidArgument.
	ListTopicProgress(ctx context.Context, userID string, limit int) ([]domain.TopicProgress, error)
}

// LearningLease is a claim on the generation step of a quiz: who is asking a model
// for its questions, and until when. The zero value means "no claim", which is what
// every edge other than created -> asking carries.
//
// It exists because generation is the only step in this workflow that can take tens
// of seconds and cannot be undone. A claim with a deadline turns a crashed
// generator into a recoverable row; without one, the crash is indistinguishable
// from work still in progress, and the conversation stays locked until the quiz
// itself expires.
type LearningLease struct {
	Token string
	Until time.Time
}

// IsEmpty reports whether this lease claims nothing.
func (lease LearningLease) IsEmpty() bool { return lease.Token == "" && lease.Until.IsZero() }

// Holds reports whether this lease is still live at now. A token with no deadline is
// not live: a claim that cannot say when it stops is the exact failure this type
// exists to prevent, so it is treated as expired rather than as permanent.
func (lease LearningLease) Holds(now time.Time) bool {
	return lease.Token != "" && !lease.Until.IsZero() && now.Before(lease.Until)
}

// LearningAttemptClaim is what BeginLearningAttempt and SkipLearningQuestion
// report back.
type LearningAttemptClaim struct {
	Attempt domain.LearningAttempt
	// Replayed is set when the submission was already finished, so the caller gets
	// the stored outcome instead of a second grading. NeedsGrading is its inverse:
	// it is the one flag that decides whether a model call is about to happen, and
	// putting it here means no caller has to infer it from a status.
	NeedsGrading bool
	// Session is the quiz as it stands after the claim, at its new version.
	Session domain.LearningSession
}

// LearningGradeCommit is what CompleteLearningAttempt reports back.
type LearningGradeCommit struct {
	Attempt domain.LearningAttempt
	Session domain.LearningSession
	// Progress is the topic projection recomputed from stored attempts. It is
	// returned rather than left for a later read because the caller answering the
	// request has to describe the same state the write produced.
	Progress domain.TopicProgress
	// Replayed is set when the grade was already committed under this submission and
	// nothing was written again.
	Replayed bool
}

// These are the learning-specific refusals. Each one is distinct because each one
// deserves a different answer to a user, and none of them is "the server is busy".
var (
	// ErrLearningSessionActive reports that this conversation already holds an
	// unfinished quiz. It satisfies ErrConflict.
	ErrLearningSessionActive = fmt.Errorf("%w: this conversation already has an unfinished quiz", ErrConflict)

	// ErrLearningTransition reports a state pair the transition table does not
	// allow. It is not a conflict: no retry can make it succeed, and a caller that
	// reported it as one would be hiding a programming error behind a status code.
	ErrLearningTransition = errors.New("repository: that learning state transition is not allowed")

	// ErrLearningStaleWrite reports that the version a caller read is no longer the
	// stored one - somebody else moved this quiz in the meantime. It satisfies
	// ErrConflict, so a caller that only wants to know the row moved can ask that,
	// and a caller that wants to re-read and retry can tell the difference from a
	// uniqueness refusal.
	ErrLearningStaleWrite = fmt.Errorf("%w: this quiz changed before the write was made", ErrConflict)

	// ErrLearningAnswerBusy reports that this quiz is grading another answer right
	// now. It satisfies ErrConflict because it is about state, but it is explicitly
	// retryable, which is why it is not folded into ErrLearningSessionActive.
	ErrLearningAnswerBusy = fmt.Errorf("%w: an answer is being graded right now", ErrConflict)

	// ErrLearningSubmissionConflict reports that a submission id is already attached
	// to different work, so the caller has reused an id and must generate a new one.
	// It satisfies ErrConflict, but it is not retryable with the same body, which is
	// the difference that matters to a client.
	ErrLearningSubmissionConflict = fmt.Errorf("%w: this submission id already belongs to another answer", ErrConflict)

	// ErrLearningQuestionOrder reports an answer for a question that is not the one
	// being asked. It exists because ordinal questions only mean something if they
	// are answered in order.
	ErrLearningQuestionOrder = errors.New("repository: that is not the question being asked")

	// ErrLearningQuizClosed reports that the quiz is finished - completed, cancelled
	// or expired - so no further work can be claimed on it. It is deliberately not a
	// conflict: retrying the same request changes nothing, and a client that was told
	// "conflict" would reasonably try again.
	ErrLearningQuizClosed = errors.New("repository: that quiz is finished")
)
