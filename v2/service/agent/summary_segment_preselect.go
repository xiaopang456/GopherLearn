package agent

import (
	"context"
	"log"
	"sort"
	"strings"
	"unicode"

	"GopherAI/common/memoryrecall"
	"GopherAI/common/rag"
	"GopherAI/model"
)

// The server-side preselection of summary segments.
//
// Every turn the learner asks something, and the server has to decide which
// older segments are worth their tokens. The decision is made here rather than
// left to the model, for two reasons that have nothing to do with cost: the
// model cannot choose a segment it was never shown, and a tool call is a
// round trip the learner waits for.
//
// The thresholds are looser than the episode recall's, and deliberately so.
// An episode is a card about another session, and a wrong card misleads. A
// segment is a piece of this session's own conversation, and a wrong one is
// merely useless - the learner loses a little context, not a fact. The
// episode rule "a pure vector hit never qualifies" exists to stop a
// semantically near-miss from another session; the same rule here would throw
// away the exact sentence the learner is asking about.

const (
	// SummarySegmentCandidateLimit bounds how many segments the preselector
	// looks at. A session with more segments than this has a long tail the
	// preselector does not see; the browse tool is what reaches it.
	SummarySegmentCandidateLimit = 128
	// summarySegmentVectorGate is the score a vector hit has to reach.
	summarySegmentVectorGate = 0.6
	// summarySegmentVectorRankGate is the rank a vector hit has to be within.
	summarySegmentVectorRankGate = 3
	// summarySegmentRRFConstant damps the reciprocal rank fusion.
	summarySegmentRRFConstant = 60
)

// SummarySegmentPreselector chooses the segments a question is about.
type SummarySegmentPreselector struct {
	// vectors is the vector channel. Nil means the channel is absent.
	vectors SummarySegmentVectorSearcher
	// embed is the provider call the query vector costs. It is only used
	// when a vector channel exists, because a lexical preselection costs no
	// embedding at all.
	embed memoryrecall.EmbedFunc
	// recent records the segments this turn reached for, so that the next
	// turn can keep them in front of the learner.
	recent SummarySegmentRecentUse
	// maxSegments is how many segments the preselection may return.
	maxSegments int
	// maxSegmentTokens bounds one returned segment.
	maxSegmentTokens int
	// maxTotalTokens bounds the whole returned set.
	maxTotalTokens int
}

// SummarySegmentPreselectLimits derives the preselector's bounds from the block
// that will render its answer.
//
// The two were separate numbers once: the preselector carried up to three
// segments within 900 tokens, and the block that rendered them had a budget of
// its own. A selection larger than the block is not harmless. The renderer walks
// its input from the newest segment backwards, so when more segments arrive than
// fit, the ones that survive are the newest - not the ones the preselector
// ranked highest - and the ranking it paid an embedding call for is thrown away.
//
// The count is a division, not a wish: a segment is indivisible here, and one
// segment is the smallest useful block. A total below one segment's worth still
// yields one, because selecting nothing is a different failure from selecting
// too little.
func SummarySegmentPreselectLimits(totalTokens, segmentTokens int) (segments, perSegment, total int) {
	if segmentTokens <= 0 {
		segmentTokens = DefaultSegmentTokens
	}
	if totalTokens <= 0 {
		totalTokens = segmentTokens
	}
	segments = totalTokens / segmentTokens
	if segments < 1 {
		segments = 1
	}
	return segments, segmentTokens, totalTokens
}

// NewSummarySegmentPreselector builds a preselector.
//
// The embedding function may be nil: a preselector without a vector channel
// never embeds anything, and requiring one would make the lexical channel
// depend on a provider it does not use.
func NewSummarySegmentPreselector(embed memoryrecall.EmbedFunc, vectors SummarySegmentVectorSearcher) (*SummarySegmentPreselector, error) {
	segments, perSegment, total := SummarySegmentPreselectLimits(DefaultRetrievedSegmentTokens, DefaultSegmentTokens)
	return &SummarySegmentPreselector{
		vectors: vectors, embed: embed,
		maxSegments:      segments,
		maxSegmentTokens: perSegment,
		maxTotalTokens:   total,
	}, nil
}

