package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// flusherKey carries the Flusher of the ResponseWriter the http.Server actually
// handed us. Gin replaces the writer with its own wrapper whose Flush method
// exists unconditionally, so without this the "streaming is unavailable" answer
// would silently disappear once routing moved to Gin.
type flusherKey struct{}

type flusherSlot struct{ flusher http.Flusher }

func streamFlusher(w http.ResponseWriter, r *http.Request) (http.Flusher, bool) {
	if slot, ok := r.Context().Value(flusherKey{}).(flusherSlot); ok {
		return slot.flusher, slot.flusher != nil
	}
	flusher, ok := w.(http.Flusher)
	return flusher, ok
}

// ginRouter declares the phase-1 route table. Handlers stay framework-neutral:
// they decode with the same helper, call the same services and write with the
// same envelope writers as the reference router, so Gin contributes routing and
// path parameters only.
func (s *Server) ginRouter() http.Handler {
	// Route registration and startup lines would otherwise print per instance,
	// including in tests. Gin's mode is a process-global by design.
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	// A 301 for "/api/v1/sessions/" or "/api/v1/me/" would replace the contract's
	// JSON 404, and Gin's built-in 405 writes plain text with an Allow header.
	engine.RedirectTrailingSlash = false
	engine.RedirectFixedPath = false
	engine.RemoveExtraSlash = false
	engine.HandleMethodNotAllowed = false
	engine.SetTrustedProxies(nil)

	engine.Any("/api/v1/auth/register", gin.WrapF(s.handleRegister))
	engine.Any("/api/v1/auth/login", gin.WrapF(s.handleLogin))
	engine.Any("/api/v1/me", gin.WrapF(s.handleMe))
	engine.Any("/api/v1/sessions", gin.WrapF(s.handleSessions))
	engine.Any("/api/v1/sessions/:id", s.ginSessionItem)
	engine.Any("/api/v1/sessions/:id/:action", s.ginSessionAction)
	engine.Any("/api/v1/collections", gin.WrapF(s.handleCollections))
	engine.Any("/api/v1/collections/:id/documents", s.ginCollectionDocuments)
	engine.Any("/api/v1/documents/:id", s.ginDocumentItem)
	engine.Any("/api/v1/memories", gin.WrapF(s.handleMemories))
	engine.Any("/api/v1/memories/:id/:action", s.ginMemoryAction)
	engine.Any("/api/v1/learning/sessions", gin.WrapF(s.handleLearningSessions))
	engine.Any("/api/v1/learning/sessions/:id/:action", s.ginLearningSessionAction)
	engine.Any("/api/v1/learning/progress", gin.WrapF(s.handleLearningProgress))
	engine.NoRoute(s.ginNoRoute)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		engine.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), flusherKey{}, flusherSlot{flusher: flusher})))
	})
}

func (s *Server) ginSessionItem(c *gin.Context) {
	sessionID := c.Param("id")
	if !isRoutableSegment(sessionID) {
		s.handleSessionPath(c.Writer, c.Request)
		return
	}
	s.dispatchSessionRequest(c.Writer, c.Request, sessionID, "")
}

func (s *Server) ginSessionAction(c *gin.Context) {
	sessionID, action := c.Param("id"), c.Param("action")
	if !isRoutableSegment(sessionID) || !isRoutableSegment(action) {
		s.handleSessionPath(c.Writer, c.Request)
		return
	}
	s.dispatchSessionRequest(c.Writer, c.Request, sessionID, action)
}

// ginNoRoute keeps the fall-through ordering of the reference router: a path
// under the session prefix is judged by the same shape rules and answers 401
// before 404 where that is what the reference did, while everything else is a
// plain not-found.
func (s *Server) ginNoRoute(c *gin.Context) {
	if strings.HasPrefix(c.Request.URL.Path, "/api/v1/sessions/") {
		s.handleSessionPath(c.Writer, c.Request)
		return
	}
	writeAPIError(c.Writer, http.StatusNotFound, apiError{Code: "not_found", Message: "resource not found"})
}

// handleSessionPath validates and dispatches a /api/v1/sessions/... path that
// Gin's tree could not route as {id} or {id}/{action}: an empty or doubled
// segment, a trailing slash, a decoded separator, or a path deeper than two
// segments. It keeps the pre-migration ordering, where authentication answers
// before the unknown-shape 404, so path probing cannot enumerate sessions
// without a token. It is a shape guard, not a second router.
func (s *Server) handleSessionPath(w http.ResponseWriter, r *http.Request) {
	parts, ok := sessionPathParts(r.URL.Path)
	if !ok {
		writeAPIError(w, http.StatusNotFound, apiError{Code: "not_found", Message: "resource not found"})
		return
	}
	if len(parts) > 2 {
		// Path shapes deeper than {id}/{action} authenticate first and then 404,
		// which is the ordering the contract shipped with.
		if _, authenticated := s.authenticate(w, r); !authenticated {
			return
		}
		writeAPIError(w, http.StatusNotFound, apiError{Code: "not_found", Message: "resource not found"})
		return
	}
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}
	s.dispatchSessionRequest(w, r, parts[0], action)
}

func (s *Server) ginCollectionDocuments(c *gin.Context) {
	collectionID := c.Param("id")
	if !isRoutableSegment(collectionID) {
		writeAPIError(c.Writer, http.StatusNotFound, apiError{Code: "not_found", Message: "resource not found"})
		return
	}
	s.handleCollectionDocuments(c.Writer, c.Request, collectionID)
}

// ginMemoryAction routes /api/v1/memories/{id}/{action}. The handler authenticates before
// it judges the shape, so an unauthenticated probe cannot learn which action names exist.
func (s *Server) ginMemoryAction(c *gin.Context) {
	s.handleMemoryAction(c.Writer, c.Request, c.Param("id"), c.Param("action"))
}

// ginLearningSessionAction routes /api/v1/learning/sessions/{id}/{action}. The handler
// authenticates before it reveals whether the quiz exists, so an unknown id and one
// belonging to somebody else stay one answer.
func (s *Server) ginLearningSessionAction(c *gin.Context) {
	s.handleLearningSessionAction(c.Writer, c.Request, c.Param("id"), c.Param("action"))
}

func (s *Server) ginDocumentItem(c *gin.Context) {
	documentID := c.Param("id")
	if !isRoutableSegment(documentID) {
		writeAPIError(c.Writer, http.StatusNotFound, apiError{Code: "not_found", Message: "resource not found"})
		return
	}
	s.handleDocument(c.Writer, c.Request, documentID)
}

func isRoutableSegment(value string) bool {
	return value != "" && !strings.Contains(value, "/")
}
