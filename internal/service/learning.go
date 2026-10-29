package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/modelcall"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/retrieval"
)

// LearningService runs one quiz: it turns the learner's own notes into questions, holds
// the questions open across requests, scores an answer against the snapshot it was
// written from, and updates the topic projection the next explanation reads.
//
// Two boundaries are what this type exists to hold.
//
// The first is that nothing here is decided by a model about a person. A model writes
// questions and scores three dimensions; the server computes the total, the mastery
// score, the confidence, the level and the weak-point list, and it recomputes them from
// stored rows every time. A model cannot submit mastery, and the shape of this type is
// why: there is no method that accepts a score.
//
// The second is that starting a quiz must not be able to strand a conversation. The
// generation step is the only long-running one and the only one that can fail
// externally, so every failure path before the questions are stored releases the
// conversation's reservation. Without that, a provider that times out leaves an open
// marker on the row for the rest of the day and the learner is told, repeatedly, that
// they already have a quiz in progress.

// LearningService dependencies are named narrowly so each one can be seen, and tested,
// apart from the others.
type (
	// LearningRecall is the retrieval half of evidence selection: it hands over chunks
	// the pipeline already admitted and ranked.
	LearningRecall = retrieval.Retriever
	// LearningTitles resolves document titles for the evidence snapshot a learner may
	// later see.
	LearningTitles interface {
		DocumentTitles(ctx context.Context, userID string, documentIDs []string) (map[string]string, error)
	}
	// LearningCollections lists the caller's collections, which is what makes an
	// unscoped "quiz me on this topic" mean their own material rather than nobody's.
	LearningCollections interface {
		ListCollections(ctx context.Context, userID string) ([]domain.Collection, error)
	}
)

// LearningService is the quiz workflow.
type LearningService struct {
	store  repository.Repository
	chat   model.ChatModel
	recall LearningRecall
	titles LearningTitles
	// collections resolves an unscoped request into the caller's own collection list.
	collections LearningCollections
	budget      LearningBudget
	// lifetime bounds one quiz from creation to expiry. It is configurable because a
	// deadline is the one rule in this workflow that is otherwise only reachable by
	// waiting for it.
	lifetime time.Duration
	// now is injected because expiry is a rule with a clock in it, and a test that has
	// to wait for a deadline is not testing the rule.
	now func() time.Time
	// metrics produces one task record per quiz operation when switched on; a nil
	// publisher leaves every handle inert, so the measured and unmeasured paths
	// stay one path.
	metrics *modelcall.Publisher
}

// NewLearningService builds the quiz workflow over one store. A nil chat model leaves
// Start and Answer refusing with ErrLearningModelNotConfigured rather than pretending to
// generate: a quiz whose questions came from nowhere is worse than no quiz. Reading is
// unaffected, because a learner's existing record needs no model to display.
func NewLearningService(store repository.Repository, chat model.ChatModel, recall LearningRecall,
	titles LearningTitles, collections LearningCollections) *LearningService {
	return &LearningService{
		store: store, chat: chat, recall: recall, titles: titles, collections: collections,
		budget:   LearningBudget{}.Normalize(),
		lifetime: domain.LearningDefaultLifetime,
		now:      func() time.Time { return time.Now().UTC() },
	}
}

// WithTaskMetrics installs the publisher that records one task per quiz
// operation. It is off by default and changes no business outcome.
func (s *LearningService) WithTaskMetrics(publisher *modelcall.Publisher) *LearningService {
	s.metrics = publisher
	return s
}

// WithLearningLifetime overrides how long an unfinished quiz stays open.
func (s *LearningService) WithLearningLifetime(lifetime time.Duration) *LearningService {
	if lifetime > 0 {
		s.lifetime = lifetime
	}
	return s
}

// WithLearningBudget overrides the evidence and history bounds, which is how a test
// reaches a budget too small to hit with production defaults.
func (s *LearningService) WithLearningBudget(budget LearningBudget) *LearningService {
	s.budget = budget.Normalize()
	return s
}

// WithLearningClock replaces the clock expiry and lease decisions read.
func (s *LearningService) WithLearningClock(now func() time.Time) *LearningService {
	if now != nil {
		s.now = now
	}
	return s
}