// Limits reports the bounds the preselector is running with.
//
// It exists so the entry can be checked against the configuration it was given:
// a preselector silently keeping its own defaults is the defect this replaces.
func (p *SummarySegmentPreselector) Limits() (segments, segmentTokens, totalTokens int) {
	if p == nil {
		return 0, 0, 0
	}
	return p.maxSegments, p.maxSegmentTokens, p.maxTotalTokens
}

// UseLimits overrides the preselector's bounds. It exists for the harnesses
// that measure a different shape; a zero value keeps the defaults.
func (p *SummarySegmentPreselector) UseLimits(maxSegments, maxSegmentTokens, maxTotalTokens int) {
	if p == nil {
		return
	}
	if maxSegments > 0 {
		p.maxSegments = maxSegments
	}
	if maxSegmentTokens > 0 {
		p.maxSegmentTokens = maxSegmentTokens
	}
	if maxTotalTokens > 0 {
		p.maxTotalTokens = maxTotalTokens
	}
}

// UseRecentUse attaches the record of the segments a turn reached for.
//
// It is a setter rather than a constructor argument because the record is
// optional: without it the preselection still works, and the learner simply
// does not get the segments they were asking about kept in front of them.
func (p *SummarySegmentPreselector) UseRecentUse(recent SummarySegmentRecentUse) {
	if p == nil {
		return
	}
	p.recent = recent
}

// summarySegmentHit is one segment's standing in the two channels.
type summarySegmentHit struct {
	// Lexical is how many query terms the segment matched.
	Lexical int
	// LexicalRank is its position in the lexical order, zero when it did not
	// match at all.
	LexicalRank int
	// VectorRank is its position in the vector order, zero when the channel
	// did not return it.
	VectorRank int
	// VectorScore is the vector channel's own score.
	VectorScore float64
	// Fusion is the reciprocal rank fusion of the two channels.
	Fusion float64
}

// Select returns the segments this question is about.
//
// The resident segments are excluded: they travel with every turn anyway, and
// a segment that is both resident and preselected would spend its tokens
// twice.
func (p *SummarySegmentPreselector) Select(ctx context.Context, input RunContext,
	candidates []model.SessionSummarySegment, resident []model.SessionSummarySegment) ([]model.SessionSummarySegment, error) {
	if p == nil || len(candidates) == 0 {
		return nil, nil
	}
	question := strings.TrimSpace(input.Question)
	if question == "" {
		return nil, nil
	}
	residentSeqs := make(map[int]struct{}, len(resident))
	for _, segment := range resident {
		residentSeqs[segment.Seq] = struct{}{}
	}
	terms := summarySegmentQueryTerms(question)
	lexicalHits := summarySegmentLexicalRanks(candidates, residentSeqs, terms)
	vectorHits, vectorErr := p.vectorChannel(ctx, input, question)
	if vectorErr != nil {
		// The vector channel is an extra signal, not the decision. A search
		// that could not run costs the vector half of the fusion and leaves
		// the lexical half standing on its own.
		log.Printf("[agent-context] summary segment vector channel degraded: session=%s error_type=%T",
			input.SessionID, vectorErr)
		vectorHits = nil
	}
	fused := summarySegmentFuse(lexicalHits, vectorHits)
	// The candidates are ranked by the fused score, not by their position in
	// the session: the whole point of the fusion is that the segment the
	// question is about wins, whichever stretch of the conversation it sits
	// in.
	ranked := make([]model.SessionSummarySegment, 0, len(candidates))
	for _, candidate := range candidates {
		if _, already := residentSeqs[candidate.Seq]; already {
			continue
		}
		if _, qualifies := fused[candidate.Seq]; !qualifies {
			continue
		}
		ranked = append(ranked, candidate)
	}
	sort.SliceStable(ranked, func(left, right int) bool {
		leftScore := fused[ranked[left].Seq].Fusion
		rightScore := fused[ranked[right].Seq].Fusion
		if leftScore != rightScore {
			return leftScore > rightScore
		}
		return ranked[left].Seq < ranked[right].Seq
	})
	selected := make([]model.SessionSummarySegment, 0, p.maxSegments)
	total := 0
	for _, candidate := range ranked {
		if len(selected) >= p.maxSegments {
			break
		}
		cost, ok := p.segmentCost(candidate)
		if !ok {
			// A segment this schema cannot read carries nothing the model
			// could use, so it is not a candidate.
			continue
		}
		if total+cost > p.maxTotalTokens {
			continue
		}
		total += cost
		selected = append(selected, candidate)
	}
	p.recordRecentUse(ctx, input, selected)
	return selected, nil
}

