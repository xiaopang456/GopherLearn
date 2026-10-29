// Package episoderelation is the episodic-memory write side: after a Session
// has been projected into an Episode and that Episode's assertions exist, it
// asks how each new assertion relates to what the same learner already
// recorded, and records the answer.
//
// It is a write-side concern, not a read-side one. The read side already knows
// how to present a replaced decision honestly - STABILIZE-063 proved that - but
// only if somebody wrote the relation down. Until now nobody did: the eight
// replacements the 064 corpus carried were seeded by hand into the fixture, so
// the governed read path had been validated against relations no part of the
// system could produce from a real conversation.
//
// Deciding at read time was the alternative and is worse. The same pair would
// be judged again on every retrieval, at the point where latency is visible and
// the model is already busy; the answer could differ between two reads of the
// same history; and a wrong "replaced" would be invisible because nothing
// durable would record that it had been decided at all. Deciding once, on
// write, leaves a row somebody can audit.
//
// Two words are kept apart throughout. A *state* is a property of one assertion
// now - current, replaced, unknown. A *relation* is a property of a pair -
// equivalent, replaces, supplements, independent, uncertain. Only replacement
// crosses from one to the other.
//
// What this package is not: it is not RAG. Knowledge RAG holds documents the
// learner uploaded, which are a source the Agent may search, not something the
// learner did. No uploaded document reaches a candidate set here, and nothing
// here writes to the knowledge store.
package episoderelation

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"GopherAI/model"
)

// DefaultMaxCandidates bounds one new assertion's comparison set.
//
// The bound exists for two reasons and the second is the one that matters. It
// caps cost, obviously. But an unbounded set also makes the classifier's job
// worse: every extra candidate is another chance to produce a confident
// "replaces" against something only loosely related, and the cost of that
// mistake is a true statement marked superseded.
const DefaultMaxCandidates = 8

// CandidateSource is the learner's own history. It is deliberately narrow:
// this package may ask for one learner's active episode assertions and nothing
// else, so there is no shape of call that could reach another learner, an
// uploaded document, or the semantic-memory store.
type CandidateSource interface {
	// ActiveAssertions returns the learner's assertions from active,
	// indexed Episodes, excluding the named session entirely.
	ActiveAssertions(ctx context.Context, userID int64, excludeSessionID string) ([]Candidate, error)
}

// Candidate is one prior assertion a new one may relate to.
//
// It carries what a relation decision legitimately needs and nothing more. In
// particular it carries no gold label, no expected relation, no subject id and
// no experiment tag: those exist only in evaluation material, and a candidate
// that could carry one is a candidate that will eventually carry one.
type Candidate struct {
	AssertionID string
	EpisodeID   string
	SessionID   string
	UserID      int64
	Kind        model.AssertionKind
	Text        string
	OccurredAt  time.Time
	TimeKnown   bool
	State       model.AssertionState
}

// NewAssertion is the freshly projected assertion a relation is sought for.
type NewAssertion struct {
	AssertionID string
	EpisodeID   string
	SessionID   string
	UserID      int64
	Kind        model.AssertionKind
	Text        string
	OccurredAt  time.Time
	TimeKnown   bool
}

// CandidateFilterReason is the closed vocabulary for why a candidate was
// dropped. It is recorded rather than silently discarded, because "no
// candidates" and "every candidate was excluded for a reason" are different
// facts and only one of them is a bug.
type CandidateFilterReason string

const (
	FilterForeignOwner     CandidateFilterReason = "foreign_owner"
	FilterSameSession      CandidateFilterReason = "same_session"
	FilterSelf             CandidateFilterReason = "self"
	FilterNotEarlier       CandidateFilterReason = "not_earlier"
	FilterTimeUnknown      CandidateFilterReason = "time_unknown"
	FilterKindIncomparable CandidateFilterReason = "kind_incomparable"
	FilterOverBound        CandidateFilterReason = "over_bound"
	FilterNoTopicOverlap   CandidateFilterReason = "no_topic_overlap"
)

// CandidateSet is one new assertion's bounded, ordered comparison set.
type CandidateSet struct {
	Candidates []Candidate
	// Excluded counts why the rest were dropped.
	Excluded map[CandidateFilterReason]int
	// Considered is how many the source returned before filtering.
	Considered int
}

// Finder builds the comparison set for one new assertion.
type Finder struct {
	source CandidateSource
	max    int
}

// NewFinder refuses a configuration that could not bound anything.
func NewFinder(source CandidateSource, max int) (*Finder, error) {
	if source == nil {
		return nil, fmt.Errorf("the candidate finder needs a source of the learner's own history")
	}
	if max <= 0 {
		max = DefaultMaxCandidates
	}
	return &Finder{source: source, max: max}, nil
}