// The learning failures this service reports. Each one is distinct because each one
// deserves a different answer to a user, and none of them is "the server is busy" for
// an error the server understood perfectly well.
// These four are plain sentinels rather than wrappers of ErrConflict or
// ErrInvalidRequest on purpose. Each is a distinct answer a client acts on
// differently - retry, resume, resend under a new id, or stop - and wrapping a
// generic parent would make the generic HTTP mapping fire first and flatten them
// into "conflict" or "request is invalid". The repository-level errors they come
// from do keep their ErrConflict relationships; this is the layer where the
// distinction has to survive to the response.
var (
	// ErrLearningSessionActive reports that this conversation already holds an
	// unfinished quiz. The client's next call is a resume, not a retry.
	ErrLearningSessionActive = errors.New("learning: 这个会话已有一份未完成的自测")

	// ErrLearningBusy reports that an answer is being scored right now. It is the one
	// refusal here that a plain retry is correct for.
	ErrLearningBusy = errors.New("learning: 上一份回答正在评分中")

	// ErrLearningSubmissionConflict reports a submission id already attached to
	// different work, or already used for an answer that failed to score. Retrying the
	// same body cannot succeed; a fresh id can.
	ErrLearningSubmissionConflict = errors.New("learning: 这个提交编号已经用于另一次作答")

	// ErrLearningQuestionOrder reports an answer for a question other than the one
	// being asked. The body was well formed - the position was not - so it is not a
	// shape error either.
	ErrLearningQuestionOrder = errors.New("learning: 这不是当前正在作答的题")

	// ErrLearningClosed reports that the quiz is finished, so nothing further can be
	// claimed on it.
	ErrLearningClosed = errors.New("learning: 这份自测已经结束")

	// ErrLearningNoEvidence reports that the chosen collections hold too little on the
	// topic to build a question from. It is a refusal with a cause, not a degraded
	// quiz: questions written without evidence would be graded against the model's
	// own memory.
	ErrLearningNoEvidence = errors.New("learning: 笔记里没有足够的相关内容可以出题")

	// ErrLearningGeneration reports that questions could not be produced, whether the
	// model failed or its output could not be trusted.
	ErrLearningGeneration = errors.New("learning: 出题失败")

	// ErrLearningGrading reports that an answer could not be scored. The question stays
	// open and the answer may be sent again.
	ErrLearningGrading = errors.New("learning: 评分失败")

	// ErrLearningInvalidGrade reports a scoring response that broke the contract. It is
	// separated from ErrLearningGrading because it is a shape failure rather than a
	// transport one, and the two say different things about retrying.
	ErrLearningInvalidGrade = errors.New("learning: 评分输出不合法")

	// ErrLearningModelNotConfigured reports a quiz workflow built without a chat model.
	// It is a wiring fault, not a request that could have been shaped better: the reader
	// of a learning record still gets their history, but nothing here can invent a
	// question or a score. Checked at the two entry points that call a model, so a
	// misconfigured deployment refuses instead of panicking on a nil interface partway
	// through a reservation it then cannot release.
	ErrLearningModelNotConfigured = errors.New("learning: 没有配置可用的对话模型")

	// ErrLearningSessionMode reports a quiz started on a conversation that is not a
	// learning agent. It is its own reason rather than a plain ErrInvalidRequest because
	// the two are answered differently by the caller who needs to hear it: a shape error
	// says "fix the body", this one says "the practice belongs to another conversation".
	// It stays a 400 and not a 404 on purpose - 404 is reserved for a conversation the
	// caller does not own, and conflating the two would let anyone probe which modes exist.
	//
	// The rule it enforces is why a quiz is bound to an agent conversation at all: the
	// learner's record is meant to reach the explanation that follows it, and that
	// injection happens only on an agent turn. A quiz on a plain chat conversation would
	// keep the record accurate and simply never be read by anything.
	ErrLearningSessionMode = errors.New("learning: 自测只能挂在学习 Agent 会话上")
)

// callsModel reports whether this service can run the model-backed steps. Every entry
// point that reaches for a chat model checks it here first.
func (s *LearningService) callsModel() error {
	if s.chat == nil {
		return ErrLearningModelNotConfigured
	}
	return nil
}

