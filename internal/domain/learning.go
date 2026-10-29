package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// This file is the learning half of the domain: the quiz that a learning-agent
// session runs over the user's own notes. It is migrated behaviour, restated for
// this architecture rather than copied - the questions, the attempts, the grading
// dimensions and the mastery projection all come from the original project's
// learning workflow, and the rules that were only conventions there are types and
// validators here.
//
// Three properties carry the whole design. A generated question keeps an
// immutable snapshot of the evidence behind it, so grading a week later scores the
// same text the question was written from even after the document is edited. A
// score is arithmetic the server performs, never a number a model supplied. And
// one chat session holds at most one unfinished quiz, because two live quizzes on
// one conversation make "answer this" mean nothing.

// LearningSessionStatus is where one quiz stands in its state machine.
type LearningSessionStatus string

const (
	// LearningSessionCreated is a quiz with no questions yet: the row exists so a
	// second Start cannot also begin, and generation is the next step.
	LearningSessionCreated LearningSessionStatus = "created"
	// LearningSessionAsking means questions are being generated, which is the one
	// step that costs a model call and can therefore take tens of seconds.
	LearningSessionAsking LearningSessionStatus = "asking"
	// LearningSessionWaitingAnswer is the suspended state: the questions are
	// stored, and the quiz can sit here across any number of requests until the
	// user answers, cancels, or the deadline passes.
	LearningSessionWaitingAnswer LearningSessionStatus = "waiting_answer"
	// LearningSessionGrading means one answer is accepted and being scored. It is
	// reachable from waiting_answer and can go back, which is what makes a failed
	// grading call recoverable instead of fatal.
	LearningSessionGrading LearningSessionStatus = "grading"
	// LearningSessionCompleted is the success terminal: every question has been
	// answered or skipped.
	LearningSessionCompleted LearningSessionStatus = "completed"
	// LearningSessionCancelled is the user's own exit.
	LearningSessionCancelled LearningSessionStatus = "cancelled"
	// LearningSessionExpired is the store's exit for a quiz nobody finished in
	// time. It is reached lazily, on read, because a background sweeper would be
	// one more moving part to trust.
	LearningSessionExpired LearningSessionStatus = "expired"
)

// IsValid reports whether s is one of the seven states.
func (s LearningSessionStatus) IsValid() bool {
	switch s {
	case LearningSessionCreated, LearningSessionAsking, LearningSessionWaitingAnswer,
		LearningSessionGrading, LearningSessionCompleted, LearningSessionCancelled,
		LearningSessionExpired:
		return true
	default:
		return false
	}
}

// IsTerminal reports whether no future transition is possible from s.
func (s LearningSessionStatus) IsTerminal() bool {
	return s == LearningSessionCompleted || s == LearningSessionCancelled || s == LearningSessionExpired
}

// CanTransitionTo reports whether a quiz may move from s to to.
//
// The table is closed, and the closed part is the point: the only backward edge
// is grading → waiting_answer, which exists so a failed model call returns the
// quiz to "answer me again" rather than destroying a turn the user already spent.
// Everything else moves forward or terminates. A repository must refuse a pair
// outside this table before it touches a row, so no caller can reach a state by
// writing the column directly.
func (s LearningSessionStatus) CanTransitionTo(to LearningSessionStatus) bool {
	if !s.IsValid() || !to.IsValid() || s.IsTerminal() || s == to {
		return false
	}
	switch s {
	case LearningSessionCreated:
		return to == LearningSessionAsking || to == LearningSessionCancelled || to == LearningSessionExpired
	case LearningSessionAsking:
		// Back to created is not a step backwards in the usual sense: it is what
		// happens when the process that was generating questions stopped, and the work
		// never happened. Without this edge an abandoned generation has nowhere to go
		// except expired, which would make a crash cost the learner a whole quiz.
		return to == LearningSessionCreated || to == LearningSessionWaitingAnswer ||
			to == LearningSessionCancelled || to == LearningSessionExpired
	case LearningSessionWaitingAnswer:
		// completed is reachable from here without grading, and only for one reason:
		// the learner skipped the last question, so there is nothing to score and the
		// quiz is genuinely over. Leaving it open until the deadline would describe a
		// finished conversation as an unfinished one, and letting the store write
		// "completed" from any state it liked would make this table decorative.
		return to == LearningSessionGrading || to == LearningSessionCompleted ||
			to == LearningSessionCancelled || to == LearningSessionExpired
	case LearningSessionGrading:
		return to == LearningSessionWaitingAnswer || to == LearningSessionCompleted ||
			to == LearningSessionCancelled || to == LearningSessionExpired
	default:
		return false
	}
}