// Find returns the bounded, stably ordered candidate set for one assertion.
//
// The ordering is by assertion time descending then id ascending, and it is
// total: two runs over the same history produce the same set in the same
// order, which is what makes a re-projection comparable with the run before it
// rather than merely similar to it.
func (f *Finder) Find(ctx context.Context, assertion NewAssertion) (CandidateSet, error) {
	set := CandidateSet{Candidates: []Candidate{}, Excluded: map[CandidateFilterReason]int{}}
	if assertion.UserID <= 0 || strings.TrimSpace(assertion.AssertionID) == "" {
		return set, fmt.Errorf("a candidate search needs an owned, identified assertion")
	}
	if !assertion.TimeKnown || assertion.OccurredAt.IsZero() {
		// A new assertion with no resolvable time cannot be ordered against
		// anything, and an unordered relation is the mis-linked pair the
		// governance layer refuses anyway. Better to find nothing than to
		// compare against an order that does not exist.
		set.Excluded[FilterTimeUnknown]++
		return set, nil
	}
	raw, err := f.source.ActiveAssertions(ctx, assertion.UserID, assertion.SessionID)
	if err != nil {
		return set, fmt.Errorf("read the learner's own history: %w", err)
	}
	set.Considered = len(raw)
	kept := make([]Candidate, 0, len(raw))
	for _, candidate := range raw {
		switch {
		case candidate.UserID != assertion.UserID:
			// Belt and braces: the source is asked for one owner, and this
			// refuses anything that arrives anyway. A scoped query that
			// returns nothing and a scoped query that returns someone else's
			// row fail differently, and only one is caught by the query.
			set.Excluded[FilterForeignOwner]++
		case candidate.SessionID == assertion.SessionID:
			// The session being projected is not its own history.
			set.Excluded[FilterSameSession]++
		case candidate.AssertionID == assertion.AssertionID ||
			candidate.EpisodeID == assertion.EpisodeID:
			set.Excluded[FilterSelf]++
		case !candidate.TimeKnown || candidate.OccurredAt.IsZero():
			set.Excluded[FilterTimeUnknown]++
		case !candidate.OccurredAt.Before(assertion.OccurredAt):
			// Only earlier assertions are candidates. A record from the
			// future cannot have been superseded by this one.
			set.Excluded[FilterNotEarlier]++
		case !ComparableKinds(candidate.Kind, assertion.Kind):
			set.Excluded[FilterKindIncomparable]++
		case !topicOverlap(candidate.Text, assertion.Text):
			set.Excluded[FilterNoTopicOverlap]++
		default:
			kept = append(kept, candidate)
		}
	}
	sort.SliceStable(kept, func(i, j int) bool {
		if !kept[i].OccurredAt.Equal(kept[j].OccurredAt) {
			return kept[i].OccurredAt.After(kept[j].OccurredAt)
		}
		return kept[i].AssertionID < kept[j].AssertionID
	})
	if len(kept) > f.max {
		set.Excluded[FilterOverBound] += len(kept) - f.max
		kept = kept[:f.max]
	}
	set.Candidates = kept
	return set, nil
}

// ComparableKinds says whether two assertion kinds may stand in any relation
// at all.
//
// A synopsis asserts nothing, so it cannot relate. An open loop is not closed
// by a relation either: "I still need to decide X" becomes resolved or
// cancelled, which is its own state and its own future stage. Letting a
// replacement stand in for that would record a decision the learner never made.
func ComparableKinds(a, b model.AssertionKind) bool {
	for _, kind := range []model.AssertionKind{a, b} {
		switch kind {
		case model.AssertionDecision, model.AssertionCorrection:
		default:
			return false
		}
	}
	return true
}

// topicOverlap is a cheap, deterministic pre-filter: the classifier is the
// thing that decides, and this only keeps it from being asked about pairs with
// nothing in common.
//
// It is intentionally crude and intentionally not the decision. A keyword rule
// that decided relations would be the "classify by wording" defect this
// project has already paid for; here a false positive costs one classification
// and a false negative costs one missed comparison, and both are visible in the
// recorded filter counts.
func topicOverlap(left, right string) bool {
	leftTokens := tokenSet(left)
	if len(leftTokens) == 0 {
		return false
	}
	for token := range tokenSet(right) {
		if leftTokens[token] {
			return true
		}
	}
	return false
}

// tokenSet splits on non-letter runs for Latin text and on individual runes
// for CJK, so the pre-filter works on the mixed Chinese/English the corpus
// actually contains.
func tokenSet(text string) map[string]bool {
	out := map[string]bool{}
	var word strings.Builder
	flush := func() {
		if word.Len() >= 3 {
			out[strings.ToLower(word.String())] = true
		}
		word.Reset()
	}
	for _, r := range text {
		switch {
		case r >= 0x4E00 && r <= 0x9FFF:
			flush()
			out[string(r)] = true
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			word.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return out
}