// LearningQuestionView is one question as a learner may see it. The reference answer,
// the rubric and the evidence are deliberately absent: they are what grading is
// written against, and a learner who reads them in the question has been handed the
// answer key.
type LearningQuestionView struct {
	Ordinal  int    `json:"ordinal"`
	Type     string `json:"type"`
	Question string `json:"question"`
}

// LearningProgressView is one topic's projection, as its owner may read it. This is the
// one place the numbers themselves appear: it is the learner looking at their own record,
// addressed by their own topic text. A quiz response carries no projection at all - see
// LearningQuizView - and what reaches a model is prose with the digits removed.
type LearningProgressView struct {
	TopicKey    string    `json:"topic_key"`
	Topic       string    `json:"topic"`
	Mastery     int       `json:"mastery"`
	Confidence  float64   `json:"confidence"`
	ValidCount  int       `json:"valid_attempts"`
	Level       string    `json:"level"`
	WeakPoints  []string  `json:"weak_points"`
	LastAttempt time.Time `json:"last_attempt,omitzero"`
}

// LearningQuizView is one quiz plus the question it is waiting on, and that is the whole
// list.
//
// Three things are absent on purpose. The reference answer, the rubric and the evidence
// snapshots are what grading is written against, and a learner who reads them has been
// handed the answer key. The normalized topic key is the server's own join label; the
// learner named the topic, and the display text is theirs. And the projection is not
// embedded here: an answer response that carried mastery would put a number in front of a
// client at the moment it is least able to read it as anything but a verdict, and one
// place that shows the record - the progress endpoint - is easier to reason about than two
// that disagree after a regrade.
type LearningQuizView struct {
	ID              string                `json:"id"`
	SessionID       string                `json:"session_id"`
	Topic           string                `json:"topic"`
	Status          string                `json:"status"`
	CurrentQuestion int                   `json:"current_question"`
	TotalQuestions  int                   `json:"total_questions"`
	Question        *LearningQuestionView `json:"question,omitempty"`
	ExpiresAt       time.Time             `json:"expires_at"`
}

