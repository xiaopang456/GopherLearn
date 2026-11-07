// Package httpwire exposes this module's HTTP wire format to the integrated
// entry, which is a separate module and therefore cannot reach the internal
// package that owns the format.
//
// The format is one implementation: the request reader refuses an oversized
// body, a body that is not valid UTF-8, an unknown field and a trailing
// document, and the envelope writer is the one every answer on this format is
// written with. A second copy of any of that in another module would be a
// second answer to the same question, and the two would drift.
package httpwire

import (
	"github.com/xiaopang456/GopherLearn/internal/httpapi"
	"net/http"
)

// DecodeRequest reads one JSON request body into value and refuses a body
// that is too large, is not valid UTF-8, names a field the receiver does not
// know, or carries a second document.
func DecodeRequest(w http.ResponseWriter, r *http.Request, value any) error {
	return httpapi.DecodeRequest(w, r, value)
}

// WriteEnvelope writes the successful answer in this module's envelope.
func WriteEnvelope(w http.ResponseWriter, status int, data any) {
	httpapi.WriteEnvelope(w, status, data)
}

// WriteError writes a refused request in this module's envelope. The operation
// names the request that failed, because two operations answer 401 differently.
func WriteError(w http.ResponseWriter, err error, operation string) {
	httpapi.WriteError(w, err, operation)
}
