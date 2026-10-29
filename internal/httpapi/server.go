package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/security"
	"github.com/xiaopang456/GopherLearn/internal/service"
)

// maxRequestBytes is the ceiling on one request body, and ErrBodyNotUTF8 is the reason a body
// is refused before the JSON decoder looks at it.
//
// `encoding/json` turns bytes that are not valid UTF-8 into U+FFFD replacement characters and
// reports no error, so a client whose console speaks another codepage (a GBK terminal, a
// Shift-JIS editor, a truncated multi-byte sequence) used to have its text stored as a run of
// replacement marks. That is the worst possible outcome for a learner: the row looks written,
// the content is gone, and nothing on the way in or the way out says so. A learner's Chinese
// topic, an emoji and a four-byte character like 𠮷 are all valid UTF-8 and all pass through
// untouched - this gate only refuses what cannot be text at all.
const maxRequestBytes = 1 << 20

var ErrBodyNotUTF8 = errors.New("httpapi: request body is not valid UTF-8")

type Server struct {
	auth      *service.AuthService
	sessions  *service.SessionService
	messages  *service.MessageService
	knowledge *service.KnowledgeService
	memory    *service.MemoryService
	learning  *service.LearningService
	tokens    *security.TokenManager
}

// Option installs optional collaborators on the HTTP server.
type Option func(*Server)

// WithKnowledge enables the collection and document endpoints and the knowledge
// authorization gate a grounded turn depends on.
func WithKnowledge(knowledge *service.KnowledgeService) Option {
	return func(server *Server) { server.knowledge = knowledge }
}

// WithMemory enables the confirmation surface: the draft listings and the three lifecycle
// actions. Nil leaves every memory path answering 500 the way an unconfigured knowledge
// service does, so a build without memory support cannot half-implement the gate.
func WithMemory(memory *service.MemoryService) Option {
	return func(server *Server) { server.memory = memory }
}

// WithLearning enables the quiz surface: starting a quiz, resuming the open one, answering,
// skipping, cancelling and reading a topic's projection. Nil leaves each path answering 500,
// the same way an unconfigured memory service does, so a build without the quiz workflow
// cannot half-serve it.
func WithLearning(learning *service.LearningService) Option {
	return func(server *Server) { server.learning = learning }
}

func NewServer(auth *service.AuthService, sessions *service.SessionService, messages *service.MessageService, tokens *security.TokenManager, options ...Option) *Server {
	server := &Server{auth: auth, sessions: sessions, messages: messages, tokens: tokens}
	for _, option := range options {
		option(server)
	}
	return server
}

// Handler is the served entry point: the Gin router.
func (s *Server) Handler() http.Handler { return s.ginRouter() }

// ServeHTTP keeps Server usable as an http.Handler for http.Server.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.Handler().ServeHTTP(w, r)
}

