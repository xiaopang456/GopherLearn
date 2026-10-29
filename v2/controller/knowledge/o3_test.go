package knowledge

import (
	daoknowledge "GopherAI/dao/knowledge"
	"GopherAI/model"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The processing state is for the document's owner only, and this build answers with the
// local (in-process) processing state.
func TestO3TimelineOwnership(t *testing.T) {
	get := func(handler *Handler, userID int64) *httptest.ResponseRecorder {
		router := testRouter(userID)
		router.GET("/documents/:id/processing", handler.Processing)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/documents/doc-1/processing", nil))
		return rec
	}
	t.Run("someone else's document is a 404 and says nothing about it", func(t *testing.T) {
		handler := NewHandler(&fakeService{err: daoknowledge.ErrDocumentNotFound}, 1024)
		rec := get(handler, 99)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status %d, want 404; a foreign document must be a 404", rec.Code)
		}
	})
	t.Run("local processing mode", func(t *testing.T) {
		handler := NewHandler(&fakeService{created: &model.KnowledgeDocument{ID: "doc-1", UserID: 73, IndexStatus: model.KnowledgeStatusReady}}, 1024)
		rec := get(handler, 73)
		var body map[string]any
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil || body["mode"] != "legacy" || body["status"] != "ready" || len(body) != 2 {
			t.Fatalf("local response = %d %s", rec.Code, rec.Body.String())
		}
	})
}