// LearningQuestionType is the closed set of things a quiz can ask. Every one of
// them is written out; there is no multiple choice, because a choice question can
// be answered by elimination and the whole point of this workflow is to find out
// what the user cannot explain.
type LearningQuestionType string

const (
	// LearningQuestionShortAnswer asks for an explanation in the user's words.
	LearningQuestionShortAnswer LearningQuestionType = "short_answer"
	// LearningQuestionConceptComparison asks what distinguishes two things.
	LearningQuestionConceptComparison LearningQuestionType = "concept_comparison"
	// LearningQuestionCodeAnalysis asks what code does. It is only lawful when
	// the evidence actually contains code - see LearningQuestion.HasCodeEvidence.
	LearningQuestionCodeAnalysis LearningQuestionType = "code_analysis"
)

// IsValid reports whether t is one of the three question types.
func (t LearningQuestionType) IsValid() bool {
	switch t {
	case LearningQuestionShortAnswer, LearningQuestionConceptComparison, LearningQuestionCodeAnalysis:
		return true
	default:
		return false
	}
}

// LearningAttemptStatus is the state of one submitted answer.
type LearningAttemptStatus string

const (
	// LearningAttemptGrading means the answer is stored and a lease is held while
	// it is scored.
	LearningAttemptGrading LearningAttemptStatus = "grading"
	// LearningAttemptCompleted means a grade was committed.
	LearningAttemptCompleted LearningAttemptStatus = "completed"
	// LearningAttemptFailed means scoring gave up; the question is not advanced.
	LearningAttemptFailed LearningAttemptStatus = "failed"
	// LearningAttemptSkipped means the user declined to answer this question.
	LearningAttemptSkipped LearningAttemptStatus = "skipped"
)

// IsValid reports whether s is one of the four attempt states.
func (s LearningAttemptStatus) IsValid() bool {
	switch s {
	case LearningAttemptGrading, LearningAttemptCompleted, LearningAttemptFailed, LearningAttemptSkipped:
		return true
	default:
		return false
	}
}

// LearningFailureCode classifies why an attempt stopped grading.
//
// The original stored a free-text reason copied out of whatever error came back,
// which meant provider payloads and driver messages landed in a 160-column and
// nothing could be counted on them. A closed set is the same amount of
// information a reader needs, and it keeps a provider's words out of storage.
type LearningFailureCode string

const (
	// LearningFailureGradingFailed means the scoring call returned an error.
	LearningFailureGradingFailed LearningFailureCode = "grading_failed"
	// LearningFailureInvalidGrade means the model's answer could not be decoded
	// into a lawful grade.
	LearningFailureInvalidGrade LearningFailureCode = "invalid_grade_output"
	// LearningFailureLeaseExpired means a grading lease ran out and a later
	// claim took the attempt over.
	LearningFailureLeaseExpired LearningFailureCode = "grading_lease_expired"
	// LearningFailureModelUnavailable means the configured model could not be
	// reached at all.
	LearningFailureModelUnavailable LearningFailureCode = "model_unavailable"
)

// IsValid reports whether code is one of the four classification codes.
func (code LearningFailureCode) IsValid() bool {
	switch code {
	case LearningFailureGradingFailed, LearningFailureInvalidGrade, LearningFailureLeaseExpired,
		LearningFailureModelUnavailable:
		return true
	default:
		return false
	}
}

