package agenttool

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"GopherAI/common/rag"
	summarysegmentdao "GopherAI/dao/summarysegment"
	"GopherAI/model"
	serviceagent "GopherAI/service/agent"
	"GopherAI/service/episode"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
)

// The tool that searches the learner's own summary segments across their
// conversations.
//
// It is the second door into the history, and it exists because the first one
// has a hole: a conversation that has no card - because no card was ever
// written for it, or because the card was written by a version that did not
// write one - is a conversation the card search cannot find, even though its
// summary segments are right there.
//
// The segments are the compressed record of a stretch of turns. A hit is a
// stretch of an earlier conversation, and the conversation it belongs to
// travels back with it: the tool's job is to find the stretch AND to say which
// conversation it is in, so that the model can go on and open that one.
//
// It is a read. It reads no message, it runs no model, and it opens nothing
// the card index did not describe: a hit in a conversation with no card is
// shown to the model as a hit that cannot be opened, rather than as a
// conversation that was silently made reachable.

const SearchHistorySegmentsName = "search_history_segments"

// The bounds of one search.
const (
	// maxSearchHistorySegments is how many segments one call may return.
	maxSearchHistorySegments = 8
	// defaultSearchHistorySegments is how many it returns when the model
	// does not say.
	defaultSearchHistorySegments = 5
	// maxHistorySegmentRunes bounds one segment's text. A segment is a
	// compressed record; the text is what the model reads of it.
	maxHistorySegmentRunes = 600
	// historySegmentCandidateLimit bounds how many segments the index is
	// asked for. The index ranks them, and the tool shows the first ones
	// that fit.
	historySegmentCandidateLimit = 32
)

// searchHistorySegmentsDescription is the contract the model reads.
const searchHistorySegmentsDescription = "Search your earlier conversations' summary segments for a clue. Use it when the conversation search did not find the conversation you are looking for. A segment is a compressed record of earlier turns, not a transcript: it returns the segment with the conversation it belongs to, and the conversation is opened only when a card describes it."

// The states a search can report.
const (
	segmentsStatusOK          = "ok"
	segmentsStatusNoEvidence  = "no_evidence"
	segmentsStatusUnavailable = "unavailable"
)

// The reasons a search gives.
const (
	segmentsReasonNone         = "none"
	segmentsReasonNoMatch      = "no_match"
	segmentsReasonInvalidInput = "invalid_request"
	segmentsReasonStoreFailure = "store_unavailable"
	// segmentsReasonNoCard says a hit was found in a conversation no card
	// describes, so the conversation cannot be opened.
	segmentsReasonNoCard = "card_unavailable"
	// segmentsReasonCurrentFirst says the question was a bare pointer and
	// THIS conversation has the history, so the answer is here before it is
	// anywhere else.
	segmentsReasonCurrentFirst = "current_session_has_history"
)

// SearchHistorySegmentsInput is what the model asks for.
type SearchHistorySegmentsInput struct {
	Query      string `json:"query" jsonschema:"required,description=The clue to look for: the subject, a name, or a phrase from the stretch of turns you are looking for."`
	MaxResults int    `json:"max_results,omitempty" jsonschema:"description=How many segments to return, 1-8."`
}

// SearchHistorySegmentsOutput is what the call reports.
type SearchHistorySegmentsOutput struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
	// Segments are the stretches that were found, best first.
	Segments []HistorySegmentHitView `json:"segments"`
	// ObservationTokens is what the answer has to spend on this observation.
	ObservationTokens int  `json:"observation_tokens"`
	Truncated         bool `json:"truncated,omitempty"`
	Retryable         bool `json:"retryable,omitempty"`
	// VectorChannel says whether the semantic channel ran.
	//
	// It is false, and it says so rather than staying silent: the tool
	// embeds nothing, because an embedding is a model call and this tool
	// makes none. The search is the lexical one.
	VectorChannel bool `json:"vector_channel"`
	// EmbeddingCalls is how many embeddings this call made.
	//
	// It is reported so that the retrieval cost can be counted separately
	// from the card's own cost: the card is written without a model call, and
	// the query embedding is paid for by the question, not by the card.
	EmbeddingCalls int `json:"embedding_calls"`
	// Notes says what the call did not do.
	Notes string `json:"notes,omitempty"`
}