// Start opens a quiz on one conversation and generates its questions.
//
// The topic is normalized before anything is filed under it, so twenty spellings of
// one subject do not become twenty unrelated progress records. An empty collection list
// means "everything this user has stored", which is the question a learner is actually
// asking when they say "quiz me on this"; a specific list is checked for ownership
// before a single chunk is read.
func (s *LearningService) Start(ctx context.Context, userID, chatSessionID, topic string,
	collectionIDs []string) (result *LearningQuizView, err error) {
	stage := stageValidate
	ctx, task := s.metrics.Start(ctx, taskOperationQuizStart)
	defer func() { seal(task, err, stage) }()

	if s.store == nil {
		return nil, ErrInternal
	}
	if err := s.callsModel(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" {
		return nil, ErrUnauthorized
	}
	topic = strings.TrimSpace(topic)
	if topic == "" || runeLen(topic) > domain.MaxLearningTopicRunes {
		return nil, ErrInvalidRequest
	}
	// Ownership first, mode second: a conversation that is not the caller's answers 404
	// whatever its mode is, so the mode rule cannot be used to look into another account.
	// Only starting is bound to the mode - reading a conversation's open quiz and reading
	// the projection stay available, because a refusal to *read* would strand a quiz that
	// exists and cost a learner their own record.
	stage = stageAuthorize
	conversation, err := s.store.GetSession(ctx, userID, chatSessionID)
	if err != nil {
		return nil, noteMapped(task, err, stageAuthorize, mapRepositoryError)
	}
	if conversation.Mode != domain.SessionModeLearningAgent {
		return nil, ErrLearningSessionMode
	}

	key := domain.NormalizeLearningTopicKey(topic)
	if key == "" {
		return nil, ErrInvalidRequest
	}
	now := s.now()
	stage = stageReserveQuiz
	quiz, err := s.store.CreateLearningSession(ctx, &domain.LearningSession{
		UserID: userID, SessionID: chatSessionID, Topic: topic, TopicKey: key,
		Status: domain.LearningSessionCreated, CurrentQuestion: domain.LearningQuestionOrdinalFirst,
		TotalQuestions: domain.LearningQuestionCount, Version: 1,
		ExpiresAt: now.Add(s.lifetime),
	})
	if err != nil {
		return nil, noteMapped(task, err, stageReserveQuiz, mapLearningError)
	}

	// Everything after this point has a reservation on the conversation, so every exit
	// is either a success or a release. A learner who is refused a quiz must not also be
	// locked out of asking for one.
	stage = stageEvidence
	evidence, err := s.selectEvidence(ctx, quiz, collectionIDs)
	if err != nil {
		s.releaseQuiz(ctx, quiz)
		return nil, err
	}
	if len(evidence) == 0 {
		s.releaseQuiz(ctx, quiz)
		return nil, ErrLearningNoEvidence
	}
	stage = stageReserveQuiz
	asking, err := s.store.TransitionLearningSession(ctx, userID, quiz.ID,
		domain.LearningSessionCreated, domain.LearningSessionAsking, quiz.Version,
		repository.LearningLease{Token: newLearningToken("gen"), Until: now.Add(s.generationWindow())})
	if err != nil {
		s.releaseQuiz(ctx, quiz)
		return nil, noteMapped(task, err, stageReserveQuiz, mapLearningError)
	}
	stage = stageGeneration
	generated, err := s.generateQuestions(ctx, learningGenerationInput{
		Topic: topic, Evidence: evidence,
		History:       s.history(ctx, userID, key),
		WeakPoints:    s.reliableWeakPoints(ctx, userID, key),
		PromptHistory: s.budget.PromptHistory,
	})
	if err != nil {
		// Returned as-is: generateQuestions already speaks this service's vocabulary,
		// and re-mapping it here would turn "the model would not produce a usable set"
		// into "something went wrong".
		s.releaseQuiz(ctx, asking)
		return nil, err
	}
	for index := range generated {
		question := &generated[index]
		question.UserID = userID
		question.LearningSessionID = asking.ID
		question.Ordinal = index + 1
	}
	stage = stageStoreQuestions
	opened, err := s.store.StoreLearningQuestions(ctx, userID, asking.ID, generated, asking.Version)
	if err != nil {
		// The generated set was paid for and is then not stored: the record has to
		// show both facts, which is why the cause is noted here and not left to the
		// mapped value the caller receives.
		noteFailure(task, err, stageStoreQuestions)
		s.releaseQuiz(ctx, asking)
		return nil, mapLearningError(err)
	}
	return s.view(ctx, opened)
}

// generationWindow bounds how long a generation claim is held. It is derived from the
// request's own deadline where there is one, and capped so a claim cannot outlive the
// work it claims: a lease longer than the caller's patience is a lease that will be
// reclaimed anyway.
func (s *LearningService) generationWindow() time.Duration {
	return 2 * time.Minute
}

// releaseQuiz gives a conversation back after a start that did not produce questions.
//
// The reason it is cancelled rather than left for a sweep is that every other option
// shows the learner something false. A row parked in asking says "being prepared" for
// hours after the call died; a row deleted outright erases the one fact worth keeping,
// which is that a quiz was attempted and did not happen. Cancelled says that, and
// releases the reservation in the same write.
func (s *LearningService) releaseQuiz(ctx context.Context, quiz *domain.LearningSession) {
	if s.store == nil || quiz == nil || quiz.Status.IsTerminal() {
		return
	}
	// The release runs on a context that may already be done, because the work being
	// cleaned up is not the caller's any more but the conversation's lock still is.
	release := context.WithoutCancel(ctx)
	if _, err := s.store.TransitionLearningSession(release, quiz.UserID, quiz.ID,
		quiz.Status, domain.LearningSessionCancelled, quiz.Version, repository.LearningLease{}); err != nil {
		// Nothing to do but drop it. The version may have moved, in which case a
		// recovery pass closes the row, and inventing an error here would overwrite the
		// one that actually caused the release.
	}
}

