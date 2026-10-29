package memory

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"GopherAI/common/code"
	memorydao "GopherAI/dao/memory"
	memoryservice "GopherAI/service/memory"

	"github.com/gin-gonic/gin"
)

func memoryBindRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("userID", int64(73))
		c.Next()
	})
	router.POST("/memories", Create)
	router.PATCH("/memories/:id", Correct)
	router.POST("/memories/:id/archive", Archive)
	router.DELETE("/memories/:id", Delete)
	router.POST("/memory-drafts/:id/resolve", ResolveDraft)
	return router
}

// TestBindingFailureAnswersBadRequest locks the client-error contract: a body
// that cannot be bound is a 400 with the invalid-params business code, never a
// 500 that hides a client mistake behind a server failure.
func TestBindingFailureAnswersBadRequest(t *testing.T) {
	router := memoryBindRouter()
	for _, tc := range []struct {
		name, method, path, body string
	}{
		{"missing required fields", http.MethodPost, "/memories", `{}`},
		{"wrong json type", http.MethodPost, "/memories", `{"scope":"profile","subject":7,"content":"x","tags":["a"],"durability":"long_term","idempotency_key":"k","reason":"r"}`},
		{"empty body", http.MethodPost, "/memories", ``},
		{"malformed json", http.MethodPost, "/memories", `{`},
		{"correction without reason", http.MethodPatch, "/memories/memory-1", `{"subject":"s","content":"c","tags":["a"]}`},
		{"archive without reason", http.MethodPost, "/memories/memory-1/archive", `{}`},
		{"delete without reason", http.MethodDelete, "/memories/memory-1", `{}`},
		{"draft resolve without action", http.MethodPost, "/memory-drafts/draft-1/resolve", `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusBadRequest, response.Body.String())
			}
			var payload struct {
				StatusCode int64  `json:"status_code"`
				StatusMsg  string `json:"status_msg"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatalf("decode: %v body=%s", err, response.Body.String())
			}
			if payload.StatusCode != code.CodeInvalidParams.Code() {
				t.Fatalf("status_code = %d, want %d; body=%s", payload.StatusCode, code.CodeInvalidParams.Code(), response.Body.String())
			}
		})
	}
}

// TestRespondErrorKeepsKnownMappings pins the mappings that were already
// correct, so the binding fix cannot quietly move a 404, a 409 or a genuine
// server failure.
func TestRespondErrorKeepsKnownMappings(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		wantStatus int
		wantCode   int64
	}{
		{"memory not found", memorydao.ErrMemoryNotFound, http.StatusNotFound, code.MemoryNotFound.Code()},
		{"candidate not found", memorydao.ErrCandidateNotFound, http.StatusNotFound, code.MemoryCandidateNotFound.Code()},
		{"draft not found", memorydao.ErrDraftNotFound, http.StatusNotFound, code.MemoryDraftNotFound.Code()},
		{"dream conflict", memorydao.ErrDreamConflict, http.StatusConflict, code.MemoryConflict.Code()},
		{"dream busy", memorydao.ErrDreamBusy, http.StatusConflict, code.MemoryDreamBusy.Code()},
		{"derived read only", memorydao.ErrDerivedMemory, http.StatusConflict, code.MemoryDerivedReadOnly.Code()},
		{"invalid correction", memoryservice.ErrInvalidCorrection, http.StatusBadRequest, code.CodeInvalidParams.Code()},
		{"invalid explicit write", memoryservice.ErrInvalidExplicitWrite, http.StatusBadRequest, code.CodeInvalidParams.Code()},
		{"dream not configured", memoryservice.ErrDreamNotConfigured, http.StatusServiceUnavailable, code.CodeServerBusy.Code()},
		{"unknown failure", errors.New("storage is unreachable"), http.StatusInternalServerError, code.CodeServerBusy.Code()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			context, _ := gin.CreateTestContext(recorder)
			respondError(context, tc.err)

			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", recorder.Code, tc.wantStatus, recorder.Body.String())
			}
			var payload struct {
				StatusCode int64 `json:"status_code"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
				t.Fatalf("decode: %v body=%s", err, recorder.Body.String())
			}
			if payload.StatusCode != tc.wantCode {
				t.Fatalf("status_code = %d, want %d; body=%s", payload.StatusCode, tc.wantCode, recorder.Body.String())
			}
		})
	}
}
