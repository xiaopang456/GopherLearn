package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/xiaopang456/GopherLearn/internal/service"
)

// The quiz surface. Like the memory confirmation surface, this file owns no rules: it
// authenticates, decodes one shape, calls LearningService, and translates what came back
// into a stable code. Everything about whether a quiz may start, whether an answer is a
// repeat, or what a score means lives behind the call.
//
// What this layer does decide is what a client may send, and the answer is narrowly: a
// topic, an optional collection scope, a submission id, a question position and an answer
// text. There is no field for a score, a mastery value, a status or an owner. A surface
// that accepted a score would be a surface where a client could grade its own answer, and
// the projection the next explanation reads would stop meaning anything.

// startLearningRequest opens a quiz. Collections is optional and means "search all of my
// notes" when empty; a supplied list is checked for ownership before anything is read.
type startLearningRequest struct {
	Topic       string   `json:"topic"`
	Collections []string `json:"collection_ids"`
}

// answerLearningRequest is one answer. SubmissionID is the caller's own deduplication
// token, kept across retries of this one answer and never reused for another: one id
// means one attempt, so a response lost in transit can be re-sent without grading the
// learner twice.
type answerLearningRequest struct {
	SubmissionID string `json:"submission_id"`
	Ordinal      int    `json:"ordinal"`
	Answer       string `json:"answer"`
}

// skipLearningRequest declines the current question. The submission id is required here
// too, for the same reason: a retry of a skip must be recognisable as the same
// operation.
type skipLearningRequest struct {
	SubmissionID string `json:"submission_id"`
}

// handleLearningSessions serves POST /api/v1/learning/sessions, which starts a quiz, and
// GET /api/v1/learning/sessions?session_id=..., which resumes the open one. Both belong to
// the collection path because "the quiz of this conversation" is one resource addressed by
// a query, the same way the memory listings name their state there.
func (s *Server) handleLearningSessions(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	if s.learning == nil {
		s.learningUnavailable(w)
		return
	}
	switch r.Method {
	case http.MethodPost:
		s.startLearningSession(w, r, userID)
	case http.MethodGet:
		s.currentLearningSession(w, r, userID)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) startLearningSession(w http.ResponseWriter, r *http.Request, userID string) {
	var request startLearningRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeAPIError(w, http.StatusBadRequest, apiError{Code: "invalid_request", Message: "request body is invalid"})
		return
	}
	// The conversation is required, and naming that is the caller's error: a missing
	// parameter is a malformed request, not a resource that happens to be absent. The
	// resume endpoint already answered this way, and a quiz whose two entry points
	// disagreed about a missing session_id would teach a client nothing useful.
	chatSessionID := requestSessionID(r)
	if chatSessionID == "" {
		writeAPIError(w, http.StatusBadRequest, apiError{Code: "invalid_request", Message: "session_id is required"})
		return
	}
	quiz, err := s.learning.Start(r.Context(), userID, chatSessionID, request.Topic, request.Collections)
	if err != nil {
		writeServiceError(w, err, "learning")
		return
	}
	writeJSON(w, http.StatusCreated, quiz)
}

// currentLearningSession is the resume endpoint. A quiz is suspended state in storage, so
// a client that reloaded the page asks here rather than restarting anything.
//
// The empty case is a success: with no quiz open on a conversation the caller owns, the
// answer is 200 with data=null and error=null. Only a conversation that cannot be read at
// all - unknown, or somebody else's - is a not-found. Collapsing the first into an error
// would make "nothing in progress" look like a server fault to every client that asks
// after finishing a quiz, which is the ordinary path through this feature.
func (s *Server) currentLearningSession(w http.ResponseWriter, r *http.Request, userID string) {
	chatSessionID := requestSessionID(r)
	if chatSessionID == "" {
		writeAPIError(w, http.StatusBadRequest, apiError{Code: "invalid_request", Message: "session_id is required"})
		return
	}
	quiz, err := s.learning.Current(r.Context(), userID, chatSessionID)
	if err != nil {
		writeServiceError(w, err, "learning")
		return
	}
	writeJSON(w, http.StatusOK, quiz)
}