// Learning question counts and size limits.
//
// The count is fixed at three rather than chosen per request: the HTTP contract
// in the original project accepted no count either, so a range of "1 to 5" was
// only ever reachable from inside the process. Saying so with one constant keeps
// the reachable set and the declared set equal.
const (
	// LearningQuestionCount is how many questions one quiz holds.
	LearningQuestionCount = 3
	// LearningQuestionOrdinalFirst is the ordinal of the question a quiz starts
	// on. Ordinals are one-based because they are shown to users.
	LearningQuestionOrdinalFirst = 1

	MaxLearningTopicRunes        = 160
	MaxLearningTopicKeyRunes     = 80
	MaxLearningQuestionRunes     = 1000
	MaxLearningGoalRunes         = 240
	MaxLearningReferenceRunes    = 4000
	MaxLearningRubricItems       = 6
	MaxLearningRubricItemRunes   = 500
	MaxLearningEvidenceItems     = 5
	MaxLearningEvidenceRunes     = 1600
	MaxLearningAnswerRunes       = 12000
	MaxLearningFeedbackRunes     = 2000
	MaxLearningMissingPointRunes = 300
	MaxLearningMissingPoints     = 8
	MaxLearningWeakPoints        = 8
	// LearningRecentAttemptWindow is how many completed attempts the mastery
	// projection reads. Ten is the whole memory of a topic: a learner who has
	// moved on is not scored on what they could not do last month, and a learner
	// who has never practised is not scored on one lucky answer.
	LearningRecentAttemptWindow = 10
	// LearningDefaultLifetime bounds one quiz from creation to expiry.
	LearningDefaultLifetime = 24 * time.Hour
)

// Grade ceilings. The three dimensions sum to 100 and a model is asked for the
// dimensions only - see the grading prompt - so the total is always this
// package's arithmetic and never a number the model chose.
const (
	MaxLearningCorrectness  = 60
	MaxLearningCompleteness = 25
	MaxLearningClarity      = 15
)

// Mastery thresholds. Level boundaries are here rather than in a repository so
// that the projection, the prompt rendering and the HTTP view cannot drift apart
// about what "mastered" means.
const (
	// LearningMasteryMasteredAt is the score at which a topic counts as mastered.
	LearningMasteryMasteredAt = 85
	// LearningMasteryConfidenceAt is how much evidence a level claim needs before
	// it is stated: fewer than three valid attempts is not a description of a
	// person, it is one data point.
	LearningMasteryConfidenceAt = 0.5
	// LearningReliableAttemptCount is that "fewer than three" boundary.
	LearningReliableAttemptCount = 3
	// LearningConfidenceAttemptDivisor spreads confidence over six attempts, after
	// which a second answer cannot add certainty, only freshness.
	LearningConfidenceAttemptDivisor = 6
	// LearningTopicMatchMinCoverage is the share of one part of a topic name a
	// question must reproduce before that topic may describe the learner. Half is a
	// judgement, not a measurement: putting the wrong topic's judgement in front of a
	// model tells it the learner is weak at something they never asked about, which is
	// worse than saying nothing.
	LearningTopicMatchMinCoverage = 0.5
	// LearningMasteryCandidateTopics bounds the projection read behind a mastery note.
	// It caps the database scan, not the answer, which is at most one topic.
	LearningMasteryCandidateTopics = 50
	// LearningMasteryStaleAfter is how long a topic's projection may still shape an
	// explanation. Two answers from half a year ago are a record of a lesson, not a
	// description of whoever is asking today. There is no environment override: a
	// window that a deployment could silently widen is not a bound.
	LearningMasteryStaleAfter = 60 * 24 * time.Hour
)

// LearningEvidence is one immutable chunk snapshot a question was written from.
//
// It is stored on the question and copied onto the attempt at claim time, because
// grading must be reproducible against the text the question generator saw. A
// document edited afterwards would otherwise make a stored question ungradeable,
// and "the reference answer no longer matches the notes" is not a fact a learner
// should be scored against.
type LearningEvidence struct {
	// Ordinal is this evidence's position in the question's own list, so a model
	// can refer to it as [1], [2] without ever seeing an internal id.
	Ordinal      int    `json:"ordinal"`
	DocumentID   string `json:"document_id"`
	CollectionID string `json:"collection_id"`
	Title        string `json:"title"`
	Section      string `json:"section,omitempty"`
	DocumentVer  int    `json:"document_version"`
	ChunkID      string `json:"chunk_id"`
	ChunkOrder   int    `json:"chunk_order"`
	StartLine    int    `json:"start_line"`
	EndLine      int    `json:"end_line"`
	Snapshot     string `json:"snapshot"`
}

