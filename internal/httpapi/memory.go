package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/service"
)

// The confirmation surface is where a memory an agent proposed changes state on a user's
// behalf; adding one is where the user states a fact outright and it needs no second
// decision. Either way this file owns no rules of its own: it authenticates, decodes a
// shape, and calls the same MemoryService the agent-facing code calls. Everything that
// decides whether a write or a transition is legal lives behind that call.

// memoryStatePending and memoryStateActive are the only two listings this API offers. The
// other states exist in storage, but a surface that could ask for them would be an audit
// log, which is a different product decision from "show me what I have to decide on".
const (
	memoryStatePending = "pending"
	memoryStateActive  = "active"
)

// handleMemories serves GET and POST /api/v1/memories: the owner's two listings and the
// addition of a memory the caller states themselves.
func (s *Server) handleMemories(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	if s.memory == nil {
		writeAPIError(w, http.StatusInternalServerError, apiError{Code: "internal_error", Message: "internal server error"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.listMemories(w, r, userID)
	case http.MethodPost:
		s.addMemory(w, r, userID)
	default:
		methodNotAllowed(w)
	}
}

// listMemories answers "what is waiting for me" and "what do I currently believe", one at a
// time. The state comes from the query and has no default: a caller that forgets to say
// which list it wanted should not silently receive the confirmed one.
func (s *Server) listMemories(w http.ResponseWriter, r *http.Request, userID string) {
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	var (
		items []domain.Memory
		err   error
	)
	switch state {
	case memoryStatePending:
		items, err = s.memory.PendingDrafts(r.Context(), userID)
	case memoryStateActive:
		items, err = s.memory.ConfirmedMemories(r.Context(), userID)
	default:
		writeAPIError(w, http.StatusBadRequest, apiError{Code: "invalid_request", Message: "request is invalid"})
		return
	}
	if err != nil {
		writeServiceError(w, err, "memory")
		return
	}
	views := make([]memoryView, 0, len(items))
	for _, item := range items {
		views = append(views, toMemoryView(item))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": views})
}

// addUserMemoryRequest is the whole of what a person may state about themselves. There is
// no status, no confidence, no memory_id, no user_id, no timestamp, no source_session_id
// and no source_message_ids here, and decodeJSON rejects any of those that arrive.
//
// Provenance is refused rather than ignored, and that is the point: a hand-written fact has
// no conversation behind it, and letting a client supply one would let the same request
// fabricate the evidence that makes an agent's claim checkable. Confidence is refused
// because the author's assertion IS the certainty this surface stores.
type addUserMemoryRequest struct {
	Scope    domain.MemoryScope `json:"scope"`
	TopicKey string             `json:"topic_key"`
	Content  string             `json:"content"`
}

func (r addUserMemoryRequest) toAuthored() domain.UserAuthoredMemory {
	return domain.UserAuthoredMemory{Scope: r.Scope, TopicKey: r.TopicKey, Content: r.Content}
}

// addMemory stores what the caller typed, active and final in one request.
func (s *Server) addMemory(w http.ResponseWriter, r *http.Request, userID string) {
	var request addUserMemoryRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeAPIError(w, http.StatusBadRequest, apiError{Code: "invalid_request", Message: "request body is invalid"})
		return
	}
	created, err := s.memory.AddUserMemory(r.Context(), userID, request.toAuthored())
	if err != nil {
		writeServiceError(w, err, "memory")
		return
	}
	writeJSON(w, http.StatusOK, toMemoryView(*created))
}

// handleMemoryAction serves POST /api/v1/memories/{id}/{action} for the three decisions the
// lifecycle allows. The verb travels in the path, not the body, so the body can be refused
// outright and an action stays idempotent-looking in logs without carrying state.
func (s *Server) handleMemoryAction(w http.ResponseWriter, r *http.Request, memoryID, action string) {
	userID, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	if s.memory == nil {
		writeAPIError(w, http.StatusInternalServerError, apiError{Code: "internal_error", Message: "internal server error"})
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if !isRoutableSegment(memoryID) || !isRoutableSegment(action) {
		writeAPIError(w, http.StatusNotFound, apiError{Code: "not_found", Message: "resource not found"})
		return
	}
	if !s.refuseActionBody(w, r) {
		return
	}
	var (
		memory *domain.Memory
		err    error
	)
	switch action {
	case "confirm":
		memory, err = s.memory.ConfirmMemoryDraft(r.Context(), userID, memoryID)
	case "reject":
		memory, err = s.memory.RejectMemoryDraft(r.Context(), userID, memoryID)
	case "archive":
		memory, err = s.memory.ArchiveMemory(r.Context(), userID, memoryID)
	default:
		// An unknown action is not found rather than invalid: the same answer the path
		// itself gets, so a wrong action name teaches nothing about which ones exist.
		writeAPIError(w, http.StatusNotFound, apiError{Code: "not_found", Message: "resource not found"})
		return
	}
	if err != nil {
		writeServiceError(w, err, "memory")
		return
	}
	writeJSON(w, http.StatusOK, toMemoryView(*memory))
}

// refuseActionBody reports whether the request may proceed. An action carries no input, so
// an absent body or an empty object is accepted and anything else is refused - including
// {"status":"active"}, which is a client trying to write the state the gate owns.
func (s *Server) refuseActionBody(w http.ResponseWriter, r *http.Request) bool {
	if r.Body == nil {
		return true
	}
	defer r.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(r.Body, 1<<10))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, apiError{Code: "invalid_request", Message: "request body is invalid"})
		return false
	}
	if strings.TrimSpace(string(payload)) == "" {
		return true
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	var body struct{}
	if err := decoder.Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, apiError{Code: "invalid_request", Message: "request body is invalid"})
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeAPIError(w, http.StatusBadRequest, apiError{Code: "invalid_request", Message: "request body is invalid"})
		return false
	}
	return true
}

