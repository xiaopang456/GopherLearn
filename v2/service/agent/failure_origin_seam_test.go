package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
)

// STABILIZE-062 v1.4, the production seams.
//
// The round before this one gave the evaluator a structural classifier and then
// discovered its real run had three turns it still could not classify, because
// nothing in production had ever given those errors a type. A classifier can
// only read categories somebody wrote down. These tests are about the writing
// down: a provider fault typed at the provider boundary, a tool fault typed at
// the tool boundary, and - the part that is easy to get wrong - nothing typed
// twice.

func TestProviderErrorsAreTypedAtTheModelBoundary(t *testing.T) {
	transport := errors.New("connection reset by peer")
	typed := TypeProviderFailure(transport)
	if !errors.Is(typed, ErrProviderTransport) {
		t.Fatalf("a raw transport error must carry the provider origin")
	}
	if !errors.Is(typed, transport) {
		t.Fatalf("the cause must stay reachable for a person debugging locally")
	}
	if got := ClassifyRunFailure(nil, typed); got != RunFailureProviderTransport {
		t.Fatalf("classified as %q, want provider_transport", got)
	}
}

func TestToolErrorsAreTypedAtTheToolBoundary(t *testing.T) {
	broken := errors.New("dial tcp 127.0.0.1:6379: refused")
	typed := TypeToolFailure(broken)
	if !errors.Is(typed, ErrToolInfrastructure) {
		t.Fatalf("a tool that failed while running must carry the tool origin")
	}
	if got := ClassifyRunFailure(nil, typed); got != RunFailureToolInfrastructure {
		t.Fatalf("classified as %q, want tool_infrastructure", got)
	}
}

func TestTheFirstTypingWins(t *testing.T) {
	// A tool that hits the run's own fuse returns a sentinel. Wrapping that on
	// the way out would turn the fuse - a method observation - into an
	// infrastructure fault, and the round would void on its own correctness.
	fused := fmt.Errorf("the draft tool: %w", ErrToolCallLimit)
	if got := TypeToolFailure(fused); !errors.Is(got, ErrToolCallLimit) {
		t.Fatalf("the sentinel must survive")
	}
	if errors.Is(TypeToolFailure(fused), ErrToolInfrastructure) {
		t.Fatalf("a fuse must not be re-typed as an infrastructure fault")
	}
	// And a fault already typed at the store keeps the store's category rather
	// than picking up the tool's on the way through it.
	store := WrapRetrievalInfrastructureFailure(errors.New("read timeout"))
	if errors.Is(TypeToolFailure(store), ErrToolInfrastructure) {
		t.Fatalf("a store fault must not become a tool fault three frames later")
	}
}

func TestContextOutcomesAreNeverRetyped(t *testing.T) {
	for name, err := range map[string]error{
		"deadline": context.DeadlineExceeded,
		"cancel":   context.Canceled,
		"eof":      io.EOF,
	} {
		if typed := TypeProviderFailure(err); !errors.Is(typed, err) ||
			errors.Is(typed, ErrProviderTransport) {
			t.Fatalf("%s must stay itself: a turn that ran out of time did not "+
				"have a transport fault", name)
		}
	}
}

func TestTypingIsIdempotent(t *testing.T) {
	once := TypeProviderFailure(errors.New("upstream 502"))
	twice := TypeProviderFailure(once)
	if once.Error() != twice.Error() {
		t.Fatalf("typing an already typed error changed it: %q then %q",
			once.Error(), twice.Error())
	}
}

func TestEveryOriginEnvelopeHasExactlyOneKind(t *testing.T) {
	seen := map[RunFailureKind]error{}
	for _, envelope := range []error{ErrProviderTransport, ErrRetrievalInfrastructure,
		ErrToolInfrastructure, ErrGroundingRejected} {
		kind := ClassifyRunFailure(nil, envelope)
		if kind == RunFailureUnknown {
			t.Fatalf("%v classifies as unknown, so wrapping it buys nothing", envelope)
		}
		if other, clash := seen[kind]; clash {
			t.Fatalf("%v and %v both classify as %q", envelope, other, kind)
		}
		seen[kind] = envelope
	}
}

func TestAnUntypedErrorIsNotQuietlyGivenAnOrigin(t *testing.T) {
	// The failure mode this round exists to remove is a bucket that looks like
	// an answer. An error nobody typed has to say so.
	if got := ClassifyRunFailure(nil, errors.New("something went wrong")); got != RunFailureUnknown {
		t.Fatalf("an untyped error classified as %q; it must be unknown_origin", got)
	}
	if RunFailureUnknown.Origin() != OriginUnknown {
		t.Fatalf("unknown_origin must not resolve to a real origin")
	}
}