// Validate reports whether this evidence is a usable, bounded snapshot.
func (e LearningEvidence) Validate() error {
	switch {
	case e.Ordinal <= 0:
		return errors.New("domain: learning evidence ordinal must be positive")
	case strings.TrimSpace(e.DocumentID) == "":
		return errors.New("domain: learning evidence document id is required")
	case strings.TrimSpace(e.ChunkID) == "":
		return errors.New("domain: learning evidence chunk id is required")
	case e.DocumentVer <= 0:
		return errors.New("domain: learning evidence document version must be positive")
	case e.StartLine <= 0 || e.EndLine < e.StartLine:
		return errors.New("domain: learning evidence line range is invalid")
	case strings.TrimSpace(e.Snapshot) == "":
		return errors.New("domain: learning evidence snapshot is required")
	case utf8.RuneCountInString(e.Snapshot) > MaxLearningEvidenceRunes:
		return errors.New("domain: learning evidence snapshot is too large")
	default:
		return nil
	}
}

// Identity is what makes this evidence countable across quizzes: the chunk it came
// from, versioned, so a re-indexed document is genuinely new evidence. It is the
// key the coverage selector counts uses against and the novelty check intersects.
func (e LearningEvidence) Identity() string {
	return fmt.Sprintf("%s:v%d:%s", e.DocumentID, e.DocumentVer, e.ChunkID)
}

// HasCodeEvidence reports whether any snapshot carries a fenced block, which is
// the only lawful basis for asking a code-analysis question. The original had a
// per-chunk content type as well; this architecture records a chunk's section and
// text but not a content-type column, so the fence marker is the check.
func HasCodeEvidence(evidence []LearningEvidence) bool {
	for _, item := range evidence {
		if strings.Contains(item.Snapshot, "```") {
			return true
		}
	}
	return false
}

// LearningEvidenceFromChunk derives the snapshot stored with a question.
func LearningEvidenceFromChunk(chunk DocumentChunk, title string, ordinal int) LearningEvidence {
	return LearningEvidence{
		Ordinal:      ordinal,
		DocumentID:   chunk.DocumentID,
		CollectionID: chunk.CollectionID,
		Title:        title,
		Section:      chunk.Section,
		DocumentVer:  chunk.Version,
		ChunkID:      chunk.ID,
		ChunkOrder:   chunk.Order,
		StartLine:    chunk.StartLine,
		EndLine:      chunk.EndLine,
		Snapshot:     chunk.Text,
	}
}

// LearningSession is one quiz over one topic of one chat session.
type LearningSession struct {
	ID     string
	UserID string
	// SessionID is the chat conversation this quiz was started from. Ownership is
	// inherited from it: a quiz is readable only by the user who owns that
	// conversation.
	SessionID string
	// Topic is what the user typed, kept for display.
	Topic string
	// TopicKey is the normalized form everything else is filed under, so twenty
	// spellings of one subject do not become twenty unrelated progress rows.
	TopicKey        string
	Status          LearningSessionStatus
	CurrentQuestion int
	TotalQuestions  int
	// Version is the optimistic-concurrency counter. Every state change in the
	// repository is conditioned on it, including the ones the original performed
	// with a bare row lock - one discipline for one state machine.
	Version uint64
	// GenerationToken and GenerationExpiresAt fence the one step that asks a model
	// for questions. Without a deadline on that step, a process killed mid-
	// generation would leave the quiz in asking and its conversation locked until
	// the quiz's own expiry - hours of "you already have a quiz open" over a call
	// that stopped existing. An abandoned generation is reclaimed back to created,
	// which is the state it never really left.
	GenerationToken     string
	GenerationExpiresAt *time.Time
	ExpiresAt           time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
	CompletedAt         *time.Time
}