// handleLearningSessionAction serves POST /api/v1/learning/sessions/{id}/{action} for the
// three things a quiz in flight can be asked to do. The verb is in the path so a body
// carries only input, and an unknown action is answered like an unknown path.
func (s *Server) handleLearningSessionAction(w http.ResponseWriter, r *http.Request, quizID, action string) {
	userID, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	if s.learning == nil {
		s.learningUnavailable(w)
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if !isRoutableSegment(quizID) || !isRoutableSegment(action) {
		writeAPIError(w, http.StatusNotFound, apiError{Code: "not_found", Message: "resource not found"})
		return
	}
	var (
		quiz *service.LearningQuizView
		err  error
	)
	switch action {
	case "answer":
		var request answerLearningRequest
		if decodeErr := decodeJSON(w, r, &request); decodeErr != nil {
			writeAPIError(w, http.StatusBadRequest, apiError{Code: "invalid_request", Message: "request body is invalid"})
			return
		}
		quiz, err = s.learning.Answer(r.Context(), userID, quizID, request.SubmissionID, request.Ordinal, request.Answer)
	case "skip":
		var request skipLearningRequest
		if decodeErr := decodeJSON(w, r, &request); decodeErr != nil {
			writeAPIError(w, http.StatusBadRequest, apiError{Code: "invalid_request", Message: "request body is invalid"})
			return
		}
		quiz, err = s.learning.Skip(r.Context(), userID, quizID, request.SubmissionID)
	case "cancel":
		if !s.refuseActionBody(w, r) {
			return
		}
		quiz, err = s.learning.Cancel(r.Context(), userID, quizID)
	default:
		writeAPIError(w, http.StatusNotFound, apiError{Code: "not_found", Message: "resource not found"})
		return
	}
	if err != nil {
		writeServiceError(w, err, "learning")
		return
	}
	writeJSON(w, http.StatusOK, quiz)
}

// handleLearningProgress serves GET /api/v1/learning/progress for one topic of the
// authenticated user's own.
func (s *Server) handleLearningProgress(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	if s.learning == nil {
		s.learningUnavailable(w)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	topic := strings.TrimSpace(r.URL.Query().Get("topic"))
	if topic == "" {
		writeAPIError(w, http.StatusBadRequest, apiError{Code: "invalid_request", Message: "topic is required"})
		return
	}
	progress, err := s.learning.Progress(r.Context(), userID, topic)
	if err != nil {
		writeServiceError(w, err, "learning")
		return
	}
	writeJSON(w, http.StatusOK, progress)
}

// learningUnavailable answers a deployment built without the quiz workflow. It is an
// internal 5xx rather than a not-found: a route that exists in the API and is not
// configured is the server's condition, not the client's mistake.
func (s *Server) learningUnavailable(w http.ResponseWriter) {
	writeAPIError(w, http.StatusInternalServerError, apiError{Code: "internal_error", Message: "internal server error"})
}

// requestSessionID reads the conversation a quiz call is addressed to. It stays a query
// parameter rather than a body field on purpose: "which conversation" is part of addressing
// the resource, and a body field for it would let a client start a quiz on a conversation it
// never mentioned in the URL.
func requestSessionID(r *http.Request) string {
	return strings.TrimSpace(r.URL.Query().Get("session_id"))
}

// learningErrorCodes names what a quiz client has to tell apart. The distinction is not
// decoration: "an answer is being scored" and "that quiz is over" are both conflicts in
// HTTP terms, and only one of them is worth retrying.
var learningErrorCodes = map[error]apiError{
	service.ErrLearningSessionActive: {
		Code: "learning_session_active", Message: "this conversation already has an unfinished quiz",
	},
	service.ErrLearningBusy: {
		Code: "learning_answer_busy", Message: "an answer is being scored right now",
	},
	service.ErrLearningSubmissionConflict: {
		Code: "learning_submission_conflict", Message: "that submission id was already used",
	},
	service.ErrLearningQuestionOrder: {
		Code: "learning_question_order", Message: "that is not the question being asked",
	},
	service.ErrLearningClosed: {
		Code: "learning_session_closed", Message: "that quiz is finished",
	},
	service.ErrLearningNoEvidence: {
		Code: "learning_no_evidence", Message: "the selected notes hold too little on this topic to build a quiz",
	},
	service.ErrLearningGeneration: {
		Code: "learning_generation_failed", Message: "the quiz could not be prepared; try again",
	},
	service.ErrLearningGrading: {
		Code: "learning_grading_failed", Message: "the answer could not be scored; send it again",
	},
	service.ErrLearningInvalidGrade: {
		Code: "learning_grading_invalid", Message: "the answer could not be scored; send it again",
	},
	service.ErrLearningModelNotConfigured: {
		Code: "learning_unavailable", Message: "internal server error",
	},
	// A quiz on a plain chat conversation is the caller's request to correct, not a
	// missing resource: the code matches the other shape refusals so a client that only
	// reads codes keeps one handling path, while 404 stays reserved for "not yours".
	service.ErrLearningSessionMode: {
		Code: "invalid_request", Message: "a quiz needs a learning agent conversation",
	},
}

// learningStatus is the HTTP status each learning refusal deserves. It is a separate map
// because the pairs differ in both directions: three refusals are conflicts, two are
// upstream model failures, and one is a client error about position.
var learningStatus = map[error]int{
	service.ErrLearningSessionActive:      http.StatusConflict,
	service.ErrLearningBusy:               http.StatusConflict,
	service.ErrLearningSubmissionConflict: http.StatusConflict,
	service.ErrLearningClosed:             http.StatusConflict,
	service.ErrLearningNoEvidence:         http.StatusConflict,
	service.ErrLearningQuestionOrder:      http.StatusBadRequest,
	// The conversation is the caller's and the request body is well formed; what is wrong
	// is which conversation the quiz was asked to run on - the client's side of the
	// bargain, so 400 rather than 409 and certainly not 404.
	service.ErrLearningSessionMode:  http.StatusBadRequest,
	service.ErrLearningGeneration:   http.StatusServiceUnavailable,
	service.ErrLearningGrading:      http.StatusServiceUnavailable,
	service.ErrLearningInvalidGrade: http.StatusServiceUnavailable,
	// A deployment with no configured model is the server's condition, not the caller's,
	// and it is not something a retry fixes either - but 500 is the honest status for a
	// wiring fault, and this route family already answers 500 when the workflow itself is
	// absent.
	service.ErrLearningModelNotConfigured: http.StatusInternalServerError,
}

// writeLearningError answers with the learning code when the service reported one, and
// reports false otherwise so the caller falls through to the general mapping.
//
// The general mapping is deliberately not allowed to reach an unmapped learning error:
// service.ErrConflict would answer "resource already exists" to a learner whose answer
// was refused because a quiz had closed, which is a different fact and a different next
// action. Every learning refusal is therefore named here.
func writeLearningError(w http.ResponseWriter, err error) bool {
	for sentinel, value := range learningErrorCodes {
		if !errors.Is(err, sentinel) {
			continue
		}
		status, ok := learningStatus[sentinel]
		if !ok {
			status = http.StatusInternalServerError
		}
		writeAPIError(w, status, value)
		return true
	}
	return false
}
