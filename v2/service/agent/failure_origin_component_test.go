package agent

import (
	"errors"
	"testing"
)

// The component envelope is the structured sub-origin of an infrastructure
// failure. It is typed at the seam that produced the error, the first typing
// wins, and it must compose with the origin envelopes: an embedding fault is
// still tool infrastructure to the gates, and still says which component broke.

func TestWrapAndExtractComponent(t *testing.T) {
	cause := errors.New("the request failed")
	wrapped := WrapComponentFailure(cause, ComponentEmbeddingProvider)
	component, ok := ComponentOf(wrapped)
	if !ok || component != ComponentEmbeddingProvider {
		t.Fatalf("component came back %q (found=%v), want embedding_provider", component, ok)
	}
	// The cause stays reachable for a local reader; nothing here carries its
	// text into an artefact, which is a property of the record, not this test.
	if !errors.Is(wrapped, cause) {
		t.Fatal("the component envelope lost the cause")
	}
}

func TestFirstComponentTypingWins(t *testing.T) {
	wrapped := WrapComponentFailure(errors.New("x"), ComponentEmbeddingProvider)
	rewrapped := WrapComponentFailure(wrapped, ComponentRetrievalStore)
	component, ok := ComponentOf(rewrapped)
	if !ok || component != ComponentEmbeddingProvider {
		t.Fatalf("re-typing overwrote the first component: %q", component)
	}
}

func TestComponentNoneAndNilAreNoOps(t *testing.T) {
	err := errors.New("plain")
	if got := WrapComponentFailure(err, ComponentNone); got != err {
		t.Fatal("wrapping with no component changed the error")
	}
	if got := WrapComponentFailure(nil, ComponentEmbeddingProvider); got != nil {
		t.Fatal("wrapping a nil error produced one")
	}
	if _, ok := ComponentOf(err); ok {
		t.Fatal("an untyped error reported a component")
	}
	if _, ok := ComponentOf(nil); ok {
		t.Fatal("a nil error reported a component")
	}
}

func TestComponentComposesWithOriginTyping(t *testing.T) {
	// The seam order this design produces: the component is typed first, at the
	// failure, and the tool boundary's origin typing wraps it on the way out.
	// Both must stay readable, because "tool_infrastructure" alone is exactly
	// the bucket that cost v1.4.1 its diagnosis.
	wrapped := TypeToolFailure(WrapComponentFailure(
		errors.New("expansion failed"), ComponentGovernedExpansion))
	if !errors.Is(wrapped, ErrToolInfrastructure) {
		t.Fatal("the component envelope suppressed the origin typing")
	}
	component, ok := ComponentOf(wrapped)
	if !ok || component != ComponentGovernedExpansion {
		t.Fatalf("origin typing buried the component: %q (found=%v)", component, ok)
	}
	// And an origin-typed error passed through a component wrap keeps its
	// origin: the first origin still wins.
	twice := WrapComponentFailure(TypeProviderFailure(errors.New("down")), ComponentRetrievalStore)
	if !errors.Is(twice, ErrProviderTransport) {
		t.Fatal("a component wrap re-typed an origin-typed error")
	}
}