// HoldsGenerationLease reports whether somebody is generating this quiz's questions
// right now.
func (s LearningSession) HoldsGenerationLease(now time.Time) bool {
	return s.Status == LearningSessionAsking && s.GenerationExpiresAt != nil && now.Before(*s.GenerationExpiresAt)
}

// GenerationAbandoned reports whether a quiz that claims to be generating questions
// has no live claim on that work. A missing deadline counts as abandoned: a row that
// says generation is happening but cannot say until when is a crashed generation,
// and treating it as live is how a conversation ends up locked by nothing.
func (s LearningSession) GenerationAbandoned(now time.Time) bool {
	return s.Status == LearningSessionAsking && !s.HoldsGenerationLease(now)
}

// ActiveKey is the uniqueness token that lets exactly one quiz stay unfinished per
// chat session. It is stored as-is on a live row and cleared to empty on any
// terminal state, so the database - not a check-then-insert - decides who wins a
// concurrent Start.
func (s LearningSession) ActiveKey() string {
	return s.UserID + ":" + s.SessionID
}

// IsExpired reports whether the quiz's own deadline has passed. Reading it here
// rather than in one chosen caller is deliberate: an expired quiz that still
// accepts answers is the bug this exists to prevent, and the check has to be as
// close to the state as possible.
func (s LearningSession) IsExpired(now time.Time) bool {
	return !s.Status.IsTerminal() && !s.ExpiresAt.IsZero() && !now.Before(s.ExpiresAt)
}

// ValidateForCreate reports whether this is a lawful first record of a quiz.
func (s *LearningSession) ValidateForCreate() error {
	switch {
	case s == nil:
		return errors.New("domain: learning session is nil")
	case strings.TrimSpace(s.UserID) == "":
		return errors.New("domain: learning session needs an owner")
	case strings.TrimSpace(s.SessionID) == "":
		return errors.New("domain: learning session needs a chat session")
	case strings.TrimSpace(s.Topic) == "" || strings.TrimSpace(s.TopicKey) == "":
		return errors.New("domain: learning session topic is required")
	case s.Status != LearningSessionCreated:
		return errors.New("domain: a new learning session starts in created")
	case s.Version != 1:
		return errors.New("domain: a new learning session starts at version 1")
	case s.CurrentQuestion != LearningQuestionOrdinalFirst:
		return errors.New("domain: a new learning session starts on question 1")
	case s.TotalQuestions != LearningQuestionCount:
		return fmt.Errorf("domain: a learning session holds exactly %d questions", LearningQuestionCount)
	case s.ExpiresAt.IsZero():
		return errors.New("domain: learning session expiry is required")
	case s.GenerationToken != "" || s.GenerationExpiresAt != nil:
		// A brand new quiz is not mid-generation. Allowing the field to be set at
		// creation would let a caller claim a quiz's generation work before anyone
		// has asked a model anything, and the reclaim path would then refuse to
		// recover it.
		return errors.New("domain: a new learning session holds no generation claim")
	default:
		return nil
	}
}

// LearningQuestion is one generated question, with the evidence it was written
// from and the answer it will be graded against.
type LearningQuestion struct {
	ID                string
	UserID            string
	LearningSessionID string
	// Ordinal is the question's position, 1-based, unique within the quiz.
	Ordinal         int
	Type            LearningQuestionType
	Question        string
	AssessmentGoal  string
	ReferenceAnswer string
	Rubric          []string
	Evidence        []LearningEvidence
	CreatedAt       time.Time
}

// ValidateForCreate reports whether this is a storable question: whose it is, where in
// the quiz it sits, and whether what it says holds together.
func (q *LearningQuestion) ValidateForCreate() error {
	if q == nil {
		return errors.New("domain: learning question is nil")
	}
	switch {
	case strings.TrimSpace(q.UserID) == "":
		return errors.New("domain: learning question needs an owner")
	case strings.TrimSpace(q.LearningSessionID) == "":
		return errors.New("domain: learning question needs a quiz")
	case q.Ordinal < LearningQuestionOrdinalFirst:
		return errors.New("domain: learning question ordinal must be positive")
	}
	return q.ValidateContent()
}

