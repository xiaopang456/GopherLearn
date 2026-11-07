package httpapi

import (
	"github.com/xiaopang456/GopherLearn/internal/service"
	"io"
	"net/http"
)

// These forwarding seams expose the request, envelope and SSE helpers to the
// packages outside this one that answer over the same wire format.
func DecodeRequest(w http.ResponseWriter, r *http.Request, value any) error {
	return decodeJSON(w, r, value)
}
func WriteEvent(w io.Writer, event service.StreamEvent) error { return writeSSE(w, event) }
func WriteError(w http.ResponseWriter, err error, operation string) {
	writeServiceError(w, err, operation)
}
func WriteEnvelope(w http.ResponseWriter, status int, data any) { writeJSON(w, status, data) }
