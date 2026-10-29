package retrieval

import (
	"github.com/xiaopang456/GopherLearn/internal/domain"
	"sort"
)

// Reciprocal-rank fusion is one behavior with two callers: the deterministic
// Pipeline, where every ranking comes from an in-process Scorer, and the semantic
// Hybrid retriever, where the vector ranking comes from a persisted index instead.
//
// It is extracted rather than reimplemented because the fused numbers are pinned by
// tests: the rank is counted over admitted candidates only, the head of each ranking
// is cut at the requested width, and equal fused scores break by stored order. A
// second copy would drift in exactly those three places and the drift would look
// like a retrieval-quality change.

// ranking is one scorer's output together with the admission rule that produced it.
// Admission travels with the ranking because the cut is expressed in the scorer's own
// units and the fusion code must never hold a threshold of its own.
type ranking struct {
	name   string
	scored []Scored
	admit  func(float64) bool
}

// fuse merges rankings into results. It returns the ordered results and how many
// admissions were counted, so the caller can distinguish "nothing matched" from "one
// chunk matched".
func fuse(rrK int, head int, byID map[string]domain.DocumentChunk, rankings []ranking) ([]Result, int) {
	fused := make(map[string]*Result, len(byID))
	admitted := 0
	for _, single := range rankings {
		rank := 0
		for _, scored := range orderDescending(single.scored) {
			if single.admit != nil && !single.admit(scored.Score) {
				continue
			}
			if rank == head {
				break
			}
			chunk, known := byID[scored.Chunk.ID]
			if !known {
				// A ranking may not introduce a candidate the scope did not supply:
				// that is how a stale index would leak a chunk across collections.
				continue
			}
			rank++
			admitted++
			result := fused[chunk.ID]
			if result == nil {
				result = &Result{Chunk: chunk}
				fused[chunk.ID] = result
			}
			result.Score += 1 / float64(rrK+rank)
			result.Contributors = append(result.Contributors, single.name)
		}
	}
	results := make([]Result, 0, len(fused))
	for _, result := range fused {
		results = append(results, *result)
	}
	return results, admitted
}

// sortedCapped orders fused results deterministically and cuts to the requested
// width. Identical fused scores are broken by stored order, never by map iteration
// order, so the same inputs always give the same top-k.
func sortedCapped(results []Result, topK int) []Result {
	ordered := make([]Result, len(results))
	copy(ordered, results)
	sort.Slice(ordered, func(left, right int) bool {
		if ordered[left].Score != ordered[right].Score {
			return ordered[left].Score > ordered[right].Score
		}
		if ordered[left].Chunk.Order != ordered[right].Chunk.Order {
			return ordered[left].Chunk.Order < ordered[right].Chunk.Order
		}
		return ordered[left].Chunk.ID < ordered[right].Chunk.ID
	})
	if topK > 0 && len(ordered) > topK {
		ordered = ordered[:topK]
	}
	return ordered
}
