package conversation

import (
	"context"
	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/httpapi"
	"io"
	"net/http"
)

func DecodeRequest(w http.ResponseWriter, r *http.Request, value any) error {
	return httpapi.DecodeOrdinaryRequest(w, r, value)
}
func WriteEvent(w io.Writer, event Event) error { return httpapi.WriteOrdinaryEvent(w, event) }
func WriteError(w http.ResponseWriter, err error, operation string) {
	httpapi.WriteOrdinaryError(w, err, operation)
}
func WriteEnvelope(w http.ResponseWriter, status int, data any) {
	httpapi.WriteOrdinaryEnvelope(w, status, data)
}
func (s *Service) ValidateStream(ctx context.Context, userID, sessionID, content string) error {
	return s.inner.ValidateStreamTurn(ctx, userID, sessionID, content, domain.KnowledgeOptions{}, false)
}