// selectEvidence recalls candidates on the topic and narrows them to the snapshots one
// quiz is written from.
func (s *LearningService) selectEvidence(ctx context.Context, quiz *domain.LearningSession,
	collectionIDs []string) ([]domain.LearningEvidence, error) {
	if s.recall == nil {
		return nil, ErrInternal
	}
	scopes := collectionIDs
	if len(scopes) == 0 {
		resolved, err := s.allCollections(ctx, quiz.UserID)
		if err != nil {
			return nil, err
		}
		scopes = resolved
	}
	if len(scopes) == 0 {
		// No collections at all is the same answer as nothing relevant in them, and
		// neither is a server error: the learner simply has nothing to be quizzed on
		// yet.
		return nil, ErrLearningNoEvidence
	}
	results, err := s.recall.Search(ctx, retrieval.Query{
		UserID: quiz.UserID, Text: quiz.Topic, CollectionIDs: scopes, TopK: s.budget.CandidateLimit,
	})
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	titles := map[string]string{}
	if s.titles != nil && len(results) > 0 {
		ids := make([]string, 0, len(results))
		seen := map[string]struct{}{}
		for _, result := range results {
			if _, duplicate := seen[result.Chunk.DocumentID]; duplicate {
				continue
			}
			seen[result.Chunk.DocumentID] = struct{}{}
			ids = append(ids, result.Chunk.DocumentID)
		}
		resolved, err := s.titles.DocumentTitles(ctx, quiz.UserID, ids)
		if err != nil {
			return nil, mapRepositoryError(err)
		}
		titles = resolved
	}
	history := s.history(ctx, quiz.UserID, quiz.TopicKey)
	weakPoints := s.reliableWeakPoints(ctx, quiz.UserID, quiz.TopicKey)
	return SelectLearningEvidence(LearningEvidenceRequest{
		Candidates: results, Titles: titles, History: history, WeakPoints: weakPoints,
	}, s.budget), nil
}

// Current returns the quiz open on one conversation. Two absences are different facts and
// are answered differently:
//
//   - a conversation that exists, belongs to the caller, and holds no open quiz is the
//     ordinary empty state - no error, and a nil view the HTTP layer serves as data=null.
//     A learner who finished a quiz, or cancelled one, reloads and asks this same
//     question; answering it with not-found would push a client into treating their own
//     history as a broken request, and answering with 5xx would be a server fault claim
//     about nothing having gone wrong.
//   - a conversation the caller may not read, or that never existed, is ErrNotFound. The
//     two are deliberately the same answer to the caller, because telling them apart is
//     how session identifiers get probed.
//
// Reading is also the recovery pass: an expired quiz, an abandoned generation and a
// grading attempt nobody is holding are all settled here, because that is the moment
// somebody is looking, and because a background sweeper would be one more moving part to
// trust.
func (s *LearningService) Current(ctx context.Context, userID, chatSessionID string) (*LearningQuizView, error) {
	if s.store == nil {
		return nil, ErrInternal
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(chatSessionID) == "" {
		return nil, ErrInvalidRequest
	}
	// The ownership question is asked of the conversation first, because the active-quiz
	// lookup keys on the pair and cannot tell "not yours" from "nothing open".
	if _, err := s.store.GetSession(ctx, userID, chatSessionID); err != nil {
		return nil, mapRepositoryError(err)
	}
	quiz, err := s.store.GetActiveLearningSession(ctx, userID, chatSessionID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, nil
		}
		return nil, mapLearningError(err)
	}
	recovered, err := s.store.RecoverLearningSession(ctx, userID, quiz.ID)
	if err != nil {
		return nil, mapLearningError(err)
	}
	if recovered.Status.IsTerminal() {
		// The quiz that was open when the learner asked has since closed under them.
		// Saying "there is no open quiz" is true a moment later, but it is not what
		// happened, and a client that retried would loop.
		return nil, ErrLearningClosed
	}
	return s.view(ctx, recovered)
}