// HistorySegmentHitView is one segment as the model sees it.
type HistorySegmentHitView struct {
	// SessionID is the conversation the segment belongs to.
	SessionID string `json:"session_id"`
	// Seq is the segment's number inside that conversation.
	Seq int `json:"seq"`
	// Topic is the segment's own title.
	Topic string `json:"topic"`
	// Text is the segment's compressed record, bounded.
	Text string `json:"text"`
	// StartMessageID and EndMessageID are the stretch of turns the segment
	// covers.
	StartMessageID int `json:"start_message_id"`
	EndMessageID   int `json:"end_message_id"`
	// EndAt is when the segment was written.
	EndAt string `json:"end_at,omitempty"`
	// Score is the search's own score.
	Score float64 `json:"score"`
	// Reachable says whether this run may open the conversation the segment
	// belongs to.
	//
	// It is false for a hit in a conversation no card describes. The hit is
	// still shown - the model asked for it and it is a fact about the
	// learner's own history - but nothing about that conversation may be
	// read because of it.
	Reachable bool `json:"reachable"`
	// UnreachableReason says why a hit cannot be opened.
	UnreachableReason string `json:"unreachable_reason,omitempty"`
}

// HistorySegmentFinder finds the learner's segments across their sessions.
//
// It is the index's cross-session search: the same index the browse tool
// reads, with the session filter replaced by the user filter.
type HistorySegmentFinder interface {
	SearchUserSegmentsByText(ctx context.Context, userID int64, terms []string, limit int) ([]summarysegmentdao.UserSegmentHit, error)
}

// HistorySegmentReader reads one segment's stored row.
//
// It is optional. Without it the tool reports what the index holds, and the
// segment's start message id is not among it.
type HistorySegmentReader interface {
	GetSummarySegment(ctx context.Context, userID int64, sessionID string, seq int) (*model.SessionSummarySegment, error)
}

// HistorySegmentVectorFinder finds the learner's segments by meaning.
//
// It is optional. The text channel is the one that always runs, because it
// costs nothing: the segments were written with their words already in the
// index.
type HistorySegmentVectorFinder interface {
	SearchUserSegmentsByVector(ctx context.Context, userID int64, vector []float32, limit int) ([]summarysegmentdao.UserSegmentHit, error)
}

// HistorySegmentEmbedder embeds the query for the semantic channel.
//
// It is optional, and it is the one model call this path makes. The
// embedding is a RETRIEVAL cost: it happens when a learner asks a question,
// not when a card is written, and it is counted separately from the card's
// own cost, which is zero.
type HistorySegmentEmbedder interface {
	EmbedQuery(ctx context.Context, text string) ([]float32, error)
}

// SearchHistorySegmentsRegistry builds the search tool.
type SearchHistorySegmentsRegistry struct {
	finder   HistorySegmentFinder
	reader   HistorySegmentReader
	cards    ThinCardRetriever
	vectors  HistorySegmentVectorFinder
	embedder HistorySegmentEmbedder
	surface  serviceagent.ToolSurface
}

// NewSearchHistorySegmentsRegistry builds the registry.
//
// The card retriever is required: a hit in a conversation no card describes
// cannot be opened, and the retriever is what says whether a conversation has
// one. The segment reader is optional.
func NewSearchHistorySegmentsRegistry(finder HistorySegmentFinder, reader HistorySegmentReader,
	cards ThinCardRetriever) (*SearchHistorySegmentsRegistry, error) {
	if finder == nil {
		return nil, fmt.Errorf("%w: a history segment finder is required", ErrInvalidKnowledgeSearchConfig)
	}
	if cards == nil {
		return nil, fmt.Errorf("%w: a thin card retriever is required", ErrInvalidKnowledgeSearchConfig)
	}
	return &SearchHistorySegmentsRegistry{finder: finder, reader: reader, cards: cards}, nil
}

// UseSemanticChannel attaches the semantic half of the search.
//
// Both parts are needed: the finder reads the segments' own vectors, and the
// embedder turns the question into one. With only one of them the channel
// stays off, because a search that silently runs half of what it promised
// reports its results as if the whole search had run.
func (r *SearchHistorySegmentsRegistry) UseSemanticChannel(finder HistorySegmentVectorFinder,
	embedder HistorySegmentEmbedder) {
	if r == nil || finder == nil || embedder == nil {
		return
	}
	r.vectors = finder
	r.embedder = embedder
}

