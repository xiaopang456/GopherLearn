package hybrid

import (
	"GopherAI/common/code"
	"GopherAI/common/mysql"
	"GopherAI/common/rag"
	agentController "GopherAI/controller/agent"
	messageDAO "GopherAI/dao/message"
	sessionDAO "GopherAI/dao/session"
	"GopherAI/entry"
	jwtAuth "GopherAI/middleware/jwt"
	"GopherAI/model"
	agentService "GopherAI/service/agent"
	sessionService "GopherAI/service/session"
	userService "GopherAI/service/user"
	"GopherAI/utils/myjwt"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/gin-gonic/gin"
	"github.com/xiaopang456/GopherLearn/conversation"
	"gorm.io/gorm"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type httpSurface struct {
	db    *gorm.DB
	chat  *conversation.Service
	agent *entry.Application
}
type rawWriterKey struct{}
type authCapture struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (w *authCapture) Header() http.Header { return w.header }
func (w *authCapture) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *authCapture) Write(b []byte) (int, error) { w.WriteHeader(200); return w.body.Write(b) }

// Only Gin/native-auth writes are captured. Successful compatibility handlers
// write directly to the actual server writer, so buffering cannot swallow SSE.
func newHTTPHandler(db *gorm.DB, chat *conversation.Service, agent *entry.Application) http.Handler {
	s := &httpSurface{db: db, chat: chat, agent: agent}
	r := gin.New()
	r.Use(gin.Recovery())
	r.RedirectTrailingSlash = false
	r.RedirectFixedPath = false
	r.SetTrustedProxies(nil)
	wrap := func(handler func(http.ResponseWriter, *http.Request, int64, string)) gin.HandlerFunc {
		return func(c *gin.Context) {
			w := c.Request.Context().Value(rawWriterKey{}).(http.ResponseWriter)
			handler(w, c.Request, c.GetInt64("userID"), c.GetString("userName"))
		}
	}
	r.Any("/api/v1/auth/login", wrap(s.login))
	r.Any("/api/v1/auth/register", wrap(func(w http.ResponseWriter, r *http.Request, _ int64, _ string) {
		apiError(w, 501, "registration_not_adapted", "registration requires the original V2 captcha contract")
	}))
	protected := r.Group("/api/v1", jwtAuth.Auth())
	protected.Any("/me", wrap(s.me))
	protected.GET("/knowledge/documents/:document/source", wrap(s.documentSource))
	protected.Any("/sessions", wrap(s.sessions))
	protected.Any("/sessions/:id", wrap(s.sessionItem))
	protected.Any("/sessions/:id/:action", func(c *gin.Context) {
		w := c.Request.Context().Value(rawWriterKey{}).(http.ResponseWriter)
		uid, name := c.GetInt64("userID"), c.GetString("userName")
		switch c.Param("action") {
		case "messages":
			s.messages(w, c.Request, uid, name)
		case "stream":
			s.stream(w, c.Request, uid, name)
		default:
			apiError(w, 404, "not_found", "resource not found")
		}
	})
	r.NoRoute(jwtAuth.Auth(), wrap(func(w http.ResponseWriter, r *http.Request, _ int64, _ string) {
		apiError(w, 404, "not_found", "resource not found")
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		path := req.URL.Path
		compat := path == "/api/v1/me" || strings.HasPrefix(path, "/api/v1/auth/") || path == "/api/v1/sessions" || strings.HasPrefix(path, "/api/v1/sessions/") || (strings.HasPrefix(path, "/api/v1/knowledge/documents/") && strings.HasSuffix(path, "/source"))
		if !compat {
			agent.Handler.ServeHTTP(w, req)
			return
		}
		captured := &authCapture{header: make(http.Header)}
		r.ServeHTTP(captured, req.WithContext(context.WithValue(req.Context(), rawWriterKey{}, w)))
		if status, key, message, ok := compatError(captured); ok {
			apiError(w, status, key, message)
		}
	})
}

// compatError translates a refusal captured from the native middleware into the
// compatibility envelope. The native middleware stays the authority for token
// validation and for the status code; only the wire envelope is translated.
// A refusal that carried no body of its own still answers with the standard
// envelope, so an unauthenticated caller never receives an empty body.
func compatError(captured *authCapture) (int, string, string, bool) {
	if captured.status == http.StatusUnauthorized {
		return http.StatusUnauthorized, "unauthorized", "authentication is required", true
	}
	if captured.body.Len() == 0 {
		return 0, "", "", false
	}
	var native struct {
		Code code.Code `json:"status_code"`
	}
	if json.Unmarshal(captured.body.Bytes(), &native) == nil && native.Code == code.CodeInvalidToken {
		return http.StatusUnauthorized, "unauthorized", "authentication is required", true
	}
	return http.StatusServiceUnavailable, "internal_error", "internal server error", true
}
func apiError(w http.ResponseWriter, status int, key, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"data": nil, "error": map[string]string{"code": key, "message": message}})
}
func noQuery(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.RawQuery != "" {
		apiError(w, 400, "invalid_request", "request is invalid")
		return false
	}
	return true
}
func method(w http.ResponseWriter, r *http.Request, expected string) bool {
	if r.Method != expected {
		apiError(w, 405, "invalid_request", "method is not allowed")
		return false
	}
	return noQuery(w, r)
}
func codeError(w http.ResponseWriter, result code.Code) {
	switch result {
	case code.CodeInvalidParams:
		apiError(w, 400, "invalid_request", "request is invalid")
	case code.CodeInvalidPassword:
		apiError(w, 401, "invalid_credentials", "invalid credentials")
	case code.SessionLimitReached:
		apiError(w, 409, "session_limit_reached", result.Msg())
	case code.AgentRequestConflict:
		apiError(w, 409, "agent_request_conflict", result.Msg())
	default:
		apiError(w, 503, "internal_error", "internal server error")
	}
}
func daoError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sessionDAO.ErrSessionNotFound), errors.Is(err, gorm.ErrRecordNotFound):
		apiError(w, 404, "not_found", "resource not found")
	case errors.Is(err, conversation.ErrInvalidArgument):
		apiError(w, 400, "invalid_request", "request is invalid")
	default:
		apiError(w, 500, "internal_error", "internal server error")
	}
}
func profile(row *model.User) any {
	return map[string]any{"id": strconv.FormatInt(row.ID, 10), "email": row.Email, "username": row.Username, "display_name": row.Name}
}
func authenticatedUser(id int64, name string) (*model.User, error) {
	row, err := mysql.GetUserByUsername(name)
	if err != nil {
		return nil, err
	}
	if row.ID != id || row.Username != name {
		return nil, gorm.ErrRecordNotFound
	}
	return row, nil
}
func (s *httpSurface) login(w http.ResponseWriter, r *http.Request, _ int64, _ string) {
	if !method(w, r, "POST") {
		return
	}
	var input struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := conversation.DecodeRequest(w, r, &input); err != nil {
		apiError(w, 400, "invalid_request", "request body is invalid")
		return
	}
	if input.Email == "" || input.Password == "" {
		codeError(w, code.CodeInvalidParams)
		return
	}
	token, result := userService.Login(input.Email, input.Password)
	if result != code.CodeSuccess {
		codeError(w, result)
		return
	}
	claims, err := myjwt.ParseToken(token)
	if err != nil {
		apiError(w, 401, "unauthorized", "authentication is required")
		return
	}
	row, err := authenticatedUser(claims.ID, claims.Username)
	if err != nil {
		daoError(w, err)
		return
	}
	conversation.WriteEnvelope(w, 200, map[string]any{"token": token, "user": profile(row)})
}
func (s *httpSurface) me(w http.ResponseWriter, r *http.Request, uid int64, name string) {
	if !method(w, r, "GET") {
		return
	}
	row, err := authenticatedUser(uid, name)
	if err != nil {
		daoError(w, err)
		return
	}
	conversation.WriteEnvelope(w, 200, profile(row))
}
func frontendMode(mode model.SessionMode) (string, error) {
	switch mode {
	case model.SessionModeNormal:
		return "chat", nil
	case model.SessionModeAgent:
		return "learning_agent", nil
	default:
		return "", conversation.ErrInvalidArgument
	}
}
func sessionView(row model.Session) (any, error) {
	mode, err := frontendMode(row.Mode)
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": row.ID, "user_id": strconv.FormatInt(row.UserID, 10), "title": row.Title, "mode": mode, "created_at": row.CreatedAt.UTC().Format(time.RFC3339Nano), "updated_at": row.UpdatedAt.UTC().Format(time.RFC3339Nano)}, nil
}
func (s *httpSurface) sessions(w http.ResponseWriter, r *http.Request, uid int64, name string) {
	if !noQuery(w, r) {
		return
	}
	switch r.Method {
	case "POST":
		var input struct {
			Title     string `json:"title"`
			Mode      string `json:"mode"`
			RequestID string `json:"requestId"`
		}
		if err := conversation.DecodeRequest(w, r, &input); err != nil {
			apiError(w, 400, "invalid_request", "request body is invalid")
			return
		}
		var mode model.SessionMode
		switch input.Mode {
		case "chat":
			mode = model.SessionModeNormal
		case "learning_agent":
			mode = model.SessionModeAgent
		default:
			apiError(w, 400, "invalid_request", "request is invalid")
			return
		}
		row, result := sessionService.CreateForAssembly(r.Context(), uid, name, input.Title, string(mode), input.RequestID)
		if result != code.CodeSuccess {
			codeError(w, result)
			return
		}
		view, err := sessionView(*row)
		if err != nil {
			daoError(w, err)
			return
		}
		conversation.WriteEnvelope(w, 200, view)
	case "GET":
		rows, err := sessionDAO.NewRepository(s.db).ListByUserID(r.Context(), uid)
		if err != nil {
			daoError(w, err)
			return
		}
		items := make([]any, 0, len(rows))
		for _, row := range rows {
			view, err := sessionView(row)
			if err != nil {
				daoError(w, err)
				return
			}
			items = append(items, view)
		}
		conversation.WriteEnvelope(w, 200, map[string]any{"items": items})
	default:
		apiError(w, 405, "invalid_request", "method is not allowed")
	}
}
func sessionID(r *http.Request) string {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/sessions/"), "/")
	return parts[0]
}
func (s *httpSurface) sessionItem(w http.ResponseWriter, r *http.Request, uid int64, _ string) {
	if !method(w, r, "DELETE") {
		return
	}
	row, err := sessionDAO.NewRepository(s.db).GetByIDAndUserID(r.Context(), sessionID(r), uid)
	if err != nil {
		daoError(w, err)
		return
	}
	if _, err := frontendMode(row.Mode); err != nil {
		daoError(w, err)
		return
	}
	ids, err := sessionDAO.NewRepository(s.db).DeleteOwned(r.Context(), uid, row.ID)
	if err != nil {
		daoError(w, err)
		return
	}
	if len(ids) > 0 {
		// SQL deletion is authoritative. A derived-index failure is reported rather
		// than pretending SQL rolled back, or silently discarding the cleanup seam.
		if s.agent.EpisodeIndex == nil || s.agent.EpisodeIndex.Delete(r.Context(), uid, ids) != nil {
			apiError(w, 503, "derived_cleanup_failed", "session deleted; derived index cleanup is incomplete")
			return
		}
	}
	conversation.WriteEnvelope(w, 200, nil)
}
func messageView(row conversation.Message) any {
	return map[string]any{"id": row.ID, "session_id": row.SessionID, "role": row.Role, "content": row.Content, "created_at": row.CreatedAt.UTC().Format(time.RFC3339Nano)}
}