// segmentCost is what one candidate costs the turn that selects it.
//
// It is measured on the block the renderer would emit under this
// preselector's own per-segment budget, so the number the decision is made on
// is the number the window pays. The stored body is JSON - braces, quotes,
// field names, evidence ids - and on a real segment it runs to three times
// the rendered block, so a budget spent on the body refused segments that
// would have fitted and left the answer's own segment out of the window.
//
// There is no size gate beside this one. Whether a segment fits inside a
// segment's allowance is the renderer's decision, and the renderer answers it
// by trimming; a second judge here would only disagree with it.
func (p *SummarySegmentPreselector) segmentCost(candidate model.SessionSummarySegment) (int, bool) {
	cost := renderedSegmentCost(candidate, p.maxSegmentTokens)
	if cost == 0 {
		return 0, false
	}
	return cost, true
}

// recordRecentUse remembers the segments this turn reached for.
//
// The record is what keeps a segment the learner keeps asking about in front
// of them after the newest segments have moved past it. A record that cannot
// be written costs the learner that memory and nothing else.
func (p *SummarySegmentPreselector) recordRecentUse(ctx context.Context, input RunContext,
	selected []model.SessionSummarySegment) {
	if p.recent == nil || len(selected) == 0 {
		return
	}
	for _, segment := range selected {
		if err := p.recent.RecordRecentUse(ctx, input.UserID, input.SessionID, segment.Seq); err != nil {
			log.Printf("[agent-context] summary segment recent use failed: session=%s seq=%d error_type=%T",
				input.SessionID, segment.Seq, err)
			return
		}
	}
}

// summarySegmentFuse ranks the candidates over both channels and applies the
// selection threshold.
//
// The threshold is the one the plan states: a lexical match is enough on its
// own, and a vector hit has to be both near the top and above a score floor.
// A segment that satisfies neither is not about this question.
func summarySegmentFuse(lexical map[int]summarySegmentHit, vector map[int]summarySegmentHit) map[int]summarySegmentHit {
	merged := make(map[int]summarySegmentHit, len(lexical)+len(vector))
	for seq, hit := range lexical {
		merged[seq] = hit
	}
	for seq, hit := range vector {
		existing, present := merged[seq]
		if !present {
			merged[seq] = hit
			continue
		}
		existing.VectorRank = hit.VectorRank
		existing.VectorScore = hit.VectorScore
		merged[seq] = existing
	}
	out := make(map[int]summarySegmentHit, len(merged))
	for seq, hit := range merged {
		lexicalMatch := hit.Lexical > 0
		vectorMatch := hit.VectorRank > 0 && hit.VectorRank <= summarySegmentVectorRankGate &&
			hit.VectorScore >= summarySegmentVectorGate
		if !lexicalMatch && !vectorMatch {
			continue
		}
		fusion := 0.0
		if hit.LexicalRank > 0 {
			fusion += 1.0 / float64(summarySegmentRRFConstant+hit.LexicalRank)
		}
		if hit.VectorRank > 0 {
			fusion += 1.0 / float64(summarySegmentRRFConstant+hit.VectorRank)
		}
		hit.Fusion = fusion
		out[seq] = hit
	}
	return out
}

