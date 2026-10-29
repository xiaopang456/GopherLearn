package topicconsent

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"GopherAI/model"
)

// Topic is the catalogue entry this package needs.
//
// It is a local shape rather than service/memory.Topic on purpose. The
// suggestion path must be structurally unable to hand back a mastery level, and
// the cheapest way to guarantee that is for the type it copies into to have
// nowhere to put one.
type Topic struct {
	Key    string
	Name   string
	Graded bool
	Notes  int
}

// TopicLister is the catalogue read. The production implementation is a thin
// adapter over service/memory.TopicCatalogue.
type TopicLister interface {
	ListTopics(ctx context.Context, userID int64) ([]Topic, error)
}

type TopicListerFunc func(context.Context, int64) ([]Topic, error)

func (f TopicListerFunc) ListTopics(ctx context.Context, userID int64) ([]Topic, error) {
	return f(ctx, userID)
}

// Suggester proposes candidate topics for one question.
//
// It is deterministic by construction: catalogue rows plus lexical overlap, no
// embedding, no vector index and no model call. STABILIZE-049 spent its whole
// budget proving a model cannot make this decision reliably, so this stage does
// not try to make it at all - it offers a short list and lets the user decide.
type Suggester struct {
	topics TopicLister
	limit  int
}

const (
	DefaultCataloguePageSize = 20
	MaxCataloguePageSize     = 20
)

// CataloguePage is a deterministic window over the authenticated learner's
// topic names. It is deliberately made of Candidate rather than Topic so the
// response has nowhere to carry mastery, note counts or other personal state.
type CataloguePage struct {
	Candidates []Candidate
	NextOffset int
	HasMore    bool
}

func NewSuggester(topics TopicLister, limit int) (*Suggester, error) {
	if topics == nil {
		return nil, fmt.Errorf("topic suggester requires a catalogue")
	}
	if limit < 1 || limit > MaxCandidates {
		return nil, fmt.Errorf("topic suggester limit must be between 1 and %d", MaxCandidates)
	}
	return &Suggester{topics: topics, limit: limit}, nil
}

// Suggest returns at most limit candidates, best first.
//
// A topic with no lexical connection to the question is not offered at all.
// Padding the list to its limit with the learner's most recent subjects would
// reintroduce, in the user interface, exactly the "nearest available topic"
// fallback the design forbids on the server.
func (s *Suggester) Suggest(ctx context.Context, userID int64, question string) ([]Candidate, error) {
	if s == nil {
		return nil, fmt.Errorf("topic suggester is not configured")
	}
	if userID <= 0 {
		return nil, fmt.Errorf("topic suggestion requires a trusted user id")
	}
	if strings.TrimSpace(question) == "" {
		return nil, fmt.Errorf("topic suggestion requires a question")
	}
	topics, err := s.topics.ListTopics(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list topics: %w", err)
	}
	terms := questionTerms(question)
	type scored struct {
		candidate Candidate
		score     int
		notes     int
	}
	ranked := make([]scored, 0, len(topics))
	for _, topic := range topics {
		key := model.NormalizeLearningTopicKey(topic.Key)
		if key == "" {
			continue
		}
		name := strings.TrimSpace(topic.Name)
		if name == "" {
			name = key
		}
		score := lexicalScore(terms, key, name)
		if score == 0 {
			continue
		}
		source := sourceForTopic(topic, SourceLexicalMatch)
		ranked = append(ranked, scored{
			candidate: Candidate{TopicKey: key, DisplayName: name, Source: source},
			score:     score, notes: topic.Notes,
		})
	}
	// Graded topics outrank ungraded ones at equal overlap: they were named by
	// the learner rather than derived from a note, so they are the stronger
	// identity. Key order breaks the final tie so the list is reproducible.
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		left, right := ranked[i].candidate.Source == SourceGradedTopic, ranked[j].candidate.Source == SourceGradedTopic
		if left != right {
			return left
		}
		if ranked[i].notes != ranked[j].notes {
			return ranked[i].notes > ranked[j].notes
		}
		return ranked[i].candidate.TopicKey < ranked[j].candidate.TopicKey
	})
	out := make([]Candidate, 0, s.limit)
	for _, item := range ranked {
		if len(out) == s.limit {
			break
		}
		out = append(out, item.candidate)
	}
	return out, nil
}

// Browse returns an explicit, paginated catalogue path when the short
// suggestion list missed what the learner meant. An empty query means "show
// all". This method only lists names; it does not issue read authority.
func (s *Suggester) Browse(ctx context.Context, userID int64, query string, offset, limit int) (CataloguePage, error) {
	if s == nil {
		return CataloguePage{}, fmt.Errorf("topic suggester is not configured")
	}
	if userID <= 0 {
		return CataloguePage{}, fmt.Errorf("topic catalogue requires a trusted user id")
	}
	if offset < 0 {
		return CataloguePage{}, fmt.Errorf("topic catalogue offset must not be negative")
	}
	if limit == 0 {
		limit = DefaultCataloguePageSize
	}
	if limit < 1 || limit > MaxCataloguePageSize {
		return CataloguePage{}, fmt.Errorf("topic catalogue limit must be between 1 and %d", MaxCataloguePageSize)
	}
	topics, err := s.topics.ListTopics(ctx, userID)
	if err != nil {
		return CataloguePage{}, fmt.Errorf("list topics: %w", err)
	}
	candidates := catalogueCandidates(topics, query)
	if offset >= len(candidates) {
		return CataloguePage{Candidates: []Candidate{}, NextOffset: len(candidates)}, nil
	}
	end := offset + limit
	if end > len(candidates) {
		end = len(candidates)
	}
	return CataloguePage{
		Candidates: append([]Candidate(nil), candidates[offset:end]...),
		NextOffset: end,
		HasMore:    end < len(candidates),
	}, nil
}

