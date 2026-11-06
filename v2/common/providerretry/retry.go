// Package providerretry decides whether a failed model call may be sent to
// the provider again.
//
// The rule is deliberately narrow. A request may be repeated only when the
// provider never processed it: the upstream answered 502, 503 or 504, or the
// connection failed before the response was complete. A rejected request
// (400, 401, 403), a rate-limited request (429) and a stream that already
// produced content are not repeated - the second attempt would fail the same
// way, or would repeat content the user has already seen.
//
// The package holds no state and talks to no provider: it reads an error and
// answers a question about it.
package providerretry

import (
	"context"
	"errors"
	"net"
	"strconv"
	"syscall"
	"time"

	einoopenai "github.com/cloudwego/eino-ext/components/model/openai"
	goopenai "github.com/meguminnnnnnnnn/go-openai"
)

// retryableStatuses are the upstream statuses that mean "not processed".
//
// They are the statuses a gateway answers with when it could not reach the
// upstream, which is the case where sending the same request again can
// succeed.
var retryableStatuses = map[int]bool{
	502: true,
	503: true,
	504: true,
}

// Retryable reports whether err is a provider failure that may be sent again.
//
// The caller is responsible for the rest of the decision - whether the turn
// still has time left and whether the model-call budget still has room -
// because only the caller knows its own budget.
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	if status, ok := Status(err); ok {
		return retryableStatuses[status]
	}
	return transportFailure(err)
}

// Status returns the HTTP status the provider answered with, when it answered
// with one.
//
// The status is read from the error the chat model returned. The eino model
// converts the upstream client error into its own type and keeps the status;
// an upstream error whose body was not JSON arrives as the upstream client's
// own request error, which keeps the status too.
func Status(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	var apiErr *einoopenai.APIError
	if errors.As(err, &apiErr) && apiErr.HTTPStatusCode > 0 {
		return apiErr.HTTPStatusCode, true
	}
	var requestErr *goopenai.RequestError
	if errors.As(err, &requestErr) && requestErr.HTTPStatusCode > 0 {
		return requestErr.HTTPStatusCode, true
	}
	return 0, false
}

// Reason names why a call failed, for a step summary and a log line.
//
// It is a bounded label, never provider text: a status the provider answered
// with, or "transport" for a connection that failed.
func Reason(err error) string {
	if status, ok := Status(err); ok {
		return "status_" + strconv.Itoa(status)
	}
	if transportFailure(err) {
		return "transport"
	}
	return "other"
}

// transportFailure reports whether err is a connection that failed before the
// provider answered.
//
// A cancelled or expired context is not a transport failure: the turn's own
// budget is what stopped the call, and sending it again would only spend
// more of a budget that is already gone.
func transportFailure(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var opErr *net.OpError
	if !errors.As(err, &opErr) {
		return false
	}
	// A dial failure never reached the provider: refused, unreachable, or a
	// connection that could not be established in time.
	if opErr.Op == "dial" {
		return true
	}
	// A connection that was cut while the response was in flight.
	return errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE)
}

// Wait waits for the pause before a second attempt.
//
// It returns false when the context ended during the pause: a retry that
// starts after the turn gave up is not worth sending, and the caller keeps the
// original error in that case.
func Wait(ctx context.Context, pause time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	if pause <= 0 {
		return true
	}
	timer := time.NewTimer(pause)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
