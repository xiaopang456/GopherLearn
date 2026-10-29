package episoderelation

import (
	"context"
	"fmt"

	"GopherAI/model"
)

// RelationWriter is the seam the Episode worker calls after a projection has
// landed and been indexed.
//
// It exists so service/episode does not have to know this package: the worker
// holds a two-method interface, and everything below - the candidate
// repository, the classifier, the governor, the transaction - is assembled in
// main and injected. An episode package that imported a relation DAO would
// have the write side depending on a decision layer that depends on it back.
//
// It is also where "off" is cheapest. When no writer is attached, or the
// generator is off, the worker's extra step is a nil check.
type RelationWriter struct {
	generator *Generator
	// projected turns a persisted Episode into the assertions this pass is
	// about. It is a function rather than a method so the worker never needs
	// the model's assertion projection either.
	projected func(model.EpisodeMemory) []NewAssertion
}

// NewRelationWriter builds the seam. A nil generator is refused here rather
// than at the first projection.
func NewRelationWriter(generator *Generator) (*RelationWriter, error) {
	if generator == nil {
		return nil, fmt.Errorf("the relation writer needs a generator")
	}
	return &RelationWriter{generator: generator, projected: AssertionsToClassify}, nil
}

// Mode reports what the writer will do, so the worker can skip the call
// entirely when nothing would happen.
func (w *RelationWriter) Mode() Mode {
	if w == nil || w.generator == nil {
		return ModeOff
	}
	return w.generator.Mode()
}

// DecidesRelations is the behavioural form of Mode for the worker, which
// cannot import this package's Mode type without closing an import cycle
// through its own test suite. False is exactly ModeOff: a writer that would
// decide nothing must not turn an index deferral into a relation retry.
func (w *RelationWriter) DecidesRelations() bool {
	return w.Mode() != ModeOff
}

// Process runs the relation pass for one freshly projected Episode.
//
// Its error contract is the worker's: a returned error means the relation
// stage did not complete and may be retried, and the worker must not record
// the projection as finished on the strength of it. A pair that could not be
// classified is not an error - it is uncertain, recorded, and the projection
// stands.
func (w *RelationWriter) Process(ctx context.Context, episode *model.EpisodeMemory) error {
	if w == nil || w.generator == nil || w.generator.Mode() == ModeOff {
		return nil
	}
	if episode == nil {
		return nil
	}
	assertions := w.projected(*episode)
	if len(assertions) == 0 {
		return nil
	}
	if _, err := w.generator.Generate(ctx, assertions); err != nil {
		return fmt.Errorf("episode relation pass: %w", err)
	}
	return nil
}

// AssertionsToClassify turns a persisted Episode into the assertions a
// relation pass is about: its decisions and corrections, with the identity and
// the time the production projection issued.
//
// Synopsis and open loops are excluded here as well as in the candidate
// filter. A synopsis asserts nothing; an open loop is resolved or cancelled,
// never replaced, and giving it a relation would record a decision the learner
// did not make.
func AssertionsToClassify(episode model.EpisodeMemory) []NewAssertion {
	out := []NewAssertion{}
	for _, assertion := range model.AssertionsOf(episode) {
		switch assertion.Kind {
		case model.AssertionDecision, model.AssertionCorrection:
		default:
			continue
		}
		out = append(out, NewAssertion{
			AssertionID: assertion.ID, EpisodeID: episode.ID,
			SessionID: episode.SessionID, UserID: episode.UserID,
			Kind: assertion.Kind, Text: assertion.Text,
			OccurredAt: assertion.OccurredAt, TimeKnown: assertion.TimeKnown,
		})
	}
	return out
}
