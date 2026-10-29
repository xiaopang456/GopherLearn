package httpapi

import (
	"errors"
	"net/http"

	"github.com/xiaopang456/GopherLearn/internal/service"
)

// topicMemoryAction is the action segment under /sessions/{id}/ that carries the learner's
// topic-memory switch: GET and PUT /api/v1/sessions/{id}/topic-memory.
//
// It lives on the conversation rather than beside it because the decision is about one
// conversation - the one the agent is reading memories for - and the path already names it under
// the caller's own authentication. A separate resource would need a session id in the body or the
// query as well, and every one of those shapes is a place where the conversation the client *meant*
// and the conversation the server *grants* can stop being the same thing.
const topicMemoryAction = "topic-memory"

// topicMemoryView is the whole response body: one boolean, and nothing else.
//
// No topic key, no scope, no owner, no internal identifier appears here, and that is not
// tidiness. The subject a "yes" unlocks is derived on the server at recall time from the quiz this
// conversation has open; echoing it in a switch's answer would hand a client a value it could send
// back later to describe a different conversation, and a claim a client can carry is a claim a
// client can be wrong about.
type topicMemoryView struct {
	Enabled bool `json:"enabled"`
}

// topicMemoryRequest is the whole request body.
//
// Enabled is a pointer because {"enabled":false} and {} are two different requests, and a plain
// bool would make an absent field mean "switch it off". Silently turning a permission off is a
// real action taken on the learner's behalf by a client that sent nothing, so an absent field is a
// malformed request instead - and the strict decoder ahead of this check already refuses any key
// that is not `enabled`, so topic_key, user_id, scope, session_id and all_users cannot arrive here
// at all.
type topicMemoryRequest struct {
	Enabled *bool `json:"enabled"`
}

// handleTopicMemory serves the switch for one conversation. It is dispatched from
// dispatchSessionRequest, so authentication and the shape rules for the path prefix have already
// run by the time this is reached.
func (s *Server) handleTopicMemory(w http.ResponseWriter, r *http.Request, userID, sessionID string) {
	switch r.Method {
	case http.MethodGet:
		s.readTopicMemory(w, r, userID, sessionID)
	case http.MethodPut:
		s.writeTopicMemory(w, r, userID, sessionID)
	default:
		// POST would look like an action pair (on/off) and invite a third verb later; PATCH would
		// imply there is something else on this resource to change. Both answers are the same
		// write, and 405 says so without pretending the surface is bigger than it is.
		methodNotAllowed(w)
	}
}

// readTopicMemory answers what the learner last decided.
//
// Absence is answered, not reported: a conversation that has never been switched on answers
// `{"enabled":false}` with a 200, because "no row" is the default state this product promises and a
// 404 would make the normal case look like a broken link. 404 stays reserved for the conversation
// not being the caller's - which is answered the same way whether it exists at all or not.
//
// The mode is not asked here. Reading your own answer is never harmful, and refusing it would give
// a chat conversation no way to find out what is stored about it; what the mode does control is
// whether that answer can ever grant a read, and that is decided on the recall side.
func (s *Server) readTopicMemory(w http.ResponseWriter, r *http.Request, userID, sessionID string) {
	enabled, err := s.sessions.TopicMemoryState(r.Context(), userID, sessionID)
	if err != nil {
		writeServiceError(w, err, "session")
		return
	}
	writeJSON(w, http.StatusOK, topicMemoryView{Enabled: enabled})
}

// writeTopicMemory stores the learner's answer for one conversation.
//
// The write is idempotent by value, so a client that retries a click the network lost is not a
// second decision and there is nothing to version; the response is the stored state rather than an
// echo of the request, so a client that raced another one reads back what the server actually
// holds.
func (s *Server) writeTopicMemory(w http.ResponseWriter, r *http.Request, userID, sessionID string) {
	var request topicMemoryRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeAPIError(w, http.StatusBadRequest, apiError{Code: "invalid_request", Message: "request body is invalid"})
		return
	}
	if request.Enabled == nil {
		writeAPIError(w, http.StatusBadRequest, apiError{Code: "invalid_request", Message: "enabled is required"})
		return
	}
	committed, err := s.sessions.CommitTopicMemory(r.Context(), userID, sessionID, *request.Enabled)
	if err != nil {
		// A plain chat conversation is the caller's request to correct, not a missing resource -
		// the same judgement the quiz surface makes about the same mistake, with the same code so
		// a client that only reads codes keeps one handling path.
		if errors.Is(err, service.ErrTopicMemoryNeedsLearningAgent) {
			writeAPIError(w, http.StatusBadRequest, apiError{Code: "invalid_request",
				Message: "topic memory needs a learning agent conversation"})
			return
		}
		writeServiceError(w, err, "session")
		return
	}
	writeJSON(w, http.StatusOK, topicMemoryView{Enabled: committed.Enabled})
}