type memoryView struct {
	MemoryID   string              `json:"memory_id"`
	Scope      domain.MemoryScope  `json:"scope"`
	TopicKey   string              `json:"topic_key,omitempty"`
	Content    string              `json:"content"`
	Status     domain.MemoryStatus `json:"status"`
	Confidence float64             `json:"confidence"`
	// The provenance is echoed so the confirmation surface can show what a memory claims to
	// rest on. It is the caller's own data: every listing here is already filtered to the
	// authenticated user, and a foreign id answers 404 before any of it is rendered.
	SourceSessionID  string   `json:"source_session_id"`
	SourceMessageIDs []string `json:"source_message_ids"`
	CreatedAt        string   `json:"created_at"`
	UpdatedAt        string   `json:"updated_at"`
	// There is deliberately no superseded_at: this API lists pending and active rows only,
	// and the one state that carries that timestamp is a state no client can be handed. A
	// field that can never be filled would read as an omission the next time someone
	// compares it with the stored record.
}

func toMemoryView(item domain.Memory) memoryView {
	return memoryView{
		MemoryID:         item.MemoryID,
		Scope:            item.Scope,
		TopicKey:         item.TopicKey,
		Content:          item.Content,
		Status:           item.Status,
		Confidence:       item.Confidence,
		SourceSessionID:  item.SourceSessionID,
		SourceMessageIDs: item.SourceMessageIDs,
		CreatedAt:        formatTimestamp(item.CreatedAt),
		UpdatedAt:        formatTimestamp(item.UpdatedAt),
	}
}

// memoryErrorCodes names the lifecycle answers a confirmation surface has to tell apart.
// Each is a conflict in HTTP terms - the request was well formed and the stored state said
// no - but they are deliberately not one code, because the next action differs for each:
// "already done" needs no retry, "the evidence is gone" needs a new draft, and "not active"
// means the user aimed at the wrong row. Collapsing them into "conflict" would make the
// interface guess, which is what the distinct service sentinels exist to prevent.
var memoryErrorCodes = map[error]apiError{
	service.ErrMemoryAlreadyConfirmed: {Code: "memory_already_confirmed", Message: "this memory was already confirmed"},
	service.ErrMemoryArchived:         {Code: "memory_archived", Message: "this memory is archived and no longer usable"},
	service.ErrMemorySuperseded:       {Code: "memory_superseded", Message: "a newer memory replaced this one"},
	service.ErrMemoryNotActive:        {Code: "memory_not_active", Message: "this memory is not in the state the action needs"},
	service.ErrMemoryEvidenceMissing:  {Code: "memory_evidence_missing", Message: "the conversation this memory cites is no longer stored"},
}

// writeMemoryError answers with the lifecycle code when the service reported one of them,
// and reports false when it did not, so the caller falls through to the general mapping.
func writeMemoryError(w http.ResponseWriter, err error) bool {
	for sentinel, value := range memoryErrorCodes {
		if errors.Is(err, sentinel) {
			writeAPIError(w, http.StatusConflict, value)
			return true
		}
	}
	return false
}