// UseToolSurface changes only the name and description shown to the model.
func (r *SearchHistorySegmentsRegistry) UseToolSurface(surface serviceagent.ToolSurface) {
	r.surface = surface
}

// Tools returns the search tool for one run.
func (r *SearchHistorySegmentsRegistry) Tools(ctx context.Context, run serviceagent.RunContext) ([]tool.BaseTool, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if run.UserID <= 0 || strings.TrimSpace(run.SessionID) == "" {
		return nil, fmt.Errorf("%w: a trusted user id and session are required", ErrInvalidKnowledgeSearchRequest)
	}
	name := serviceagent.MustToolNameFor(serviceagent.CapabilitySearchHistorySegments, r.resolvedSurface())
	search, err := toolutils.InferTool(name, searchHistorySegmentsDescription,
		func(ctx context.Context, input *SearchHistorySegmentsInput) (*SearchHistorySegmentsOutput, error) {
			return r.search(ctx, run, input)
		})
	if err != nil {
		return nil, fmt.Errorf("build history segment search schema: %w", err)
	}
	return []tool.BaseTool{search}, nil
}

func (r *SearchHistorySegmentsRegistry) resolvedSurface() serviceagent.ToolSurface {
	if r.surface == "" {
		return serviceagent.SurfaceLegacy
	}
	return r.surface
}

// search runs one call.
func (r *SearchHistorySegmentsRegistry) search(ctx context.Context, run serviceagent.RunContext,
	input *SearchHistorySegmentsInput) (*SearchHistorySegmentsOutput, error) {
	out := &SearchHistorySegmentsOutput{Status: segmentsStatusOK, Reason: segmentsReasonNone,
		Segments: []HistorySegmentHitView{}, VectorChannel: false}
	if input == nil || strings.TrimSpace(input.Query) == "" {
		out.Status = segmentsStatusUnavailable
		out.Reason = segmentsReasonInvalidInput
		return out, nil
	}
	limit := input.MaxResults
	if limit <= 0 {
		limit = defaultSearchHistorySegments
	}
	if limit > maxSearchHistorySegments {
		limit = maxSearchHistorySegments
	}
	query := strings.TrimSpace(input.Query)
	// The same order rule the card search follows: a bare pointer with a
	// history here is answered here first, and the segment search is not the
	// place to start.
	intent := episode.CrossSessionIntentOf(run.Question, run.CurrentSessionHasHistory)
	if !intent.Cross {
		out.Status = segmentsStatusNoEvidence
		out.Reason = segmentsReasonCurrentFirst
		out.Notes = "This question points at a conversation without naming a subject, and this conversation already has a history. Answer from it. Search again only if you can name a subject, a name or a phrase from the conversation you are looking for."
		return out, nil
	}
	terms, _, _ := thinCardQueryTerms(query)
	hits, err := r.finder.SearchUserSegmentsByText(ctx, run.UserID, terms, historySegmentCandidateLimit)
	if err != nil {
		if ctxErr := contextError(ctx); ctxErr != nil {
			return nil, ctxErr
		}
		out.Status = segmentsStatusUnavailable
		out.Reason = segmentsReasonStoreFailure
		out.Retryable = true
		return out, nil
	}
	// The semantic channel runs beside the lexical one, not after it.
	//
	// A question about an earlier conversation is usually phrased in the
	// learner's own words about a subject ("the three exercises you set me"),
	// and the words of a segment are the compressed record of a stretch of
	// turns - the two do not always share a token. The segments are already
	// embedded, so the only thing the semantic channel costs is the query's
	// own embedding.
	vectorHits, embeddingCalls, vectorErr := r.semanticHits(ctx, run, query)
	if vectorErr == nil && len(vectorHits) > 0 {
		hits = mergeSegmentHits(hits, vectorHits)
		out.VectorChannel = true
		out.EmbeddingCalls = embeddingCalls
		out.Notes = "the semantic channel ran beside the lexical one"
	} else if out.Notes == "" {
		out.Notes = "the semantic channel did not run: " + vectorChannelNote(r.vectors, r.embedder)
	}
	// The cards are asked for once, and only when a hit is in a conversation
	// that is not open yet.
	//
	// They are asked for BY IDENTITY and not by the question's words: a hit
	// is a stretch of turns in a conversation, and the card of that
	// conversation is what says the conversation may be opened. Looking the
	// card up with the same words that the segment search used would make
	// the hit unreachable in exactly the case the segment search exists for
	// - the case where the card search did not find the conversation.
	cards := r.cardsFor(ctx, run, hits)
	grants := make([]serviceagent.HistoryMessageGrant, 0, 16)
	tokens := 0
	for _, hit := range hits {
		if len(out.Segments) >= limit {
			out.Truncated = true
			break
		}
		session := strings.TrimSpace(hit.SessionID)
		if session == "" || session == strings.TrimSpace(run.SessionID) {
			// The conversation being answered is in the window already, and
			// a card about it is not a door to the past.
			continue
		}
		view, segmentGrants, ok := r.render(ctx, run, hit, cards)
		if !ok {
			continue
		}
		out.Segments = append(out.Segments, view)
		grants = append(grants, segmentGrants...)
		tokens += historySegmentTokens(view)
	}
	out.ObservationTokens = tokens
	// The ids the search just showed the model are now readable back in full,
	// and the run has to know it: the read-back tool refuses ids it was not
	// given.
	if run.GrantSet != nil && len(grants) > 0 {
		run.GrantSet.Extend(run, grants)
	}
	if len(out.Segments) == 0 {
		out.Status = segmentsStatusNoEvidence
		out.Reason = segmentsReasonNoMatch
	}
	return out, nil
}

