package providerretry

import (
	"context"
	"errors"
	"net"
	"net/url"
	"syscall"
	"testing"
	"time"

	einoopenai "github.com/cloudwego/eino-ext/components/model/openai"
	goopenai "github.com/meguminnnnnnnnn/go-openai"
)

func TestRetryableAcceptsOnlyTheStatusesThatMeanNotProcessed(t *testing.T) {
	for _, status := range []int{502, 503, 504} {
		if !Retryable(&einoopenai.APIError{HTTPStatusCode: status}) {
			t.Fatalf("status %d must be retried", status)
		}
		if !Retryable(&goopenai.RequestError{HTTPStatusCode: status}) {
			t.Fatalf("status %d in a request error must be retried", status)
		}
	}
	for _, status := range []int{400, 401, 403, 404, 409, 422, 429, 500, 501} {
		if Retryable(&einoopenai.APIError{HTTPStatusCode: status}) {
			t.Fatalf("status %d must not be retried", status)
		}
		if Retryable(&goopenai.RequestError{HTTPStatusCode: status}) {
			t.Fatalf("status %d in a request error must not be retried", status)
		}
	}
}

func TestRetryableRejectsTheErrorsThatAreNotProviderFailures(t *testing.T) {
	if Retryable(nil) {
		t.Fatal("no error is nothing to retry")
	}
	if Retryable(errors.New("something else")) {
		t.Fatal("an error that is not a provider failure must not be retried")
	}
	if Retryable(context.Canceled) {
		t.Fatal("a cancelled context must not be retried")
	}
	if Retryable(context.DeadlineExceeded) {
		t.Fatal("an expired context must not be retried")
	}
}

func TestRetryableAcceptsAConnectionThatFailedBeforeTheProviderAnswered(t *testing.T) {
	refused := &url.Error{Op: "Post", URL: "https://provider.invalid", Err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}}
	if !Retryable(refused) {
		t.Fatal("a refused connection must be retried")
	}
	reset := &url.Error{Op: "Post", URL: "https://provider.invalid", Err: &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}}
	if !Retryable(reset) {
		t.Fatal("a connection that was cut must be retried")
	}
}

func TestStatusReadsTheStatusBehindTheError(t *testing.T) {
	if status, ok := Status(&einoopenai.APIError{HTTPStatusCode: 503}); !ok || status != 503 {
		t.Fatalf("the status must be read from the model error, got %d %v", status, ok)
	}
	if status, ok := Status(&goopenai.RequestError{HTTPStatusCode: 502}); !ok || status != 502 {
		t.Fatalf("the status must be read from the request error, got %d %v", status, ok)
	}
	if _, ok := Status(refusedError()); ok {
		t.Fatal("a connection failure carries no status")
	}
}

func TestReasonNamesTheFailureWithoutProviderText(t *testing.T) {
	if reason := Reason(&einoopenai.APIError{HTTPStatusCode: 503, Message: "internal gateway error"}); reason != "status_503" {
		t.Fatalf("the reason must be the status, got %q", reason)
	}
	if reason := Reason(refusedError()); reason != "transport" {
		t.Fatalf("the reason must be the transport label, got %q", reason)
	}
	if reason := Reason(errors.New("something else")); reason != "other" {
		t.Fatalf("an unknown failure must have a bounded reason, got %q", reason)
	}
}

func TestWaitReturnsWhenTheContextEnds(t *testing.T) {
	if !Wait(context.Background(), time.Millisecond) {
		t.Fatal("the wait must finish when the pause is over")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if Wait(ctx, time.Hour) {
		t.Fatal("a cancelled context must end the wait at once")
	}
}

func refusedError() error {
	return &url.Error{Op: "Post", URL: "https://provider.invalid", Err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}}
}
