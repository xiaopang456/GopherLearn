package knowledge

import (
	daoknowledge "GopherAI/dao/knowledge"
	"GopherAI/model"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestHandlerCreateDocument(t *testing.T) {
	service := &fakeService{created: &model.KnowledgeDocument{
		ID: "doc-1", UserID: 73, OriginalName: "gin.md", FileType: "md",
		FileSize: 7, Version: 1, IndexStatus: model.KnowledgeStatusPending,
	}}
	handler := NewHandler(service, 1024)
	router := testRouter(73)
	router.POST("/documents", handler.Create)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "gin.md")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("# Gin\n"))
	_ = writer.Close()
	request := httptest.NewRequest(http.MethodPost, "/documents", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusCreated || service.gotUserID != 73 || service.gotName != "gin.md" {
		t.Fatalf("create result: status=%d user=%d name=%q body=%s", response.Code, service.gotUserID, service.gotName, response.Body.String())
	}
	var payload struct {
		StatusCode int          `json:"status_code"`
		Document   DocumentView `json:"document"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.StatusCode != 1000 || payload.Document.ID != "doc-1" || payload.Document.IndexStatus != model.KnowledgeStatusPending {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestHandlerListsOnlyServiceDocuments(t *testing.T) {
	service := &fakeService{documents: []model.KnowledgeDocument{{ID: "own", UserID: 73, OriginalName: "gin.md"}}}
	handler := NewHandler(service, 1024)
	router := testRouter(73)
	router.GET("/documents", handler.List)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/documents", nil))
	if response.Code != http.StatusOK || service.gotUserID != 73 {
		t.Fatalf("list status=%d user=%d", response.Code, service.gotUserID)
	}
	if !bytes.Contains(response.Body.Bytes(), []byte(`"id":"own"`)) {
		t.Fatalf("body = %s", response.Body.String())
	}
}

func TestHandlerMapsNotFound(t *testing.T) {
	service := &fakeService{err: daoknowledge.ErrDocumentNotFound}
	handler := NewHandler(service, 1024)
	router := testRouter(73)
	router.GET("/documents/:id", handler.Get)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/documents/missing", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestHandlerRetriesFailedDocument(t *testing.T) {
	service := &fakeService{created: &model.KnowledgeDocument{ID: "failed", IndexStatus: model.KnowledgeStatusPending}}
	handler := NewHandler(service, 1024)
	router := testRouter(73)
	router.POST("/documents/:id/retry", handler.Retry)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/documents/failed/retry", nil))
	if response.Code != http.StatusAccepted || service.gotUserID != 73 || service.gotDocumentID != "failed" {
		t.Fatalf("retry status=%d user=%d doc=%q body=%s", response.Code, service.gotUserID, service.gotDocumentID, response.Body.String())
	}
}

func TestHandlerUpdateDeleteAndReindex(t *testing.T) {
	t.Run("update", func(t *testing.T) {
		service := &fakeService{created: &model.KnowledgeDocument{ID: "doc-1", Version: 2, IndexStatus: model.KnowledgeStatusPending}}
		handler := NewHandler(service, 1024)
		router := testRouter(73)
		router.PUT("/documents/:id", handler.Update)
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		part, _ := writer.CreateFormFile("file", "gin.md")
		_, _ = part.Write([]byte("new"))
		_ = writer.Close()
		request := httptest.NewRequest(http.MethodPut, "/documents/doc-1", &body)
		request.Header.Set("Content-Type", writer.FormDataContentType())
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusAccepted || service.gotDocumentID != "doc-1" || service.gotName != "gin.md" {
			t.Fatalf("update status=%d doc=%q name=%q", response.Code, service.gotDocumentID, service.gotName)
		}
	})

	t.Run("delete", func(t *testing.T) {
		service := &fakeService{}
		handler := NewHandler(service, 1024)
		router := testRouter(73)
		router.DELETE("/documents/:id", handler.Delete)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/documents/doc-1", nil))
		if response.Code != http.StatusAccepted || service.gotDocumentID != "doc-1" {
			t.Fatalf("delete status=%d doc=%q", response.Code, service.gotDocumentID)
		}
	})

	t.Run("reindex", func(t *testing.T) {
		service := &fakeService{created: &model.KnowledgeDocument{ID: "doc-1", IndexStatus: model.KnowledgeStatusPending}}
		handler := NewHandler(service, 1024)
		router := testRouter(73)
		router.POST("/documents/:id/reindex", handler.Reindex)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/documents/doc-1/reindex", nil))
		if response.Code != http.StatusAccepted || service.gotDocumentID != "doc-1" {
			t.Fatalf("reindex status=%d doc=%q", response.Code, service.gotDocumentID)
		}
	})
}

func testRouter(userID int64) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("userID", userID)
		c.Next()
	})
	return router
}

type fakeService struct {
	created       *model.KnowledgeDocument
	documents     []model.KnowledgeDocument
	err           error
	gotUserID     int64
	gotName       string
	gotDocumentID string
}

func (f *fakeService) CreateDocument(_ context.Context, userID int64, name string, source io.Reader) (*model.KnowledgeDocument, error) {
	f.gotUserID, f.gotName = userID, name
	_, _ = io.ReadAll(source)
	return f.created, f.err
}

func (f *fakeService) ListDocuments(_ context.Context, userID int64) ([]model.KnowledgeDocument, error) {
	f.gotUserID = userID
	return f.documents, f.err
}

func (f *fakeService) GetDocument(_ context.Context, userID int64, _ string) (*model.KnowledgeDocument, error) {
	f.gotUserID = userID
	return f.created, f.err
}

func (f *fakeService) RetryDocument(_ context.Context, userID int64, documentID string) (*model.KnowledgeDocument, error) {
	f.gotUserID, f.gotDocumentID = userID, documentID
	return f.created, f.err
}

func (f *fakeService) UpdateDocument(_ context.Context, userID int64, documentID, name string, source io.Reader) (*model.KnowledgeDocument, bool, error) {
	f.gotUserID, f.gotDocumentID, f.gotName = userID, documentID, name
	_, _ = io.ReadAll(source)
	return f.created, false, f.err
}

func (f *fakeService) DeleteDocument(_ context.Context, userID int64, documentID string) error {
	f.gotUserID, f.gotDocumentID = userID, documentID
	return f.err
}

func (f *fakeService) ReindexDocument(_ context.Context, userID int64, documentID string) (*model.KnowledgeDocument, error) {
	f.gotUserID, f.gotDocumentID = userID, documentID
	return f.created, f.err
}