// dispatchSessionRequest is the shared body of every /api/v1/sessions/{id}...
// route, so both routers resolve the caller before choosing a method or an
// action. Answering 405/404 ahead of authentication would let an unauthenticated
// request map which session ids exist.
func (s *Server) dispatchSessionRequest(w http.ResponseWriter, r *http.Request, sessionID, action string) {
	if s.sessions == nil {
		writeAPIError(w, http.StatusInternalServerError, apiError{Code: "internal_error", Message: "internal server error"})
		return
	}
	userID, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	switch action {
	case "":
		if r.Method != http.MethodDelete {
			methodNotAllowed(w)
			return
		}
		if err := s.sessions.Delete(r.Context(), userID, sessionID); err != nil {
			writeServiceError(w, err, "session")
			return
		}
		writeJSON(w, http.StatusOK, nil)
	case "messages":
		s.handleMessages(w, r, userID, sessionID)
	case "stream":
		s.handleStream(w, r, userID, sessionID)
	case topicMemoryAction:
		s.handleTopicMemory(w, r, userID, sessionID)
	default:
		writeAPIError(w, http.StatusNotFound, apiError{Code: "not_found", Message: "resource not found"})
	}
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var request registerRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeAPIError(w, http.StatusBadRequest, apiError{Code: "invalid_request", Message: "request body is invalid"})
		return
	}
	if s.auth == nil {
		writeAPIError(w, http.StatusInternalServerError, apiError{Code: "internal_error", Message: "internal server error"})
		return
	}
	user, err := s.auth.Register(r.Context(), request.Email, request.Password, request.DisplayName)
	if err != nil {
		writeServiceError(w, err, "register")
		return
	}
	writeJSON(w, http.StatusOK, user)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var request loginRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeAPIError(w, http.StatusBadRequest, apiError{Code: "invalid_request", Message: "request body is invalid"})
		return
	}
	if s.auth == nil {
		writeAPIError(w, http.StatusInternalServerError, apiError{Code: "internal_error", Message: "internal server error"})
		return
	}
	result, err := s.auth.Login(r.Context(), request.Email, request.Password)
	if err != nil {
		if errors.Is(err, service.ErrUnauthorized) {
			writeAPIError(w, http.StatusUnauthorized, apiError{Code: "invalid_credentials", Message: "invalid credentials"})
			return
		}
		writeServiceError(w, err, "login")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	userID, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	if s.auth == nil {
		writeAPIError(w, http.StatusInternalServerError, apiError{Code: "internal_error", Message: "internal server error"})
		return
	}
	user, err := s.auth.Me(r.Context(), userID)
	if err != nil {
		writeServiceError(w, err, "me")
		return
	}
	writeJSON(w, http.StatusOK, user)
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	if s.sessions == nil {
		writeAPIError(w, http.StatusInternalServerError, apiError{Code: "internal_error", Message: "internal server error"})
		return
	}
	switch r.Method {
	case http.MethodPost:
		var request createSessionRequest
		if err := decodeJSON(w, r, &request); err != nil {
			writeAPIError(w, http.StatusBadRequest, apiError{Code: "invalid_request", Message: "request body is invalid"})
			return
		}
		session, err := s.sessions.Create(r.Context(), userID, request.Title, request.Mode)
		if err != nil {
			writeServiceError(w, err, "session")
			return
		}
		writeJSON(w, http.StatusOK, toSessionView(*session))
	case http.MethodGet:
		items, err := s.sessions.List(r.Context(), userID)
		if err != nil {
			writeServiceError(w, err, "session")
			return
		}
		views := make([]sessionView, 0, len(items))
		for _, item := range items {
			views = append(views, toSessionView(item))
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": views})
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request, userID, sessionID string) {
	if s.sessions == nil {
		writeAPIError(w, http.StatusInternalServerError, apiError{Code: "internal_error", Message: "internal server error"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		items, err := s.sessions.Messages(r.Context(), userID, sessionID)
		if err != nil {
			writeServiceError(w, err, "message")
			return
		}
		views := make([]messageView, 0, len(items))
		for _, item := range items {
			views = append(views, toMessageView(item))
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": views})
	case http.MethodPost:
		if s.messages == nil {
			writeAPIError(w, http.StatusInternalServerError, apiError{Code: "internal_error", Message: "internal server error"})
			return
		}
		var request messageRequest
		if err := decodeJSON(w, r, &request); err != nil {
			writeAPIError(w, http.StatusBadRequest, apiError{Code: "invalid_request", Message: "request body is invalid"})
			return
		}
		content, knowledge := request.toTurn()
		pair, err := s.messages.Send(r.Context(), userID, sessionID, content, knowledge)
		if err != nil {
			writeServiceError(w, err, "message")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"user_message":      toMessageView(pair.User),
			"assistant_message": toMessageView(pair.Assistant),
		})
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request, userID, sessionID string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if s.sessions == nil || s.messages == nil {
		writeAPIError(w, http.StatusInternalServerError, apiError{Code: "internal_error", Message: "internal server error"})
		return
	}
	var request messageRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeAPIError(w, http.StatusBadRequest, apiError{Code: "invalid_request", Message: "request body is invalid"})
		return
	}
	content, knowledge := request.toTurn()
	// Ownership, content shape and this turn's knowledge options are checked
	// with the same logic the streaming pipeline runs, so a cross-user or
	// malformed request stays an ordinary JSON error instead of becoming an
	// SSE failure event. Asking for step events is checked here too, for the same
	// reason: a turn that has no steps must be refused before the response becomes
	// a stream, not answered with a quieter one.
	if err := s.messages.ValidateStreamTurn(r.Context(), userID, sessionID, content, knowledge, request.StreamSteps); err != nil {
		writeServiceError(w, err, "stream")
		return
	}
	flusher, ok := streamFlusher(w, r)
	if !ok {
		writeAPIError(w, http.StatusInternalServerError, apiError{Code: "internal_error", Message: "streaming is unavailable"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	terminal := false
	emit := func(event service.StreamEvent) error {
		if err := writeSSE(w, event); err != nil {
			return err
		}
		flusher.Flush()
		if event.Name == "message.completed" || event.Name == "message.failed" {
			terminal = true
		}
		return nil
	}
	// One turn per request, whichever channel it reports on: the step stream is the same
	// lifecycle with progress beside it, never a second run of the model.
	var err error
	if request.StreamSteps {
		err = s.messages.StreamSteps(r.Context(), userID, sessionID, content, knowledge, emit)
	} else {
		err = s.messages.Stream(r.Context(), userID, sessionID, content, knowledge, emit)
	}
	if err != nil && !terminal && r.Context().Err() == nil {
		_ = emit(service.StreamEvent{Name: "message.failed", Code: "internal_error", Message: "回答生成失败。"})
	}
}

type registerRequest struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type createSessionRequest struct {
	Title string             `json:"title"`
	Mode  domain.SessionMode `json:"mode"`
}

// messageRequest carries only per-turn input. Mode is a session fact set at
// creation and a strategy is derived server-side, so both are unknown fields
// here and decodeJSON rejects them. "content" has no alias.
type messageRequest struct {
	Content   string                `json:"content"`
	Knowledge knowledgeRequestField `json:"knowledge"`
	// StreamSteps asks for the agent's progress beside its answer. It is a request to
	// watch, not a mode: the answer, its events and their order are the ones a stream
	// already produces. A turn that has no steps to report is refused rather than
	// quietly streamed without them.
	StreamSteps bool `json:"stream_steps"`
}

type knowledgeRequestField struct {
	Enabled       bool     `json:"enabled"`
	CollectionIDs []string `json:"collection_ids"`
}

func (r messageRequest) toTurn() (string, domain.KnowledgeOptions) {
	return r.Content, domain.KnowledgeOptions{
		Enabled:       r.Knowledge.Enabled,
		CollectionIDs: r.Knowledge.CollectionIDs,
	}
}

type sessionView struct {
	ID        string             `json:"id"`
	UserID    string             `json:"user_id,omitempty"`
	Title     string             `json:"title"`
	Mode      domain.SessionMode `json:"mode"`
	CreatedAt string             `json:"created_at"`
	UpdatedAt string             `json:"updated_at"`
}

func toSessionView(item domain.Session) sessionView {
	return sessionView{ID: item.ID, UserID: item.UserID, Title: item.Title, Mode: item.Mode, CreatedAt: formatTimestamp(item.CreatedAt), UpdatedAt: formatTimestamp(item.UpdatedAt)}
}

type messageView struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	// Sources is omitted unless the message cites something, so an ordinary
	// conversation reads exactly as it did before retrieval existed - in history
	// as well as in the turn that produced the citation.
	Sources   []domain.Source `json:"sources,omitempty"`
	CreatedAt string          `json:"created_at"`
}

func toMessageView(item domain.Message) messageView {
	return messageView{
		ID:        item.ID,
		SessionID: item.SessionID,
		Role:      item.Role,
		Content:   item.Content,
		Sources:   item.Sources,
		CreatedAt: formatTimestamp(item.CreatedAt),
	}
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type apiEnvelope struct {
	Data  any       `json:"data"`
	Error *apiError `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiEnvelope{Data: data, Error: nil})
}

func writeAPIError(w http.ResponseWriter, status int, errorValue apiError) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiEnvelope{Data: nil, Error: &errorValue})
}

func writeServiceError(w http.ResponseWriter, err error, operation string) {
	if err == nil {
		return
	}
	switch {
	case errors.Is(err, service.ErrInvalidRequest):
		writeAPIError(w, http.StatusBadRequest, apiError{Code: "invalid_request", Message: "request is invalid"})
	case errors.Is(err, service.ErrUnauthorized):
		code, message := "unauthorized", "authentication is required"
		if operation == "login" {
			code, message = "invalid_credentials", "invalid credentials"
		}
		writeAPIError(w, http.StatusUnauthorized, apiError{Code: code, Message: message})
	case errors.Is(err, service.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, apiError{Code: "not_found", Message: "resource not found"})
	case errors.Is(err, service.ErrConflict):
		writeAPIError(w, http.StatusConflict, apiError{Code: "conflict", Message: "resource already exists"})
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		if operation == "stream" {
			return
		}
		writeAPIError(w, http.StatusRequestTimeout, apiError{Code: "request_cancelled", Message: "request was cancelled"})
	default:
		// The memory lifecycle answers next, each with its own code: they are all conflicts
		// in HTTP terms, and a confirmation surface that received one generic "conflict"
		// would have to guess which of five situations it is looking at.
		if writeMemoryError(w, err) {
			return
		}
		// The quiz refusals next, each with its own code and status: they are conflicts,
		// client errors and upstream model failures that all reach this default branch, and
		// a client that was handed one generic "internal error" cannot tell whether to
		// retry, to resend, or to stop.
		if writeLearningError(w, err) {
			return
		}
		writeAPIError(w, http.StatusInternalServerError, apiError{Code: "internal_error", Message: "internal server error"})
	}
}

func methodNotAllowed(w http.ResponseWriter) {
	writeAPIError(w, http.StatusMethodNotAllowed, apiError{Code: "invalid_request", Message: "method is not allowed"})
}

func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (string, bool) {
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	parts := strings.Fields(value)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" || s.tokens == nil {
		writeAPIError(w, http.StatusUnauthorized, apiError{Code: "unauthorized", Message: "authentication is required"})
		return "", false
	}
	claims, err := s.tokens.Parse(parts[1])
	if err != nil || claims == nil || strings.TrimSpace(claims.UserID) == "" {
		writeAPIError(w, http.StatusUnauthorized, apiError{Code: "unauthorized", Message: "authentication is required"})
		return "", false
	}
	return claims.UserID, true
}

// decodeJSON reads one request body and nothing else: every route that takes a body calls it,
// and every one of them maps its error to the same `400 invalid_request`, so the checks below
// apply to a chat turn, a knowledge document, a quiz answer and a memory proposal without any
// of them having to remember to ask.
func decodeJSON(w http.ResponseWriter, r *http.Request, destination any) error {
	if r.Body == nil {
		return errors.New("empty body")
	}
	defer r.Body.Close()
	// Read one byte past the ceiling so "exactly at the limit" and "over the limit" are
	// distinguishable. Truncating instead would hand the decoder a body that is merely
	// incomplete, and an incomplete body that happens to parse is the dangerous case.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
	if err != nil {
		return err
	}
	if len(body) > maxRequestBytes {
		return errors.New("request body is too large")
	}
	if !utf8.Valid(body) {
		return ErrBodyNotUTF8
	}
	// The same destruction has a second door: `\ud800` is pure ASCII on the wire, passes the
	// byte check above, and still decodes to U+FFFD. A correctly paired `\ud83d\udd2c` is an
	// emoji and must survive, so this is a pairing test rather than a ban on the range.
	if hasLoneSurrogateEscape(body) {
		return ErrBodyNotUTF8
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("multiple JSON values")
	}
	return nil
}

// hasLoneSurrogateEscape reports whether JSON text contains a \uXXXX escape that cannot stand
// as UTF-8: a high surrogate with no low surrogate immediately after it, or a low surrogate with
// no high surrogate before it.
//
// It walks the escapes instead of searching for a pattern because `\\u0041` is an escaped
// backslash followed by text, not an escape at all, and because "adjacent" is the rule - a
// high surrogate interrupted by any other character was never part of a pair.
func hasLoneSurrogateEscape(text []byte) bool {
	pending := false
	for index := 0; index < len(text); {
		if text[index] != '\\' {
			if pending {
				return true
			}
			index++
			continue
		}
		if index+1 >= len(text) {
			return false
		}
		if text[index+1] != 'u' {
			if pending {
				return true
			}
			index += 2
			continue
		}
		if index+6 > len(text) {
			return false
		}
		value, ok := parseHex4(text[index+2 : index+6])
		if !ok {
			if pending {
				return true
			}
			index += 2
			continue
		}
		switch {
		case value >= 0xD800 && value <= 0xDBFF:
			if pending {
				return true
			}
			pending = true
		case value >= 0xDC00 && value <= 0xDFFF:
			if !pending {
				return true
			}
			pending = false
		default:
			if pending {
				return true
			}
		}
		index += 6
	}
	return pending
}

func parseHex4(digits []byte) (rune, bool) {
	var value rune
	for _, digit := range digits {
		value <<= 4
		switch {
		case digit >= '0' && digit <= '9':
			value |= rune(digit - '0')
		case digit >= 'a' && digit <= 'f':
			value |= rune(digit-'a') + 10
		case digit >= 'A' && digit <= 'F':
			value |= rune(digit-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

func writeSSE(w io.Writer, event service.StreamEvent) error {
	payload := map[string]any{}
	if event.MessageID != "" {
		payload["message_id"] = event.MessageID
	}
	if event.Delta != "" {
		payload["delta"] = event.Delta
	}
	if event.Content != "" {
		payload["content"] = event.Content
	}
	if event.Code != "" {
		payload["code"] = event.Code
	}
	if event.Message != "" {
		payload["message"] = event.Message
	}
	if len(event.Sources) > 0 {
		payload["sources"] = event.Sources
	}
	// The step fields are added the same way Sources is: an unset one is not written at
	// all, so a chat or knowledge frame keeps the exact body it had before the agent
	// protocol existed. This is the whole compatibility argument for the feature, and it
	// is why these keys are listed one by one instead of marshalling a struct.
	if event.RunID != "" {
		payload["run_id"] = event.RunID
	}
	if event.Sequence != 0 {
		payload["sequence"] = event.Sequence
	}
	if event.StepType != "" {
		payload["step_type"] = event.StepType
	}
	if event.ToolName != "" {
		payload["tool_name"] = event.ToolName
	}
	if event.DurationMillis != 0 {
		payload["duration_millis"] = event.DurationMillis
	}
	if event.TerminationReason != "" {
		payload["termination_reason"] = event.TerminationReason
	}
	// The text-block keys follow the same one-by-one rule as the step keys above: an unset
	// one is not written, so a chat, knowledge or plain agent frame keeps the body it has
	// always had. block_id names one model call's public text, round says which call that
	// was, kind closes it, and final_block_id on the completed frame is the block whose
	// fragments the confirmed content is made of.
	if event.BlockID != "" {
		payload["block_id"] = event.BlockID
	}
	if event.Round != 0 {
		payload["round"] = event.Round
	}
	if event.Kind != "" {
		payload["kind"] = event.Kind
	}
	if event.FinalBlockID != "" {
		payload["final_block_id"] = event.FinalBlockID
	}
	// A waiting_confirmation frame additionally carries the draft it asks about, so the
	// card can be acted on where it appears. The fields below are the whole object, listed
	// one by one for the same reason as the step keys above: a stored row that grew a new
	// column could not grow the frame along with it. `draft_id` is the one identifier here,
	// and it is the address the confirm/reject endpoints take; a card that could not name
	// its draft could not be decided on.
	if event.Draft != nil {
		draft := map[string]any{
			"draft_id": event.Draft.DraftID,
			"scope":    event.Draft.Scope,
			"content":  event.Draft.Content,
		}
		// A subject belongs to a topic-scoped draft and to nothing else, so an unset one is
		// not written as an empty string either.
		if event.Draft.TopicKey != "" {
			draft["topic_key"] = event.Draft.TopicKey
		}
		payload["memory_draft"] = draft
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, "event: "+event.Name+"\ndata: "+string(encoded)+"\n\n")
	return err
}

func sessionPathParts(path string) ([]string, bool) {
	const prefix = "/api/v1/sessions/"
	if !strings.HasPrefix(path, prefix) {
		return nil, false
	}
	rest := strings.TrimPrefix(path, prefix)
	if rest == "" || strings.HasSuffix(rest, "/") {
		return nil, false
	}
	rawParts := strings.Split(rest, "/")
	parts := make([]string, len(rawParts))
	for index, raw := range rawParts {
		if raw == "" {
			return nil, false
		}
		decoded, err := url.PathUnescape(raw)
		if err != nil || decoded == "" || strings.Contains(decoded, "/") {
			return nil, false
		}
		parts[index] = decoded
	}
	return parts, true
}