// semanticHits runs the semantic channel and reports what it cost.
//
// It is best effort on purpose: a query that cannot be embedded is a query
// the lexical channel can still answer, and refusing the whole search
// because the meaning channel failed would make the cheaper path depend on
// the dearer one.
func (r *SearchHistorySegmentsRegistry) semanticHits(ctx context.Context, run serviceagent.RunContext,
	query string) ([]summarysegmentdao.UserSegmentHit, int, error) {
	if r.vectors == nil || r.embedder == nil {
		return nil, 0, fmt.Errorf("the semantic channel is not attached")
	}
	vectors, err := r.embedder.EmbedQuery(ctx, query)
	if err != nil {
		return nil, 0, fmt.Errorf("embed the history segment query: %w", err)
	}
	if len(vectors) == 0 {
		return nil, 0, fmt.Errorf("the embedder returned no vector for the history segment query")
	}
	hits, err := r.vectors.SearchUserSegmentsByVector(ctx, run.UserID, vectors, historySegmentCandidateLimit)
	if err != nil {
		return nil, 1, fmt.Errorf("search the history segment vectors: %w", err)
	}
	return hits, 1, nil
}

// vectorChannelNote says why the semantic channel did not run.
func vectorChannelNote(finder HistorySegmentVectorFinder, embedder HistorySegmentEmbedder) string {
	switch {
	case finder == nil && embedder == nil:
		return "no vector index and no embedder are attached"
	case finder == nil:
		return "no vector index is attached"
	case embedder == nil:
		return "no embedder is attached"
	default:
		return "the semantic channel found nothing"
	}
}

// mergeSegmentHits joins the two channels.
//
// A segment found by both is one segment, and it keeps the better score: the
// two channels rank on different scales of the same thing, and the higher one
// is the better description of how close it is.
func mergeSegmentHits(lexical, semantic []summarysegmentdao.UserSegmentHit) []summarysegmentdao.UserSegmentHit {
	merged := make([]summarysegmentdao.UserSegmentHit, 0, len(lexical)+len(semantic))
	index := make(map[string]int, len(lexical)+len(semantic))
	for _, hit := range append(append([]summarysegmentdao.UserSegmentHit{}, lexical...), semantic...) {
		key := strings.TrimSpace(hit.SessionID) + "\x00" + strconv.Itoa(hit.Seq)
		if at, exists := index[key]; exists {
			if hit.Score > merged[at].Score {
				merged[at].Score = hit.Score
			}
			continue
		}
		index[key] = len(merged)
		merged = append(merged, hit)
	}
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].Score > merged[j].Score })
	return merged
}