// ValidateContent checks what a question says, apart from whose it is.
//
// The split exists because the two are decided at different times by different parties:
// a model writing questions has no business naming an owner or a quiz, so content is
// checked the moment its answer is decoded, and identity is checked by the store once
// the service has filled those fields in. Checking them together would make every
// freshly decoded question look invalid for a reason the generator cannot act on.
func (q LearningQuestion) ValidateContent() error {
	switch {
	case !q.Type.IsValid():
		return errors.New("domain: learning question type is unknown")
	case strings.TrimSpace(q.Question) == "":
		return errors.New("domain: learning question text is required")
	case utf8.RuneCountInString(q.Question) > MaxLearningQuestionRunes:
		return errors.New("domain: learning question text is too large")
	case strings.TrimSpace(q.AssessmentGoal) == "":
		return errors.New("domain: learning question needs an assessment goal")
	case utf8.RuneCountInString(q.AssessmentGoal) > MaxLearningGoalRunes:
		return errors.New("domain: learning question goal is too large")
	case strings.TrimSpace(q.ReferenceAnswer) == "":
		return errors.New("domain: learning question needs a reference answer")
	case utf8.RuneCountInString(q.ReferenceAnswer) > MaxLearningReferenceRunes:
		return errors.New("domain: learning question reference answer is too large")
	case len(q.Rubric) == 0 || len(q.Rubric) > MaxLearningRubricItems:
		return fmt.Errorf("domain: learning question rubric must hold 1 to %d items", MaxLearningRubricItems)
	case len(q.Evidence) == 0 || len(q.Evidence) > MaxLearningEvidenceItems:
		return fmt.Errorf("domain: learning question evidence must hold 1 to %d items", MaxLearningEvidenceItems)
	case q.Type == LearningQuestionCodeAnalysis && !HasCodeEvidence(q.Evidence):
		return errors.New("domain: a code analysis question needs evidence that contains code")
	default:
		for _, item := range q.Rubric {
			if strings.TrimSpace(item) == "" || utf8.RuneCountInString(item) > MaxLearningRubricItemRunes {
				return errors.New("domain: learning question rubric item is invalid")
			}
		}
		for _, evidence := range q.Evidence {
			if err := evidence.Validate(); err != nil {
				return err
			}
		}
		return nil
	}
}

// LearningGrade is one scoring outcome. It is a value rather than a set of columns
// because the three dimensions and the total are one calculation: the model
// supplies the dimensions and the server supplies everything else.
type LearningGrade struct {
	Correctness   int
	Completeness  int
	Clarity       int
	Feedback      string
	MissingPoints []string
	Confidence    float64
}

// Total is the sum the mastery projection reads. It is a method, not a field, so
// there is no version of a grade in which the total disagrees with its parts.
func (g LearningGrade) Total() int {
	return g.Correctness + g.Completeness + g.Clarity
}

// Validate reports whether this grade is lawful. It runs on the way in (before a
// model's numbers are trusted) and again on the way out of the repository, because
// a grade that reached storage with an out-of-range dimension would corrupt every
// projection built from it.
func (g LearningGrade) Validate() error {
	switch {
	case g.Correctness < 0 || g.Correctness > MaxLearningCorrectness:
		return fmt.Errorf("domain: correctness must be 0 to %d", MaxLearningCorrectness)
	case g.Completeness < 0 || g.Completeness > MaxLearningCompleteness:
		return fmt.Errorf("domain: completeness must be 0 to %d", MaxLearningCompleteness)
	case g.Clarity < 0 || g.Clarity > MaxLearningClarity:
		return fmt.Errorf("domain: clarity must be 0 to %d", MaxLearningClarity)
	case g.Confidence < 0 || g.Confidence > 1:
		return errors.New("domain: grading confidence must be 0 to 1")
	case strings.TrimSpace(g.Feedback) == "":
		return errors.New("domain: grade feedback is required")
	case utf8.RuneCountInString(g.Feedback) > MaxLearningFeedbackRunes:
		return errors.New("domain: grade feedback is too large")
	case len(g.MissingPoints) > MaxLearningMissingPoints:
		return fmt.Errorf("domain: at most %d missing points may be recorded", MaxLearningMissingPoints)
	default:
		for _, point := range g.MissingPoints {
			if strings.TrimSpace(point) == "" || utf8.RuneCountInString(point) > MaxLearningMissingPointRunes {
				return errors.New("domain: missing point is invalid")
			}
		}
		return nil
	}
}

