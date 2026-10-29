package hybrid

import (
	dao "GopherAI/dao/knowledge"
	knowledge "GopherAI/service/knowledge"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// Presentation-only range selection after the original owner/version/file
// validation. It never fetches snippets/chunks or substitutes another version.
func (s *httpSurface) documentSource(w http.ResponseWriter, r *http.Request, uid int64, _ string) {
	query := r.URL.Query()
	for key, values := range query {
		if (key != "version" && key != "start_line" && key != "end_line") || len(values) != 1 {
			apiError(w, 400, "invalid_request", "invalid source query")
			return
		}
	}
	version, err := strconv.Atoi(query.Get("version"))
	if err != nil || version <= 0 {
		apiError(w, 400, "invalid_request", "explicit positive version is required")
		return
	}
	start, end := 1, 0
	if query.Has("start_line") || query.Has("end_line") {
		start, err = strconv.Atoi(query.Get("start_line"))
		if err != nil || start <= 0 {
			apiError(w, 400, "invalid_request", "invalid line range")
			return
		}
		end, err = strconv.Atoi(query.Get("end_line"))
		if err != nil || end < start {
			apiError(w, 400, "invalid_request", "invalid line range")
			return
		}
	}
	path := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/knowledge/documents/"), "/source")
	if path == "" || strings.Contains(path, "/") {
		apiError(w, 400, "invalid_request", "invalid document identity")
		return
	}
	source, err := s.agent.Documents.ReadActiveSource(r.Context(), uid, path, version)
	if err != nil {
		switch {
		case errors.Is(err, dao.ErrDocumentNotFound):
			apiError(w, 404, "not_found", "document not found")
		case errors.Is(err, knowledge.ErrDocumentVersionNotActive):
			apiError(w, 409, "document_version_not_active", "requested version is not active")
		case errors.Is(err, os.ErrNotExist):
			apiError(w, 404, "document_source_missing", "original source is unavailable")
		case errors.Is(err, knowledge.ErrDocumentSourceInvalid):
			apiError(w, 409, "document_source_invalid", "original source is invalid")
		case errors.Is(err, knowledge.ErrInvalidDocument):
			apiError(w, 400, "invalid_request", "invalid source request")
		default:
			apiError(w, 500, "internal_error", "original source cannot be read")
		}
		return
	}
	lines := strings.Split(source.Content, "\n")
	// Same terminal-newline convention as the original document_read reader.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if end == 0 {
		end = len(lines)
	}
	if start > len(lines) || end > len(lines) {
		apiError(w, 400, "invalid_request", "line range exceeds original source")
		return
	}
	selected := make([]map[string]any, 0, end-start+1)
	for n := start; n <= end; n++ {
		selected = append(selected, map[string]any{"number": n, "text": lines[n-1]})
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"document_id": source.DocumentID, "document_version": source.Version, "document_name": source.DocumentName, "start_line": start, "end_line": end, "lines": selected}, "error": nil})
}