// cardsFor is the card of every conversation the hits name.
//
// It is asked for lazily: a hit in a conversation this run may already open
// needs no card, and a search that found nothing needs no card either.
//
// The lookup is by identity. The words are the question's, and the card's
// words are the card's: a conversation whose card the WORD search did not
// find is exactly the conversation a segment hit points at, and asking for
// its card with the question's words would find nothing.
func (r *SearchHistorySegmentsRegistry) cardsFor(ctx context.Context, run serviceagent.RunContext,
	hits []summarysegmentdao.UserSegmentHit) map[string]segmentCard {
	cards := map[string]segmentCard{}
	needed := false
	for _, hit := range hits {
		session := strings.TrimSpace(hit.SessionID)
		if session == "" || session == strings.TrimSpace(run.SessionID) {
			continue
		}
		if run.GrantSet != nil && run.GrantSet.AuthorisesSession(run, session) {
			continue
		}
		needed = true
		break
	}
	if !needed {
		return cards
	}
	for _, hit := range hits {
		session := strings.TrimSpace(hit.SessionID)
		if session == "" || session == strings.TrimSpace(run.SessionID) {
			continue
		}
		if _, exists := cards[session]; exists {
			continue
		}
		candidate, found, err := r.cards.CardOfSession(ctx, run.UserID, session)
		if err != nil || !found {
			continue
		}
		cards[session] = segmentCard{CardID: candidate.CardID, CardVersion: candidate.CardVersion,
			Watermark: candidate.Watermark}
	}
	return cards
}

// segmentCard is the card identity of one conversation.
type segmentCard struct {
	CardID      string
	CardVersion uint64
	// Watermark is how far the card reached: the ceiling of every read taken
	// in that conversation through this card.
	Watermark uint
}

