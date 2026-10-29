package httpapi

import (
	"net/http"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/service"
)

// handleCollections serves POST /api/v1/collections and GET /api/v1/collections.
func (s *Server) handleCollections(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	if s.knowledge == nil {
		writeAPIError(w, http.StatusInternalServerError, apiError{Code: "internal_error", Message: "internal server error"})
		return
	}
	switch r.Method {
	case http.MethodPost:
		var request createCollectionRequest
		if err := decodeJSON(w, r, &request); err != nil {
			writeAPIError(w, http.StatusBadRequest, apiError{Code: "invalid_request", Message: "request body is invalid"})
			return
		}
		collection, err := s.knowledge.CreateCollection(r.Context(), userID, request.Title)
		if err != nil {
			writeServiceError(w, err, "knowledge")
			return
		}
		writeJSON(w, http.StatusOK, toCollectionView(*collection))
	case http.MethodGet:
		collections, err := s.knowledge.ListCollections(r.Context(), userID)
		if err != nil {
			writeServiceError(w, err, "knowledge")
			return
		}
		views := make([]collectionView, 0, len(collections))
		for _, item := range collections {
			views = append(views, toCollectionView(item))
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": views})
	default:
		methodNotAllowed(w)
	}
}

// handleCollectionDocuments serves POST and GET
// /api/v1/collections/{id}/documents: adding a document, and listing the ones a
// collection already holds.
func (s *Server) handleCollectionDocuments(w http.ResponseWriter, r *http.Request, collectionID string) {
	userID, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	if s.knowledge == nil {
		writeAPIError(w, http.StatusInternalServerError, apiError{Code: "internal_error", Message: "internal server error"})
		return
	}
	switch r.Method {
	case http.MethodPost:
		var request createDocumentRequest
		if err := decodeJSON(w, r, &request); err != nil {
			writeAPIError(w, http.StatusBadRequest, apiError{Code: "invalid_request", Message: "request body is invalid"})
			return
		}
		created, err := s.knowledge.AddDocument(r.Context(), userID, collectionID, service.DocumentInput{
			Title:   request.Title,
			Format:  request.Format,
			Content: request.Content,
		})
		if err != nil {
			writeServiceError(w, err, "knowledge")
			return
		}
		writeJSON(w, http.StatusOK, toDocumentView(*created))
	case http.MethodGet:
		documents, err := s.knowledge.ListDocuments(r.Context(), userID, collectionID)
		if err != nil {
			writeServiceError(w, err, "knowledge")
			return
		}
		views := make([]documentView, 0, len(documents))
		for _, item := range documents {
			views = append(views, toDocumentView(item))
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": views})
	default:
		methodNotAllowed(w)
	}
}

// handleDocument serves PUT and DELETE /api/v1/documents/{id}. PUT re-chunks the
// supplied content and advances the document version; DELETE removes the document
// and everything it contributed to retrieval. There is no GET for one document:
// the list endpoint already carries every field a document has.
func (s *Server) handleDocument(w http.ResponseWriter, r *http.Request, documentID string) {
	userID, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	if s.knowledge == nil {
		writeAPIError(w, http.StatusInternalServerError, apiError{Code: "internal_error", Message: "internal server error"})
		return
	}
	switch r.Method {
	case http.MethodPut:
		var request replaceDocumentRequest
		if err := decodeJSON(w, r, &request); err != nil {
			writeAPIError(w, http.StatusBadRequest, apiError{Code: "invalid_request", Message: "request body is invalid"})
			return
		}
		replaced, err := s.knowledge.ReplaceDocumentContent(r.Context(), userID, documentID, service.DocumentInput{
			Title:   request.Title,
			Format:  request.Format,
			Content: request.Content,
		})
		if err != nil {
			writeServiceError(w, err, "knowledge")
			return
		}
		writeJSON(w, http.StatusOK, toDocumentView(*replaced))
	case http.MethodDelete:
		if err := s.knowledge.DeleteDocument(r.Context(), userID, documentID); err != nil {
			writeServiceError(w, err, "knowledge")
			return
		}
		writeJSON(w, http.StatusOK, nil)
	default:
		methodNotAllowed(w)
	}
}

type createCollectionRequest struct {
	Title string `json:"title"`
}

type createDocumentRequest struct {
	Title   string `json:"title"`
	Format  string `json:"format"`
	Content string `json:"content"`
}

type replaceDocumentRequest struct {
	Title   string `json:"title"`
	Format  string `json:"format"`
	Content string `json:"content"`
}

type collectionView struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func toCollectionView(item domain.Collection) collectionView {
	return collectionView{
		ID:        item.ID,
		Title:     item.Title,
		CreatedAt: formatTimestamp(item.CreatedAt),
		UpdatedAt: formatTimestamp(item.UpdatedAt),
	}
}

type documentView struct {
	ID           string `json:"id"`
	CollectionID string `json:"collection_id"`
	Title        string `json:"title"`
	Format       string `json:"format"`
	Version      int    `json:"version"`
	ChunkCount   int    `json:"chunk_count"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

func toDocumentView(item domain.Document) documentView {
	return documentView{
		ID:           item.ID,
		CollectionID: item.CollectionID,
		Title:        item.Title,
		Format:       string(item.Format),
		Version:      item.Version,
		ChunkCount:   item.ChunkCount,
		CreatedAt:    formatTimestamp(item.CreatedAt),
		UpdatedAt:    formatTimestamp(item.UpdatedAt),
	}
}

// timestampLayout is the representation every existing endpoint already emits;
// it lives in one place now so the new knowledge endpoints cannot drift from it.
const timestampLayout = "2006-01-02T15:04:05.999999999Z07:00"

func formatTimestamp(value time.Time) string {
	return value.UTC().Format(timestampLayout)
}
