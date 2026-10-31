package validate

import (
	"fmt"

	"github.com/xiaopang456/GopherLearn/eval/internal/dataset"
)

// TrimRecord is one evidence span that the trimmer shortened.
//
// The record is the audit trail: every line the program removed is written
// down, so a reviewer can see what happened to the evidence ranges without
// diffing the question file by hand.
type TrimRecord struct {
	ID          string `json:"id"`
	File        string `json:"file"`
	BeforeStart int    `json:"before_start"`
	BeforeEnd   int    `json:"before_end"`
	AfterStart  int    `json:"after_start"`
	AfterEnd    int    `json:"after_end"`
	// TrimmedLines are the line numbers that were removed.
	TrimmedLines []int `json:"trimmed_lines"`
	// Empty is true when the span held nothing but headings and blank lines.
	// The span is then left as it was, so the validator reports it as an
	// error instead of the trimmer quietly swallowing it.
	Empty bool `json:"empty,omitempty"`
}

// TrimEvidence removes the heading and blank lines at the edges of every
// evidence span.
//
// A model that points at a range of lines tends to include the heading above
// the paragraph or the blank line after it. Those lines carry no content, so
// they are cut here instead of sending the question back to the model: a
// regeneration costs a call and may bring a different question back.
//
// Only the edges are cut. A heading in the middle of a span is a real signal
// (the validator warns about it), not something to trim silently.
func TrimEvidence(questions []dataset.Question, docs map[string]*Document) ([]dataset.Question, []TrimRecord) {
	out := make([]dataset.Question, 0, len(questions))
	var records []TrimRecord
	for _, q := range questions {
		trimmed := q
		trimmed.Evidence = make([]dataset.EvidenceSet, len(q.Evidence))
		for setIndex, set := range q.Evidence {
			newSet := make([]dataset.Span, len(set))
			for spanIndex, span := range set {
				doc, ok := docs[span.File]
				if !ok {
					newSet[spanIndex] = span
					continue
				}
				start, end, cut := trimEdges(doc, span.Start, span.End)
				if len(cut) == 0 {
					newSet[spanIndex] = span
					continue
				}
				if start > end {
					// Nothing but headings and blank lines. Leave the span
					// alone: the validator has to report it.
					newSet[spanIndex] = span
					records = append(records, TrimRecord{
						ID: q.ID, File: span.File,
						BeforeStart: span.Start, BeforeEnd: span.End,
						AfterStart: span.Start, AfterEnd: span.End,
						TrimmedLines: cut, Empty: true,
					})
					continue
				}
				newSet[spanIndex] = dataset.Span{File: span.File, Start: start, End: end}
				records = append(records, TrimRecord{
					ID: q.ID, File: span.File,
					BeforeStart: span.Start, BeforeEnd: span.End,
					AfterStart: start, AfterEnd: end,
					TrimmedLines: cut,
				})
			}
			trimmed.Evidence[setIndex] = newSet
		}
		out = append(out, trimmed)
	}
	return out, records
}

// trimEdges moves the start and the end of a span inwards over the heading
// and blank lines at its edges, and returns the lines it cut.
func trimEdges(doc *Document, start, end int) (int, int, []int) {
	var cut []int
	for start <= end && isEdgeLine(doc.Kind(start)) {
		cut = append(cut, start)
		start++
	}
	for end >= start && isEdgeLine(doc.Kind(end)) {
		cut = append(cut, end)
		end--
	}
	return start, end, cut
}

// isEdgeLine reports whether a line may be cut off the edge of a span.
func isEdgeLine(kind LineKind) bool {
	return kind == LineHeading || kind == LineBlank
}

// FormatTrimRecords renders the trim records as a table for the terminal.
func FormatTrimRecords(records []TrimRecord) string {
	if len(records) == 0 {
		return "no evidence span needed trimming\n"
	}
	out := ""
	for _, r := range records {
		note := ""
		if r.Empty {
			note = "  (the span held nothing but headings and blank lines; it was left as it was)"
		}
		out += fmt.Sprintf("  %-18s %-44s %d-%d -> %d-%d  cut %v%s\n",
			r.ID, r.File, r.BeforeStart, r.BeforeEnd, r.AfterStart, r.AfterEnd, r.TrimmedLines, note)
	}
	return out
}