// render turns one hit into the view the model reads, and draws the
// read-back authorisations for it.
//
// It reads no message: the segment's record is what the tool shows, and the
// ids it cites are what the model may ask for afterwards.
func (r *SearchHistorySegmentsRegistry) render(ctx context.Context, run serviceagent.RunContext,
	hit summarysegmentdao.UserSegmentHit, cards map[string]segmentCard) (HistorySegmentHitView,
	[]serviceagent.HistoryMessageGrant, bool) {
	session := strings.TrimSpace(hit.SessionID)
	view := HistorySegmentHitView{
		SessionID: session, Seq: hit.Seq, Topic: hit.Topic, Score: hit.Score,
		EndMessageID: int(hit.EndMessageID),
	}
	if !hit.EndAt.IsZero() {
		view.EndAt = hit.EndAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	startMessageID, endMessageID, endAt, content, ok := r.segment(ctx, run, hit)
	if !ok {
		return HistorySegmentHitView{}, nil, false
	}
	view.StartMessageID = int(startMessageID)
	if endMessageID > 0 {
		view.EndMessageID = int(endMessageID)
	}
	if !endAt.IsZero() {
		view.EndAt = endAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	points := historySegmentPoints(content)
	view.Text = historySegmentText(points, view.Topic)
	// The conversation has to be open before anything in it may be read.
	cardID, cardVersion, watermark, reachable := r.authorise(run, session, cards)
	view.Reachable = reachable
	if !reachable {
		view.UnreachableReason = segmentsReasonNoCard
		return view, nil, true
	}
	return view, historySegmentGrants(session, hit.Seq, startMessageID, view.EndMessageID,
		points, cardID, cardVersion, watermark), true
}

// segment reads one segment's row.
//
// The index is a derived view, so the row is what the tool reports whenever
// the row is there. Without a reader the index's own words are used, and the
// segment's start message id is not among them.
func (r *SearchHistorySegmentsRegistry) segment(ctx context.Context, run serviceagent.RunContext,
	hit summarysegmentdao.UserSegmentHit) (uint, uint, time.Time, string, bool) {
	if r.reader != nil {
		segment, err := r.reader.GetSummarySegment(ctx, run.UserID, strings.TrimSpace(hit.SessionID), hit.Seq)
		if err != nil || segment == nil {
			// The index row outlived the row it was written from.
			return 0, 0, time.Time{}, "", false
		}
		return segment.StartMessageID, segment.EndMessageID, segment.EndAt, segment.Content, true
	}
	if strings.TrimSpace(hit.Content) == "" {
		return 0, 0, time.Time{}, "", false
	}
	return 0, hit.EndMessageID, hit.EndAt, hit.Content, true
}

// authorise says whether this run may open the conversation, and which card
// opened it.
//
// The run's own conversation is always open. Another one is open when a card
// in this run pointed at it, or when the card index names one now - and then
// the card is what every read in that conversation is checked against.
func (r *SearchHistorySegmentsRegistry) authorise(run serviceagent.RunContext, session string,
	cards map[string]segmentCard) (string, uint64, uint, bool) {
	if session == strings.TrimSpace(run.SessionID) {
		return "", 0, 0, true
	}
	if run.GrantSet != nil {
		if run.GrantSet.AuthorisesSession(run, session) {
			cardID, cardVersion, watermark, ok := run.GrantSet.CardAuthorisation(session)
			if ok {
				return cardID, cardVersion, watermark, true
			}
			// The conversation is open, but nothing in it may be read
			// against a card this run did not show.
			return "", 0, 0, true
		}
	}
	card, found := cards[session]
	if !found || card.CardID == "" || card.CardVersion == 0 {
		return "", 0, 0, false
	}
	if run.GrantSet != nil {
		run.GrantSet.ExtendCardScope(run, serviceagent.CardScope{
			SessionID: session, CardID: card.CardID, CardVersion: card.CardVersion,
			// The ceiling of every read in that conversation. A scope
			// without one puts the ceiling at zero, and a ceiling of zero
			// hides every message of the conversation the hit points at.
			CardWatermark: card.Watermark,
		})
	}
	return card.CardID, card.CardVersion, card.Watermark, true
}

// historySegmentPoints reads the points out of a segment's body.
//
// A body that cannot be read has no points, and the text falls back to the
// topic: the segment still says which stretch of turns it was.
func historySegmentPoints(content string) []model.SessionSummarySegmentPoint {
	if strings.TrimSpace(content) == "" {
		return nil
	}
	decoded, err := model.DecodeSessionSummarySegmentContent(content)
	if err != nil {
		return nil
	}
	return decoded.Points
}

// historySegmentText renders the points as the text the model reads.
func historySegmentText(points []model.SessionSummarySegmentPoint, topic string) string {
	parts := make([]string, 0, len(points))
	for _, point := range points {
		rendered := serviceagent.RenderSummarySegmentPoint(point)
		if strings.TrimSpace(rendered) == "" {
			continue
		}
		parts = append(parts, rendered)
	}
	text := strings.Join(parts, "\n")
	if strings.TrimSpace(text) == "" {
		text = topic
	}
	bounded, _ := boundRunes(text, maxHistorySegmentRunes)
	return bounded
}

// historySegmentGrants draws the read-back authorisations for one segment.
//
// The ids come from the points the segment carries, and they are bounded by
// the segment's own stretch of turns: a message before the segment starts or
// after it ends was not one of the messages this segment compressed.
func historySegmentGrants(session string, seq int, startMessageID uint, endMessageID int,
	points []model.SessionSummarySegmentPoint, cardID string, cardVersion uint64,
	watermark uint) []serviceagent.HistoryMessageGrant {
	grants := make([]serviceagent.HistoryMessageGrant, 0, 8)
	for _, point := range points {
		for _, id := range point.EvidenceMessageIDs {
			if id == 0 || endMessageID > 0 && uint(endMessageID) < id {
				continue
			}
			if startMessageID > 0 && id < startMessageID {
				continue
			}
			grants = append(grants, serviceagent.HistoryMessageGrant{
				MessageID: id, SourceKind: serviceagent.HistorySourceSummarySegment,
				SourceSessionID: session, SegmentSeq: seq,
				SegmentStartMessageID: startMessageID,
				SegmentEndMessageID:   uint(endMessageID),
				CardID:                cardID, CardVersion: cardVersion, CardWatermark: watermark,
			})
		}
	}
	return grants
}

// historySegmentTokens is what one segment costs the answer.
func historySegmentTokens(view HistorySegmentHitView) int {
	tokens := rag.EstimateTokens(view.Topic) + rag.EstimateTokens(view.Text) + 12
	return tokens
}