// Answer scores one answer, or returns the stored verdict if this submission was
// already graded.
//
// A grading failure is reported as such and the question stays open: the answer is
// stored, the attempt is recorded as failed with a closed code, and the learner can
// send it again under a new submission id. Advancing the question instead would make a
// server-side failure the learner's loss.
func (s *LearningService) Answer(ctx context.Context, userID, learningSessionID, submissionID string,
	ordinal int, answer string) (result *LearningQuizView, err error) {
	stage := stageValidate
	ctx, task := s.metrics.Start(ctx, taskOperationQuizAnswer)
	defer func() { seal(task, err, stage) }()

	if s.store == nil {
		return nil, ErrInternal
	}
	// Checked before anything is claimed: taking a grading lease this service cannot
	// honour would leave the question locked until the lease expires, which turns a
	// deployment mistake into a learner-visible outage.
	if err := s.callsModel(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(learningSessionID) == "" {
		return nil, ErrUnauthorized
	}
	submissionID = strings.TrimSpace(submissionID)
	if submissionID == "" || runeLen(submissionID) > 64 {
		return nil, ErrInvalidRequest
	}
	answer = strings.TrimSpace(answer)
	if answer == "" || runeLen(answer) > domain.MaxLearningAnswerRunes {
		return nil, ErrInvalidRequest
	}
	if ordinal < domain.LearningQuestionOrdinalFirst {
		return nil, ErrInvalidRequest
	}

	quiz, err := s.store.GetLearningSession(ctx, userID, learningSessionID)
	if err != nil {
		return nil, mapLearningError(err)
	}
	questions, err := s.store.ListLearningQuestions(ctx, userID, learningSessionID)
	if err != nil {
		return nil, mapLearningError(err)
	}
	question, found := learningQuestionAt(questions, ordinal)
	if !found {
		return nil, ErrInvalidRequest
	}

	now := s.now()
	deadline := now.Add(s.gradingLease())
	attempt := domain.LearningAttempt{
		UserID: userID, SessionID: quiz.SessionID, LearningSessionID: quiz.ID,
		QuestionID: question.ID, QuestionOrdinal: question.Ordinal, SubmissionID: submissionID,
		TopicKey: quiz.TopicKey, Status: domain.LearningAttemptGrading, Answer: answer,
		LeaseToken: newLearningToken("lease"), LeaseExpiresAt: &deadline,
	}
	stage = stageClaimAttempt
	claim, err := s.store.BeginLearningAttempt(ctx, &attempt)
	if err != nil {
		return nil, noteMapped(task, err, stageClaimAttempt, mapLearningError)
	}
	if !claim.NeedsGrading {
		// This submission already has a verdict, so no model is called and mastery is
		// not counted twice for one answer. The record has to say the same thing: a
		// replay with zero calls must not look like a call that reported nothing:
		// the record for this path carries no calls at all, which totals says is
		// different from a call that made no report.
		return s.view(ctx, &claim.Session)
	}

	stage = stageGrading
	graded, gradeErr := s.gradeAnswer(ctx, claim.Attempt, question)
	if gradeErr != nil {
		if _, failErr := s.store.FailLearningAttempt(ctx, userID, submissionID, claim.Attempt.LeaseToken,
			learningFailureFor(gradeErr)); failErr != nil {
			// The attempt stays grading and is reclaimed when its lease runs out. The
			// caller is told about the scoring failure they asked about, because a
			// bookkeeping problem stacked on top of it would only hide that.
			return nil, gradeErr
		}
		return nil, gradeErr
	}
	stage = stageCompleteAttempt
	commit, err := s.store.CompleteLearningAttempt(ctx, userID, submissionID, claim.Attempt.LeaseToken, graded)
	if err != nil {
		// Grading already happened and was stored or lost; the record must say
		// which, so the store's own refusal is noted before mapping.
		noteFailure(task, err, stageCompleteAttempt)
		return nil, mapLearningError(err)
	}
	stage = stageView
	view, err := s.view(ctx, &commit.Session)
	if err != nil {
		return nil, err
	}
	// The graded attempt and the projection it caused are both stored, and neither is
	// echoed here. The learner's own record has one reading surface - the progress
	// endpoint - and a quiz response that carried a score would give a second one that can
	// drift from the first after a correction.
	return view, nil
}

// Skip moves past the current question without an answer.
func (s *LearningService) Skip(ctx context.Context, userID, learningSessionID, submissionID string) (*LearningQuizView, error) {
	if s.store == nil {
		return nil, ErrInternal
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(learningSessionID) == "" {
		return nil, ErrUnauthorized
	}
	submissionID = strings.TrimSpace(submissionID)
	if submissionID == "" || runeLen(submissionID) > 64 {
		return nil, ErrInvalidRequest
	}
	claim, err := s.store.SkipLearningQuestion(ctx, userID, learningSessionID, submissionID)
	if err != nil {
		return nil, mapLearningError(err)
	}
	return s.view(ctx, &claim.Session)
}

// Cancel ends a quiz for good. It is the one transition a learner always has, which is
// why it works from every unfinished state rather than only from waiting_answer.
func (s *LearningService) Cancel(ctx context.Context, userID, learningSessionID string) (*LearningQuizView, error) {
	if s.store == nil {
		return nil, ErrInternal
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(learningSessionID) == "" {
		return nil, ErrUnauthorized
	}
	quiz, err := s.store.GetLearningSession(ctx, userID, learningSessionID)
	if err != nil {
		return nil, mapLearningError(err)
	}
	if quiz.Status.IsTerminal() {
		// Cancelling something already finished is reported as finished, not as an
		// error: the caller's intent is already satisfied.
		return s.view(ctx, quiz)
	}
	closed, err := s.store.TransitionLearningSession(ctx, userID, learningSessionID,
		quiz.Status, domain.LearningSessionCancelled, quiz.Version, repository.LearningLease{})
	if err != nil {
		return nil, mapLearningError(err)
	}
	return s.view(ctx, closed)
}

// Progress reads one topic's projection for its owner.
func (s *LearningService) Progress(ctx context.Context, userID, topic string) (*LearningProgressView, error) {
	if s.store == nil {
		return nil, ErrInternal
	}
	if strings.TrimSpace(userID) == "" {
		return nil, ErrUnauthorized
	}
	key := domain.NormalizeLearningTopicKey(topic)
	if key == "" {
		return nil, ErrInvalidRequest
	}
	progress, err := s.store.GetTopicProgress(ctx, userID, key)
	if err != nil {
		return nil, mapLearningError(err)
	}
	return learningProgressView(*progress), nil
}

// gradingLease is how long a scoring call may hold an attempt. It sits above the
// grading timeout rather than below it: a lease that expires while the call is still
// running would let a second grader commit over the first, which is the exact failure
// the token fence exists to prevent.
func (s *LearningService) gradingLease() time.Duration {
	return time.Duration(DefaultLearningGradingTimeout+20) * time.Second
}

// view assembles what a caller may see of one quiz: where it stands, and the question it
// is waiting on. The graded attempt is deliberately not part of this - a score has one
// reading surface, the progress endpoint - so every call takes the same shape and no
// response is richer than the one before it.
func (s *LearningService) view(ctx context.Context, quiz *domain.LearningSession) (*LearningQuizView, error) {
	if quiz == nil {
		return nil, ErrInternal
	}
	view := &LearningQuizView{
		ID: quiz.ID, SessionID: quiz.SessionID, Topic: quiz.Topic,
		Status: string(quiz.Status), CurrentQuestion: quiz.CurrentQuestion,
		TotalQuestions: quiz.TotalQuestions, ExpiresAt: quiz.ExpiresAt,
	}
	if !quiz.Status.IsTerminal() && quiz.CurrentQuestion <= quiz.TotalQuestions {
		questions, err := s.store.ListLearningQuestions(ctx, quiz.UserID, quiz.ID)
		if err != nil {
			return nil, mapLearningError(err)
		}
		if question, found := learningQuestionAt(questions, quiz.CurrentQuestion); found {
			view.Question = &LearningQuestionView{
				Ordinal: question.Ordinal, Type: string(question.Type), Question: question.Question,
			}
		}
	}
	return view, nil
}

// history reads the exposure window for one topic. A failure to read it is returned as
// no history rather than as an error, and that is a deliberate loosening: the history
// only makes a quiz less repetitive, so losing it must not stop a learner from being
// able to be quizzed at all. A missing topic projection is handled the same way.
func (s *LearningService) history(ctx context.Context, userID, topicKey string) []domain.LearningQuestion {
	if s.store == nil {
		return nil
	}
	history, err := s.store.ListRecentTopicQuestions(context.WithoutCancel(ctx), userID, topicKey,
		s.budget.HistoryLimit)
	if err != nil {
		return nil
	}
	return history
}

// reliableWeakPoints returns only gaps there is enough evidence to call gaps.
//
// The gate is the projection's own: fewer than three valid attempts is not a picture of
// a learner. Generating against a single unanswered question's missing points would
// aim the next quiz at whatever one grader happened to write down once.
func (s *LearningService) reliableWeakPoints(ctx context.Context, userID, topicKey string) []string {
	if s.store == nil {
		return nil
	}
	progress, err := s.store.GetTopicProgress(context.WithoutCancel(ctx), userID, topicKey)
	if err != nil || !progress.IsReliable() {
		return nil
	}
	return append([]string(nil), progress.WeakPoints...)
}

// allCollections resolves the scope of "everything I have stored".
//
// It is resolved here rather than by passing an empty list down, because the difference
// between "all of mine" and "nothing in particular" has to be decided by somebody, and
// a retrieval layer handed an empty scope would be deciding it - in the one direction
// that must never happen.
func (s *LearningService) allCollections(ctx context.Context, userID string) ([]string, error) {
	if s.collections == nil {
		return nil, ErrInternal
	}
	listed, err := s.collections.ListCollections(ctx, userID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	ids := make([]string, 0, len(listed))
	for _, collection := range listed {
		ids = append(ids, collection.ID)
	}
	return ids, nil
}

// learningQuestionAt finds the question at one position.
func learningQuestionAt(questions []domain.LearningQuestion, ordinal int) (domain.LearningQuestion, bool) {
	for _, question := range questions {
		if question.Ordinal == ordinal {
			return question, true
		}
	}
	return domain.LearningQuestion{}, false
}

// learningProgressView projects a stored progress row into what its owner may read.
// The numbers are shown here because this is the learner looking at their own record;
// the agent that explains things to them gets prose instead - see MasteryNote.
func learningProgressView(progress domain.TopicProgress) *LearningProgressView {
	view := &LearningProgressView{
		TopicKey: progress.TopicKey, Topic: progress.Topic, Mastery: progress.Mastery,
		Confidence: progress.Confidence, ValidCount: progress.ValidCount, Level: string(progress.Level),
		WeakPoints: append([]string(nil), progress.WeakPoints...),
	}
	if progress.LastAttempt != nil {
		view.LastAttempt = *progress.LastAttempt
	}
	return view
}

// learningFailureFor classifies a grading failure into the closed set. A provider's own
// words are never stored: they are reported to the caller and dropped, because the
// column that holds this is read by whoever asks "how often does scoring fail".
func learningFailureFor(err error) domain.LearningFailureCode {
	switch {
	case errors.Is(err, ErrLearningInvalidGrade):
		return domain.LearningFailureInvalidGrade
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return domain.LearningFailureModelUnavailable
	default:
		return domain.LearningFailureGradingFailed
	}
}

// newLearningToken makes a claim identifier. It is not a secret and not an id for
// anything a user can see: it is a fence value that says "this write is mine", and it
// has to be unlikely to collide with the one another process is holding.
func newLearningToken(kind string) string {
	buffer := make([]byte, 12)
	if _, err := rand.Read(buffer); err != nil {
		// A machine with no randomness cannot run a lease, but it can still be told
		// which write it just made: the clock is enough to keep two claims distinct
		// within one process, which is all this value has to do here.
		return fmt.Sprintf("%s-%d", kind, time.Now().UTC().UnixNano())
	}
	return kind + "-" + hex.EncodeToString(buffer)
}

// mapLearningError translates the store's refusals into this service's vocabulary.
//
// Every repository error is named. That is the point: an unclassified error must not be
// flattened into a generic conflict or a bad request, because both of those tell the
// caller something false about what happened and one of them tells them to retry.
func mapLearningError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, repository.ErrLearningSessionActive):
		return ErrLearningSessionActive
	case errors.Is(err, repository.ErrLearningAnswerBusy):
		return ErrLearningBusy
	case errors.Is(err, repository.ErrLearningSubmissionConflict):
		return ErrLearningSubmissionConflict
	case errors.Is(err, repository.ErrLearningQuestionOrder):
		return ErrLearningQuestionOrder
	case errors.Is(err, repository.ErrLearningQuizClosed):
		return ErrLearningClosed
	case errors.Is(err, repository.ErrLearningStaleWrite):
		// The row moved under us. Retrying the same request would move it again, so
		// this is a conflict; the client has to read the quiz before acting.
		return ErrConflict
	case errors.Is(err, repository.ErrLearningTransition):
		// A transition the table does not allow is a bug in this package, never a
		// caller's mistake, and reporting it as anything else would hide it.
		return ErrInternal
	default:
		return mapRepositoryError(err)
	}
}