// ResolveSelection re-reads the current catalogue before signing. A topic may
// disappear between browse and submit; in that case the whole selection is
// refused rather than partially authorising a different context.
func (s *Suggester) ResolveSelection(ctx context.Context, userID int64, selected []string) ([]Candidate, error) {
	if s == nil {
		return nil, fmt.Errorf("topic suggester is not configured")
	}
	if userID <= 0 {
		return nil, fmt.Errorf("topic selection requires a trusted user id")
	}
	keys := normalizeKeys(selected)
	if len(keys) == 0 {
		return nil, fmt.Errorf("%w: at least one topic is required", ErrInvalidSelection)
	}
	if len(keys) > MaxSelectableTopics {
		return nil, fmt.Errorf("%w: %d selected", ErrTooManyTopics, len(keys))
	}
	topics, err := s.topics.ListTopics(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list topics: %w", err)
	}
	owned := make(map[string]Candidate, len(topics))
	for _, candidate := range catalogueCandidates(topics, "") {
		owned[candidate.TopicKey] = candidate
	}
	out := make([]Candidate, 0, len(keys))
	for _, key := range keys {
		candidate, ok := owned[key]
		if !ok {
			return nil, fmt.Errorf("%w: selection is not in the current user's catalogue", ErrInvalidSelection)
		}
		out = append(out, candidate)
	}
	return out, nil
}

func catalogueCandidates(topics []Topic, query string) []Candidate {
	wanted := compactTopicText(query)
	seen := map[string]struct{}{}
	out := make([]Candidate, 0, len(topics))
	for _, topic := range topics {
		key := model.NormalizeLearningTopicKey(topic.Key)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		name := strings.TrimSpace(topic.Name)
		if name == "" {
			name = key
		}
		if wanted != "" && !strings.Contains(compactTopicText(key), wanted) &&
			!strings.Contains(compactTopicText(name), wanted) {
			continue
		}
		seen[key] = struct{}{}
		// Browse intentionally erases whether this topic was graded or has
		// notes. Those facts are personal state; the catalogue endpoint is
		// allowed to expose only identity.
		out = append(out, Candidate{TopicKey: key, DisplayName: name, Source: SourceCatalogueBrowse})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TopicKey < out[j].TopicKey })
	return out
}

func sourceForTopic(topic Topic, fallback string) string {
	switch {
	case topic.Graded:
		return SourceGradedTopic
	case topic.Notes > 0:
		return SourceRecentNotes
	default:
		return fallback
	}
}

func compactTopicText(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsPunct(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, strings.TrimSpace(value))
}

// lexicalScore counts how much of the topic's own wording the question repeats.
//
// It scores the topic against the question rather than the reverse, because a
// long question would otherwise match every short topic. Matching is on
// segments of the key and name, so "jwt-鉴权" is reachable from a question that
// says either half.
func lexicalScore(terms map[string]struct{}, key, name string) int {
	score := 0
	for _, segment := range topicSegments(key, name) {
		if _, ok := terms[segment]; ok {
			score += 2
			continue
		}
		// A CJK topic segment rarely appears as a whitespace token, so a
		// containment check against the raw question text is what actually
		// matches "粘包" inside a sentence.
		for term := range terms {
			if len([]rune(segment)) >= 2 && strings.Contains(term, segment) {
				score++
				break
			}
		}
	}
	return score
}

func topicSegments(key, name string) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, raw := range append(splitTopic(key), splitTopic(name)...) {
		segment := strings.ToLower(strings.TrimSpace(raw))
		if len([]rune(segment)) < 2 {
			continue
		}
		if _, ok := seen[segment]; ok {
			continue
		}
		seen[segment] = struct{}{}
		out = append(out, segment)
	}
	return out
}

func splitTopic(value string) []string {
	return strings.FieldsFunc(value, func(r rune) bool {
		return r == '-' || r == '_' || r == '/' || r == '.' || unicode.IsSpace(r)
	})
}

// questionTerms lowercases the question and keeps both its whitespace tokens
// and the whole normalised string, so containment checks have something to run
// against for languages that do not space their words.
func questionTerms(question string) map[string]struct{} {
	lowered := strings.ToLower(question)
	out := map[string]struct{}{}
	for _, field := range strings.FieldsFunc(lowered, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsPunct(r)
	}) {
		if len([]rune(field)) >= 2 {
			out[field] = struct{}{}
		}
	}
	out[strings.Join(strings.Fields(lowered), "")] = struct{}{}
	return out
}
