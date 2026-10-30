// Package metrics scores retrieval records against evidence sets.
//
// All measures work on lines, not chunks, so a result is judged by how much of the
// evidence it actually contains and different chunk sizes stay comparable. A line
// is a (file, line number) pair; the lines of several results are united, so an
// overlap between neighbouring chunks counts once.
//
// Two measures answer different questions and use different criteria:
//
//   - hit@K  asks "did the top K contain enough of the evidence?": the best
//     evidence set is covered to at least the hit threshold (default 0.5).
//   - MRR    asks "how early does the first useful result come?": the first
//     result that shares at least one line with any evidence set.
//
// A result can therefore count for MRR without the question being a hit, and the
// report states the two criteria separately.
package metrics

import (
	"github.com/xiaopang456/GopherLearn/eval/internal/dataset"
)

// Span is a retrieved interval, the same shape as an evidence span.
type Span = dataset.Span

// Classes of an answerable question in one cell.
const (
	// ClassNormal means the question is a hit at K.
	ClassNormal = "normal"
	// ClassMisranked means the fused candidate pool covered the evidence but the
	// final top K did not: the evidence was retrieved and then ranked out.
	ClassMisranked = "misranked"
	// ClassNotRecalled means not even the candidate pool covered the evidence.
	ClassNotRecalled = "not_recalled"
	// ClassError means the retrieval failed and produced nothing to score.
	ClassError = "error"
)

// Outcome is one answerable question's scores in one cell.
type Outcome struct {
	// Coverage is the best fraction of any evidence set's lines the results contain.
	Coverage float64
	// Hit is Coverage >= the hit threshold.
	Hit bool
	// Purity is the share of the retrieved lines that belong to the best set.
	Purity float64
	// ReciprocalRank is 1/rank of the first result sharing a line with any set, or 0.
	ReciprocalRank float64
	// PoolCoverage and PoolHit are Coverage and Hit for the fused candidate pool.
	PoolCoverage float64
	PoolHit      bool
	// Class is one of the Class constants.
	Class string
}

// retrievedLines is the union of the lines of the given spans.
func retrievedLines(spans []Span) map[dataset.LineRef]struct{} {
	return dataset.EvidenceSet(spans).Lines()
}

// bestSet returns the index of the evidence set the retrieved lines cover best,
// that coverage, and how many of that set's lines are covered. A tie goes to the
// set listed first, so the choice is a function of the data alone.
func bestSet(sets []dataset.EvidenceSet, retrieved map[dataset.LineRef]struct{}) (index int, coverage float64, covered int) {
	index = -1
	for i, set := range sets {
		gold := set.Lines()
		got := 0
		for line := range gold {
			if _, ok := retrieved[line]; ok {
				got++
			}
		}
		if c := float64(got) / float64(len(gold)); index < 0 || c > coverage {
			index, coverage, covered = i, c, got
		}
	}
	return index, coverage, covered
}

// Coverage returns the best coverage of any evidence set by the spans.
func Coverage(sets []dataset.EvidenceSet, spans []Span) float64 {
	_, c, _ := bestSet(sets, retrievedLines(spans))
	return c
}

// ReciprocalRank is 1/rank of the first span that shares at least one line with
// any evidence set, or 0 when none does. The spans are in rank order.
func ReciprocalRank(sets []dataset.EvidenceSet, spans []Span) float64 {
	gold := map[dataset.LineRef]struct{}{}
	for _, set := range sets {
		for line := range set.Lines() {
			gold[line] = struct{}{}
		}
	}
	for rank, span := range spans {
		for n := span.Start; n <= span.End; n++ {
			if _, ok := gold[dataset.LineRef{File: span.File, Line: n}]; ok {
				return 1 / float64(rank+1)
			}
		}
	}
	return 0
}

// Evaluate scores one answerable question. results is the cell's final top K in
// rank order and pool the fused candidate list before any rerank. failed marks a
// retrieval that returned an error: it scores zero and is classed ClassError, so
// a failure is neither dropped nor mistaken for a poor ranking.
func Evaluate(sets []dataset.EvidenceSet, results, pool []Span, failed bool, threshold float64) Outcome {
	if failed {
		return Outcome{Class: ClassError}
	}
	retrieved := retrievedLines(results)
	index, coverage, covered := bestSet(sets, retrieved)
	out := Outcome{Coverage: coverage, Hit: coverage >= threshold, ReciprocalRank: ReciprocalRank(sets, results)}
	if len(retrieved) > 0 && index >= 0 {
		out.Purity = float64(covered) / float64(len(retrieved))
	}
	out.PoolCoverage = Coverage(sets, pool)
	out.PoolHit = out.PoolCoverage >= threshold
	switch {
	case out.Hit:
		out.Class = ClassNormal
	case out.PoolHit:
		out.Class = ClassMisranked
	default:
		out.Class = ClassNotRecalled
	}
	return out
}