// summarySegmentLexicalRanks scores the candidates against the query terms and
// ranks them.
func summarySegmentLexicalRanks(candidates []model.SessionSummarySegment, resident map[int]struct{},
	terms []string) map[int]summarySegmentHit {
	type scored struct {
		seq   int
		count int
	}
	matches := make([]scored, 0, len(candidates))
	for _, candidate := range candidates {
		if _, already := resident[candidate.Seq]; already {
			continue
		}
		count := summarySegmentTermHits(terms, candidate)
		if count == 0 {
			continue
		}
		matches = append(matches, scored{seq: candidate.Seq, count: count})
	}
	sort.SliceStable(matches, func(left, right int) bool {
		if matches[left].count != matches[right].count {
			return matches[left].count > matches[right].count
		}
		return matches[left].seq < matches[right].seq
	})
	out := make(map[int]summarySegmentHit, len(matches))
	for index, match := range matches {
		out[match.seq] = summarySegmentHit{Lexical: match.count, LexicalRank: index + 1}
	}
	return out
}

// vectorChannel runs the vector half of the preselection.
//
// The query vector comes from the request's recall cycle, so a turn that also
// recalls episodes or pre-retrieves the learner's documents pays for one
// embedding rather than three.
func (p *SummarySegmentPreselector) vectorChannel(ctx context.Context, input RunContext,
	question string) (map[int]summarySegmentHit, error) {
	if p.vectors == nil || p.embed == nil {
		return nil, nil
	}
	// A turn that switched its capabilities off does not reach for the
	// question: the promise is that this turn's answer runs no retrieval, and a
	// query vector is the first half of one. The lexical channel still ranks
	// the segments, so the block is keyword-only rather than empty - and the
	// zero below is about the answer, not about the background writer, which
	// embeds what it stores so that later turns can find it.
	if !input.KnowledgePolicy.CarriesAnyCapability() {
		return nil, nil
	}
	vector, err := input.RecallCycle.QueryVector(ctx, question, p.embed)
	if err != nil {
		return nil, err
	}
	if len(vector) == 0 {
		return nil, nil
	}
	hits, err := p.vectors.SearchByVector(ctx, input.UserID, input.SessionID,
		vector, p.maxSegments*4)
	if err != nil {
		return nil, err
	}
	ordered := make([]SummarySegmentVectorHit, len(hits))
	copy(ordered, hits)
	sort.SliceStable(ordered, func(left, right int) bool { return ordered[left].Score > ordered[right].Score })
	out := make(map[int]summarySegmentHit, len(ordered))
	for index, hit := range ordered {
		out[hit.Seq] = summarySegmentHit{VectorRank: index + 1, VectorScore: hit.Score}
	}
	return out, nil
}

// summarySegmentQueryTerms extracts the terms the lexical channel matches.
//
// Han runs become bigrams, because a search dictionary that does not know this
// project's vocabulary splits a Chinese term into single characters and then
// matches every document that contains one of them. ASCII words and numbers
// are kept whole, because those are what the learner writes down: a
// duration, a name, a version.
func summarySegmentQueryTerms(question string) []string {
	terms := rag.HanBigrams(question)
	seen := make(map[string]struct{}, len(terms))
	for _, term := range terms {
		seen[term] = struct{}{}
	}
	for _, word := range strings.FieldsFunc(question, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		word = strings.ToLower(strings.TrimSpace(word))
		if len([]rune(word)) < 2 {
			continue
		}
		if _, exists := seen[word]; exists {
			continue
		}
		seen[word] = struct{}{}
		terms = append(terms, word)
	}
	return terms
}

// summarySegmentTermHits counts the query terms one segment matches.
//
// The raw stored body is searched rather than the decoded one: the body
// already contains the topic, the points and the names as text, and decoding
// every candidate's JSON only to run a substring test would spend the turn's
// time on candidates the test is about to throw away.
func summarySegmentTermHits(terms []string, segment model.SessionSummarySegment) int {
	if len(terms) == 0 {
		return 0
	}
	haystack := strings.ToLower(segment.Topic + "\n" + segment.NamesAndNumbers + "\n" + segment.Content)
	hits := 0
	for _, term := range terms {
		if strings.Contains(haystack, strings.ToLower(term)) {
			hits++
		}
	}
	return hits
}
