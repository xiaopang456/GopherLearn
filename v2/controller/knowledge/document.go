package knowledge

import (
	"GopherAI/common/code"
	"GopherAI/controller"
	daoknowledge "GopherAI/dao/knowledge"
	"GopherAI/model"
	serviceknowledge "GopherAI/service/knowledge"
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

type DocumentService interface {
	CreateDocument(context.Context, int64, string, io.Reader) (*model.KnowledgeDocument, error)
	ListDocuments(context.Context, int64) ([]model.KnowledgeDocument, error)
	GetDocument(context.Context, int64, string) (*model.KnowledgeDocument, error)
	RetryDocument(context.Context, int64, string) (*model.KnowledgeDocument, error)
	UpdateDocument(context.Context, int64, string, string, io.Reader) (*model.KnowledgeDocument, bool, error)
	DeleteDocument(context.Context, int64, string) error
	ReindexDocument(context.Context, int64, string) (*model.KnowledgeDocument, error)
}

// knowledgeFileMissingCode answers a request that carried no "file" part at
// all. It takes the next free value of the knowledge range (7001-7010 belong
// to the shared code table) and lives in this package because that table is
// owned elsewhere and cannot be edited from here.
const knowledgeFileMissingCode = code.Code(7011)

// knowledgeCodeMessages holds the messages of the codes allocated in this
// package. The shared code table cannot be reached for them, so they carry
// their own text here.
var knowledgeCodeMessages = map[code.Code]string{
	knowledgeFileMissingCode: "缺少文件字段 file",
}

type Handler struct {
	service         DocumentService
	maxRequestBytes int64
}

// Processing returns the processing state of one of the caller's documents. A document
// that is not theirs is a 404, exactly like the other document endpoints, so its existence is not
// revealed.
//
// This build processes documents in-process, so the answer is the local processing state:
// {"mode":"legacy","status":<index_status>}.
func (h *Handler) Processing(c *gin.Context) {
	document, err := h.service.GetDocument(c.Request.Context(), c.GetInt64("userID"), c.Param("id"))
	if err != nil {
		respondError(c, err)
		return
	}
	{
		c.JSON(http.StatusOK, map[string]string{"mode": "legacy", "status": string(document.IndexStatus)})
		return
	}
}

func NewHandler(service DocumentService, maxFileSizeBytes int64) *Handler {
	return &Handler{service: service, maxRequestBytes: maxFileSizeBytes + 1024*1024}
}

type DocumentView struct {
	ID            string                        `json:"id"`
	OriginalName  string                        `json:"original_name"`
	FileType      string                        `json:"file_type"`
	FileSize      int64                         `json:"file_size"`
	Version       int                           `json:"version"`
	ActiveVersion int                           `json:"active_version"`
	ChunkCount    int                           `json:"chunk_count"`
	IndexStatus   model.KnowledgeDocumentStatus `json:"index_status"`
	ErrorMessage  string                        `json:"error_message,omitempty"`
	CreatedAt     time.Time                     `json:"created_at"`
	UpdatedAt     time.Time                     `json:"updated_at"`
}

type documentResponse struct {
	controller.Response
	Document  *DocumentView `json:"document,omitempty"`
	Unchanged bool          `json:"unchanged,omitempty"`
}

type documentListResponse struct {
	controller.Response
	Documents []DocumentView `json:"documents"`
}

func (h *Handler) Create(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.maxRequestBytes)
	fileHeader, err := c.FormFile("file")
	if err != nil {
		respondError(c, err)
		return
	}
	file, err := fileHeader.Open()
	if err != nil {
		respondError(c, err)
		return
	}
	defer file.Close()
	document, err := h.service.CreateDocument(c.Request.Context(), c.GetInt64("userID"), fileHeader.Filename, file)
	if err != nil {
		respondError(c, err)
		return
	}
	response := documentResponse{Document: toView(document)}
	response.Success()
	c.JSON(http.StatusCreated, response)
}

func (h *Handler) List(c *gin.Context) {
	documents, err := h.service.ListDocuments(c.Request.Context(), c.GetInt64("userID"))
	if err != nil {
		respondError(c, err)
		return
	}
	views := make([]DocumentView, 0, len(documents))
	for i := range documents {
		views = append(views, *toView(&documents[i]))
	}
	response := documentListResponse{Documents: views}
	response.Success()
	c.JSON(http.StatusOK, response)
}

func (h *Handler) Get(c *gin.Context) {
	document, err := h.service.GetDocument(c.Request.Context(), c.GetInt64("userID"), c.Param("id"))
	if err != nil {
		respondError(c, err)
		return
	}
	response := documentResponse{Document: toView(document)}
	response.Success()
	c.JSON(http.StatusOK, response)
}

