package rag

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"GopherAI/config"
)

// TestOperatorMessageNamesEveryFailureKind proves each failure an operator can
// meet has its own sentence, so a startup failure is actionable rather than a
// refusal.
func TestOperatorMessageNamesEveryFailureKind(t *testing.T) {
	for _, test := range []struct {
		name     string
		err      error
		contains string
	}{
		{"the configuration is wrong", fmt.Errorf("start: %w", config.ErrRerankConfig), "RAG_RERANK_AUTH_TOKEN"},
		{"the credential is refused", fmt.Errorf("start: %w", ErrRerankRejected), "RAG_RERANK_AUTH_TOKEN"},
		{"the service throttles", fmt.Errorf("start: %w", ErrRerankRateLimited), "rate"},
		{"the service is too slow", fmt.Errorf("start: %w", ErrRerankTimeout), "rerankTimeoutMs"},
		{"the service serves another model", fmt.Errorf("start: %w", ErrRerankIdentity), "model"},
		{"the client cannot be built", fmt.Errorf("start: %w", ErrRerankConfig), "rerankProvider"},
		{"the service is unreachable", fmt.Errorf("start: %w", ErrRerankUnavailable), "unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			message := OperatorMessage(test.err)
			if message == "" {
				t.Fatal("expected an operator message")
			}
			if !strings.Contains(strings.ToLower(message), strings.ToLower(test.contains)) {
				t.Fatalf("message %q does not mention %q", message, test.contains)
			}
		})
	}
}

// TestOperatorMessageStaysQuietAboutOtherFailures proves the entry keeps its
// generic refusal for everything that is not about the reranker.
func TestOperatorMessageStaysQuietAboutOtherFailures(t *testing.T) {
	if message := OperatorMessage(nil); message != "" {
		t.Fatalf("message = %q, want empty", message)
	}
	if message := OperatorMessage(errors.New("dial tcp: connection refused")); message != "" {
		t.Fatalf("message = %q, want empty", message)
	}
}

// TestOperatorMessageNeverQuotesAConfigurationValue is the property that lets
// the entry print these sentences: a startup error usually reaches a console
// or a log, and a configuration value in it is a leak.
func TestOperatorMessageNeverQuotesAConfigurationValue(t *testing.T) {
	const token = "super-secret-rerank-token-value"
	const endpoint = "https://rerank.example.invalid/v1/rerank"
	for _, err := range []error{
		fmt.Errorf("%w: %s", config.ErrRerankConfig, token),
		fmt.Errorf("%w: %s", ErrRerankRejected, token),
		fmt.Errorf("%w: %s", ErrRerankRateLimited, endpoint),
		fmt.Errorf("%w: %s", ErrRerankTimeout, endpoint),
		fmt.Errorf("%w: %s", ErrRerankIdentity, endpoint),
		fmt.Errorf("%w: %s", ErrRerankConfig, token),
		fmt.Errorf("%w: %s", ErrRerankUnavailable, endpoint),
	} {
		message := OperatorMessage(err)
		if strings.Contains(message, token) || strings.Contains(message, endpoint) {
			t.Fatalf("message %q quotes a configuration value", message)
		}
	}
}