type messageInput struct {
	Content   string `json:"content"`
	Knowledge struct {
		Enabled       bool     `json:"enabled"`
		CollectionIDs []string `json:"collection_ids"`
	} `json:"knowledge"`
	StreamSteps bool `json:"stream_steps"`
}

func inputTurn(w http.ResponseWriter, r *http.Request) (messageInput, bool) {
	var input messageInput
	if err := conversation.DecodeRequest(w, r, &input); err != nil {
		apiError(w, 400, "invalid_request", "request body is invalid")
		return input, false
	}
	if input.StreamSteps || input.Knowledge.Enabled || len(input.Knowledge.CollectionIDs) > 0 {
		apiError(w, 400, "invalid_request", "request is invalid")
		return input, false
	}
	return input, true
}
func (s *httpSurface) messages(w http.ResponseWriter, r *http.Request, uid int64, name string) {
	if !noQuery(w, r) {
		return
	}
	sid := sessionID(r)
	switch r.Method {
	case "GET":
		items, err := s.history(r.Context(), uid, sid)
		if err != nil {
			daoError(w, err)
			return
		}
		conversation.WriteEnvelope(w, 200, map[string]any{"items": items})
	case "POST":
		owned, err := sessionDAO.NewRepository(s.db).GetByIDAndUserID(r.Context(), sid, uid)
		if err != nil {
			daoError(w, err)
			return
		}
		switch owned.Mode {
		case model.SessionModeAgent:
			s.agentMessage(w, r, uid, name, sid)
			return
		case model.SessionModeNormal:
		default:
			daoError(w, conversation.ErrInvalidArgument)
			return
		}
		input, ok := inputTurn(w, r)
		if !ok {
			return
		}
		pair, err := s.chat.Send(r.Context(), strconv.FormatInt(uid, 10), sid, input.Content)
		if err != nil {
			conversation.WriteError(w, err, "message")
			return
		}
		conversation.WriteEnvelope(w, 200, map[string]any{"user_message": messageView(pair.User), "assistant_message": messageView(pair.Assistant)})
	default:
		apiError(w, 405, "invalid_request", "method is not allowed")
	}
}
func (s *httpSurface) agentMessage(w http.ResponseWriter, r *http.Request, uid int64, name, sid string) {
	var input struct {
		Content         string                              `json:"content"`
		RequestID       string                              `json:"requestId"`
		KnowledgePolicy rag.KnowledgePolicy                 `json:"knowledgePolicy"`
		TopicConsent    agentService.TopicConsentSubmission `json:"topicConsent"`
	}
	if err := conversation.DecodeRequest(w, r, &input); err != nil {
		apiError(w, 400, "invalid_request", "request body is invalid")
		return
	}
	result, err := s.agent.Agent.Run(r.Context(), agentService.RunRequest{
		UserID: uid, UserName: name, SessionID: sid, Question: input.Content,
		RequestID: input.RequestID, KnowledgePolicy: input.KnowledgePolicy, TopicConsent: input.TopicConsent,
	}, agentService.DiscardEvents)
	if err == nil {
		conversation.WriteEnvelope(w, 200, result)
		return
	}
	// Native classification and Run identity survive acquisition failures.
	// This layer never retries or writes Agent messages.
	native := agentController.ErrorCodeForAssembly(err)
	status, key := 500, "agent_model_failed"
	switch native {
	case code.CodeInvalidParams:
		status, key = 400, "invalid_request"
	case code.CodeRecordNotFound:
		status, key = 404, "not_found"
	case code.AgentRunBusy:
		status, key = 409, "agent_run_busy"
	case code.AgentRequestConflict:
		status, key = 409, "agent_request_conflict"
	case code.AgentRequestFinished:
		status, key = 409, "agent_request_finished"
	case code.CodeServerBusy:
		status, key = 503, "internal_error"
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"data": result, "error": map[string]any{"code": key, "native_code": native, "message": native.Msg()}})
}

