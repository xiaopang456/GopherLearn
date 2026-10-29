package router

import (
	"GopherAI/common/code"
	"GopherAI/controller"
	"mime"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const defaultMaxRequestBodyBytes int64 = 1 << 20

// limitRequestBody bounds ordinary API bodies before a controller parses
// them. Multipart knowledge uploads are deliberately delegated to the
// document handler, which has a separate configured file and multipart limit.
func limitRequestBody(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if maxBytes <= 0 || c.Request == nil || c.Request.Body == nil || isKnowledgeMultipartRequest(c.Request) {
			c.Next()
			return
		}
		if c.Request.ContentLength > maxBytes {
			response := new(controller.Response)
			c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, response.CodeOf(code.CodeInvalidParams))
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		c.Next()
	}
}

func isKnowledgeMultipartRequest(request *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "multipart/form-data") {
		return false
	}
	if request.Method == http.MethodPost && request.URL.Path == "/api/v1/knowledge/documents" {
		return true
	}
	return request.Method == http.MethodPut && strings.HasPrefix(request.URL.Path, "/api/v1/knowledge/documents/")
}
