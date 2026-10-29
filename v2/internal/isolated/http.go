package isolated

import (
	"net/http"
	"time"
)

const (
	httpReadHeaderTimeout = 5 * time.Second
	httpReadTimeout       = 30 * time.Second
	httpIdleTimeout       = 60 * time.Second
	httpMaxHeaderBytes    = 1 << 20
)

func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: httpReadHeaderTimeout,
		ReadTimeout:       httpReadTimeout,
		IdleTimeout:       httpIdleTimeout,
		MaxHeaderBytes:    httpMaxHeaderBytes,
		// WriteTimeout intentionally remains zero. Agent and legacy chat stream
		// SSE responses for longer than a fixed socket write deadline; their
		// model/tool work already has request-scoped deadlines and cancellation.
	}
}

// NewHTTPServer exposes the existing server settings to the hybrid entry.
func NewHTTPServer(addr string, handler http.Handler) *http.Server {
	return newHTTPServer(addr, handler)
}