func (s *httpSurface) history(ctx context.Context, uid int64, sid string) ([]any, error) {
	items := make([]any, 0)
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row, err := sessionDAO.NewRepository(tx).GetByIDAndUserID(ctx, sid, uid)
		if err != nil {
			return err
		}
		if _, err := frontendMode(row.Mode); err != nil {
			return err
		}
		var pages [][]model.Message
		before := uint(0)
		for {
			page, err := messageDAO.NewRepository(tx).ListOwnedHistoryPage(ctx, uid, sid, before, 100)
			if err != nil {
				return err
			}
			pages = append(pages, page.Messages)
			if !page.HasMore {
				break
			}
			before = page.NextBeforeID
		}
		for i := len(pages) - 1; i >= 0; i-- {
			for _, row := range pages[i] {
				view := messageView(domainMessage(row)).(map[string]any)
				// The run that produced this message is part of the message's
				// own fact: it is what lets a conversation that is opened again
				// show the steps its turn took. A message of an ordinary chat
				// has no run, and then the key is absent rather than empty.
				if row.AgentRunID != nil && strings.TrimSpace(*row.AgentRunID) != "" {
					view["run_id"] = *row.AgentRunID
				}
				if len(row.Sources) > 0 {
					sources := make([]any, 0, len(row.Sources))
					for _, source := range row.Sources {
						// Citation is a per-message ordinal, not a invented Collection/chunk ID.
						sources = append(sources, map[string]any{"source_id": strconv.Itoa(source.Citation), "document_id": source.DocID, "title": source.FileName, "section": strings.Join(source.TitlePath, " / "), "snippet": source.Snippet, "original_v2": map[string]any{"citation": strconv.Itoa(source.Citation), "doc_id": source.DocID, "doc_version": source.DocVersion, "file_name": source.FileName, "title_path": source.TitlePath, "start_line": source.StartLine, "end_line": source.EndLine, "chunk_index": source.ChunkIndex, "score": source.Score, "snippet": source.Snippet}})
					}
					view["sources"] = sources
				}
				items = append(items, view)
			}
		}
		return nil
	})
	return items, err
}
func (s *httpSurface) stream(w http.ResponseWriter, r *http.Request, uid int64, name string) {
	if !method(w, r, "POST") {
		return
	}
	owned, err := sessionDAO.NewRepository(s.db).GetByIDAndUserID(r.Context(), sessionID(r), uid)
	if err != nil {
		daoError(w, err)
		return
	}
	if owned.Mode == model.SessionModeAgent {
		s.agentStream(w, r, uid, name)
		return
	}
	if owned.Mode != model.SessionModeNormal {
		daoError(w, conversation.ErrInvalidArgument)
		return
	}
	input, ok := inputTurn(w, r)
	if !ok {
		return
	}
	sid := sessionID(r)
	user := strconv.FormatInt(uid, 10)
	if err := s.chat.ValidateStream(r.Context(), user, sid, input.Content); err != nil {
		conversation.WriteError(w, err, "stream")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		apiError(w, 500, "internal_error", "streaming is unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	terminal := false
	emit := func(event conversation.Event) error {
		if err := conversation.WriteEvent(w, event); err != nil {
			return err
		}
		flusher.Flush()
		if event.Name == "message.completed" || event.Name == "message.failed" {
			terminal = true
		}
		return nil
	}
	err = s.chat.Stream(r.Context(), user, sid, input.Content, emit)
	if err != nil && !terminal && r.Context().Err() == nil {
		_ = emit(conversation.Event{Name: "message.failed", Code: "internal_error", Message: "回答生成失败。"})
	}
}
