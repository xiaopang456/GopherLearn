package httpapi

import (
	"github.com/xiaopang456/GopherLearn/internal/service"
	"io"
	"net/http"
)

// These forwarding seams retain the existing request, envelope and SSE helpers.
func DecodeOrdinaryRequest(w http.ResponseWriter, r *http.Request, value any) error {
	return decodeJSON(w, r, value)
}
func WriteOrdinaryEvent(w io.Writer, event service.StreamEvent) error { return writeSSE(w, event) }
func WriteOrdinaryError(w http.ResponseWriter, err error, operation string) {
	writeServiceError(w, err, operation)
}
func WriteOrdinaryEnvelope(w http.ResponseWriter, status int, data any) { writeJSON(w, status, data) }
