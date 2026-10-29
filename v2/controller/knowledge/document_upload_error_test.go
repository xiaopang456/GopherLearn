package knowledge

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"GopherAI/common/code"
	serviceknowledge "GopherAI/service/knowledge"
)

// wantMissingFileCode is the business code reserved for a request that carries
// no "file" part at all. It is the next free value of the knowledge range
// (7001-7010 are taken by the shared code table).
const wantMissingFileCode int64 = 7011

func multipartRequest(t *testing.T, path, field, filename, content string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile(field, filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = part.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	return request
}

func decodeStatus(t *testing.T, response *httptest.ResponseRecorder) (int64, string) {
	t.Helper()
	var payload struct {
		StatusCode int64  `json:"status_code"`
		StatusMsg  string `json:"status_msg"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v body=%s", err, response.Body.String())
	}
	return payload.StatusCode, payload.StatusMsg
}

// TestUploadRejectsNonMultipartRequest locks the client-error contract: a body
// that is not multipart/form-data never reached a document at all, so it is a
// 400 and not a server failure.
func TestUploadRejectsNonMultipartRequest(t *testing.T) {
	handler := NewHandler(&fakeService{}, 1024)
	router := testRouter(73)
	router.POST("/documents", handler.Create)

	request := httptest.NewRequest(http.MethodPost, "/documents", strings.NewReader(`{"file":"notes.md"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusBadRequest, response.Body.String())
	}
	status, _ := decodeStatus(t, response)
	if status != code.CodeInvalidParams.Code() {
		t.Fatalf("status_code = %d, want %d; body=%s", status, code.CodeInvalidParams.Code(), response.Body.String())
	}
}

// TestUploadWithoutFileFieldReportsMissingField makes sure a request that
// carries no "file" part is not reported as an unsupported file type: no
// extension was ever supplied.
func TestUploadWithoutFileFieldReportsMissingField(t *testing.T) {
	handler := NewHandler(&fakeService{}, 1024)
	router := testRouter(73)
	router.POST("/documents", handler.Create)

	request := multipartRequest(t, "/documents", "upload", "notes.md", "# notes")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusBadRequest, response.Body.String())
	}
	status, message := decodeStatus(t, response)
	if status == code.KnowledgeInvalidFile.Code() {
		t.Fatalf("a missing file field must not reuse the unsupported-type code %d; body=%s", status, response.Body.String())
	}
	if status != wantMissingFileCode {
		t.Fatalf("status_code = %d, want %d; body=%s", status, wantMissingFileCode, response.Body.String())
	}
	if message == code.KnowledgeInvalidFile.Msg() {
		t.Fatalf("a missing file field must not reuse the unsupported-type message %q", message)
	}
	if !strings.Contains(message, "file") {
		t.Fatalf("message = %q, want it to name the missing file field", message)
	}
}

// TestUploadKeepsUnsupportedExtensionMapping pins the 7001 mapping: a .pdf
// really is an unsupported file type.
func TestUploadKeepsUnsupportedExtensionMapping(t *testing.T) {
	service := &fakeService{err: serviceknowledge.ErrUnsupportedFile}
	handler := NewHandler(service, 1024)
	router := testRouter(73)
	router.POST("/documents", handler.Create)

	request := multipartRequest(t, "/documents", "file", "notes.pdf", "content")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusBadRequest, response.Body.String())
	}
	status, message := decodeStatus(t, response)
	if status != code.KnowledgeInvalidFile.Code() || message != code.KnowledgeInvalidFile.Msg() {
		t.Fatalf("status_code = %d message = %q, want %d / %q", status, message, code.KnowledgeInvalidFile.Code(), code.KnowledgeInvalidFile.Msg())
	}
}

// TestUploadKeepsOversizedBodyAsPayloadTooLarge keeps the 413 answer for a
// body that exceeds the reader limit.
func TestUploadKeepsOversizedBodyAsPayloadTooLarge(t *testing.T) {
	handler := NewHandler(&fakeService{}, 1024)
	router := testRouter(73)
	router.POST("/documents", handler.Create)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "notes.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = part.Write(bytes.Repeat([]byte("x"), 2*1024*1024)); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/documents", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusRequestEntityTooLarge, response.Body.String())
	}
	status, _ := decodeStatus(t, response)
	if status != code.KnowledgeFileTooLarge.Code() {
		t.Fatalf("status_code = %d, want %d; body=%s", status, code.KnowledgeFileTooLarge.Code(), response.Body.String())
	}
}