func (h *Handler) Retry(c *gin.Context) {
	document, err := h.service.RetryDocument(c.Request.Context(), c.GetInt64("userID"), c.Param("id"))
	if err != nil {
		respondError(c, err)
		return
	}
	response := documentResponse{Document: toView(document)}
	response.Success()
	c.JSON(http.StatusAccepted, response)
}

func (h *Handler) Update(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.maxRequestBytes)
	fileHeader, err := c.FormFile("file")
	if err != nil {
		respondError(c, err)
		return
	}
	file, err := fileHeader.Open()
	if err != nil {
		respondError(c, err)
		return
	}
	defer file.Close()
	document, unchanged, err := h.service.UpdateDocument(c.Request.Context(), c.GetInt64("userID"), c.Param("id"), fileHeader.Filename, file)
	if err != nil {
		respondError(c, err)
		return
	}
	response := documentResponse{Document: toView(document), Unchanged: unchanged}
	response.Success()
	status := http.StatusAccepted
	if unchanged {
		status = http.StatusOK
	}
	c.JSON(status, response)
}

func (h *Handler) Delete(c *gin.Context) {
	if err := h.service.DeleteDocument(c.Request.Context(), c.GetInt64("userID"), c.Param("id")); err != nil {
		respondError(c, err)
		return
	}
	response := new(controller.Response)
	response.Success()
	c.JSON(http.StatusAccepted, response)
}

func (h *Handler) Reindex(c *gin.Context) {
	document, err := h.service.ReindexDocument(c.Request.Context(), c.GetInt64("userID"), c.Param("id"))
	if err != nil {
		respondError(c, err)
		return
	}
	response := documentResponse{Document: toView(document)}
	response.Success()
	c.JSON(http.StatusAccepted, response)
}

func toView(document *model.KnowledgeDocument) *DocumentView {
	return &DocumentView{
		ID: document.ID, OriginalName: document.OriginalName, FileType: document.FileType,
		FileSize: document.FileSize, Version: document.Version, ActiveVersion: document.ActiveVersion,
		ChunkCount: document.ChunkCount, IndexStatus: document.IndexStatus, ErrorMessage: document.ErrorMessage,
		CreatedAt: document.CreatedAt, UpdatedAt: document.UpdatedAt,
	}
}

func respondError(c *gin.Context, err error) {
	status, businessCode := http.StatusInternalServerError, code.CodeServerBusy
	var maxBytesError *http.MaxBytesError
	switch {
	case errors.As(err, &maxBytesError):
		status, businessCode = http.StatusRequestEntityTooLarge, code.KnowledgeFileTooLarge
	case errors.Is(err, http.ErrMissingFile):
		status, businessCode = http.StatusBadRequest, knowledgeFileMissingCode
	case errors.Is(err, http.ErrNotMultipart):
		status, businessCode = http.StatusBadRequest, code.CodeInvalidParams
	case errors.Is(err, serviceknowledge.ErrUnsupportedFile), errors.Is(err, serviceknowledge.ErrInvalidDocument):
		status, businessCode = http.StatusBadRequest, code.KnowledgeInvalidFile
	case errors.Is(err, serviceknowledge.ErrFileTooLarge):
		status, businessCode = http.StatusRequestEntityTooLarge, code.KnowledgeFileTooLarge
	case errors.Is(err, serviceknowledge.ErrEmptyDocument):
		status, businessCode = http.StatusBadRequest, code.KnowledgeEmptyFile
	case errors.Is(err, serviceknowledge.ErrDocumentLimit):
		status, businessCode = http.StatusConflict, code.KnowledgeDocumentLimit
	case errors.Is(err, serviceknowledge.ErrStorageLimit):
		status, businessCode = http.StatusConflict, code.KnowledgeStorageLimit
	case errors.Is(err, serviceknowledge.ErrDocumentNameExists):
		status, businessCode = http.StatusConflict, code.KnowledgeNameConflict
	case errors.Is(err, serviceknowledge.ErrDocumentHashExists):
		status, businessCode = http.StatusConflict, code.KnowledgeContentConflict
	case errors.Is(err, daoknowledge.ErrDocumentNotFound):
		status, businessCode = http.StatusNotFound, code.KnowledgeDocumentNotFound
	case errors.Is(err, serviceknowledge.ErrDocumentNotRetryable):
		status, businessCode = http.StatusConflict, code.KnowledgeDocumentNotRetryable
	case errors.Is(err, serviceknowledge.ErrDocumentBusy), errors.Is(err, serviceknowledge.ErrDocumentChanged):
		status, businessCode = http.StatusConflict, code.KnowledgeDocumentBusy
	}
	response := new(controller.Response)
	response.CodeOf(businessCode)
	if message, ok := knowledgeCodeMessages[businessCode]; ok {
		response.StatusMsg = message
	}
	c.JSON(status, response)
}
