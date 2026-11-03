package agent

import (
	"context"

	"GopherAI/common/rag"
)

// The server-side pre-retrieval.
//
// A model cannot tell whether a question is about the learner's own material
// unless it has seen that material, and it cannot see it without asking for it.
// So the server answers the question first, cheaply, and tells the model what it
// found. The model then decides: answer from general knowledge, or spend a tool
// round on a search that the server has already shown is worth doing.
//
// Two rules keep this honest:
//
//  1. A pre-retrieval that found nothing is reported as "this retrieval found
//     nothing close", never as "your notes have nothing to say about this".
//     The second sentence would stop the model from searching when the
//     pre-retrieval was wrong, and a wrong pre-retrieval that stops a needed
//     search is worse than no pre-retrieval at all.
//  2. The evidence it hands over is the same evidence a search would have
//     returned: same chunks, same citation numbers, same documents. A model
//     that cites it cites real material, and the final citation check resolves
//     it like any other.
//
// It is a relevance signal, not a decision. The model still decides.

// RelevanceStatus is what the pre-retrieval concluded.
type RelevanceStatus string

const (
	// RelevanceHit means the pre-retrieval found material that scored at or
	// above the threshold. The evidence is handed to the model and the search
	// tool stays available, so a model that wants more can look for more.
	RelevanceHit RelevanceStatus = "hit"
	// RelevanceBelowThreshold means the pre-retrieval ran and nothing scored
	// above the threshold. It says what happened to this retrieval and nothing
	// else: the material may well contain an answer this one query missed.
	RelevanceBelowThreshold RelevanceStatus = "below_threshold"
	// RelevanceUnavailable means the pre-retrieval could not run - the index
	// was down, the embedder refused, the reranker timed out. It is not a
	// statement about the material, and the model is told so.
	RelevanceUnavailable RelevanceStatus = "unavailable"
)

// RelevanceChunk is one chunk of evidence the pre-retrieval hands over.
//
// The search tool assigns its own citation numbers, and it starts at 1. A
// pre-retrieval that also numbered from 1 would hand the model two different
// chunks under the same number, and the final citation check would resolve [1]
// to whichever of them it saw last. So the pre-retrieval's numbers are carried
// forward here, and the search tool continues from where they stop.
type RelevanceChunk struct {
	// Citation is the number the model sees.
	Citation int
	// Identity is the retrieval's own identity for the chunk: document,
	// version and chunk id. The search tool computes the same string and uses
	// it to recognise a chunk it has already shown.
	Identity string
	// DocID and DocVersion name the document the chunk belongs to. The search
	// tool records them as eligible for a follow-up read.
	DocID      string
	DocVersion int
	// Source is the citation-shaped view of the chunk: the same one a search
	// result would produce.
	Source rag.Source
	// Content is the text handed to the model.
	Content string
}

// RelevanceOutcome is the result of one server-side pre-retrieval.
type RelevanceOutcome struct {
	// Status is what the pre-retrieval concluded.
	Status RelevanceStatus
	// Threshold is the score the retrieval was compared against.
	Threshold float64
	// TopScore is the best score this retrieval produced.
	TopScore float64
	// RerankApplied says whether the score came from the reranker. A score
	// from the vector channel alone is not comparable with a rerank score, and
	// the threshold is written against rerank scores.
	RerankApplied bool
	// Chunks are the pieces of evidence handed to the model, already numbered.
	Chunks []RelevanceChunk
	// NextCitation is the number the search tool continues from.
	NextCitation int
}

// Available reports whether the pre-retrieval produced evidence the model can
// cite. A pre-retrieval that found nothing produces no chunks: there is
// nothing to cite and nothing for the search tool to continue from.
func (o RelevanceOutcome) Available() bool {
	return o.Status == RelevanceHit && len(o.Chunks) > 0
}

// Sources is the citation-shaped view of the chunks, in the order the model
// was given them.
func (o RelevanceOutcome) Sources() []rag.Source {
	if len(o.Chunks) == 0 {
		return nil
	}
	sources := make([]rag.Source, 0, len(o.Chunks))
	for _, chunk := range o.Chunks {
		sources = append(sources, chunk.Source)
	}
	return sources
}

// RelevanceProber is the optional capability a tool registry may implement.
//
// It is optional on purpose. A registry that does not implement it simply has
// no pre-retrieval, and the run proceeds exactly as it did before the
// pre-retrieval existed - which is what every test fixture and every mode that
// registers no document tools gets.
type RelevanceProber interface {
	// ProbeRelevance runs one retrieval for the question and reports whether
	// the learner's material covers it. The threshold is the score above which
	// the evidence is handed over.
	ProbeRelevance(ctx context.Context, run RunContext, question string, threshold float64) (RelevanceOutcome, error)
}

// DefaultRelevanceThreshold is the rerank score above which the server-side
// pre-retrieval hands its evidence to the model.
//
// It is a starting point, not a calibrated number. The rerank score separates
// answerable from unanswerable questions almost completely on the retrieval
// layer's own measurement - 0.937 for the median answerable question against a
// 0.733 maximum for the unanswerable ones - so 0.80 sits between them with room
// on both sides. The comparison run is what turns it into a threshold that was
// measured on this product's own traffic, which is why it is a configuration
// value rather than a constant.
const DefaultRelevanceThreshold = 0.80