// LearningAttempt is one answer and what became of it. It is also the idempotency
// boundary: the submission id the client chose is unique per user, so a retried
// POST cannot be graded twice.
type LearningAttempt struct {
	ID                string
	UserID            string
	SessionID         string
	LearningSessionID string
	QuestionID        string
	QuestionOrdinal   int
	// SubmissionID is client-generated and kept across retries. It is scoped to
	// the user, not to a quiz, so one id can never mean two different answers.
	SubmissionID string
	TopicKey     string
	Status       LearningAttemptStatus
	Answer       string
	// Rubric and Evidence are copied from the question at claim time so a grade can
	// be reproduced from this row alone.
	Rubric   []string
	Evidence []LearningEvidence
	Grade    LearningGrade
	// EffectiveTotal is what mastery is computed from. It equals the computed total
	// for every attempt this store writes: the original allowed a human correction
	// to overwrite it, and that operation is not migrated here.
	EffectiveTotal int
	FailureCode    LearningFailureCode
	// LeaseToken fences the grading write: a grader whose lease was taken over
	// cannot commit.
	LeaseToken     string
	LeaseExpiresAt *time.Time
	GradingAt      *time.Time
	CompletedAt    *time.Time
	CreatedAt      time.Time
}

// HoldsLease reports whether this attempt is currently being graded by somebody.
func (a LearningAttempt) HoldsLease(now time.Time) bool {
	return a.Status == LearningAttemptGrading && a.LeaseExpiresAt != nil && now.Before(*a.LeaseExpiresAt)
}

// LeaseExpired reports whether a grading lease has run out. A missing deadline is
// treated as expired: a row that says it is being graded but cannot say by whom or
// until when is abandoned work, not live work, and the alternative is to leave the
// quiz stuck forever.
func (a LearningAttempt) LeaseExpired(now time.Time) bool {
	return a.Status == LearningAttemptGrading && (a.LeaseExpiresAt == nil || !now.Before(*a.LeaseExpiresAt))
}

// ValidateForClaim reports whether this is a lawful in-flight attempt.
func (a *LearningAttempt) ValidateForClaim() error {
	switch {
	case a == nil:
		return errors.New("domain: learning attempt is nil")
	case strings.TrimSpace(a.UserID) == "":
		return errors.New("domain: learning attempt needs an owner")
	case strings.TrimSpace(a.SessionID) == "":
		return errors.New("domain: learning attempt needs a chat session")
	case strings.TrimSpace(a.LearningSessionID) == "":
		return errors.New("domain: learning attempt needs a quiz")
	case strings.TrimSpace(a.QuestionID) == "":
		return errors.New("domain: learning attempt needs a question")
	case a.QuestionOrdinal < LearningQuestionOrdinalFirst:
		return errors.New("domain: learning attempt ordinal must be positive")
	case strings.TrimSpace(a.SubmissionID) == "":
		return errors.New("domain: learning attempt submission id is required")
	case strings.TrimSpace(a.TopicKey) == "":
		return errors.New("domain: learning attempt topic key is required")
	case a.Status != LearningAttemptGrading:
		return errors.New("domain: a claimed attempt is grading")
	case strings.TrimSpace(a.Answer) == "":
		return errors.New("domain: a claimed attempt carries an answer")
	case utf8.RuneCountInString(a.Answer) > MaxLearningAnswerRunes:
		return errors.New("domain: learning attempt answer is too large")
	case strings.TrimSpace(a.LeaseToken) == "":
		return errors.New("domain: a claimed attempt holds a lease")
	case a.LeaseExpiresAt == nil:
		return errors.New("domain: a claimed lease has a deadline")
	default:
		return nil
	}
}

// TopicProgressLevel is the coarse statement a topic earns. It is stored because
// it is shown, and it is derived because a level that could be written directly
// would be a claim nobody could trace.
type TopicProgressLevel string

