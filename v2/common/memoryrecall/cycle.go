// Package memoryrecall carries the per-request coordination state that the two
// long-term memory readers share.
//
// Episode recall and TopicKey resolution ask the same question of the same
// embedding provider. Before STABILIZE-030 they could not know about each
// other, so a mixed question that needed both paid for two identical embedding
// calls — or, worse, one of them was switched off to save the call and lost the
// channel it needed. A Cycle is created once per Agent turn, handed to both
// readers, and thrown away with the request.
//
// It is deliberately not a cache. There is no eviction policy, no key derived
// from user text, and no lifetime beyond the request that created it: a global
// cache keyed on a personal question is a cross-user leak waiting to happen,
// and this type cannot become one because nothing outside one request can
// reach it.
package memoryrecall

import (
	"context"
	"strings"
	"sync"
)

// EmbedFunc is the provider call a Cycle memoises.
type EmbedFunc func(context.Context, string) ([]float32, error)

// Cycle is one request's shared recall state.
//
// Every method is safe on a nil receiver: callers that were built without a
// cycle (focused tests, maintenance commands) must keep working, and they do so
// by falling straight through to the provider.
type Cycle struct {
	mu sync.Mutex
	// question is the exact text the memoised vector belongs to. A second
	// reader asking about different text gets its own call rather than the
	// wrong vector.
	question string

	vectorDone bool
	vector     []float32
	vectorErr  error

	embeddingCalls int

	episodeTopicKeys []string
	proposedTopics   []string
	proposedDone     bool

	reasons []string
}

// New returns a Cycle bound to one question.
func New(question string) *Cycle {
	return &Cycle{question: strings.TrimSpace(question)}
}

// QueryVector returns the request's query vector, calling embed at most once.
//
// The second caller in a request receives the first caller's vector, including
// its failure: an embedding provider that just refused this question will
// refuse it again, and retrying inside one turn only spends budget and latency
// to reach the same answer.
func (c *Cycle) QueryVector(ctx context.Context, text string, embed EmbedFunc) ([]float32, error) {
	if embed == nil {
		return nil, errNoEmbedder
	}
	if c == nil {
		return embed(ctx, text)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if strings.TrimSpace(text) != c.question {
		// Not the request's question. Reusing the memoised vector here would
		// silently answer a different query, so this call is neither served
		// from nor written to the cycle.
		c.embeddingCalls++
		return embed(ctx, text)
	}
	if c.vectorDone {
		return c.vector, c.vectorErr
	}
	c.vectorDone = true
	c.embeddingCalls++
	c.vector, c.vectorErr = embed(ctx, text)
	return c.vector, c.vectorErr
}

// VectorAttempted reports whether the request's query vector has already been
// requested, successfully or not.
func (c *Cycle) VectorAttempted() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.vectorDone
}

// EmbeddingCalls is how many provider calls this request actually made. It is
// the number the "at most one added embedding per question" gate is measured
// on, so it counts failures too.
func (c *Cycle) EmbeddingCalls() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.embeddingCalls
}

// SetProposedTopicKeys records the TopicKeys the resolver proposed for this
// request so a later reader uses the same answer instead of resolving again.
func (c *Cycle) SetProposedTopicKeys(keys []string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.proposedDone = true
	c.proposedTopics = append([]string(nil), keys...)
}

// ProposedTopicKeys returns the recorded proposal and whether one was made.
func (c *Cycle) ProposedTopicKeys() ([]string, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.proposedTopics...), c.proposedDone
}

// SetEpisodeTopicKeys records the TopicKeys of the episodes that survived the
// MySQL reload. They are authoritative: the server selected those episodes, so
// their keys need no semantic confirmation.
func (c *Cycle) SetEpisodeTopicKeys(keys []string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.episodeTopicKeys = append([]string(nil), keys...)
}

// EpisodeTopicKeys returns the recorded authoritative keys.
func (c *Cycle) EpisodeTopicKeys() []string {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.episodeTopicKeys...)
}

// Reason appends a diagnostic reason code. Codes never carry user text.
func (c *Cycle) Reason(codes ...string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reasons = append(c.reasons, codes...)
}

// Reasons returns the reason codes recorded for this request.
func (c *Cycle) Reasons() []string {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.reasons...)
}

type embedderError string

func (e embedderError) Error() string { return string(e) }

const errNoEmbedder = embedderError("memory recall cycle has no embedder")
