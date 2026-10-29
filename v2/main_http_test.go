package main

import (
	"net/http"
	"testing"
)

func TestHTTPServerHasResourceTimeoutsWithoutBreakingSSE(t *testing.T) {
	server := newHTTPServer("127.0.0.1:0", http.NewServeMux())
	if server.ReadHeaderTimeout != httpReadHeaderTimeout || server.ReadTimeout != httpReadTimeout || server.IdleTimeout != httpIdleTimeout {
		t.Fatalf("unexpected HTTP timeouts: header=%v read=%v idle=%v", server.ReadHeaderTimeout, server.ReadTimeout, server.IdleTimeout)
	}
	if server.MaxHeaderBytes != httpMaxHeaderBytes {
		t.Fatalf("MaxHeaderBytes = %d, want %d", server.MaxHeaderBytes, httpMaxHeaderBytes)
	}
	if server.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout = %v, want zero for SSE", server.WriteTimeout)
	}
}