const (
	// TopicProgressInsufficient means too few valid attempts to say anything.
	TopicProgressInsufficient TopicProgressLevel = "insufficient"
	// TopicProgressDeveloping means the learner has practised but not mastered it.
	TopicProgressDeveloping TopicProgressLevel = "developing"
	// TopicProgressMastered means the recent weighted score and the amount of
	// evidence both clear the bar.
	TopicProgressMastered TopicProgressLevel = "mastered"
)

// IsValid reports whether level is one of the three levels. The original defined
// three and enforced none, so a test could construct a fourth and nothing
// objected.
func (level TopicProgressLevel) IsValid() bool {
	switch level {
	case TopicProgressInsufficient, TopicProgressDeveloping, TopicProgressMastered:
		return true
	default:
		return false
	}
}

// TopicProgress is the projection of a learner's recent valid attempts at one
// topic. It is what the next explanation reads, and it is written by arithmetic
// over stored attempts and by nothing else - not a model, not a caller.
type TopicProgress struct {
	ID          string
	UserID      string
	TopicKey    string
	Topic       string
	Mastery     int
	Confidence  float64
	ValidCount  int
	Level       TopicProgressLevel
	WeakPoints  []string
	LastAttempt *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Validate reports whether this is a storable projection.
func (p TopicProgress) Validate() error {
	switch {
	case strings.TrimSpace(p.UserID) == "":
		return errors.New("domain: topic progress needs an owner")
	case strings.TrimSpace(p.TopicKey) == "":
		return errors.New("domain: topic progress needs a topic key")
	case p.Mastery < 0 || p.Mastery > 100:
		return errors.New("domain: mastery must be 0 to 100")
	case p.Confidence < 0 || p.Confidence > 1:
		return errors.New("domain: confidence must be 0 to 1")
	case p.ValidCount < 0:
		return errors.New("domain: valid attempt count cannot be negative")
	case !p.Level.IsValid():
		return errors.New("domain: topic progress level is unknown")
	case len(p.WeakPoints) > MaxLearningWeakPoints:
		return fmt.Errorf("domain: at most %d weak points are kept", MaxLearningWeakPoints)
	default:
		return nil
	}
}

// IsReliable reports whether there is enough evidence to treat this progress as a
// description of the learner rather than a first data point. It gates weak-point
// reuse during question generation and it gates the mastery note the agent is
// allowed to read: the same bar, stated once.
func (p TopicProgress) IsReliable() bool {
	return p.Level.IsValid() &&
		p.Level != TopicProgressInsufficient &&
		p.ValidCount >= LearningReliableAttemptCount
}

// LastActive is the one precedence for "when did this topic last mean something": the
// last graded answer when there is one, and the row's own update afterwards. Both the
// store's most-recent-first listing and the mastery note's staleness test read it
// through here, so the two cannot disagree about which topic is current while claiming to
// answer the same question.
func (p TopicProgress) LastActive() time.Time {
	if p.LastAttempt != nil {
		return *p.LastAttempt
	}
	return p.UpdatedAt
}

// NormalizeLearningTopicKey performs deterministic lexical normalization only:
// lower case, letters and digits kept, runs of anything else collapsed to a
// single hyphen, truncated at the storage limit.
//
// Semantic alias merging - "Redis 持久化" and "redis持久化" and "RDB/AOF" being one
// subject - deliberately does not happen here. That needs a model or a curated
// table, and both belong in an auditable layer above this one, where a wrong merge
// can be seen and undone.
func NormalizeLearningTopicKey(topic string) string {
	var builder strings.Builder
	separatorPending := false
	for _, r := range strings.ToLower(strings.TrimSpace(topic)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if separatorPending && builder.Len() > 0 {
				builder.WriteByte('-')
			}
			separatorPending = false
			builder.WriteRune(r)
			continue
		}
		separatorPending = builder.Len() > 0
	}
	value := strings.Trim(builder.String(), "-")
	if runes := []rune(value); len(runes) > MaxLearningTopicKeyRunes {
		value = strings.TrimRight(string(runes[:MaxLearningTopicKeyRunes]), "-")
	}
	return value
}
